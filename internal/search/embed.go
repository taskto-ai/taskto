package search

import (
	"bytes"
	"cmp"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/providers"
)

type Embeddings struct {
	Account  int64  `json:"account"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

func (e Embeddings) key() string { return strconv.FormatInt(e.Account, 10) + "/" + e.Model }

const keySQL = `(e.account_id || '/' || e.model)`

const maxModelLen = 256

var errNoEmbeddings = errors.New("no embeddings model")

func (x *Index) embeddings(ctx context.Context, workspaceID int64) (Embeddings, providers.Endpoint, error) {
	e := Embeddings{}
	err := x.db.QueryRowContext(ctx, `SELECT account_id, model FROM search_settings WHERE workspace_id = ?`, workspaceID).
		Scan(&e.Account, &e.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return Embeddings{}, providers.Endpoint{}, errNoEmbeddings
	}
	if err != nil {
		return Embeddings{}, providers.Endpoint{}, err
	}
	ep, err := x.providers.WorkspaceEndpoint(ctx, workspaceID, e.Account)
	if errors.Is(err, providers.ErrAccountNotFound) {
		return Embeddings{}, providers.Endpoint{}, errNoEmbeddings
	}
	e.Provider = ep.Provider
	return e, ep, err
}

func (x *Index) setEmbeddings(ctx context.Context, workspaceID, userID int64, e *Embeddings) error {
	err := x.write(ctx, func(tx *sql.Tx) error {
		var old string
		err := tx.QueryRowContext(ctx, `SELECT `+keySQL+` FROM search_settings e WHERE workspace_id = ?`, workspaceID).Scan(&old)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if e == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM search_settings WHERE workspace_id = ?`, workspaceID)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO search_settings (workspace_id, account_id, model, updated_by) VALUES (?, ?, ?, ?)
				ON CONFLICT (workspace_id) DO UPDATE SET account_id = excluded.account_id, model = excluded.model,
					updated_by = excluded.updated_by, updated_at = excluded.updated_at`,
				workspaceID, e.Account, e.Model, userID)
		}
		if err != nil {
			return err
		}
		details := audit.Details{"enabled": e != nil}
		if e != nil {
			details["account"] = e.Account
		}
		if err := audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: userID, Action: audit.SearchEmbeddingsChanged,
			TargetType: audit.TargetWorkspace, Target: workspaceID, Details: details}); err != nil {
			return err
		}
		if e != nil && e.key() == old {
			return nil
		}
		const docs = `SELECT d.id FROM search_docs d JOIN spaces sp ON sp.id = d.space_id WHERE sp.workspace_id = ?`
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_chunks WHERE doc_id IN (`+docs+`)`, workspaceID); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE search_docs SET embed_version = NULL, embed_key = NULL, embed_tries = 0,
			embed_after = NULL WHERE id IN (`+docs+`)`, workspaceID)
		return err
	})
	x.vectors.changedWorkspace(workspaceID)
	if err == nil {
		wake(x.embedWake)
	}
	return err
}

type Consent struct {
	Enabled  bool             `json:"enabled"`
	Endpoint *ConsentEndpoint `json:"endpoint"`
	GivenBy  *int64           `json:"given_by"`
	GivenAt  *string          `json:"given_at"`
}

type ConsentEndpoint struct {
	Account  int64  `json:"account"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
}

var (
	errNoConsentEndpoint      = errors.New("no embeddings model to consent to")
	errConsentEndpointChanged = errors.New("the embeddings endpoint has changed")
	errConsentForbidden       = errors.New("only the space's owner or one of its editors can change this")
)

func (x *Index) consent(ctx context.Context, sp access.Space) (Consent, error) {
	var c Consent
	e, ep, err := x.embeddings(ctx, sp.WorkspaceID)
	switch {
	case err == nil:
		c.Endpoint = &ConsentEndpoint{Account: e.Account, Provider: e.Provider, Model: e.Model, BaseURL: ep.BaseURL}
	case !errors.Is(err, errNoEmbeddings):
		return Consent{}, err
	}
	var account int64
	var model, baseURL, at string
	var by sql.NullInt64
	var there int
	err = x.db.QueryRowContext(ctx, `SELECT 1, coalesce(c.account_id, 0), coalesce(c.model, ''), coalesce(c.base_url, ''), c.given_by, coalesce(c.given_at, '')
		FROM spaces sp LEFT JOIN search_embedding_consent c ON c.space_id = sp.id
		WHERE sp.id = ? AND sp.workspace_id = ? AND (? = 0 OR sp.generation = ?)`,
		sp.ID, sp.WorkspaceID, sp.Generation, sp.Generation).Scan(&there, &account, &model, &baseURL, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return Consent{}, fmt.Errorf("%w: the space is gone, or is another space with the ID", access.ErrNotFound)
	}
	if err != nil {
		return Consent{}, err
	}
	if at == "" {
		return c, nil
	}
	c.GivenAt = &at
	if by.Valid {
		c.GivenBy = &by.Int64
	}
	c.Enabled = c.Endpoint != nil && account == c.Endpoint.Account && model == c.Endpoint.Model && baseURL == c.Endpoint.BaseURL
	return c, nil
}

func (x *Index) setConsent(ctx context.Context, sp access.Space, userID int64, on bool, want *ConsentEndpoint,
	authorize func(ctx context.Context, tx *sql.Tx) error) error {
	workspaceID, spaceID := sp.WorkspaceID, sp.ID
	const docs = `SELECT id FROM search_docs WHERE space_id = ?`
	dropVectors := func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_chunks WHERE doc_id IN (`+docs+`)`, spaceID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE search_docs SET embed_version = NULL, embed_key = NULL, embed_tries = 0,
			embed_after = NULL WHERE space_id = ?`, spaceID)
		return err
	}
	const endpointSQL = `SELECT coalesce((SELECT account_id || ' ' || model || ' ' || base_url
		FROM search_embedding_consent WHERE space_id = ?), '')`
	apply := func(tx *sql.Tx) error {
		if on {
			var before, after string
			if err := tx.QueryRowContext(ctx, endpointSQL, spaceID).Scan(&before); err != nil {
				return err
			}
			if want != nil {
				var now ConsentEndpoint
				err := tx.QueryRowContext(ctx, `SELECT e.account_id, e.model, pa.base_url FROM search_settings e
					JOIN provider_accounts pa ON pa.id = e.account_id WHERE e.workspace_id = ?`, workspaceID).
					Scan(&now.Account, &now.Model, &now.BaseURL)
				if errors.Is(err, sql.ErrNoRows) {
					return errNoConsentEndpoint
				}
				if err != nil {
					return err
				}
				if now.Account != want.Account || now.Model != want.Model || now.BaseURL != want.BaseURL {
					return errConsentEndpointChanged
				}
			}
			res, err := tx.ExecContext(ctx, `INSERT INTO search_embedding_consent (space_id, account_id, model, base_url, given_by)
				SELECT ?, e.account_id, e.model, pa.base_url, ? FROM search_settings e
				JOIN provider_accounts pa ON pa.id = e.account_id WHERE e.workspace_id = ?
				ON CONFLICT (space_id) DO UPDATE SET account_id = excluded.account_id, model = excluded.model,
					base_url = excluded.base_url, given_by = excluded.given_by, given_at = excluded.given_at`,
				spaceID, userID, workspaceID)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil || n == 0 {
				return cmp.Or(err, errNoConsentEndpoint)
			}
			if err := tx.QueryRowContext(ctx, endpointSQL, spaceID).Scan(&after); err != nil {
				return err
			}
			if after != before {
				return dropVectors(tx)
			}
			_, err = tx.ExecContext(ctx, `UPDATE search_docs SET embed_tries = 0, embed_after = NULL WHERE space_id = ?`, spaceID)
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_embedding_consent WHERE space_id = ?`, spaceID); err != nil {
			return err
		}
		return dropVectors(tx)
	}
	err := x.write(ctx, func(tx *sql.Tx) error {
		if err := stillSpace(ctx, tx, sp); err != nil {
			return err
		}
		if authorize != nil {
			if err := authorize(ctx, tx); err != nil {
				return err
			}
		}
		if err := apply(tx); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Event{Workspace: workspaceID, Actor: userID, Action: audit.SearchConsentChanged,
			TargetType: audit.TargetSpace, Target: spaceID, Details: audit.Details{"enabled": on}})
	})
	if err == nil {
		x.vectors.changedSpace(spaceID)
		if on {
			wake(x.embedWake)
		}
	}
	return err
}

const embeddableSQL = `EXISTS (SELECT 1 FROM search_embedding_consent c
	JOIN provider_accounts pa ON pa.id = c.account_id
	WHERE c.space_id = sp.id AND c.account_id = e.account_id AND c.model = e.model AND c.base_url = pa.base_url)`

const embeddableAtSQL = `EXISTS (SELECT 1 FROM search_embedding_consent c
	JOIN provider_accounts pa ON pa.id = c.account_id
	WHERE c.space_id = sp.id AND c.account_id = e.account_id AND c.model = e.model AND c.base_url = pa.base_url
		AND c.base_url = ?)`

const pendingSQL = `FROM search_docs d
	JOIN spaces sp ON sp.id = d.space_id
	JOIN search_settings e ON e.workspace_id = sp.workspace_id
	WHERE d.markdown AND ` + embeddableSQL + `
	AND EXISTS (SELECT 1 FROM search_fts f WHERE f.rowid = d.id)
	AND (d.embed_version IS NOT d.version OR d.embed_key IS NOT ` + keySQL + `)`

const (
	embedBatch    = 8
	inputsPerCall = 32
	retryFirst    = 30 * time.Second
	retryMax      = time.Hour
)

type pending struct {
	id, space, workspace, generation int64
	version                          string
	title                            string
	tries                            int
	key, url                         string
}

func (x *Index) embedPending(ctx context.Context) (int, error) {
	now := x.now().UTC().Format(timeFormat)
	rows, err := x.db.QueryContext(ctx, `SELECT d.id, d.space_id, sp.workspace_id, sp.generation, d.version, d.title, d.embed_tries `+pendingSQL+`
		AND (d.embed_after IS NULL OR d.embed_after <= ?) ORDER BY d.embed_tries, d.id LIMIT ?`, now, embedBatch)
	if err != nil {
		return 0, err
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.space, &p.workspace, &p.generation, &p.version, &p.title, &p.tries); err != nil {
			_ = rows.Close()
			return 0, err
		}
		batch = append(batch, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	if x.afterPending != nil {
		x.afterPending()
	}
	for _, p := range batch {
		if err := x.embedDoc(ctx, &p); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			x.log.Warn("search: embedding a document failed; it will be retried", "doc", p.id, "tries", p.tries+1, "err", err)
			wait := min(retryFirst<<min(p.tries, 16), retryMax)
			if _, err := x.db.ExecContext(ctx, `UPDATE search_docs SET embed_tries = embed_tries + 1, embed_after = ?
				WHERE id = ? AND version = ?
				AND EXISTS (SELECT 1 FROM spaces sp WHERE sp.id = search_docs.space_id AND sp.workspace_id = ? AND sp.generation = ?)
				AND (? = '' OR EXISTS (SELECT 1 FROM search_settings e JOIN provider_accounts pa ON pa.id = e.account_id
					JOIN spaces sp ON sp.id = search_docs.space_id
					WHERE e.workspace_id = sp.workspace_id AND `+keySQL+` = ? AND pa.base_url = ?))`,
				x.now().Add(wait).UTC().Format(timeFormat), p.id, p.version, p.workspace, p.generation, p.key, p.key, p.url); err != nil {
				return 0, err
			}
		}
	}
	return len(batch), nil
}

func (x *Index) embedDoc(ctx context.Context, p *pending) error {
	e, ep, err := x.embeddings(ctx, p.workspace)
	if errors.Is(err, errNoEmbeddings) {
		return nil
	}
	if err != nil {
		return err
	}
	p.key, p.url = e.key(), ep.BaseURL
	if x.afterEndpoint != nil {
		x.afterEndpoint()
	}
	var body string
	err = x.db.QueryRowContext(ctx, `SELECT f.body FROM search_docs d
		JOIN search_fts f ON f.rowid = d.id
		JOIN spaces sp ON sp.id = d.space_id
		JOIN search_settings e ON e.workspace_id = sp.workspace_id
		WHERE d.id = ? AND d.version = ? AND d.markdown AND sp.workspace_id = ? AND sp.generation = ?
		AND `+keySQL+` = ? AND `+embeddableAtSQL, p.id, p.version, p.workspace, p.generation, e.key(), ep.BaseURL).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	chunks := chunk(unsegment(body))
	vectors := make([][]float32, 0, len(chunks))
	for start := 0; start < len(chunks); start += inputsPerCall {
		if start > 0 {
			var one int
			err := x.db.QueryRowContext(ctx, `SELECT 1 FROM search_docs d
				JOIN spaces sp ON sp.id = d.space_id
				JOIN search_settings e ON e.workspace_id = sp.workspace_id
				WHERE d.id = ? AND d.version = ? AND d.markdown AND sp.workspace_id = ? AND sp.generation = ?
				AND `+keySQL+` = ? AND `+embeddableAtSQL, p.id, p.version, p.workspace, p.generation, e.key(), ep.BaseURL).Scan(&one)
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
		}
		inputs := make([]string, 0, inputsPerCall)
		for _, c := range chunks[start:min(start+inputsPerCall, len(chunks))] {
			inputs = append(inputs, p.title+"\n\n"+c)
		}
		vs, err := x.embed(ctx, ep, e.Model, inputs)
		if x.afterEmbedRequest != nil {
			x.afterEmbedRequest()
		}
		if err != nil {
			return err
		}
		vectors = append(vectors, vs...)
	}
	defer x.vectors.changedDocs(p.space, p.id)
	if x.beforeStore != nil {
		x.beforeStore()
	}
	return x.write(ctx, func(tx *sql.Tx) error {
		var version string
		var key sql.NullString
		var allowed bool
		err := tx.QueryRowContext(ctx, `SELECT d.version, `+keySQL+`, `+embeddableAtSQL+` FROM search_docs d
			JOIN spaces sp ON sp.id = d.space_id LEFT JOIN search_settings e ON e.workspace_id = sp.workspace_id
			WHERE d.id = ? AND sp.workspace_id = ? AND sp.generation = ?`, ep.BaseURL, p.id, p.workspace, p.generation).Scan(&version, &key, &allowed)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (version != p.version || key.String != e.key() || !allowed)) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_chunks WHERE doc_id = ?`, p.id); err != nil {
			return err
		}
		for i, c := range chunks {
			if _, err := tx.ExecContext(ctx, `INSERT INTO search_chunks (doc_id, seq, text, vector) VALUES (?, ?, ?, ?)`,
				p.id, i, c, encodeVector(vectors[i])); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE search_docs SET embed_version = ?, embed_key = ?, embed_tries = 0, embed_after = NULL
			WHERE id = ?`, p.version, e.key(), p.id)
		return err
	})
}

const (
	maxDims       = 16384
	maxEmbedReply = 64 << 20
)

var errEndpoint = errors.New("the embeddings endpoint failed")

func (x *Index) embed(ctx context.Context, ep providers.Endpoint, model string, inputs []string) ([][]float32, error) {
	body, err := json.Marshal(map[string]any{"model": model, "input": inputs})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if ep.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}
	resp, err := x.client.Do(req)
	if refused := egress.Refusal(err); refused != nil {
		return nil, fmt.Errorf("%w: %w", errEndpoint, refused)
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %s: no answer", errEndpoint, ep)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: %s answered %d", errEndpoint, ep, resp.StatusCode)
	}
	var reply struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxEmbedReply)).Decode(&reply); err != nil {
		return nil, fmt.Errorf("%w: %s: the reply isn't embeddings", errEndpoint, ep)
	}
	if len(reply.Data) != len(inputs) {
		return nil, fmt.Errorf("%w: %s returned %d embeddings for %d inputs", errEndpoint, ep, len(reply.Data), len(inputs))
	}
	out := make([][]float32, len(inputs))
	for _, d := range reply.Data {
		if d.Index < 0 || d.Index >= len(out) || out[d.Index] != nil {
			return nil, fmt.Errorf("%w: %s: the reply's indexes are wrong", errEndpoint, ep)
		}
		v := normalize(d.Embedding)
		if v == nil || len(v) > maxDims || len(v) != len(reply.Data[0].Embedding) {
			return nil, fmt.Errorf("%w: %s: an embedding is empty, too long, not finite or of another size", errEndpoint, ep)
		}
		out[d.Index] = v
	}
	return out, nil
}

func normalize(v []float64) []float32 {
	var sum float64
	for _, f := range v {
		sum += f * f
	}
	norm := math.Sqrt(sum)
	if len(v) == 0 || norm == 0 || math.IsInf(norm, 0) || math.IsNaN(norm) {
		return nil
	}
	out := make([]float32, len(v))
	for i, f := range v {
		out[i] = float32(f / norm)
	}
	return out
}

func encodeVector(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(f))
	}
	return b
}

const (
	chunkSize    = 1500
	chunkOverlap = 200
	maxChunks    = 128
)

func chunk(body string) []string {
	var sections []string
	var cur strings.Builder
	fence := ""
	for _, line := range strings.SplitAfter(body, "\n") {
		t := strings.TrimSpace(line)
		if f := codeFence(t); f != "" {
			switch {
			case fence == "":
				fence = f
			case strings.HasPrefix(t, fence):
				fence = ""
			}
		} else if fence == "" && isHeading(t) && cur.Len() > 0 {
			sections = append(sections, cur.String())
			cur.Reset()
		}
		cur.WriteString(line)
	}
	sections = append(sections, cur.String())

	var out []string
	buf := ""
	for _, s := range sections {
		s = strings.TrimSpace(s)
		switch {
		case s == "":
		case buf != "" && utf8.RuneCountInString(buf)+2+utf8.RuneCountInString(s) <= chunkSize:
			buf += "\n\n" + s
		default:
			if buf != "" {
				out = append(out, buf)
			}
			buf = ""
			if utf8.RuneCountInString(s) <= chunkSize {
				buf = s
			} else {
				out = append(out, windows(s)...)
			}
		}
		if len(out) >= maxChunks {
			return out[:maxChunks]
		}
	}
	if buf != "" {
		out = append(out, buf)
	}
	return out[:min(len(out), maxChunks)]
}

func isHeading(t string) bool {
	n := len(t) - len(strings.TrimLeft(t, "#"))
	return n >= 1 && n <= 6 && (len(t) == n || t[n] == ' ')
}

func windows(s string) []string {
	r := []rune(s)
	var out []string
	for start := 0; start < len(r) && len(out) < maxChunks; {
		end := min(start+chunkSize, len(r))
		if end < len(r) {
			end = breakAt(r, start+chunkSize/2, end)
		}
		if p := strings.TrimSpace(string(r[start:end])); p != "" {
			out = append(out, p)
		}
		if end == len(r) {
			break
		}
		start = end - chunkOverlap
	}
	return out
}

func breakAt(r []rune, from, to int) int {
	s := string(r[from:to])
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(s, sep); i >= 0 {
			return from + utf8.RuneCountInString(s[:i+len(sep)])
		}
	}
	return to
}
