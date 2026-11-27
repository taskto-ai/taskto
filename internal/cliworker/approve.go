package cliworker

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The approval socket (docs/agent-safety.md): every CLI's hook runs `taskto cli-approve
// <socket> <cli>` (RunApprove) before a tool runs, which sends the hook's input here and
// prints the answer in the hook's reply format.
//
// The socket is one per worker, in the layout's root. In container mode every CLI user may
// open it (the root is 711 and the socket 666), and the worker knows who asks from the
// socket's peer uid: a user can only ask about their own turns. In local mode it is the
// server's user's alone.
//
// A request is one JSON line, {"cli", "hook": <the hook's input>}; the answer one line,
// {"allow", "reason"}.

// ApproveArg is the hook program's subcommand: `taskto cli-approve <socket> <cli>`.
const ApproveArg = "cli-approve"

// The hook events the worker answers: Claude Code's PermissionRequest (it would ask) and
// Codex's and Grok Build's PreToolUse (every tool call).
const (
	hookPermissionRequest = "PermissionRequest"
	hookPreToolUse        = "PreToolUse"
)

// hookTimeout is how long a CLI lets our hook run, in seconds: longer than any wait for an
// answer (AskTimeout), so the worker's denial, never the CLI's timeout, ends a question.
const hookTimeout = 3600

// replyWait bounds how long the hook program waits for the worker's answer: past any
// AskTimeout a worker would use, and short of hookTimeout.
const replyWait = 45 * time.Minute

type approveRequest struct {
	CLI  CLI             `json:"cli"`
	Hook json.RawMessage `json:"hook"`
}

type approveReply struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

// hookInput is what the worker reads of a hook's input (the CLIs share Claude Code's shape).
type hookInput struct {
	Event   string          `json:"hook_event_name"`
	Session string          `json:"session_id"`
	Tool    string          `json:"tool_name"`
	Input   json.RawMessage `json:"tool_input"`
}

// approvalSocket returns where the worker's approval socket goes: in the layout's root, or,
// if that path is too long for a socket, in a directory of its own under the system's
// temporary directory, which the worker removes on Close. With isolation that directory is
// passable (711), as the root is, so every CLI user can reach the socket.
func approvalSocket(root string, shared bool) (path, tmpDir string, err error) {
	path = filepath.Join(root, ".approve.sock")
	if len(path) <= 100 {
		return path, "", nil
	}
	dir, err := os.MkdirTemp("", "taskto-approve-")
	if err != nil {
		return "", "", err
	}
	if shared {
		if err := os.Chmod(dir, 0o711); err != nil { //nolint:gosec // G302: passable, not listable
			_ = os.RemoveAll(dir)
			return "", "", err
		}
	}
	return filepath.Join(dir, "s"), dir, nil
}

// listenApprovals opens the approval socket and serves it until Close.
func (w *Worker) listenApprovals() error {
	path, tmpDir, err := approvalSocket(w.cfg.Layout.Root, w.cfg.Isolation != nil)
	if err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode().Type() != os.ModeSocket {
			return fmt.Errorf("%s exists and isn't a socket", path)
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	// No umask change (it is the whole process's): the socket's mode is set right after, and
	// until then its directory guards it (the layout's root, or the fallback directory).
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	if err != nil {
		return fmt.Errorf("the approval socket: %w", err)
	}
	// With isolation every CLI user asks here, and the peer uid says which; otherwise it is
	// the server's user's alone.
	mode := os.FileMode(0o600)
	if w.cfg.Isolation != nil {
		mode = 0o666
	}
	if err := os.Chmod(path, mode); err != nil {
		ln.Close()
		return err
	}
	w.approvals, w.approvalsDir, w.approveSocket = ln, tmpDir, path
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // closed
			}
			w.wg.Add(1)
			go func() {
				defer w.wg.Done()
				defer conn.Close()
				w.serveApproval(conn)
			}()
		}
	}()
	return nil
}

// closeApprovals closes the approval socket.
func (w *Worker) closeApprovals() {
	if w.approvals != nil {
		_ = w.approvals.Close()
		_ = os.Remove(w.approveSocket)
	}
	if w.approvalsDir != "" {
		_ = os.RemoveAll(w.approvalsDir)
	}
}

// approveCommand is the hook command line for cli: the helper binary, ApproveArg, the
// socket and the CLI.
func (w *Worker) approveCommand(cli CLI) []string {
	return []string{w.cfg.Helper, ApproveArg, w.approveSocket, string(cli)}
}

// serveApproval answers one hook's question.
func (w *Worker) serveApproval(conn net.Conn) {
	deny := func(reason string) {
		_ = json.NewEncoder(conn).Encode(approveReply{Reason: reason})
	}
	// The account that asks: by the peer uid with isolation, which is its home's (uidOf) and no
	// other account's; any (home 0) in local mode, where all turns are the server's user's.
	var home int64
	if iso := w.cfg.Isolation; iso != nil {
		uid, err := peerUID(conn)
		if err != nil || uid < iso.UIDBase+firstHome || uid >= iso.UIDBase+maxHome {
			deny("only a CLI user may ask")
			return
		}
		home = int64(uid - iso.UIDBase)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := bufio.NewReaderSize(io.LimitReader(conn, 1<<20), 64<<10).ReadBytes('\n')
	if err != nil {
		deny("unreadable request")
		return
	}
	_ = conn.SetReadDeadline(time.Time{})
	var req approveRequest
	var h hookInput
	if json.Unmarshal(line, &req) != nil || json.Unmarshal(req.Hook, &h) != nil {
		deny("unreadable request")
		return
	}
	t := w.waitTurnForHook(home, req.CLI, h.Session)
	if t == nil {
		deny("no running turn of this user has this session")
		return
	}
	allow, reason := w.decide(t, h)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = json.NewEncoder(conn).Encode(approveReply{Allow: allow, Reason: reason})
}

// sessionWait bounds how long a hook's question waits for its turn's CLI to name the session
// the question is about (waitTurnForHook).
const sessionWait = 10 * time.Second

// waitTurnForHook is turnForHook, and, while the session isn't one of a turn's yet but a
// running turn of home's on cli hasn't named its own, it waits for that turn to name it, up
// to sessionWait or until a turn ends. Codex names a new thread on its stdout before it runs
// a tool, but the hook asks on another channel: the worker may not have read that line yet.
func (w *Worker) waitTurnForHook(home int64, cli CLI, session string) *Turn {
	deadline := time.NewTimer(sessionWait)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		named, freed := w.named, w.freed
		w.mu.Unlock()
		t, unnamed := w.turnForHook(home, cli, session)
		if t != nil || !unnamed {
			return t
		}
		select {
		case <-named:
		case <-freed: // a turn ended: it won't name its session any more
		case <-deadline.C:
			return nil
		}
	}
}

// turnForHook finds the running turn a hook's question belongs to: the account's with home (any
// account's in local mode, home 0) on cli with session; or, from a hook that names no session,
// that account's only running turn on cli. A session no running turn of the account's on cli has
// is no turn. unnamed reports whether a running turn of the account's on cli runs a session it
// hasn't named yet.
func (w *Worker) turnForHook(home int64, cli CLI, session string) (turn *Turn, unnamed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var only *Turn
	count := 0
	for _, t := range w.turns {
		if home != 0 && t.acct.home != home {
			continue
		}
		running, has, waiting := t.running(session)
		if !running {
			continue
		}
		if t.spec.CLI == cli && waiting {
			unnamed = true
		}
		if session != "" {
			if has && t.spec.CLI == cli {
				return t, false
			}
			continue
		}
		if t.spec.CLI == cli {
			only = t
			count++
		}
	}
	if count == 1 {
		return only, false
	}
	return nil, unnamed && session != ""
}

// decide answers a hook's question for turn t: at once for an AllowAll turn, a read-only
// taskto tool or a read-only PreToolUse call, or else by asking the owner.
func (w *Worker) decide(t *Turn, h hookInput) (bool, string) {
	if t.spec.Permissions == AllowAll {
		return true, ""
	}
	if h.Event != hookPermissionRequest && h.Event != hookPreToolUse {
		return false, "unknown hook event " + strconv.Quote(cut(h.Event, 40))
	}
	// Claude Code asks about every MCP tool, read-only or not: taskto's read-only tools don't
	// ask, whichever hook it is.
	if t.spec.Tools.readOnly(t.spec.CLI, h.Tool, h.Input) {
		return true, ""
	}
	if h.Event == hookPreToolUse && readOnly(h.Tool, h.Input, w.cfg.Layout.Home(t.acct.home).Conversation(t.spec.Conversation)) {
		return true, ""
	}
	return w.ask(t, h.Tool, describeInput(h.Input))
}

// describeInput is a tool's input for a person: the command of a shell tool, else its JSON.
func describeInput(input json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(input, &fields) == nil {
		if c, ok := fields["command"].(string); ok {
			return c
		}
	}
	return string(input)
}

// RunApprove is the hook program's main (`taskto cli-approve <socket> <cli>`, args without
// the program and subcommand): it reads a hook's input on stdin, asks the worker, and prints
// the answer in the format of the hook's event. Anything that goes wrong denies: it prints a
// denial and exits 2, which every CLI also reads as "block".
func RunApprove(args []string) int {
	return runApprove(args, os.Stdin, os.Stdout, os.Stderr)
}

func runApprove(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	in, err := io.ReadAll(io.LimitReader(stdin, 1<<20))
	var h hookInput
	if err == nil {
		err = json.Unmarshal(in, &h)
	}
	reply := approveReply{Reason: "taskto's permission check failed"}
	if err == nil && len(args) == 2 {
		reply, err = askWorker(args[0], CLI(args[1]), in)
	} else if err == nil {
		err = errors.New("usage: cli-approve <socket> <cli>")
	}
	if err != nil {
		fmt.Fprintln(stderr, "cli-approve:", err)
		reply = approveReply{Reason: "taskto's permission check failed: " + err.Error()}
	}
	out := hookReply(h.Event, reply)
	_, _ = stdout.Write(append(out, '\n'))
	if !reply.Allow {
		// A denial is said three ways, so every CLI reads it: the reply on stdout, exit code 2,
		// and the reason on stderr. Codex 0.169 ignores a hook that exits 2 with nothing on
		// stderr, its JSON included, and runs the tool (checked against the real CLI).
		fmt.Fprintln(stderr, denial(reply.Reason))
		return 2
	}
	return 0
}

// askWorker asks the worker on socket about a hook's input.
func askWorker(socket string, cli CLI, hook []byte) (approveReply, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socket)
	if err != nil {
		return approveReply{}, fmt.Errorf("can't reach the CLI worker: %w", err)
	}
	defer conn.Close()
	b, _ := json.Marshal(approveRequest{CLI: cli, Hook: hook})
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return approveReply{}, err
	}
	// The answer can take as long as the owner does (AskTimeout, 20 minutes by default), but
	// never as long as the CLI's own hook timeout: a worker that doesn't answer is a denial.
	_ = conn.SetDeadline(time.Now().Add(replyWait))
	var reply approveReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return approveReply{}, fmt.Errorf("no answer from the CLI worker: %w", err)
	}
	return reply, nil
}

// hookReply is the answer in the reply format of the hook event (Claude Code's, which Codex
// and Grok Build share for PreToolUse).
func hookReply(event string, r approveReply) []byte {
	var out any
	if event == hookPermissionRequest {
		decision := map[string]any{"behavior": "allow"}
		if !r.Allow {
			decision = map[string]any{"behavior": "deny", "message": denial(r.Reason)}
		}
		out = map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": hookPermissionRequest, "decision": decision}}
	} else {
		o := map[string]any{"hookEventName": hookPreToolUse, "permissionDecision": "allow"}
		if !r.Allow {
			o["permissionDecision"], o["permissionDecisionReason"] = "deny", denial(r.Reason)
		}
		out = map[string]any{"hookSpecificOutput": o}
	}
	b, _ := json.Marshal(out)
	return b
}

// denial is what the model is told when a tool is denied.
func denial(reason string) string {
	if reason == "" {
		reason = "denied"
	}
	return "Not allowed: " + strings.TrimSuffix(reason, ".") + ". Don't try to get around this; ask the user if you need it."
}
