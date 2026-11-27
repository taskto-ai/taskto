package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Entry struct {
	User      int64
	Workspace int64
	Source    string
	Engine    string
	Input     int64
	Output    int64
	At        time.Time
}

type Day struct {
	Day          string `json:"day"`
	Source       string `json:"source"`
	Engine       string `json:"engine"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Turns        int64  `json:"turns"`
}

func Record(ctx context.Context, db *sql.DB, e Entry) error {
	if e.User < 1 || e.Workspace < 1 || (e.Source != "cli" && e.Source != "agent") || len(e.Engine) < 1 || len(e.Engine) > 64 || e.Input < 0 || e.Output < 0 || e.At.IsZero() {
		return errors.New("invalid usage entry")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_usage_daily
		(user_id, workspace_id, day, source, engine, input_tokens, output_tokens, turns)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT (user_id, workspace_id, day, source, engine) DO UPDATE SET
		input_tokens = input_tokens + excluded.input_tokens,
		output_tokens = output_tokens + excluded.output_tokens,
		turns = turns + 1`, e.User, e.Workspace, e.At.UTC().Format("2006-01-02"), e.Source, e.Engine, e.Input, e.Output); err != nil {
		return fmt.Errorf("record daily agent usage: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM agent_usage_daily WHERE user_id = ? AND day < date('now', '-400 days')`, e.User); err != nil {
		return fmt.Errorf("prune daily agent usage: %w", err)
	}
	return tx.Commit()
}

func Daily(ctx context.Context, db *sql.DB, user, workspace int64, from, to time.Time) ([]Day, error) {
	if user < 1 || workspace < 1 || from.IsZero() || to.IsZero() || from.After(to) {
		return nil, errors.New("invalid daily usage range")
	}
	rows, err := db.QueryContext(ctx, `SELECT day, source, engine, input_tokens, output_tokens, turns
		FROM agent_usage_daily WHERE user_id = ? AND workspace_id = ? AND day >= ? AND day <= ?
		ORDER BY day, source, engine`, user, workspace, from.UTC().Format("2006-01-02"), to.UTC().Format("2006-01-02"))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []Day
	for rows.Next() {
		var d Day
		if err := rows.Scan(&d.Day, &d.Source, &d.Engine, &d.InputTokens, &d.OutputTokens, &d.Turns); err != nil {
			return nil, err
		}
		days = append(days, d)
	}
	return days, rows.Err()
}
