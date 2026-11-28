package cliaccounts

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/agenttools"
	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/cliworker"
	"taskto.ai/taskto/internal/conversations"
	"taskto.ai/taskto/internal/usage"
)

type Turn interface {
	Spec() cliworker.TurnSpec
	Wait(ctx context.Context, after int) ([]cliworker.Event, bool, error)
	Over() bool
}

func (l localWorker) StartTurn(spec cliworker.TurnSpec) (Turn, bool, error) {
	t, attached, err := l.Start(spec)
	if err != nil {
		return nil, false, err
	}
	return t, attached, nil
}

func (l localWorker) FindTurn(a cliowner.Account, id string) (Turn, error) {
	t, ok := l.TurnOf(a, id)
	if !ok {
		return nil, cliworker.ErrNotFound
	}
	return t, nil
}

func (l localWorker) StopTurn(a cliowner.Account, id string) error {
	l.StopTurnOf(a, id)
	return nil
}

type Authorizer func(*http.Request) (conversations.Principal, error)

func (s *Service) TurnRoutes(authorize Authorizer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /cli/turn", s.authorized(authorize, s.handleTurn))
	mux.Handle("POST /cli/stop", s.authorized(authorize, s.handleStop))
	mux.Handle("POST /cli/wait", s.authorized(authorize, s.handleWait))
	return mux
}

type principalHandler func(w http.ResponseWriter, r *http.Request, p conversations.Principal)

func (s *Service) authorized(authorize Authorizer, h principalHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, err := authorize(r)
		var refusal *agenttools.Refusal
		switch {
		case errors.As(err, &refusal):
			code := refusal.Code
			if code == "" {
				code = refusalCode(refusal.Status)
			}
			writeError(w, refusal.Status, code, refusal.Message)
			return
		case err != nil:
			serverError(w, r, err)
			return
		}
		h(w, r, p)
	})
}

func refusalCode(status int) apierr.Code {
	switch status {
	case http.StatusUnauthorized:
		return apierr.CodeNotLoggedIn
	case http.StatusNotFound:
		return apierr.CodeNotFound
	}
	return apierr.CodeBadRequest
}

var Models = map[cliworker.CLI][]string{
	cliworker.Claude: {"default", "opus", "sonnet", "haiku"},
	cliworker.Codex:  {"default"},
	cliworker.Grok:   {"default"},
}

var turnIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)

type turnRequest struct {
	Turn    string         `json:"turn"`
	CLI     cliworker.CLI  `json:"cli"`
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	History []historyEntry `json:"history"`
	Replay  bool           `json:"replay"`
}

type historyEntry struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

const maxTurnBody = 512 << 10

const maxHandOff = 32 << 10

func (s *Service) handleTurn(w http.ResponseWriter, r *http.Request, p conversations.Principal) {
	var req turnRequest
	if !readJSONLimit(w, r, &req, maxTurnBody) {
		return
	}
	if !turnIDPattern.MatchString(req.Turn) || !req.CLI.Valid() || req.Prompt == "" || !knownModel(req.CLI, req.Model) {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "turn, cli, model and prompt are required, and the model must be one the CLI offers")
		return
	}
	out := newEventWriter(w)
	fail := func(status cliworker.Status, msg string) {
		out.write(cliworker.Event{Type: cliworker.EventDone, Status: status, Text: msg})
	}
	if err := s.Usable(r.Context()); err != nil {
		if errors.Is(err, ErrOff) || errors.Is(err, ErrNoBoundary) {
			fail(cliworker.StatusFailed, err.Error())
			return
		}
		serverError(w, r, err)
		return
	}
	if ok, err := s.cliAvailable(req.CLI); err != nil {
		workerError(w, r, ref{cli: req.CLI}, err)
		return
	} else if !ok {
		fail(cliworker.StatusFailed, req.CLI.Name()+" is not installed on this server")
		return
	}
	id := fmt.Sprintf("c%d-%s", p.Conversation, req.Turn)

	t, found, err := s.worker.FindTurn(p.User, id)
	if err != nil {
		workerError(w, r, ref{cli: req.CLI}, err)
		return
	}
	if found {
		if sp := t.Spec(); sp.User != p.User || sp.Conversation != p.Conversation || sp.CLI != req.CLI {
			fail(cliworker.StatusFailed, "this turn belongs to another conversation")
			return
		}
		s.chain(r.Context(), out, p, req, id, t)
		return
	}

	c, refusal, err := s.pick(r.Context(), p, req.CLI)
	if err != nil {
		serverError(w, r, err)
		return
	}
	switch {
	case refusal != "":
		fail(cliworker.StatusFailed, refusal)
		return
	case c.shared == nil && c.row.state == Reconnect:
		out.write(cliworker.Event{Type: cliworker.EventLoginRequired, Text: req.CLI.Name() + " needs you to sign in again"})
		fail(cliworker.StatusLoginRequired, req.CLI.Name()+" needs you to sign in again")
		return
	}
	on, err := s.switchingOn(r.Context(), p.User)
	if err != nil {
		serverError(w, r, err)
		return
	}
	notice := ""
	if on {
		if c, notice, err = s.startChoice(r.Context(), p, req.CLI, c); err != nil {
			serverError(w, r, err)
			return
		}
	}
	note := ""
	if req.Replay {
		note = "Your previous turn was interrupted by a restart, so some of its steps may already have run: " +
			"check before you repeat anything. Continue the task:\n\n"
	}
	t, err = s.start(r.Context(), p, req, id, c, note)
	if err != nil && !out.started && outage(err) {
		workerError(w, r, ref{cli: req.CLI}, err)
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "cli turn: the worker refused it", "conversation", p.Conversation, "cli", c.cli(), "err", err)
		fail(cliworker.StatusFailed, "the CLI worker refused the turn: "+err.Error())
		return
	}
	if req.Replay {
		out.write(cliworker.Event{Type: cliworker.EventInterrupted, Text: "the previous run of this turn was interrupted by a restart; resuming"})
	}
	if notice != "" {
		s.switchModel(r.Context(), p, req.CLI, c.cli())
		out.write(cliworker.Event{Type: cliworker.EventNotice, Text: notice})
	}
	s.chain(r.Context(), out, p, req, id, t)
}

func (s *Service) start(ctx context.Context, p conversations.Principal, req turnRequest, id string, c choice, note string) (Turn, error) {
	acct, cli := c.row, c.cli()
	perms, err := s.conversationPermissions(ctx, p.Conversation)
	if err != nil {
		return nil, err
	}
	spec := cliworker.TurnSpec{ID: id, User: p.User, CLI: cli, Slot: acct.ref.slot, Conversation: p.Conversation, Prompt: req.Prompt, Permissions: perms}
	if req.Model != "default" && cli == req.CLI {
		spec.Model = req.Model
	}
	if spec.Tools, err = s.toolsFor(ctx, id, p); err != nil {
		return nil, err
	}
	if c.shared != nil {
		a, err := s.Account(ctx, p.User)
		if err != nil {
			return nil, err
		}
		token, err := s.mintToken(proxyClaims{Account: c.shared.ID, User: p.User, Identity: a.Identity, Workspace: p.Workspace, Conversation: p.Conversation, Turn: id})
		if err != nil {
			return nil, err
		}
		spec.Proxy = &cliworker.Proxy{URL: s.shared.ProxyURL, Token: token, Model: c.shared.Model}
		if c.shared.Model != "" {
			spec.Model = c.shared.Model
		}
	}
	session, found, err := s.session(ctx, p.Conversation, acct.id)
	if err != nil {
		return nil, err
	}
	if found {
		last, err := s.lastAccount(ctx, p.Conversation)
		if err != nil {
			return nil, err
		}
		found = last == acct.id
	}
	switch {
	case found:
		spec.Session, spec.Resume = session, true
	case cli != cliworker.Codex:
		spec.Session = newUUID()
	}
	if spec.Session != "" {
		if err := s.useSession(ctx, p.Conversation, acct.id, spec.Session); err != nil {
			return nil, err
		}
	}
	if len(req.History) > 0 {
		if spec.Resume {
			spec.FreshPrompt = handOff(req.History, req.Prompt)
		} else {
			spec.Prompt = handOff(req.History, req.Prompt)
		}
	}
	if note != "" {
		spec.Prompt = note + spec.Prompt
		if spec.FreshPrompt != "" {
			spec.FreshPrompt = note + spec.FreshPrompt
		}
	}
	if len(spec.Prompt) > maxPromptBytes {
		spec.Prompt = spec.Prompt[len(spec.Prompt)-maxPromptBytes:]
	}
	if len(spec.FreshPrompt) > maxPromptBytes {
		spec.FreshPrompt = spec.FreshPrompt[len(spec.FreshPrompt)-maxPromptBytes:]
	}
	t, _, err := s.worker.StartTurn(spec)
	if err != nil {
		s.endTurn(id)
		return nil, err
	}
	slog.InfoContext(ctx, "cli turn started", "turn", id, "conversation", p.Conversation, "user", p.User, "account", c.key(), "resume", spec.Resume)
	return t, nil
}

const maxPromptBytes = 90 << 10

func (s *Service) markStopped(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, at := range s.stops {
		if now.Sub(at) > time.Hour {
			delete(s.stops, k)
		}
	}
	s.stops[id] = now
}

func (s *Service) isStopped(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.stops[id]
	return ok
}

func chainID(id string, n int) string {
	if n == 0 {
		return id
	}
	return id + "-s" + strconv.Itoa(n)
}

func (s *Service) chain(ctx context.Context, out *eventWriter, p conversations.Principal, req turnRequest, id string, t Turn) {
	tried := map[string]bool{}
	offset := 0
	for n := 0; ; n++ {
		if c, ok := s.choiceOf(ctx, p, t.Spec()); ok {
			tried[c.key()] = true
		}
		res, ok := s.pass(ctx, out, p, t, offset)
		if !ok {
			return
		}
		nextID := chainID(id, n+1)
		next, found, _ := s.worker.FindTurn(p.User, nextID)
		notice := s.switchNotice(nextID)
		if !found {
			if res.hop = s.hopOf(t.Spec().ID); res.hop == nil {
				next, notice = s.switchAfter(ctx, out, p, req, t, &res, id, nextID, tried, n < maxSwitches)
			}
		}
		if next == nil {
			s.finish(out, res, offset)
			return
		}
		offset += res.last + 1
		out.write(cliworker.Event{Seq: offset, Type: cliworker.EventNotice, Text: notice})
		t = next
	}
}

type passed struct {
	done     cliworker.Event
	last     int
	limited  bool
	text     string
	input    int64
	output   int64
	hasUsage bool
	hop      *modelHop
}

type modelHop struct {
	notice, note string
	at           time.Time
}

func (s *Service) rememberHop(turn string, h *modelHop) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for k, old := range s.hops {
		if now.Sub(old.at) > time.Hour {
			delete(s.hops, k)
		}
	}
	h.at = now
	s.hops[turn] = h
}

func (s *Service) hopOnce(ctx context.Context, p conversations.Principal, turn, stopped, note string) *modelHop {
	s.hopMu.Lock()
	defer s.hopMu.Unlock()
	if h := s.hopOf(turn); h != nil {
		return h
	}
	h := s.hopToModel(ctx, p, stopped, note)
	if h != nil {
		s.rememberHop(turn, h)
	}
	return h
}

func (s *Service) hopOf(turn string) *modelHop {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hops[turn]
}

func (s *Service) finish(out *eventWriter, res passed, offset int) {
	if res.hop == nil {
		res.done.Seq += offset
		out.write(res.done)
		return
	}
	res.done.Seq = offset + res.last + 1
	res.done.Continue, res.done.Note = true, res.hop.note
	out.write(cliworker.Event{Seq: offset + res.last, Type: cliworker.EventNotice, Text: res.hop.notice})
	out.write(res.done)
}

func (s *Service) pass(ctx context.Context, out *eventWriter, p conversations.Principal, t Turn, offset int) (passed, bool) {
	record := context.WithoutCancel(ctx)
	spec := t.Spec()
	accountID := int64(0)
	if acct, ok, err := s.accountOf(record, p.User, spec); err == nil && ok {
		accountID = acct.id
	}
	var res passed
	for seen := 0; ; {
		events, done, err := t.Wait(ctx, seen)
		if err != nil {
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.drain(record, p, t, seen, accountID, res.input, res.output, res.hasUsage)
			}()
			return res, false
		}
		for _, e := range events {
			seen = e.Seq
			e = s.seen(spec, e)
			s.recordEvent(record, p, accountID, e)
			switch e.Type {
			case cliworker.EventUsage:
				if e.Tokens != nil {
					res.input += e.Tokens.Input
					res.output += e.Tokens.Output
					res.hasUsage = true
				}
				for _, l := range e.Limits {
					if l.Status == "rejected" || (l.UsedPercent != nil && *l.UsedPercent >= 100) {
						res.limited = true
					}
				}
			case cliworker.EventText:
				if len(res.text) < 8<<10 {
					res.text += e.Text
				}
			case cliworker.EventDone:
				res.done, res.last = e, e.Seq
				s.recordTokens(record, p, spec, res)
				continue
			}
			e.Seq += offset
			out.write(e)
		}
		if done {
			return res, true
		}
	}
}

func (s *Service) recordTokens(ctx context.Context, p conversations.Principal, spec cliworker.TurnSpec, res passed) {
	if !res.hasUsage {
		return
	}
	if err := usage.Record(ctx, s.db, usage.Entry{User: p.User, Workspace: p.Workspace, Source: "cli", Engine: string(spec.CLI), Input: res.input, Output: res.output, Turns: 1, At: time.Now(), Turn: spec.ID}); err != nil {
		slog.ErrorContext(ctx, "cli turn: recording token usage", "conversation", p.Conversation, "err", err)
	}
}

func (s *Service) choiceOf(ctx context.Context, p conversations.Principal, spec cliworker.TurnSpec) (choice, bool) {
	if id := sharedOfSlot(spec.Slot); id != 0 {
		a, ok, err := s.sharedAccount(ctx, p.Workspace, id)
		if err != nil || !ok {
			return choice{}, false
		}
		r, ok, err := s.accountOf(ctx, p.User, spec)
		if err != nil || !ok {
			return choice{}, false
		}
		return choice{row: r, shared: &a}, true
	}
	r, ok, err := s.load(ctx, p.User, ref{spec.CLI, spec.Slot})
	if err != nil || !ok {
		return choice{}, false
	}
	return choice{row: r}, true
}

func (s *Service) stopped(ctx context.Context, p conversations.Principal, t Turn, res passed) string {
	switch res.done.Status {
	case cliworker.StatusLoginRequired:
		return "needs you to sign in again"
	case cliworker.StatusFailed:
	default:
		return ""
	}
	c, ok := s.choiceOf(ctx, p, t.Spec())
	if !ok {
		return ""
	}
	limited, until := c.limited(time.Now())
	if res.limited || limited {
		return "reached its usage limit" + untilText(until)
	}
	return ""
}

func (s *Service) switchAfter(ctx context.Context, out *eventWriter, p conversations.Principal, req turnRequest, t Turn, res *passed, id, nextID string, tried map[string]bool, canSwitch bool) (Turn, string) {
	why := s.stopped(ctx, p, t, *res)
	if why == "" || s.isStopped(id) || !s.stillActive(ctx, p) {
		return nil, ""
	}
	if on, err := s.switchingOn(ctx, p.User); err != nil || !on {
		return nil, ""
	}
	from := t.Spec().CLI.Name()
	if c, ok := s.choiceOf(ctx, p, t.Spec()); ok {
		from = c.title()
	}
	note := "The previous agent (" + from + ") stopped part-way through this request: it " + why + ". " +
		"Some of its steps may already have run, and it may have changed files in the working directory: " +
		"check before you redo anything, then carry on with the request."
	if strings.TrimSpace(res.text) != "" {
		note += "\n\nWhat it had said so far:\n\n" + strings.TrimSpace(res.text)
	}
	var next choice
	ok := false
	if canSwitch {
		var err error
		if next, ok, err = s.nextChoice(ctx, p, tried); err != nil {
			slog.ErrorContext(ctx, "cli turn: choosing the next account", "conversation", p.Conversation, "err", err)
			return nil, ""
		}
	}
	if !ok {
		res.hop = s.hopOnce(ctx, p, t.Spec().ID, from+" "+why, note)
		return nil, ""
	}
	note += "\n\n"
	tn, err := s.start(ctx, p, req, nextID, next, note)
	if err != nil {
		slog.ErrorContext(ctx, "cli turn: switching", "conversation", p.Conversation, "err", err)
		return nil, ""
	}
	if s.isStopped(id) {
		_ = s.worker.StopTurn(p.User, nextID)
	}
	notice := fmt.Sprintf("%s %s: continuing on %s", from, why, next.title())
	s.mu.Lock()
	s.switches[nextID] = notice
	s.mu.Unlock()
	s.switchModel(ctx, p, t.Spec().CLI, next.cli())
	return tn, notice
}

func (s *Service) stillActive(ctx context.Context, p conversations.Principal) bool {
	now, err := conversations.PrincipalOf(ctx, s.db, p.Conversation)
	return err == nil && now == p
}

func (s *Service) hopToModel(ctx context.Context, p conversations.Principal, stopped, note string) *modelHop {
	models, err := s.modelsAfter(ctx, p)
	if err != nil {
		slog.ErrorContext(ctx, "cli turn: choosing the next model", "conversation", p.Conversation, "err", err)
		return nil
	}
	for _, m := range models {
		if s.setModel == nil || ctx.Err() != nil {
			return nil
		}
		if err := s.setModel(context.WithoutCancel(ctx), p.AgentConversation, m.provider, m.model); err != nil {
			slog.WarnContext(ctx, "cli turn: setting the conversation's model after a limit", "conversation", p.Conversation, "err", err)
			continue
		}
		return &modelHop{notice: fmt.Sprintf("%s: continuing on %s", stopped, m.model), note: note}
	}
	return nil
}

func (s *Service) switchNotice(nextID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.switches[nextID]; ok {
		return n
	}
	return "continuing on another account"
}

func (s *Service) switchModel(ctx context.Context, p conversations.Principal, from, to cliworker.CLI) {
	if from == to || s.setModel == nil {
		return
	}
	if err := s.setModel(context.WithoutCancel(ctx), p.AgentConversation, cliProviderPrefix+string(to), "default"); err != nil {
		slog.WarnContext(ctx, "cli turn: setting the conversation's model after a switch", "conversation", p.Conversation, "err", err)
	}
}

type choice struct {
	row    row
	shared *sharedRow
}

func (s *Service) pick(ctx context.Context, p conversations.Principal, cli cliworker.CLI) (choice, string, error) {
	mode, err := s.modeOf(ctx, p.Workspace, p.User)
	if err != nil {
		return choice{}, "", err
	}
	var own []row
	if mode.usesOwn() {
		if own, err = s.own(ctx, p.User, cli); err != nil {
			return choice{}, "", err
		}
	}
	var shared []sharedRow
	if mode.usesShared() {
		if shared, err = s.sharedAccounts(ctx, p.Workspace, cli); err != nil {
			return choice{}, "", err
		}
	}
	if err := s.byQueue(ctx, p.User, own, shared); err != nil {
		return choice{}, "", err
	}
	for _, r := range own {
		if r.state == Connected {
			return choice{row: r}, "", nil
		}
	}
	unusable := ""
	if len(shared) > 0 {
		if unusable, _, err = s.sharedUnusable(ctx); err != nil {
			return choice{}, "", err
		}
		if unusable == "" {
			a := shared[0]
			r, err := s.usageRow(ctx, p.User, a)
			return choice{row: r, shared: &a}, "", err
		}
	}
	if len(own) > 0 {
		return choice{row: own[0]}, "", nil
	}
	switch {
	case unusable != "":
		return choice{}, "this workspace's shared account for " + cli.Name() + " can't be used: " + unusable, nil
	case mode == SharedOnly:
		return choice{}, fmt.Sprintf("in this workspace, %s runs on the workspace's shared accounts only, and it has none: ask an admin to add one", cli.Name()), nil
	}
	return choice{}, fmt.Sprintf("%s isn't connected: connect it in Settings → CLI accounts, then send the message again", cli.Name()), nil
}

func (s *Service) accountOf(ctx context.Context, user int64, spec cliworker.TurnSpec) (row, bool, error) {
	if id := sharedOfSlot(spec.Slot); id != 0 {
		r, err := scanRow(s.db.QueryRowContext(ctx, `SELECT `+rowColumns+` FROM cli_accounts WHERE user_id = ? AND shared_account_id = ?`, user, id))
		if errors.Is(err, sql.ErrNoRows) {
			return row{}, false, nil
		}
		return r, err == nil, err
	}
	return s.load(ctx, user, ref{spec.CLI, spec.Slot})
}

func sharedView(e cliworker.Event) cliworker.Event {
	const refused = "the provider refused the workspace's shared account: an admin should check its key"
	switch {
	case e.Type == cliworker.EventLoginRequired:
		e.Type, e.Text = cliworker.EventNotice, refused
	case e.Type == cliworker.EventDone && e.Status == cliworker.StatusLoginRequired:
		e.Status, e.Text = cliworker.StatusFailed, refused
	}
	return e
}

func (s *Service) seen(spec cliworker.TurnSpec, e cliworker.Event) cliworker.Event {
	if e.Type == cliworker.EventDone {
		s.endTurn(spec.ID)
	}
	if !spec.Slot.Shared() {
		return e
	}
	return sharedView(e)
}

func (s *Service) recordEvent(ctx context.Context, p conversations.Principal, accountID int64, e cliworker.Event) {
	if accountID == 0 {
		return
	}
	switch {
	case e.Type == cliworker.EventSession:
		if err := s.useSession(ctx, p.Conversation, accountID, e.Session); err != nil {
			slog.Error("cli turn: recording a session", "conversation", p.Conversation, "err", err)
		}
	case e.Type == cliworker.EventUsage && len(e.Limits) > 0:
		if err := s.recordLimits(ctx, accountID, e.Limits); err != nil {
			slog.Error("cli turn: recording usage limits", "account", accountID, "err", err)
		}
	case e.Type == cliworker.EventLoginRequired:
		if err := s.setReconnect(ctx, accountID); err != nil {
			slog.Error("cli turn: marking a login for Reconnect", "account", accountID, "err", err)
		}
	}
}

func (s *Service) drain(ctx context.Context, p conversations.Principal, t Turn, seen int, accountID, input, output int64, hasUsage bool) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Hour)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	for failures := 0; ; {
		events, done, err := t.Wait(ctx, seen)
		if err != nil {
			if failures++; failures > 5 || ctx.Err() != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		failures = 0
		for _, e := range events {
			seen = e.Seq
			e = s.seen(t.Spec(), e)
			s.recordEvent(ctx, p, accountID, e)
			if e.Type == cliworker.EventUsage && e.Tokens != nil {
				input += e.Tokens.Input
				output += e.Tokens.Output
				hasUsage = true
			}
			if e.Type == cliworker.EventDone {
				s.recordTokens(ctx, p, t.Spec(), passed{input: input, output: output, hasUsage: hasUsage})
			}
		}
		if done {
			return
		}
	}
}

func (s *Service) handleStop(w http.ResponseWriter, r *http.Request, p conversations.Principal) {
	var req struct {
		Turn string `json:"turn"`
	}
	if !readJSONLimit(w, r, &req, maxBody) {
		return
	}
	if !turnIDPattern.MatchString(req.Turn) {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "turn is required")
		return
	}
	id := fmt.Sprintf("c%d-%s", p.Conversation, req.Turn)
	s.markStopped(id)
	for n := 0; n <= maxSwitches; n++ {
		t, ok, err := s.worker.FindTurn(p.User, chainID(id, n))
		if err != nil {
			workerError(w, r, ref{}, err)
			return
		}
		if !ok {
			continue
		}
		if t.Spec().User == p.User && t.Spec().Conversation == p.Conversation {
			if err := s.worker.StopTurn(p.User, chainID(id, n)); err != nil {
				workerError(w, r, ref{}, err)
				return
			}
			slog.InfoContext(r.Context(), "cli turn stopped", "conversation", p.Conversation, "turn", chainID(id, n))
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

var (
	waitPoll = 5 * time.Second
	waitFor  = 50 * time.Second
)

func (s *Service) handleWait(w http.ResponseWriter, r *http.Request, p conversations.Principal) {
	var req struct {
		CLI cliworker.CLI `json:"cli"`
	}
	if !readJSONLimit(w, r, &req, maxBody) {
		return
	}
	if !req.CLI.Valid() {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "unknown CLI")
		return
	}
	deadline := time.Now().Add(waitFor)
	for {
		state := Disconnected
		c, refusal, err := s.pick(r.Context(), p, req.CLI)
		if err != nil {
			if r.Context().Err() == nil {
				serverError(w, r, err)
			}
			return
		}
		switch {
		case refusal != "":
		case c.shared != nil:
			state = Connected
		default:
			state = c.row.state
		}
		if state == Connected || time.Now().After(deadline) {
			writeJSON(w, http.StatusOK, map[string]State{"state": state})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-s.accountChanged():
		case <-time.After(waitPoll):
		}
	}
}

func knownModel(cli cliworker.CLI, model string) bool {
	for _, m := range Models[cli] {
		if m == model {
			return true
		}
	}
	return false
}

func handOff(history []historyEntry, prompt string) string {
	var lines []string
	size := 0
	for i := len(history) - 1; i >= 0; i-- {
		h := history[i]
		if h.Role != "user" && h.Role != "assistant" {
			continue
		}
		text := strings.TrimSpace(h.Text)
		if text == "" {
			continue
		}
		line := h.Role + ": " + text
		if size+len(line) > maxHandOff {
			break
		}
		size += len(line) + 1
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return prompt
	}
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}
	return "Earlier in this conversation the user worked with another agent. The conversation so far:\n\n" +
		strings.Join(lines, "\n") + "\n\nThe user's new message:\n\n" + prompt
}

func (s *Service) session(ctx context.Context, conversation, account int64) (string, bool, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT session_id FROM cli_sessions WHERE conversation_id = ? AND account_id = ?`,
		conversation, account).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id, err == nil, err
}

func (s *Service) useSession(ctx context.Context, conversation, account int64, session string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO cli_sessions (conversation_id, account_id, session_id, used)
		VALUES (?, ?, ?, (SELECT coalesce(max(used), 0) + 1 FROM cli_sessions WHERE conversation_id = ?))
		ON CONFLICT (conversation_id, account_id) DO UPDATE SET session_id = excluded.session_id, used = excluded.used`,
		conversation, account, session, conversation)
	return err
}

func (s *Service) lastAccount(ctx context.Context, conversation int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT account_id FROM cli_sessions WHERE conversation_id = ?
		ORDER BY used DESC LIMIT 1`, conversation).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type eventWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	started bool
	failed  bool
}

func newEventWriter(w http.ResponseWriter) *eventWriter {
	f, _ := w.(http.Flusher)
	return &eventWriter{w: w, flusher: f}
}

func (o *eventWriter) write(e any) bool {
	if o.failed {
		return false
	}
	if !o.started {
		o.w.Header().Set("Content-Type", "application/x-ndjson")
		o.w.WriteHeader(http.StatusOK)
		o.started = true
	}
	b, err := json.Marshal(e)
	if err == nil {
		_, err = o.w.Write(append(b, '\n'))
	}
	if err != nil {
		o.failed = true
		return false
	}
	if o.flusher != nil {
		o.flusher.Flush()
	}
	return true
}

func (s *Service) conversationPermissions(ctx context.Context, conversation int64) (cliworker.Permissions, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT cli_permissions FROM conversations WHERE id = ?`, conversation).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return cliworker.Ask, nil
	}
	if err != nil {
		return "", err
	}
	if v == string(cliworker.AllowAll) {
		return cliworker.AllowAll, nil
	}
	return cliworker.Ask, nil
}

func (s *Service) Questions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error) {
	if s.worker == nil {
		return nil, nil
	}
	return s.worker.Permissions(ctx, user, conversation)
}

func (s *Service) Answer(ctx context.Context, user, conversation int64, id string, allow bool) error {
	if s.worker == nil {
		return cliworker.ErrNoPermission
	}
	return s.worker.Answer(ctx, user, conversation, id, allow)
}
