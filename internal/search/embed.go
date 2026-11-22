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

	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/providers"
)

// Embeddings is a workspace's embeddings model: one of its provider accounts, and a model
// that account's endpoint serves at /embeddings.
type Embeddings struct {
	Account  int64  `json:"account"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// key names the model in search_docs.embed_key: vectors of another model never mix in.
func (e Embeddings) key() string { return strconv.FormatInt(e.Account, 10) + "/" + e.Model }

// keySQL is key() in SQL, over search_settings e.
const keySQL = `(e.account_id || '/' || e.model)`

// maxModelLen is the longest model ID, as for provider accounts.
const maxModelLen = 256

// errNoEmbeddings is for a workspace without an embeddings model.
var errNoEmbeddings = errors.New("no embeddings model")

// embeddings returns workspaceID's embeddings model and its endpoint, or errNoEmbeddings.
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
		// Deleted since: the setting goes with it (ON DELETE CASCADE).
		return Embeddings{}, providers.Endpoint{}, errNoEmbeddings
	}
	e.Provider = ep.Provider
	return e, ep, err
}

// setEmbeddings sets workspaceID's embeddings model, or removes it if e is nil. Vectors of
// another model are dropped, and the workspace's documents wait to be embedded again.
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
		if err != nil || (e != nil && e.key() == old) {
			return err
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

// Consent says whether a space's documents may go to the workspace's embeddings endpoint.
type Consent struct {
	// Enabled is true while the workspace's endpoint is the one consent was given for.
	Enabled bool `json:"enabled"`
	// Endpoint is the workspace's embeddings endpoint now, which consent would be given
	// for; null without an embeddings model.
	Endpoint *ConsentEndpoint `json:"endpoint"`
	// GivenBy and GivenAt are of the consent on record, for this endpoint or an earlier
	// one; null without one.
	GivenBy *int64  `json:"given_by"`
	GivenAt *string `json:"given_at"`
}

// ConsentEndpoint is an embeddings endpoint, as consent names it.
type ConsentEndpoint struct {
	Account  int64  `json:"account"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	BaseURL  string `json:"base_url"`
}

var (
	// errNoConsentEndpoint is for consent asked of a workspace without an embeddings model.
	errNoConsentEndpoint = errors.New("no embeddings model to consent to")
	// errConsentEndpointChanged is for consent to an endpoint that isn't the workspace's now.
	errConsentEndpointChanged = errors.New("the embeddings endpoint has changed")
)

// consent returns spaceID's consent, in workspaceID.
func (x *Index) consent(ctx context.Context, workspaceID, spaceID int64) (Consent, error) {
	var c Consent
	e, ep, err := x.embeddings(ctx, workspaceID)
	switch {
	case err == nil:
		c.Endpoint = &ConsentEndpoint{Account: e.Account, Provider: e.Provider, Model: e.Model, BaseURL: ep.BaseURL}
	case !errors.Is(err, errNoEmbeddings):
		return Consent{}, err
	}
	var account int64
	var model, baseURL, at string
	var by sql.NullInt64
	err = x.db.QueryRowContext(ctx, `SELECT account_id, model, base_url, given_by, given_at
		FROM search_embedding_consent WHERE space_id = ?`, spaceID).Scan(&account, &model, &baseURL, &by, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return Consent{}, err
	}
	c.GivenAt = &at
	if by.Valid {
		c.GivenBy = &by.Int64
	}
	c.Enabled = c.Endpoint != nil && account == c.Endpoint.Account && model == c.Endpoint.Model && baseURL == c.Endpoint.BaseURL
	return c, nil
}

// setConsent gives consent, as userID, to send spaceID's documents to workspaceID's
// embeddings endpoint as it is now, or errNoConsentEndpoint without one; or withdraws it:
// then the space's vectors are dropped, and its documents are searched by their text only.
// With want, consent is given only if the endpoint is still want (its account, model and
// base URL), else errConsentEndpointChanged.
func (x *Index) setConsent(ctx context.Context, workspaceID, spaceID, userID int64, on bool, want *ConsentEndpoint) error {
	const docs = `SELECT id FROM search_docs WHERE space_id = ?`
	// dropVectors drops the space's vectors: they are only ever of the endpoint consented to.
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
	err := x.write(ctx, func(tx *sql.Tx) error {
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
			// The endpoint is read in the same transaction: consent is to what it is now.
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
				// Consent to another endpoint (the same account and model at a new base
				// URL, say): vectors from the old one don't mix in.
				return dropVectors(tx)
			}
			// Failures from before consent was last withdrawn don't delay it.
			_, err = tx.ExecContext(ctx, `UPDATE search_docs SET embed_tries = 0, embed_after = NULL WHERE space_id = ?`, spaceID)
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM search_embedding_consent WHERE space_id = ?`, spaceID); err != nil {
			return err
		}
		return dropVectors(tx)
	})
	x.vectors.changedSpace(spaceID)
	if err == nil && on {
		wake(x.embedWake)
	}
	return err
}

// embeddableSQL is true for a space sp whose documents, and the queries searching it, may
// go to the workspace's embeddings endpoint, search_settings e: someone with the right to
// consented to that very endpoint (search_embedding_consent).
const embeddableSQL = `EXISTS (SELECT 1 FROM search_embedding_consent c
	JOIN provider_accounts pa ON pa.id = c.account_id
	WHERE c.space_id = sp.id AND c.account_id = e.account_id AND c.model = e.model AND c.base_url = pa.base_url)`

// pendingSQL selects, from search_docs d joined to spaces sp and search_settings e, the
// Markdown documents whose vectors aren't those of their version and model, in spaces
// consented to that model, and whose text is indexed (a migration to a new textFormat
// empties search_fts until the next sync).
const pendingSQL = `FROM search_docs d
	JOIN spaces sp ON sp.id = d.space_id
	JOIN search_settings e ON e.workspace_id = sp.workspace_id
	WHERE d.markdown AND ` + embeddableSQL + `
	AND EXISTS (SELECT 1 FROM search_fts f WHERE f.rowid = d.id)
	AND (d.embed_version IS NOT d.version OR d.embed_key IS NOT ` + keySQL + `)`

const (
	// embedBatch is how many documents one pass of the worker takes on.
	embedBatch = 8
	// inputsPerCall is how many chunks one call to an endpoint embeds.
	inputsPerCall = 32
	// retryFirst and retryMax bound the wait before embedding a document again after a
	// failure; it doubles with each failure in a row.
	retryFirst = 30 * time.Second
	retryMax   = time.Hour
)

// pending is a document waiting for its vectors.
type pending struct {
	id, space, workspace int64
	version              string
	title                string
	tries                int
}

// embedPending embeds up to embedBatch waiting documents whose time has come, and returns
// how many it tried: the worker calls it until none are left. A document that fails waits,
// longer after each failure, and never holds up the others.
func (x *Index) embedPending(ctx context.Context) (int, error) {
	now := x.now().UTC().Format(timeFormat)
	rows, err := x.db.QueryContext(ctx, `SELECT d.id, d.space_id, sp.workspace_id, d.version, d.title, d.embed_tries `+pendingSQL+`
		AND (d.embed_after IS NULL OR d.embed_after <= ?) ORDER BY d.embed_tries, d.id LIMIT ?`, now, embedBatch)
	if err != nil {
		return 0, err
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.space, &p.workspace, &p.version, &p.title, &p.tries); err != nil {
			_ = rows.Close()
			return 0, err
		}
		batch = append(batch, p)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return 0, err
	}
	for _, p := range batch {
		if err := x.embedDoc(ctx, p); err != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			x.log.Warn("search: embedding a document failed; it will be retried", "doc", p.id, "tries", p.tries+1, "err", err)
			wait := min(retryFirst<<min(p.tries, 16), retryMax)
			// Only for the version that failed: one changed meanwhile starts afresh.
			if _, err := x.db.ExecContext(ctx, `UPDATE search_docs SET embed_tries = embed_tries + 1, embed_after = ?
				WHERE id = ? AND version = ?`, x.now().Add(wait).UTC().Format(timeFormat), p.id, p.version); err != nil {
				return 0, err
			}
		}
	}
	return len(batch), nil
}

// embedDoc embeds the document p and stores its chunks, unless it changed meanwhile.
func (x *Index) embedDoc(ctx context.Context, p pending) error {
	e, ep, err := x.embeddings(ctx, p.workspace)
	if errors.Is(err, errNoEmbeddings) {
		return nil // removed since the batch was read
	}
	if err != nil {
		return err
	}
	var body string
	if err := x.db.QueryRowContext(ctx, `SELECT body FROM search_fts WHERE rowid = ?`, p.id).Scan(&body); err != nil {
		return err
	}
	chunks := chunk(unsegment(body))
	vectors := make([][]float32, 0, len(chunks))
	for start := 0; start < len(chunks); start += inputsPerCall {
		inputs := make([]string, 0, inputsPerCall)
		for _, c := range chunks[start:min(start+inputsPerCall, len(chunks))] {
			// The title gives each passage its context.
			inputs = append(inputs, p.title+"\n\n"+c)
		}
		vs, err := x.embed(ctx, ep, e.Model, inputs)
		if err != nil {
			return err
		}
		vectors = append(vectors, vs...)
	}
	// Once written, or not: the vector cache reads the document again either way.
	defer x.vectors.changedDocs(p.space, p.id)
	return x.write(ctx, func(tx *sql.Tx) error {
		var version string
		var key sql.NullString
		var allowed bool
		err := tx.QueryRowContext(ctx, `SELECT d.version, `+keySQL+`, `+embeddableSQL+` FROM search_docs d
			JOIN spaces sp ON sp.id = d.space_id LEFT JOIN search_settings e ON e.workspace_id = sp.workspace_id
			WHERE d.id = ?`, p.id).Scan(&version, &key, &allowed)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (version != p.version || key.String != e.key() || !allowed)) {
			// Changed meanwhile, or consent withdrawn or no longer for this endpoint: keep
			// nothing; the next pass embeds what it is now, if it may.
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
	// maxDims is the most dimensions a vector may have.
	maxDims = 16384
	// maxEmbedReply is the largest reply read from an endpoint, in bytes.
	maxEmbedReply = 64 << 20
)

// errEndpoint is for an endpoint that didn't answer with embeddings. Its messages never
// carry what the endpoint said, which could echo the key or the text.
var errEndpoint = errors.New("the embeddings endpoint failed")

// embed asks ep for model's embeddings of inputs, OpenAI's way (POST {base}/embeddings),
// and returns them normalized to length 1, in the order of inputs.
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
		// The URL is the account's, which its admins set: no key in it.
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

// normalize returns v scaled to length 1 as float32s, or nil if it is empty, zero or not
// finite.
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
	// chunkSize is the longest passage embedded, in characters (about 400 tokens), and
	// chunkOverlap how much of a long section's passages share with the one before.
	chunkSize    = 1500
	chunkOverlap = 200
	// maxChunks is the most passages of one document that are embedded: the rest of a
	// very long document is found by full text only.
	maxChunks = 128
)

// chunk splits a Markdown document into passages to embed: at its headings (outside code
// blocks), packing short sections together, and a section too long for one passage into
// overlapping windows broken at a paragraph, a line or a word where it can be.
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

// isHeading reports whether a trimmed line is an ATX heading.
func isHeading(t string) bool {
	n := len(t) - len(strings.TrimLeft(t, "#"))
	return n >= 1 && n <= 6 && (len(t) == n || t[n] == ' ')
}

// windows splits s into passages of at most chunkSize characters, each starting
// chunkOverlap characters before the last one ended.
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

// breakAt returns where to end a passage in r[from:to]: after its last blank line, else
// its last line end, else its last space, else at to.
func breakAt(r []rune, from, to int) int {
	s := string(r[from:to])
	for _, sep := range []string{"\n\n", "\n", " "} {
		if i := strings.LastIndex(s, sep); i >= 0 {
			return from + utf8.RuneCountInString(s[:i+len(sep)])
		}
	}
	return to
}
