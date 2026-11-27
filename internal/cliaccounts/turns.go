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
)

// The agent runtime's cli_turn tool (agent/src/cli.ts) runs a conversation's turn on a CLI
// worker through these routes on the tools socket. A request names its conversation with the
// conversation's capability, like the knowledge-base tools; the turn always runs on the
// conversation owner's own account for the CLI, whatever the request says.
//
//	POST /cli/turn  {turn, cli, model, prompt, history?, replay?} → NDJSON events, the last one "done"
//	POST /cli/stop  {turn}                                         → 204
//	POST /cli/wait  {cli}                                          → {state}, once connected or after ~50 s
//
// The events are the worker's (cliworker.Event), and "interrupted" when a turn resumes after
// a restart. A turn the worker still has (the sidecar restarted, the worker didn't) is read
// from its start instead of run again.

// Turn is a running or finished turn of the worker (*cliworker.Turn).
type Turn interface {
	Spec() cliworker.TurnSpec
	Wait(ctx context.Context, after int) ([]cliworker.Event, bool, error)
	// Over reports whether the turn has ended, as of when it was found.
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

// Authorizer says whom a request on the tools socket is for (agenttools.Service.Authorize).
type Authorizer func(*http.Request) (conversations.Principal, error)

// TurnRoutes returns the handler of the tools socket's /cli/ routes.
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

// refusalCode is the code of a refusal of the sidecar's capability: these routes are the
// sidecar's, whose errors people don't see.
func refusalCode(status int) apierr.Code {
	switch status {
	case http.StatusUnauthorized:
		return apierr.CodeNotLoggedIn
	case http.StatusNotFound:
		return apierr.CodeNotFound
	}
	return apierr.CodeBadRequest
}

// Models are the CLIs' models offered for conversations: "default" leaves the CLI's own
// choice, and the others are the CLI's own names for them.
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

// maxTurnBody bounds a turn request: a prompt and the hand-off history.
const maxTurnBody = 512 << 10

// maxHandOff bounds the history put before a new session's first prompt.
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
		workerError(w, r, ref{cli: req.CLI}, err) // the stream hasn't started
		return
	} else if !ok {
		fail(cliworker.StatusFailed, req.CLI.Name()+" is not installed on this server")
		return
	}
	id := fmt.Sprintf("c%d-%s", p.Conversation, req.Turn)

	// A turn the worker still has is read from its start, with the turns it switched to: the
	// CLIs run once.
	t, found, err := s.worker.FindTurn(p.User, id)
	if err != nil {
		workerError(w, r, ref{cli: req.CLI}, err) // the stream hasn't started
		return
	}
	if found {
		// The ID names the conversation; its spec must be this conversation's owner's too,
		// as for a stop (a conversation's owner never changes, but the check is cheap).
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
	// With switching on, a turn doesn't start on an account known to be at its limit.
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
		workerError(w, r, ref{cli: req.CLI}, err) // an outage keeps its code while nothing was streamed
		return
	}
	if err != nil {
		slog.ErrorContext(r.Context(), "cli turn: the worker refused it", "conversation", p.Conversation, "cli", c.cli(), "err", err)
		fail(cliworker.StatusFailed, "the CLI worker refused the turn: "+err.Error())
		return
	}
	// Only once the worker has the turn: an outage before it is the answer above, not a stream.
	if req.Replay {
		// The tool ran before and the worker doesn't know the turn: the worker (or the whole
		// server) restarted in the middle of it, and stopped the CLI on its way up.
		out.write(cliworker.Event{Type: cliworker.EventInterrupted, Text: "the previous run of this turn was interrupted by a restart; resuming"})
	}
	if notice != "" {
		// The conversation moves to the CLI chosen only now that its turn runs there.
		s.switchModel(r.Context(), p, req.CLI, c.cli())
		out.write(cliworker.Event{Type: cliworker.EventNotice, Text: notice})
	}
	s.chain(r.Context(), out, p, req, id, t)
}

// start starts a worker turn with id for req on c's account: in the conversation's session
// there if it was the conversation's last, else in a new one that starts with the conversation
// so far. note goes before the prompt.
func (s *Service) start(ctx context.Context, p conversations.Principal, req turnRequest, id string, c choice, note string) (Turn, error) {
	acct, cli := c.row, c.cli()
	// The CLI asks the owner before it acts, unless the owner allowed everything in this
	// conversation (docs/agent-safety.md). Read for every turn: a change applies to the next.
	perms, err := s.conversationPermissions(ctx, p.Conversation)
	if err != nil {
		return nil, err
	}
	spec := cliworker.TurnSpec{ID: id, User: p.User, CLI: cli, Slot: acct.ref.slot, Conversation: p.Conversation, Prompt: req.Prompt, Permissions: perms}
	// The conversation's model is for its own CLI; another CLI it switched to takes its default.
	if req.Model != "default" && cli == req.CLI {
		spec.Model = req.Model
	}
	// taskto's own tools, with this turn's token for them (docs/cli-tools.md).
	if spec.Tools, err = s.toolsFor(id, p); err != nil {
		return nil, err
	}
	if c.shared != nil {
		// The workspace's account: the CLI goes through the proxy with this turn's token.
		token, err := s.mintToken(proxyClaims{Account: c.shared.ID, User: p.User, Conversation: p.Conversation, Turn: id})
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
	// A session on an account the conversation has left since doesn't know what happened on
	// the other one: a new session starts, with the conversation so far.
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
		// Claude Code and Grok take our ID: recorded before the CLI starts, so a crash resumes it.
		spec.Session = newUUID()
	}
	// The session in use is recorded before the CLI starts (Codex's new thread, once it names
	// it): it is the conversation's last from now.
	if spec.Session != "" {
		if err := s.useSession(ctx, p.Conversation, acct.id, spec.Session); err != nil {
			return nil, err
		}
	}
	// A new session in the middle of a conversation starts with the conversation so far; a
	// resumed one has it, unless it turns out to be gone and the worker starts a new one.
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
		s.endTurn(id) // its tokens go with it
		return nil, err
	}
	slog.InfoContext(ctx, "cli turn started", "turn", id, "conversation", p.Conversation, "user", p.User, "account", c.key(), "resume", spec.Resume)
	return t, nil
}

// maxPromptBytes keeps a prompt with its hand-off under the worker's bound.
const maxPromptBytes = 90 << 10

// markStopped records that the user stopped turn id; isStopped asks.
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

// chainID is the worker turn of a turn's n-th switch: the turn's own ID for none.
func chainID(id string, n int) string {
	if n == 0 {
		return id
	}
	return id + "-s" + strconv.Itoa(n)
}

// chain passes the events of the turn id, which runs as t, and of the turns it switched to,
// as one stream: their events numbered on from each other's, with a notice at each switch.
// When t's account stops while switching is on, it starts the next turn in the chain on the
// next account of the queue, with a hand-off. A chain read again (the sidecar re-attached)
// follows the switches it made before.
func (s *Service) chain(ctx context.Context, out *eventWriter, p conversations.Principal, req turnRequest, id string, t Turn) {
	tried := map[string]bool{}
	offset := 0
	for n := 0; ; n++ {
		if c, ok := s.choiceOf(ctx, p, t.Spec()); ok {
			tried[c.key()] = true
		}
		res, ok := s.pass(ctx, out, p, t, offset)
		if !ok {
			return // the client left: the rest is recorded in the background
		}
		nextID := chainID(id, n+1)
		next, found, _ := s.worker.FindTurn(p.User, nextID) // a worker gone now ends the chain below
		notice := s.switchNotice(nextID)
		if !found && n < maxSwitches {
			next, notice = s.switchAfter(ctx, out, p, req, t, res, id, nextID, tried)
		}
		if next == nil {
			res.done.Seq += offset
			out.write(res.done)
			return
		}
		offset += res.last + 1
		out.write(cliworker.Event{Seq: offset, Type: cliworker.EventNotice, Text: notice})
		t = next
	}
}

// passed is how a worker turn ended, as pass saw it.
type passed struct {
	done cliworker.Event
	// last is the done event's Seq; limited says the turn reported a limit reached; text is
	// what it had answered.
	last    int
	limited bool
	text    string
}

// pass passes t's events on, numbered from offset, until its done event, which it returns
// instead of passing. If the client leaves first, it records the rest in the background and
// returns false. On the way it records what the server keeps: a Codex thread's session, usage
// limits, and a login that needs renewing.
func (s *Service) pass(ctx context.Context, out *eventWriter, p conversations.Principal, t Turn, offset int) (passed, bool) {
	// Recording carries on if the client goes away: the events happened.
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
			// The client left, or the worker didn't answer (a worker in its own process that
			// restarts): either way the rest of the turn is recorded in the background.
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.drain(record, p, t, seen, accountID)
			}()
			return res, false
		}
		for _, e := range events {
			seen = e.Seq
			e = s.seen(spec, e)
			s.recordEvent(record, p, accountID, e)
			switch e.Type {
			case cliworker.EventUsage:
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

// choiceOf returns the account spec's turn ran on.
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

// stopped says why t's account stopped, so the turn may go on with another; "" if it didn't
// (it answered, the user stopped it, or it failed for another reason).
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

// switchAfter starts the next turn of a chain, on the next account of the queue, if switching
// is on and t's account stopped. It returns nil when the turn ends with t.
func (s *Service) switchAfter(ctx context.Context, out *eventWriter, p conversations.Principal, req turnRequest, t Turn, res passed, id, nextID string, tried map[string]bool) (Turn, string) {
	why := s.stopped(ctx, p, t, res)
	if why == "" || s.isStopped(id) {
		return nil, ""
	}
	if on, err := s.switchingOn(ctx, p.User); err != nil || !on {
		return nil, ""
	}
	next, ok, err := s.nextChoice(ctx, p, tried)
	if err != nil || !ok {
		if err != nil {
			slog.ErrorContext(ctx, "cli turn: choosing the next account", "conversation", p.Conversation, "err", err)
		}
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
	note += "\n\n"
	tn, err := s.start(ctx, p, req, nextID, next, note)
	if err != nil {
		slog.ErrorContext(ctx, "cli turn: switching", "conversation", p.Conversation, "err", err)
		return nil, ""
	}
	if s.isStopped(id) {
		// The user stopped the turn while this one started: it stops too, and the chain ends.
		_ = s.worker.StopTurn(p.User, nextID) // a worker gone now has no turn running
	}
	notice := fmt.Sprintf("%s %s: continuing on %s", from, why, next.title())
	s.mu.Lock()
	s.switches[nextID] = notice
	s.mu.Unlock()
	s.switchModel(ctx, p, t.Spec().CLI, next.cli())
	return tn, notice
}

// switchNotice is what the conversation showed when the turn switched to nextID, for a chain
// read again; a plain one if the server restarted since.
func (s *Service) switchNotice(nextID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n, ok := s.switches[nextID]; ok {
		return n
	}
	return "continuing on another account"
}

// switchModel sets the conversation's model to the CLI it switched to, when that is another.
func (s *Service) switchModel(ctx context.Context, p conversations.Principal, from, to cliworker.CLI) {
	if from == to || s.setModel == nil {
		return
	}
	if err := s.setModel(context.WithoutCancel(ctx), p.AgentConversation, to); err != nil {
		slog.WarnContext(ctx, "cli turn: setting the conversation's model after a switch", "conversation", p.Conversation, "err", err)
	}
}

// choice is the account a turn runs on: one of the owner's own, or a shared account of the
// conversation's workspace (then row is the owner's directory for it).
type choice struct {
	row    row
	shared *sharedRow
}

// pick returns the account a turn of the conversation on cli runs on, as the owner's mode in
// the conversation's workspace allows (docs/account-switching.md):
//
//   - own_else_shared: their first own account for the CLI that is connected; else the
//     workspace's first shared account for it; else their first own (which needs a new
//     sign-in);
//   - own: their own accounts only;
//   - shared: the workspace's shared accounts only.
//
// With none, refusal says why.
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
	// The user's queue orders their accounts, also while switching is off.
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

// accountOf returns the account directory spec's turn runs in: one of user's own, or theirs
// for a shared account.
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

// sharedView is how a turn on a shared account shows an event: a refused key is not the
// user's login to renew, but the account's, for an admin to check.
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

// seen is what the server makes of each of a turn's events before it records and passes them:
// its tokens stop working once it is done, and a turn on a shared account shows a refused key
// as the account's.
func (s *Service) seen(spec cliworker.TurnSpec, e cliworker.Event) cliworker.Event {
	if e.Type == cliworker.EventDone {
		s.endTurn(spec.ID)
	}
	if !spec.Slot.Shared() {
		return e
	}
	return sharedView(e)
}

// recordEvent keeps what the server keeps of an event: a Codex thread or a new session in
// place of a lost one, usage limits, and an expired login.
func (s *Service) recordEvent(ctx context.Context, p conversations.Principal, accountID int64, e cliworker.Event) {
	if accountID == 0 {
		return // the account was disconnected in the meantime: nothing to keep it on
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

// drain records the rest of t's events after seen, when no client reads them any more.
func (s *Service) drain(ctx context.Context, p conversations.Principal, t Turn, seen int, accountID int64) {
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
			// A worker that doesn't answer is asked again a few times; one that has lost the
			// turn (it restarted) has nothing more to give.
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
			s.recordEvent(ctx, p, accountID, s.seen(t.Spec(), e))
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
	// The ID names the conversation, so a capability only ever reaches its own turns. The turn
	// and every turn it switched to are stopped.
	id := fmt.Sprintf("c%d-%s", p.Conversation, req.Turn)
	// A stopped turn switches no more: a switch in flight sees this before and after it starts
	// the next turn.
	s.markStopped(id)
	// Every place in the chain is looked at: an earlier turn may be gone from the worker
	// while a later one runs.
	// A worker that can't be asked is the answer: the turn wasn't stopped, and the caller tries
	// again, rather than hearing it was.
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

// waitPoll and waitFor bound a wait for a Reconnect: the tool asks again after each.
var (
	waitPoll = 5 * time.Second // a backstop: a connect wakes the waiters at once (accountChanged)
	waitFor  = 50 * time.Second
)

// handleWait answers once the account a turn of the conversation on the CLI would run on is
// connected (the one that needed a new sign-in, or another the owner connected), or after
// waitFor with its state as it is, so the tool asks again.
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

// handOff puts the conversation so far before prompt, for a CLI session that starts in the
// middle of a conversation (another CLI, model provider or account had it): the newest
// entries that fit in maxHandOff.
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

// useSession records that conversation runs in session on account, in place of the session
// it had there, and that this is the conversation's last session from now.
func (s *Service) useSession(ctx context.Context, conversation, account int64, session string) error {
	// Writes take the database's write lock when they begin, so the counter can't be read twice.
	_, err := s.db.ExecContext(ctx, `INSERT INTO cli_sessions (conversation_id, account_id, session_id, used)
		VALUES (?, ?, ?, (SELECT coalesce(max(used), 0) + 1 FROM cli_sessions WHERE conversation_id = ?))
		ON CONFLICT (conversation_id, account_id) DO UPDATE SET session_id = excluded.session_id, used = excluded.used`,
		conversation, account, session, conversation)
	return err
}

// lastAccount returns the account of conversation's last used session, 0 if it has none.
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

// eventWriter writes NDJSON events and flushes each.
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

// write sends e, reporting false once the client is gone.
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

// conversationPermissions is the conversation's cli_permissions: Ask unless its owner chose
// AllowAll.
func (s *Service) conversationPermissions(ctx context.Context, conversation int64) (cliworker.Permissions, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT cli_permissions FROM conversations WHERE id = ?`, conversation).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return cliworker.Ask, nil // gone meanwhile: the safe answer
	}
	if err != nil {
		return "", err
	}
	if v == string(cliworker.AllowAll) {
		return cliworker.AllowAll, nil
	}
	return cliworker.Ask, nil
}

// Questions returns the permission questions waiting in user's conversation (none while CLI
// workers are off).
func (s *Service) Questions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error) {
	if s.worker == nil {
		return nil, nil
	}
	return s.worker.Permissions(ctx, user, conversation)
}

// Answer answers permission question id in user's conversation.
func (s *Service) Answer(ctx context.Context, user, conversation int64, id string, allow bool) error {
	if s.worker == nil {
		return cliworker.ErrNoPermission
	}
	return s.worker.Answer(ctx, user, conversation, id, allow)
}
