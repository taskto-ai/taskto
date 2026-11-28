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
	"taskto.ai/taskto/internal/desk"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/skills"
)

const Format = 1

const maxSkipped = 1000

type Options struct {
	PerPerson   int
	Window      time.Duration
	AtOnce      int
	Now         func() time.Time
	TicketTTL   time.Duration
	IdleTimeout time.Duration
}

type Service struct {
	db            *sql.DB
	spaces        *access.Checker
	knowledge     *knowledge.Store
	boards        *boards.Service
	apps          *apps.Service
	conversations *conversations.Service
	desks         *desk.Service
	skills        *skills.Store
	gate          *gate
	tickets       *tickets
	wsTickets     *tickets
	idle          time.Duration
	auth          *auth.Service
	now           func() time.Time
}

func New(sqlDB *sql.DB, spaces *access.Checker, kb *knowledge.Store, b *boards.Service, a *apps.Service, convs *conversations.Service, desks *desk.Service, opts Options) *Service {
	g := newGate(opts.PerPerson, opts.Window, opts.AtOnce, opts.Now)
	idle := opts.IdleTimeout
	if idle <= 0 {
		idle = defaultIdleTimeout
	}
	return &Service{db: sqlDB, spaces: spaces, knowledge: kb, boards: b, apps: a, conversations: convs, desks: desks, gate: g,
		tickets: newTickets(opts.TicketTTL), wsTickets: newTickets(opts.TicketTTL), idle: idle, now: g.now}
}

const (
	Path                  = "/api/workspaces/{workspace}/export"
	DownloadPath          = Path + "/{token}"
	WorkspacePath         = Path + "/workspace"
	WorkspaceDownloadPath = WorkspacePath + "/{token}"
)

func (s *Service) WithSkills(store *skills.Store) *Service {
	s.skills = store
	return s
}

func (s *Service) Routes(mux *http.ServeMux, a *auth.Service) {
	s.auth = a
	mine, ws := s.personalFlow(), s.workspaceFlow()
	mux.Handle("POST "+Path, a.RequireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleStart(w, r, mine) })))
	mux.Handle("GET "+DownloadPath, a.RequireMember(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleDownload(w, r, mine) })))
	mux.Handle("POST "+WorkspacePath, a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleStart(w, r, ws) })))
	mux.Handle("GET "+WorkspaceDownloadPath, a.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleDownload(w, r, ws) })))
}

type started struct {
	Download  string `json:"download"`
	ExpiresIn int    `json:"expires_in"`
}

type flow struct {
	tickets *tickets
	base    func(workspace int64) string
	prepare func(ctx context.Context, u auth.User, m auth.Membership) (*run, error)
	record  func(ctx context.Context, u auth.User, m auth.Membership, ex *run)
}

func (s *Service) personalFlow() flow {
	return flow{
		tickets: s.tickets,
		base:    func(ws int64) string { return "/api/workspaces/" + strconv.FormatInt(ws, 10) + "/export/" },
		prepare: s.prepare,
		record: func(ctx context.Context, u auth.User, m auth.Membership, _ *run) {
			audit.Log(ctx, s.db, audit.Event{Workspace: m.WorkspaceID, Actor: u.ID, Action: audit.DataExportStarted,
				TargetType: audit.TargetUser, Target: u.ID})
		},
	}
}

func (s *Service) workspaceFlow() flow {
	return flow{
		tickets: s.wsTickets,
		base:    func(ws int64) string { return "/api/workspaces/" + strconv.FormatInt(ws, 10) + "/export/workspace/" },
		prepare: s.prepareWorkspace,
		record: func(ctx context.Context, u auth.User, m auth.Membership, ex *run) {
			audit.Log(ctx, s.db, audit.Event{Workspace: m.WorkspaceID, Actor: u.ID, Action: audit.WorkspaceExportStarted,
				TargetType: audit.TargetWorkspace, Target: m.WorkspaceID,
				Details: audit.Details{"spaces": len(ex.shared)}})
		},
	}
}

func (s *Service) handleStart(w http.ResponseWriter, r *http.Request, f flow) {
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
			apierr.Write(w, http.StatusConflict, apierr.CodeExportRunning, "an export of yours is running already")
			return
		}
		apierr.Write(w, http.StatusTooManyRequests, apierr.CodeExportRateLimited, "too many exports: try again in a little while")
		return
	}
	ex, err := f.prepare(r.Context(), u, m)
	if err != nil {
		s.gate.refund(u.ID)
		leave()
		slog.ErrorContext(r.Context(), "export: preparing", "user", u.ID, "workspace", m.WorkspaceID, "err", err)
		apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
		return
	}
	token, err := f.tickets.issue(u.ID, m.WorkspaceID, session, leave, ex)
	if err != nil {
		s.gate.refund(u.ID)
		leave()
		slog.ErrorContext(r.Context(), "export: ticket", "user", u.ID, "err", err)
		apierr.Write(w, http.StatusInternalServerError, apierr.CodeInternal, "internal error")
		return
	}
	f.record(r.Context(), u, m, ex)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(started{
		Download:  f.base(m.WorkspaceID) + token,
		ExpiresIn: max(1, int(math.Ceil(f.tickets.ttl.Seconds()))),
	})
}

func (s *Service) handleDownload(w http.ResponseWriter, r *http.Request, f flow) {
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
		ex, leave, ok = f.tickets.redeem(r.PathValue("token"), u.ID, m.WorkspaceID, session)
	}
	if !ok {
		apierr.Write(w, http.StatusNotFound, apierr.CodeNotFound, "this export is gone or isn't yours: start it again")
		return
	}
	defer leave()

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
		slog.WarnContext(r.Context(), "export: stopped", "user", u.ID, "workspace", m.WorkspaceID, "bytes", cw.n, "err", err)
		panic(http.ErrAbortHandler)
	}
	slog.InfoContext(r.Context(), "export: done", "user", u.ID, "workspace", m.WorkspaceID, "bytes", cw.n)
}

type idleWriter struct {
	w    io.Writer
	rc   *http.ResponseController
	idle time.Duration
}

func (i *idleWriter) arm() {
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

type run struct {
	svc   *Service
	user  auth.User
	ws    auth.Membership
	space *access.Space
	convs []conversations.Conversation
	at    time.Time

	shared []access.Space

	zw    *zip.Writer
	names *names
	mf    manifest
}

type manifest struct {
	Format     int             `json:"format"`
	Scope      string          `json:"scope"`
	ExportedAt string          `json:"exported_at"`
	Workspace  manifestWS      `json:"workspace"`
	User       manifestUser    `json:"user"`
	Counts     map[string]int  `json:"counts"`
	Spaces     []manifestSpace `json:"spaces,omitempty"`
	Skipped    []skipped       `json:"skipped"`
	Truncated  bool            `json:"skipped_truncated,omitempty"`
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

type manifestSpace struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Dir        string  `json:"dir"`
	ArchivedAt *string `json:"archived_at"`
}

type skipped struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
	Ref    string `json:"ref,omitempty"`
	Space  int64  `json:"space,omitempty"`
}

func (s *Service) prepare(ctx context.Context, u auth.User, m auth.Membership) (*run, error) {
	ex := &run{svc: s, user: u, ws: m, at: s.now().UTC(), names: newNames()}
	readable, err := s.spaces.Readable(ctx, u.ID, m.WorkspaceID)
	if err != nil {
		return nil, err
	}
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
		Format: Format, Scope: "user", ExportedAt: ex.at.Format(time.RFC3339),
		Workspace: manifestWS{ID: m.WorkspaceID, Name: m.WorkspaceName},
		User:      manifestUser{ID: u.ID, Name: u.Name, Email: u.Email},
		Counts:    map[string]int{"documents": 0, "boards": 0, "cards": 0, "comments": 0, "conversations": 0, "desks": 0, "apps": 0, "records": 0, "skills": 0},
		Skipped:   []skipped{},
	}
	return ex, nil
}

func (ex *run) write(ctx context.Context, out io.Writer) error {
	ex.zw = zip.NewWriter(out)
	steps := []func(context.Context) error{ex.privateDocuments, ex.privateBoards, ex.conversations, ex.desks, ex.privateApps, ex.personalSkills}
	if ex.mf.Scope == "workspace" {
		steps = []func(context.Context) error{ex.members, ex.spaces}
	}
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := step(ctx); err != nil {
			return err
		}
	}
	if ex.mf.Scope == "workspace" {
		if err := ex.checkAdmin(ctx); err != nil {
			return err
		}
	}
	if err := ex.json("manifest.json", ex.mf); err != nil {
		return err
	}
	return ex.zw.Close()
}

func (ex *run) skip(p place, kind, reason, ref string) {
	if len(ex.mf.Skipped) >= maxSkipped {
		ex.mf.Truncated = true
		return
	}
	item := skipped{Kind: kind, Reason: reason, Ref: ref}
	if p.shared {
		item.Space = p.space.ID
	}
	ex.mf.Skipped = append(ex.mf.Skipped, item)
}

type place struct {
	space  access.Space
	dir    []string
	shared bool
}

func (p place) path(elems ...string) []string {
	return append(p.dir[:len(p.dir):len(p.dir)], elems...)
}

var errLostAccess = errors.New("the space isn't readable any more")

func (ex *run) checkAdmin(ctx context.Context) error {
	return ex.adminIn(ctx, ex.svc.db)
}

func (ex *run) adminIn(ctx context.Context, q interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}) error {
	var admin int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM memberships m JOIN users u ON u.id = m.user_id
		WHERE m.workspace_id = ? AND m.user_id = ? AND m.role = ? AND u.disabled_at IS NULL`,
		ex.ws.WorkspaceID, ex.user.ID, string(auth.RoleAdmin)).Scan(&admin)
	if err != nil {
		return fmt.Errorf("checking the admin: %w", err)
	}
	if admin == 0 {
		return errNotAdmin
	}
	return nil
}

var errNotAdmin = errors.New("the exporter isn't an admin of the workspace any more")

func (ex *run) checkWho(ctx context.Context, p place) error {
	if !p.shared {
		return nil
	}
	return ex.checkAdmin(ctx)
}

func (ex *run) readAsAdmin(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := ex.svc.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ex.adminIn(ctx, tx); err != nil {
		return err
	}
	return fn(tx)
}

func (ex *run) current(ctx context.Context, p place) (access.Space, error) {
	if err := ex.checkAdmin(ctx); err != nil {
		return access.Space{}, err
	}
	sp, err := ex.svc.spaces.Check(ctx, ex.user.ID, p.space.ID)
	if errors.Is(err, access.ErrNotFound) || (err == nil && (sp.WorkspaceID != ex.ws.WorkspaceID || sp.Kind != access.Shared)) {
		return access.Space{}, errLostAccess
	}
	return sp, err
}

func (ex *run) check(ctx context.Context, p place) error {
	if !p.shared {
		return nil
	}
	_, err := ex.current(ctx, p)
	return err
}

func (ex *run) lost(ctx context.Context, p place, err error) error {
	if cerr := ex.check(ctx, p); cerr != nil {
		return cerr
	}
	return err
}

func (ex *run) entry(name string, store bool) (io.Writer, error) {
	method := zip.Deflate
	if store {
		method = zip.Store
	}
	return ex.zw.CreateHeader(&zip.FileHeader{Name: name, Method: method, Modified: ex.at})
}

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

func (ex *run) privateDocuments(ctx context.Context) error {
	if ex.space == nil {
		return nil
	}
	return ex.documents(ctx, place{space: *ex.space})
}

func (ex *run) documents(ctx context.Context, p place) error {
	kb := ex.svc.knowledge
	if kb == nil {
		ex.skip(p, "documents", "off", "")
		return nil
	}
	if err := ex.check(ctx, p); err != nil {
		return err
	}
	at, err := kb.Head(ctx, p.space)
	if err != nil {
		return fmt.Errorf("reading the knowledge base's head: %w", err)
	}
	if at == "" {
		return nil
	}
	after := ""
	for {
		if err := ex.check(ctx, p); err != nil {
			return err
		}
		page, err := kb.ListAt(ctx, p.space, at, "", after, knowledge.MaxListLimit)
		if errors.Is(err, knowledge.ErrNotFound) {
			ex.skip(p, "documents", "unavailable", "")
			return nil
		}
		if err != nil {
			return fmt.Errorf("listing documents: %w", err)
		}
		for _, e := range page.Docs {
			if err := ex.document(ctx, p, e, at); err != nil {
				return err
			}
		}
		if page.Next == "" {
			return nil
		}
		after = page.Next
	}
}

func (ex *run) document(ctx context.Context, p place, e knowledge.Entry, at string) error {
	if err := ex.check(ctx, p); err != nil {
		return err
	}
	_, f, err := ex.svc.knowledge.Stream(ctx, p.space, e.Path, at)
	switch {
	case errors.Is(err, knowledge.ErrNotKept), errors.Is(err, knowledge.ErrNotFound):
		ex.skip(p, "document", "unavailable", e.Path)
		return nil
	case err != nil:
		return fmt.Errorf("opening document: %w", err)
	}
	defer f.Close()
	if err := ex.check(ctx, p); err != nil {
		return err
	}

	w, err := ex.entry(ex.names.claim(p.path(append([]string{"documents"}, strings.Split(e.Path, "/")...)...)...), !e.Versioned)
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, &ctxReader{ctx: ctx, r: f}); err != nil {
		return fmt.Errorf("copying document: %w", err)
	}
	ex.mf.Counts["documents"]++
	return nil
}

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

func (ex *run) privateBoards(ctx context.Context) error {
	if ex.space == nil {
		return nil
	}
	return ex.boards(ctx, place{space: *ex.space})
}

func (ex *run) boards(ctx context.Context, p place) error {
	err := ex.svc.boards.ExportBoards(ctx, ex.user.ID, p.space.ID, func(b boards.Board) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ex.checkWho(ctx, p); err != nil {
			return err
		}
		base := cleanElem(strconv.FormatInt(b.ID, 10)+"-"+slug(b.Name), maxElemBytes)
		if err := ex.boardJSON(ctx, p, b, base); err != nil {
			return err
		}
		return ex.boardMarkdown(ctx, p, b, base)
	})
	if errors.Is(err, boards.ErrNotFound) {
		return ex.lost(ctx, p, fmt.Errorf("exporting boards: %w", err))
	}
	if err != nil {
		return fmt.Errorf("exporting boards: %w", err)
	}
	return nil
}

func (ex *run) boardJSON(ctx context.Context, p place, b boards.Board, base string) error {
	w, err := ex.entry(ex.names.claim(p.path("boards", base+".json")...), false)
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
	ex.mf.Counts["boards"]++
	n := 0
	err = ex.svc.boards.ExportCards(ctx, ex.user.ID, p.space.ID, b.ID, func(t boards.Thread) error {
		if err := ex.checkWho(ctx, p); err != nil {
			return err
		}
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
		if errors.Is(err, boards.ErrNotFound) {
			if n > 0 {
				_, _ = io.WriteString(w, "\n  ")
			}
			_, _ = io.WriteString(w, "]\n}\n")
			return ex.lost(ctx, p, fmt.Errorf("exporting a board's cards: %w", err))
		}
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

func (ex *run) boardMarkdown(ctx context.Context, p place, b boards.Board, base string) error {
	w, err := ex.entry(ex.names.claim(p.path("boards", base+".md")...), false)
	if err != nil {
		return err
	}
	if err := writeBoardHeading(w, b); err != nil {
		return err
	}
	err = ex.svc.boards.ExportCards(ctx, ex.user.ID, p.space.ID, b.ID, func(t boards.Thread) error {
		if err := ex.checkWho(ctx, p); err != nil {
			return err
		}
		if err := writeCardMarkdown(w, t); err != nil {
			return err
		}
		return ctx.Err()
	})
	if errors.Is(err, boards.ErrNotFound) {
		return ex.lost(ctx, p, fmt.Errorf("exporting a board's cards: %w", err))
	}
	if err != nil {
		return fmt.Errorf("exporting a board's cards: %w", err)
	}
	return nil
}

func compactJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func (ex *run) conversations(ctx context.Context) error {
	if ex.svc.conversations == nil {
		ex.skip(place{}, "conversations", "off", "")
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
		status = "unavailable"
		if n > 0 {
			status = "incomplete"
		}
		ex.skip(place{}, "transcript", status, strconv.FormatInt(c.ID, 10))
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

func (ex *run) personalSkills(ctx context.Context) error {
	if ex.svc.skills == nil {
		ex.skip(place{}, "skills", "off", "")
		return nil
	}
	list, err := ex.svc.skills.List(ctx, skills.Owner{Scope: skills.ScopeUser, WorkspaceID: ex.ws.WorkspaceID, UserID: ex.user.ID}, ex.user.ID)
	if err != nil {
		return fmt.Errorf("listing skills: %w", err)
	}
	for _, sk := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		w, err := ex.entry(ex.names.claim("skills", cleanElem(sk.Name, maxElemBytes)+".md"), false)
		if err != nil {
			return err
		}
		if err := writeSkillMarkdown(w, sk); err != nil {
			return err
		}
		ex.mf.Counts["skills"]++
	}
	return nil
}

type appFile struct {
	App  appInfo          `json:"app"`
	Live *apps.Release    `json:"live,omitempty"`
	Rows []map[string]any `json:"records"`
}

type appInfo struct {
	ID         int64   `json:"id"`
	Name       string  `json:"name"`
	Icon       string  `json:"icon"`
	ArchivedAt *string `json:"archived_at"`
	CreatedAt  string  `json:"created_at"`
	UpdatedAt  string  `json:"updated_at"`
}

func newAppFile(got apps.ExportedApp, shared bool) appFile {
	file := appFile{App: appInfo{ID: got.App.ID, Name: got.App.Name, Icon: got.App.Icon, ArchivedAt: got.App.ArchivedAt, CreatedAt: got.App.CreatedAt, UpdatedAt: got.App.UpdatedAt}, Rows: got.Rows}
	if shared {
		file.Live = got.Live
	}
	if file.Rows == nil {
		file.Rows = []map[string]any{}
	}
	return file
}

func (ex *run) privateApps(ctx context.Context) error {
	if ex.space == nil {
		return nil
	}
	return ex.apps(ctx, place{space: *ex.space})
}

func (ex *run) apps(ctx context.Context, p place) error {
	actor := apps.Actor{UserID: ex.user.ID}
	list, err := ex.svc.apps.ListApps(ctx, actor, p.space.ID, true)
	if errors.Is(err, apps.ErrNotFound) {
		return ex.lost(ctx, p, fmt.Errorf("listing apps: %w", err))
	}
	if err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}
	for _, ap := range list {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := ex.checkWho(ctx, p); err != nil {
			return err
		}
		got, err := ex.svc.apps.ExportApp(ctx, actor, p.space.ID, ap.ID, p.shared)
		if errors.Is(err, apps.ErrNotAdmin) {
			return errNotAdmin
		}
		if errors.Is(err, apps.ErrNotFound) {
			if err := ex.check(ctx, p); err != nil {
				return err
			}
			ex.skip(p, "app", "unavailable", strconv.FormatInt(ap.ID, 10))
			continue
		}
		if err != nil {
			return fmt.Errorf("reading an app's records: %w", err)
		}
		file := newAppFile(got, p.shared)
		name := ex.names.claim(p.path("apps", cleanElem(strconv.FormatInt(got.App.ID, 10)+"-"+slug(got.App.Name), maxElemBytes)+".json")...)
		if err := ex.json(name, file); err != nil {
			return err
		}
		ex.mf.Counts["apps"]++
		ex.mf.Counts["records"] += len(file.Rows)
	}
	return nil
}
