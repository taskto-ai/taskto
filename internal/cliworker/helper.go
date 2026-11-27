package cliworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"taskto.ai/taskto/internal/browser"
)

const HelperArg = "cli-helper"

const promptFilePlaceholder = "{prompt-file}"

type helperRequest struct {
	Op           string            `json:"op"`
	LayoutRoot   string            `json:"layout_root"`
	Home         int64             `json:"home"`
	CLI          CLI               `json:"cli"`
	Slot         Slot              `json:"slot,omitempty"`
	Proxy        *Proxy            `json:"proxy,omitempty"`
	Tools        *Tools            `json:"tools,omitempty"`
	Bin          string            `json:"bin,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Path         string            `json:"path,omitempty"`
	Overrides    Overrides         `json:"overrides,omitzero"`
	Conversation int64             `json:"conversation,omitempty"`
	PromptFile   bool              `json:"prompt_file,omitempty"`
	Sandbox      string            `json:"sandbox,omitempty"`
	Thread       string            `json:"thread,omitempty"`
	Approve      []string          `json:"approve,omitempty"`
	AllowRoot    bool              `json:"allow_root,omitempty"`
	Browser      *browser.ProcSpec `json:"browser,omitempty"`
}

func RunHelper() int {
	runtime.LockOSThread()
	if err := runHelper(os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cli-helper:", err)
		return 2
	}
	return 0
}

func runHelper(stdin *os.File, stdout io.Writer) error {
	line, err := readLine(stdin, 1<<20)
	if err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	var req helperRequest
	if err := json.Unmarshal(line, &req); err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}
	if req.Home < firstHome || req.Home >= maxHome || !filepath.IsAbs(req.LayoutRoot) {
		return errors.New("invalid request")
	}
	layout := Layout{Root: req.LayoutRoot}
	if strings.HasPrefix(req.Op, "browser") {
		return runBrowserOp(req, layout, stdout)
	}
	if !req.CLI.Valid() || !req.Slot.Valid() {
		return errors.New("invalid request")
	}
	switch req.Op {
	case "limits":
		dirs := layout.Home(req.Home)
		return json.NewEncoder(stdout).Encode(codexLimits(dirs.ConfigOf(Codex, req.Slot), req.Thread))
	case "authfile":
		dirs := layout.Home(req.Home)
		info, err := os.Stat(filepath.Join(dirs.ConfigOf(req.CLI, req.Slot), credentials(req.CLI)))
		return json.NewEncoder(stdout).Encode(map[string]bool{"exists": err == nil && info.Mode().IsRegular() && info.Size() > 0})
	case "reset":
		if err := os.RemoveAll(layout.Home(req.Home).ConfigOf(req.CLI, req.Slot)); err != nil {
			return err
		}
		_, err := layout.Ensure(req.Home)
		if err == nil {
			_, err = io.WriteString(stdout, "{}\n")
		}
		return err
	case "exec":
	default:
		return fmt.Errorf("unknown op %q", req.Op)
	}

	if err := unprivileged(req); err != nil {
		return err
	}
	dirs, err := layout.EnsureSlot(req.Home, req.CLI, req.Slot)
	if err != nil {
		return err
	}
	cleanTmp(dirs.Tmp)
	if req.Proxy.check(req.Slot) != nil {
		return errors.New("invalid proxy")
	}
	if req.Tools.check() != nil {
		return errors.New("invalid tools")
	}
	if err := prepare(req.CLI, req.Slot, dirs, req.Overrides, req.Sandbox, req.Proxy, req.Tools, req.Approve); err != nil {
		return fmt.Errorf("writing the CLI's settings: %w", err)
	}
	dir := dirs.Home
	if req.Conversation > 0 {
		dir = dirs.Conversation(req.Conversation)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	args := make([]string, len(req.Args))
	for i, a := range req.Args {
		args[i] = strings.ReplaceAll(a, toolsFilePlaceholder, filepath.Join(dirs.ConfigOf(req.CLI, req.Slot), toolsFile))
	}
	if req.PromptFile {
		tmp, err := os.MkdirTemp(dirs.Tmp, "turn-")
		if err != nil {
			return err
		}
		file := filepath.Join(tmp, "prompt.txt")
		prompt, err := io.ReadAll(io.LimitReader(stdin, maxPrompt+1))
		if err != nil {
			return err
		}
		if len(prompt) > maxPrompt {
			return errors.New("the prompt is over 96 KiB")
		}
		if err := os.WriteFile(file, prompt, 0o600); err != nil {
			return err
		}
		for i, a := range args {
			args[i] = strings.ReplaceAll(a, promptFilePlaceholder, file)
		}
		if err := closeStdin(); err != nil {
			return err
		}
	}
	if err := os.Chdir(dir); err != nil {
		return err
	}
	bin, err := exec.LookPath(req.Bin)
	if err != nil {
		return err
	}
	return execve(bin, append([]string{bin}, args...), env(req.CLI, req.Slot, dirs, req.Path, req.Overrides, req.Proxy, req.Tools))
}

func readLine(f *os.File, limit int) ([]byte, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < limit {
		n, err := f.Read(b)
		if n == 1 {
			if b[0] == '\n' {
				return line, nil
			}
			line = append(line, b[0])
		}
		if err != nil {
			return nil, err
		}
	}
	return nil, errors.New("request too long")
}

func cleanTmp(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err == nil && strings.HasPrefix(e.Name(), "turn-") && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}

func credentials(cli CLI) string {
	if cli == Claude {
		return ".credentials.json"
	}
	return "auth.json"
}

func (w *Worker) helperCommand(acct account, req helperRequest) (*exec.Cmd, []byte, error) {
	w.helperLaunches.Add(1)
	if req.Op != "browser-purge" {
		w.mu.Lock()
		err := w.admissible(acct, "", "")
		w.mu.Unlock()
		if err != nil {
			return nil, nil, err
		}
	}
	w.helpersBuilt.Add(1)
	req.LayoutRoot, req.Home = w.cfg.Layout.Root, acct.home
	if req.Path == "" {
		req.Path = w.cfg.Path
	}
	req.Overrides = w.cfg.Overrides[req.CLI]
	req.Approve = w.approveCommand(req.CLI)
	req.AllowRoot = w.cfg.UnsafePrivileged
	if w.cfg.Isolation != nil && req.Sandbox == "" {
		req.Sandbox = "danger-full-access"
	}
	header, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.CommandContext(context.Background(), w.cfg.Helper, HelperArg)
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=" + w.cfg.Path}
	newGroup(cmd)
	if err := w.asUser(cmd, acct.home); err != nil {
		return nil, nil, err
	}
	return cmd, append(header, '\n'), nil
}

func unprivileged(req helperRequest) error {
	if (os.Getuid() == 0 || os.Geteuid() == 0) && !req.AllowRoot {
		return errors.New("refusing to run an agent's program as root (docs/agent-safety.md)")
	}
	if err := noNewPrivs(); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}
	return nil
}
