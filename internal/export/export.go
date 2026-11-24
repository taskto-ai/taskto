// Package export lets a person download their own data in a workspace as a zip
// (docs/api.md#export-my-data): the documents of their private space, their personal board's
// cards with their comments, their conversations, and the records of the apps in their private
// space.
//
// Only what the person owns goes in. The export reads through the packages that own the data
// (internal/knowledge, internal/boards, internal/apps, internal/conversations), each of which
// checks the person's access, and it only ever names the person's own private space: a shared
// space's content, and other people's records and conversations, are never read. It never
// writes anything: the knowledge base's git repositories are only read, through
// knowledge.Store.
//
// Starting an export is a POST (it uses one of the person's few exports of the hour and is
// recorded on the audit log), which hands back a short-lived, single-use download address bound
// to the person's session; the download is a GET of that address, which only reads (ticket.go).
//
// The zip is streamed: it is written to the response as it is made, one entry at a time, and a
// file's bytes are copied through, never held, so a large file costs a buffer, not its size. What
// is held at a time, besides the zip's own header for each entry so far, is a page of a listing,
// a card's thread, a page of a conversation's history or one app's records. The documents are read at one commit of the knowledge base, taken when
// the export begins.
package export

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/audit"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/boards"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/knowledge"
)

// Format is the version of the zip's layout, in manifest.json. It changes when a reader of the
// zip would need to change.
const Format = 1

// maxSkipped bounds the manifest's list of what the export left out.
const maxSkipped = 1000

// Options are the export's limits; zero values are the defaults, and tests set their own.
type Options struct {
	// PerPerson is how many exports a person may start per Window; negative: no limit.
	PerPerson int
	// Window is how long PerPerson counts.
	Window time.Duration
	// AtOnce is how many exports the whole server makes at a time; negative: no limit.
	AtOnce int
	// Now is the clock; nil is time.Now.
	Now func() time.Time
	// TicketTTL is how long a download address is good for; zero is a minute.
	TicketTTL time.Duration
	// IdleTimeout is how long the export waits for the person's connection to take what it
	// has written, before it gives up and frees its slot; zero is two minutes.
	IdleTimeout time.Duration
}

// Service serves the export.
type Service struct {
	db            *sql.DB
	spaces        *access.Checker
	knowledge     *knowledge.Store // nil: the server has no knowledge base
	boards        *boards.Service
	apps          *apps.Service
	conversations *conversations.Service // nil: the server has none
	gate          *gate
	tickets       *tickets
	idle          time.Duration
	auth          *auth.Service
	now           func() time.Time
}

// New returns the export over the services of one server. kb and convs may be nil, when the
// server runs without a knowledge base or conversations: the zip then lacks them, and its
// manifest says so.
func New(sqlDB *sql.DB, spaces *access.Checker, kb *knowledge.Store, b *boards.Service, a *apps.Service, convs *conversations.Service, opts Options) *Service {
	g := newGate(opts.PerPerson, opts.Window, opts.AtOnce, opts.Now)
	idle := opts.IdleTimeout
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	return &Service{db: sqlDB, spaces: spaces, knowledge: kb, boards: b, apps: a, conversations: convs, gate: g,
		tickets: newTickets(opts.TicketTTL), idle: idle, now: g.now}
}

// Path is the route that starts an export, for the workspace named in the path; DownloadPath is
// the address it hands back.
const (
	Path         = "/api/workspaces/{workspace}/export"
	DownloadPath = Path + "/{token}"
)

// Routes mounts the export on mux behind a's membership check. It doesn't accept access tokens
// (auth.AcceptTokens), so a leaked token can't take a person's data out.
func (s *Service) Routes(mux *http.ServeMux, a *auth.Service) {
	s.auth = a
	mux.Handle("POST "+Path, a.RequireMember(http.HandlerFunc(s.handleStart)))
	mux.Handle("GET "+DownloadPath, a.RequireMember(http.HandlerFunc(s.handleDownload)))
}

// started is the answer of a start: where to get the zip, and for how many seconds.
type started struct {
	Download  string `json:"download"`
	ExpiresIn int    `json:"expires_in"`
}

// handleStart is POST /api/workspaces/{workspace}/export: it takes the person's slot (one at a
// time, a few an hour), records the export on the audit log, and returns the address to download
// it from, once, within a minute, from this session. Only this request counts and is recorded, so
// a link someone else sends the person, or a page that navigates them, starts nothing.
func (s *Service) handleStart(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	session, ok, err := s.auth.SessionID(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "export: session", "user", u.ID, "err", err)
		apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
		return
	}
	if !ok {
		apierr.Write(w, http.StatusForbidden, apierr.CodeTokenRefused, "exporting your data needs a browser login")
		return
	}

	leave, why, wait := s.gate.enter(u.ID)
	if why != allowed {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
		if why == running {
			apierr.Write(w, http.StatusConflict, apierr.CodeExportRunning, "an export of your data is running already")
			return
		}
		apierr.Write(w, http.StatusTooManyRequests, apierr.CodeExportRateLimited, "too many exports: try again in a little while")
		return
	}
	// What can fail with a proper answer is read now, before the export is counted and before any
	// byte of it is sent: a failure here uses none of the person's hour.
	ex, err := s.prepare(r.Context(), u, m)
	if err != nil {
		s.gate.refund(u.ID)
		leave()
		slog.ErrorContext(r.Context(), "export: preparing", "user", u.ID, "workspace", m.WorkspaceID, "err", err)
		apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
		return
	}
	token, err := s.tickets.issue(u.ID, m.WorkspaceID, session, leave, ex)
	if err != nil {
		s.gate.refund(u.ID)
		leave()
		slog.ErrorContext(r.Context(), "export: ticket", "user", u.ID, "err", err)
		apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
		return
	}
	// Taking data out is a security event: recorded once the export is known to begin.
	audit.Log(r.Context(), s.db, audit.Event{Workspace: m.WorkspaceID, Actor: u.ID, Action: audit.DataExportStarted,
		TargetType: audit.TargetUser, Target: u.ID})

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(started{
		Download:  "/api/workspaces/" + strconv.FormatInt(m.WorkspaceID, 10) + "/export/" + token,
		ExpiresIn: max(1, int(math.Ceil(s.tickets.ttl.Seconds()))),
	})
}

// handleDownload is GET /api/workspaces/{workspace}/export/{token}: the zip of an export that was
// started. It changes nothing on the server, so a GET serves it; a HEAD (which would be the same
// route) is refused, so nothing but the download itself uses up the ticket.
func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		apierr.Write(w, http.StatusMethodNotAllowed, apierr.CodeBadRequest, "download the export with a GET")
		return
	}
	u, _ := auth.UserFrom(r.Context())
	m, _ := auth.MembershipFrom(r.Context())
	session, ok, err := s.auth.SessionID(r)
	if err != nil {
		slog.ErrorContext(r.Context(), "export: session", "user", u.ID, "err", err)
		apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
		return
	}
	var ex *run
	var leave func()
	if ok {
		ex, leave, ok = s.tickets.redeem(r.PathValue("token"), u.ID, m.WorkspaceID, session)
	}
	if !ok {
		apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "this export is gone or isn't yours: start it again")
		return
	}
	defer leave()

	// The zip can take longer than any timeout the server might have, for a person on a slow
	// connection, so there is no deadline for the whole of it: each write has its own, and one
	// the connection doesn't take in time ends the export and frees its slot.
	rc := http.NewResponseController(w)
	out := &idleWriter{w: w, rc: rc, idle: s.idle}
	out.arm()
	h := w.Header()
	h.Set("Content-Type", "application/zip")
	h.Set("Content-Disposition", `attachment; filename="taskto-export-`+s.now().UTC().Format("20060102")+`.zip"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	cw := &countingWriter{w: out}
	if err := ex.write(r.Context(), cw); err != nil {
		// The status went out with the first bytes: a zip that stops is the way to say it failed
		// (it has no end, so no unzipper takes it for whole), and the connection is cut.
		slog.WarnContext(r.Context(), "export: stopped", "user", u.ID, "workspace", m.WorkspaceID, "bytes", cw.n, "err", err)
		panic(http.ErrAbortHandler)
	}
	slog.InfoContext(r.Context(), "export: done", "user", u.ID, "workspace", m.WorkspaceID, "bytes", cw.n)
}

// idleWriter gives each write its own deadline, the idle timeout from when it starts: a
// connection that takes what is written keeps the export going however long the zip is, and one
// that stops taking it fails the write, which ends the export.
type idleWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
}

func (i *idleWriter) arm() {
	// A writer that can't have a deadline (a test's) has none.
	_ = i.rc.SetWriteDeadline(time.Now().Add(i.idle))
}

func (i *idleWriter) Write(p []byte) (int, error) {
	i.arm()
	return i.w.Write(p)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// run is one export: whose, where, and what it has gathered so far.
type run struct {
	svc   *Service
	user  auth.User
	ws    auth.Membership
	space *access.Space // the person's private space in the workspace; nil if they have none
	convs []conversations.Conversation
	at    time.Time

	zw    *zip.Writer
	names *names
	mf    manifest
}

// manifest is manifest.json: what the zip holds and what it leaves out.
type manifest struct {
	Format     int            `json:"format"`
	ExportedAt string         `json:"exported_at"`
	Workspace  manifestWS     `json:"workspace"`
	User       manifestUser   `json:"user"`
	Counts     map[string]int `json:"counts"`
	// Skipped lists what could not be included: Kind is "documents" or "conversations" (all of them), "document",
	// "app" or "transcript" (a conversation's history), Reason "unavailable", "incomplete" (a
	// history cut short) or "off", and Ref says which one (a document's path, an app's or a
	// conversation's ID). Capped; Truncated says if it was.
	Skipped   []skipped `json:"skipped"`
	Truncated bool      `json:"skipped_truncated,omitempty"`
}

type manifestWS struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type manifestUser struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

type skipped struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Ref    string `json:"ref,omitempty"`
}

func (s *Service) prepare(ctx context.Context, u auth.User, m auth.Membership) (*run, error) {
	ex := &run{svc: s, user: u, ws: m, at: s.now().UTC(), names: newNames()}
	readable, err := s.spaces.Readable(ctx, u.ID, m.WorkspaceID)
	if err != nil {
		return nil, err
	}
	// Only the person's own private space: never a shared one, whoever else reads it.
	for _, sp := range readable {
		if sp.Kind == access.Private && sp.Role == access.RoleOwner {
			own := sp
			ex.space = &own
			break
		}
	}
	if s.conversations != nil {
		if ex.convs, err = s.conversations.ExportList(ctx, m.WorkspaceID, u.ID); err != nil {
			return nil, err
		}
	}
	ex.mf = manifest{
		Format: Format, ExportedAt: ex.at.Format(time.RFC3339),
		Workspace: manifestWS{ID: m.WorkspaceID, Name: m.WorkspaceName},
		User:      manifestUser{ID: u.ID, Name: u.Name, Email: u.Email},
		Counts:    map[string]int{"documents": 0, "boards": 0, "cards": 0, "comments": 0, "conversations": 0, "apps": 0, "records": 0},
		Skipped:   []skipped{},
	}
	return ex, nil
}

// write makes the zip on out. An error means the zip is not whole: a write failed (the person
// went away), the context ended, or something that should read didn't.
func (ex *run) write(ctx context.Context, out io.Writer) error {
	ex.zw = zip.NewWriter(out)
	steps := []func(context.Context) error{ex.documents, ex.boards, ex.conversations, ex.apps}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := step(ctx); err != nil {
			return err
		}
	}
	// The manifest is last, once it knows the counts.
	if err := ex.json("manifest.json", ex.mf); err != nil {
		return err
	}
	return ex.zw.Close()
}

func (ex *run) skip(kind, reason, ref string) {
	if len(ex.mf.Skipped) >= maxSkipped {
		ex.mf.Truncated = true
		return
	}
	ex.mf.Skipped = append(ex.mf.Skipped, skipped{Kind: kind, Reason: reason, Ref: ref})
}

// entry starts an entry of the zip, named name (already unique and clean). Bytes that are
// already compressed are stored, not deflated again.
func (ex *run) entry(name string, store bool) (io.Writer, error) {
	method := zip.Deflate
	if store {
		method = zip.Store
	}
	return ex.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: ex.at})
}

// json writes v, indented, as the entry name.
func (ex *run) json(name string, v any) error {
	w, err := ex.entry(name, false)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// documents writes the current version of every file of the private space's knowledge base, under
// documents/, with the names they have there. The space is read at one commit, the head when the
// export reaches it: the listing, in pages, and every file are of that commit, so a file renamed
// or replaced meanwhile is neither listed twice nor missed, and an unversioned file is the one the
// commit points to.
func (ex *run) documents(ctx context.Context) error {
	kb := ex.svc.knowledge
	if ex.space == nil {
		return nil
	}
	if kb == nil {
		ex.skip("documents", "off", "")
		return nil
	}
	at, err := kb.Head(ctx, *ex.space)
	if err != nil {
		return fmt.Errorf("reading the knowledge base's head: %w", err)
	}
	if at == "" {
		return nil // nothing was ever saved
	}
	after := ""
	for {
		page, err := kb.ListAt(ctx, *ex.space, at, "", after, knowledge.MaxListLimit)
		if errors.Is(err, knowledge.ErrNotFound) {
			// The commit went: the space's history was rewritten since (an old version taken out of it).
			ex.skip("documents", "unavailable", "")
			return nil
		}
		if err != nil {
			return fmt.Errorf("listing documents: %w", err)
		}
		for _, e := range page.Docs {
			if err := ex.document(ctx, e, at); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		after = page.Next
	}
}

func (ex *run) document(ctx context.Context, e knowledge.Entry, at string) error {
	_, f, err := ex.svc.knowledge.Stream(ctx, *ex.space, e.Path, at)
	switch {
	case errors.Is(err, knowledge.ErrNotKept), errors.Is(err, knowledge.ErrNotFound):
		// A file whose bytes the server no longer has: the manifest says so, and the rest is
		// exported.
		ex.skip("document", "unavailable", e.Path)
		return nil
	case err != nil:
		return fmt.Errorf("opening document: %w", err)
	}
	defer f.Close()

	// Files git doesn't version are large or already compressed: audio, video, archives.
	w, err := ex.entry(ex.names.claim(append([]string{"documents"}, strings.Split(e.Path, "/")...)...), !e.Versioned)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, &ctxReader{ctx: ctx, r: f}); err != nil {
		return fmt.Errorf("copying document: %w", err)
	}
	ex.mf.Counts["documents"]++
	return nil
}

// ctxReader stops a copy when the request ends.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// boards writes each board of the private space as JSON and as Markdown, under boards/. A board's
// cards are read a page at a time and written as they come, once for each file: what is held is a
// card's thread, however many cards there are. The two files are read on their own, so a card
// edited between them can differ in them.
func (ex *run) boards(ctx context.Context) error {
	if ex.space == nil {
		return nil
	}
	list, err := ex.svc.boards.ExportBoards(ctx, ex.user.ID, ex.space.ID)
	if err != nil {
		return fmt.Errorf("exporting boards: %w", err)
	}
	for _, b := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		base := cleanElem(strconv.FormatInt(b.ID, 10)+"-"+slug(b.Name), maxElemBytes)
		if err := ex.boardJSON(ctx, b, base); err != nil {
			return err
		}
		if err := ex.boardMarkdown(ctx, b, base); err != nil {
			return err
		}
		ex.mf.Counts["boards"]++
	}
	return nil
}

// boardJSON is the board's file: {"board": {…}, "cards": [{…card and its comments…}, …]}, written by
// hand, card by card.
func (ex *run) boardJSON(ctx context.Context, b boards.Board, base string) error {
	w, err := ex.entry(ex.names.claim("boards", base+".json"), false)
	if err != nil {
		return err
	}
	head, err := compactJSON(b)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "{\n  \"board\": %s,\n  \"cards\": [", head); err != nil {
		return err
	}
	n := 0
	err = ex.svc.boards.ExportCards(ctx, ex.user.ID, ex.space.ID, b.ID, func(t boards.Thread) error {
		sep := ",\n    "
		if n == 0 {
			sep = "\n    "
		}
		n++
		line, err := compactJSON(t)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, sep); err != nil {
			return err
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
		ex.mf.Counts["cards"]++
		ex.mf.Counts["comments"] += len(t.Comments)
		return ctx.Err()
	})
	if err != nil {
		return fmt.Errorf("exporting a board's cards: %w", err)
	}
	if n > 0 {
		if _, err := io.WriteString(w, "\n  "); err != nil {
			return err
		}
	}
	_, err = io.WriteString(w, "]\n}\n")
	return err
}

func (ex *run) boardMarkdown(ctx context.Context, b boards.Board, base string) error {
	w, err := ex.entry(ex.names.claim("boards", base+".md"), false)
	if err != nil {
		return err
	}
	if err := writeBoardHeading(w, b); err != nil {
		return err
	}
	err = ex.svc.boards.ExportCards(ctx, ex.user.ID, ex.space.ID, b.ID, func(t boards.Thread) error {
		if err := writeCardMarkdown(w, t); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return fmt.Errorf("exporting a board's cards: %w", err)
	}
	return nil
}

// compactJSON is v as one line, with <, > and & as they are.
func compactJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

// A conversation's file is {"conversation": {…}, "entries": […], "transcript": "complete", "unavailable"
// (the agent runtime couldn't give the history at all) or "incomplete" (it stopped part-way: the
// entries are those read)}. Its entries are the agent runtime's, as the web app reads them
// (docs/api.md#export-my-data). It is written by hand, entry by entry, so a long history isn't
// encoded in one piece.

// conversations writes each of the person's conversations in the workspace under conversations/.
func (ex *run) conversations(ctx context.Context) error {
	if ex.svc.conversations == nil {
		ex.skip("conversations", "off", "") // the server has none, which isn't the same as a person with none
		return nil
	}
	for _, c := range ex.convs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ex.conversation(ctx, c); err != nil {
			return err
		}
	}
	return nil
}

func (ex *run) conversation(ctx context.Context, c conversations.Conversation) error {
	name := ex.names.claim("conversations", cleanElem(strconv.FormatInt(c.ID, 10)+"-"+slug(c.Title), maxElemBytes)+".json")
	var w io.Writer
	n := 0
	start := func() error {
		var err error
		if w, err = ex.entry(name, false); err != nil {
			return err
		}
		meta, err := compactJSON(c)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(w, "{\n  \"conversation\": %s,\n  \"entries\": [", meta)
		return err
	}
	err := ex.svc.conversations.ExportEntries(ctx, c, func(e json.RawMessage) error {
		if w == nil {
			if err := start(); err != nil {
				return err
			}
		}
		sep := ",\n    "
		if n == 0 {
			sep = "\n    "
		}
		n++
		if _, err := io.WriteString(w, sep); err != nil {
			return err
		}
		var line bytes.Buffer
		if err := json.Compact(&line, e); err != nil {
			return fmt.Errorf("an entry isn't JSON: %w", err)
		}
		_, err := w.Write(line.Bytes())
		return err
	})
	status := "complete"
	switch {
	case errors.Is(err, conversations.ErrNoTranscript):
		// The agent runtime is off, lost the history or stopped answering: the conversation's own
		// record is still the person's, and the manifest says what is missing of its history.
		status = "unavailable"
		if n > 0 {
			status = "incomplete"
		}
		ex.skip("transcript", status, strconv.FormatInt(c.ID, 10))
	case err != nil:
		return fmt.Errorf("exporting conversation: %w", err)
	}
	if w == nil {
		if err := start(); err != nil {
			return err
		}
	}
	if n > 0 {
		if _, err := io.WriteString(w, "\n  "); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "],\n  \"transcript\": %q\n}\n", status); err != nil {
		return err
	}
	ex.mf.Counts["conversations"]++
	return nil
}

// appFile is an app's JSON: its identity and its records, as the app shows them (internal/apps
// Items): each record's fields by name, with its id and revision.
type appFile struct {
	App  appInfo          `json:"app"`
	Rows []map[string]any `json:"records"`
}

type appInfo struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	ArchivedAt *string `json:"archived_at"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

// apps writes the records of each app in the private space, under apps/, one JSON file each.
func (ex *run) apps(ctx context.Context) error {
	if ex.space == nil {
		return nil
	}
	actor := apps.Actor{UserID: ex.user.ID}
	list, err := ex.svc.apps.ListApps(ctx, actor, ex.space.ID, true)
	if err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}
	for _, ap := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		rows, err := ex.svc.apps.ListRows(ctx, actor, ex.space.ID, ap.ID)
		if errors.Is(err, apps.ErrNotFound) {
			// The app moved out of the private space (into a shared one) since the listing: it is
			// the space's now, not the person's, so its records stay out.
			ex.skip("app", "unavailable", strconv.FormatInt(ap.ID, 10))
			continue
		}
		if err != nil {
			return fmt.Errorf("reading an app's records: %w", err)
		}
		if rows == nil {
			rows = []map[string]any{}
		}
		file := appFile{App: appInfo{ID: ap.ID, Name: ap.Name, ArchivedAt: ap.ArchivedAt, CreatedAt: ap.CreatedAt, UpdatedAt: ap.UpdatedAt}, Rows: rows}
		name := ex.names.claim("apps", cleanElem(strconv.FormatInt(ap.ID, 10)+"-"+slug(ap.Name), maxElemBytes)+".json")
		if err := ex.json(name, file); err != nil {
			return err
		}
		ex.mf.Counts["apps"]++
		ex.mf.Counts["records"] += len(rows)
	}
	return nil
}
