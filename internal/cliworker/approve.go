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

const ApproveArg = "cli-approve"

const (
	hookPermissionRequest = "PermissionRequest"
	hookPreToolUse        = "PreToolUse"
)

const hookTimeout = 3600

const replyWait = 45 * time.Minute

type approveRequest struct {
	CLI  CLI             `json:"cli"`
	Hook json.RawMessage `json:"hook"`
}

type approveReply struct {
	Allow  bool   `json:"allow"`
	Reason string `json:"reason,omitempty"`
}

type hookInput struct {
	Event   string          `json:"hook_event_name"`
	Session string          `json:"session_id"`
	Tool    string          `json:"tool_name"`
	Input   json.RawMessage `json:"tool_input"`
}

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
		if err := os.Chmod(dir, 0o711); err != nil {
			_ = os.RemoveAll(dir)
			return "", "", err
		}
	}
	return filepath.Join(dir, "s"), dir, nil
}

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
	ln, err := new(net.ListenConfig).Listen(context.Background(), "unix", path)
	if err != nil {
		return fmt.Errorf("the approval socket: %w", err)
	}
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
				return
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

func (w *Worker) closeApprovals() {
	if w.approvals != nil {
		_ = w.approvals.Close()
		_ = os.Remove(w.approveSocket)
	}
	if w.approvalsDir != "" {
		_ = os.RemoveAll(w.approvalsDir)
	}
}

func (w *Worker) approveCommand(cli CLI) []string {
	return []string{w.cfg.Helper, ApproveArg, w.approveSocket, string(cli)}
}

func (w *Worker) serveApproval(conn net.Conn) {
	deny := func(reason string) {
		_ = json.NewEncoder(conn).Encode(approveReply{Reason: reason})
	}
	var user int64
	if iso := w.cfg.Isolation; iso != nil {
		uid, err := peerUID(conn)
		if err != nil || uid < iso.UIDBase+1 {
			deny("only a CLI user may ask")
			return
		}
		user = int64(uid - iso.UIDBase)
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
	t := w.waitTurnForHook(user, req.CLI, h.Session)
	if t == nil {
		deny("no running turn of this user has this session")
		return
	}
	allow, reason := w.decide(t, h)
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = json.NewEncoder(conn).Encode(approveReply{Allow: allow, Reason: reason})
}

const sessionWait = 10 * time.Second

func (w *Worker) waitTurnForHook(user int64, cli CLI, session string) *Turn {
	deadline := time.NewTimer(sessionWait)
	defer deadline.Stop()
	for {
		w.mu.Lock()
		named, freed := w.named, w.freed
		w.mu.Unlock()
		t, unnamed := w.turnForHook(user, cli, session)
		if t != nil || !unnamed {
			return t
		}
		select {
		case <-named:
		case <-freed:
		case <-deadline.C:
			return nil
		}
	}
}

func (w *Worker) turnForHook(user int64, cli CLI, session string) (turn *Turn, unnamed bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var only *Turn
	count := 0
	for _, t := range w.turns {
		if user != 0 && t.spec.User != user {
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

func (w *Worker) decide(t *Turn, h hookInput) (bool, string) {
	if t.spec.Permissions == AllowAll {
		return true, ""
	}
	if h.Event != hookPermissionRequest && h.Event != hookPreToolUse {
		return false, "unknown hook event " + strconv.Quote(cut(h.Event, 40))
	}
	if t.spec.Tools.readOnly(t.spec.CLI, h.Tool, h.Input) {
		return true, ""
	}
	if h.Event == hookPreToolUse && readOnly(h.Tool, h.Input, w.cfg.Layout.User(t.spec.User).Conversation(t.spec.Conversation)) {
		return true, ""
	}
	return w.ask(t, h.Tool, describeInput(h.Input))
}

func describeInput(input json.RawMessage) string {
	var fields map[string]any
	if json.Unmarshal(input, &fields) == nil {
		if c, ok := fields["command"].(string); ok {
			return c
		}
	}
	return string(input)
}

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
		fmt.Fprintln(stderr, denial(reply.Reason))
		return 2
	}
	return 0
}

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
	_ = conn.SetDeadline(time.Now().Add(replyWait))
	var reply approveReply
	if err := json.NewDecoder(conn).Decode(&reply); err != nil {
		return approveReply{}, fmt.Errorf("no answer from the CLI worker: %w", err)
	}
	return reply, nil
}

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

func denial(reason string) string {
	if reason == "" {
		reason = "denied"
	}
	return "Not allowed: " + strings.TrimSuffix(reason, ".") + ". Don't try to get around this; ask the user if you need it."
}
