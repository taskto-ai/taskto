package export

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/auth"
)

const membersPage = 500

func (s *Service) prepareWorkspace(ctx context.Context, u auth.User, m auth.Membership) (*run, error) {
	readable, err := s.spaces.Readable(ctx, u.ID, m.WorkspaceID)
	if err != nil {
		return nil, err
	}
	ex := &run{svc: s, user: u, ws: m, at: s.now().UTC(), names: newNames()}
	for _, sp := range readable {
		if sp.Kind == access.Shared && sp.Role != "" {
			ex.shared = append(ex.shared, sp)
		}
	}
	ex.mf = manifest{
		Format: Format, Scope: "workspace", ExportedAt: ex.at.Format(time.RFC3339),
		Workspace: manifestWS{ID: m.WorkspaceID, Name: m.WorkspaceName},
		User:      manifestUser{ID: u.ID, Name: u.Name, Email: u.Email},
		Counts:    map[string]int{"members": 0, "spaces": 0, "documents": 0, "boards": 0, "cards": 0, "comments": 0, "apps": 0, "records": 0},
		Spaces:    []manifestSpace{},
		Skipped:   []skipped{},
	}
	return ex, nil
}

type memberFile struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	Role     string `json:"role"`
	JoinedAt string `json:"joined_at"`
}

func (ex *run) members(ctx context.Context) error {
	if err := ex.checkAdmin(ctx); err != nil {
		return err
	}
	w, err := ex.entry(ex.names.claim("members.json"), false)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "{\n  \"members\": ["); err != nil {
		return err
	}
	var after int64
	n := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var page []auth.Member
		err := ex.readAsAdmin(ctx, func(tx *sql.Tx) (err error) {
			page, err = auth.Roster(ctx, tx, ex.ws.WorkspaceID, auth.RosterPage{ByID: true, AfterID: after, Limit: membersPage})
			return err
		})
		if errors.Is(err, errNotAdmin) {
			return err
		}
		if err != nil {
			return fmt.Errorf("reading the members: %w", err)
		}
		for _, mem := range page {
			line, err := compactJSON(memberFile{ID: mem.ID, Name: mem.Name, Email: mem.Email, Role: string(mem.Role), JoinedAt: mem.JoinedAt})
			if err != nil {
				return err
			}
			sep := ",\n    "
			if n == 0 {
				sep = "\n    "
			}
			n++
			if _, err := io.WriteString(w, sep); err != nil {
				return err
			}
			if _, err := w.Write(line); err != nil {
				return err
			}
		}
		if len(page) < membersPage {
			break
		}
		after = page[len(page)-1].ID
	}
	if n > 0 {
		if _, err := io.WriteString(w, "\n  "); err != nil {
			return err
		}
	}
	if _, err := io.WriteString(w, "]\n}\n"); err != nil {
		return err
	}
	ex.mf.Counts["members"] = n
	return nil
}

type spaceFile struct {
	ID         int64       `json:"id"`
	Name       string      `json:"name"`
	ArchivedAt *string     `json:"archived_at"`
	CreatedAt  string      `json:"created_at"`
	Role       access.Role `json:"role"`
}

func (ex *run) spaces(ctx context.Context) error {
	for _, sp := range ex.shared {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ex.space1(ctx, sp); err != nil {
			return err
		}
	}
	return nil
}

func (ex *run) space1(ctx context.Context, sp access.Space) error {
	p := place{space: sp, shared: true, dir: []string{"spaces", strconv.FormatInt(sp.ID, 10) + "-" + slug(sp.Name)}}
	cur, err := ex.current(ctx, p)
	if err != nil {
		return ex.leftOut(p, err)
	}
	file := ex.names.claim(p.path("space.json")...)
	if err := ex.json(file, spaceFile{ID: cur.ID, Name: cur.Name, ArchivedAt: cur.ArchivedAt, CreatedAt: cur.CreatedAt, Role: cur.Role}); err != nil {
		return err
	}
	ex.mf.Spaces = append(ex.mf.Spaces, manifestSpace{ID: cur.ID, Name: cur.Name, Dir: strings.TrimSuffix(file, "/space.json"), ArchivedAt: cur.ArchivedAt})
	ex.mf.Counts["spaces"]++
	for _, step := range []func(context.Context, place) error{ex.documents, ex.boards, ex.apps} {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := step(ctx, p); err != nil {
			return ex.leftOut(p, err)
		}
	}
	return nil
}

func (ex *run) leftOut(p place, err error) error {
	if errors.Is(err, errLostAccess) {
		ex.skip(p, "space", "access_lost", strconv.FormatInt(p.space.ID, 10))
		return nil
	}
	return err
}
