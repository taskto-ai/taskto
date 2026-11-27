package cliworker

import (
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

// The helper is how every CLI process starts, and the only code that touches a user's
// directories: `taskto cli-helper`, the same binary, started for one user. In container mode
// it runs as that user's own Unix user, so nothing the worker does as root ever opens a path
// inside a home (a user can make any path there a symlink to anywhere). In local mode it
// runs as the server's user, the same code either way.
//
// It reads one JSON request from its stdin, up to a newline, byte by byte so that nothing
// after it is consumed, and then:
//
//   - "exec": prepares the user's directories and the CLI's settings, and execs the CLI in
//     the conversation's working directory (or the home) with the clean environment. What
//     follows the request on stdin is the CLI's stdin (a prompt, or a login's code); with
//     PromptFile it is read into a file the CLI gets instead (Grok Build) and stdin closes.
//   - "limits": prints the usage limits Codex recorded for a thread, as JSON.
//   - "authfile": prints whether the CLI's credentials file exists, as JSON.
//   - "browser", "browser-vnc", "browser-clean", "browser-purge": the user's agent browser (browser.go).
//
// The exec replaces the helper, so the CLI keeps its PID: the worker's process group,
// records and signals work as if it had started the CLI itself.

// HelperArg is the helper's subcommand.
const HelperArg = "cli-helper"

// promptFilePlaceholder stands for the prompt file's path in an exec request's Args.
const promptFilePlaceholder = "{prompt-file}"

type helperRequest struct {
	Op         string `json:"op"`
	LayoutRoot string `json:"layout_root"`
	// Home is the account's home number (accounts.go): never its identity, which no CLI sees.
	Home int64 `json:"home"`
	CLI  CLI   `json:"cli"`
	// Slot is the account directory: the CLI's config directory for it.
	Slot Slot `json:"slot,omitempty"`
	// Proxy, for a turn on a shared account (see TurnSpec.Proxy).
	Proxy *Proxy `json:"proxy,omitempty"`
	// Tools, for a turn with taskto's tools (see TurnSpec.Tools).
	Tools     *Tools    `json:"tools,omitempty"`
	Bin       string    `json:"bin,omitempty"`
	Args      []string  `json:"args,omitempty"`
	Path      string    `json:"path,omitempty"`
	Overrides Overrides `json:"overrides,omitzero"`
	// Conversation sets the working directory; 0 means the home.
	Conversation int64 `json:"conversation,omitempty"`
	// PromptFile: the rest of stdin is the prompt, for a file (promptFilePlaceholder in Args).
	PromptFile bool `json:"prompt_file,omitempty"`
	// Sandbox is Codex's sandbox_mode.
	Sandbox string `json:"sandbox,omitempty"`
	// Thread is the Codex thread for "limits".
	Thread string `json:"thread,omitempty"`
	// Approve is the permission hook's command line, written into the CLI's settings.
	Approve []string `json:"approve,omitempty"`
	// AllowRoot lets an exec run the CLI as root (the worker's UnsafePrivileged).
	AllowRoot bool `json:"allow_root,omitempty"`
	// Browser is the process a "browser" op starts (browser.go).
	Browser *browser.ProcSpec `json:"browser,omitempty"`
}

// RunHelper is the helper's main: call it when os.Args[1] is HelperArg, and exit with what
// it returns. An exec never returns: the CLI has taken the process over.
func RunHelper() int {
	// no_new_privs (unprivileged) is a property of the OS thread that sets it, and the program
	// exec'd gets the attributes of the thread that calls execve. Without this lock the
	// goroutine may move to another thread between the two (the helper does file I/O in
	// between), and the CLI or browser would then run without no_new_privs. Never unlocked:
	// the exec replaces the process, and an error ends it.
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
		// Disconnecting: everything the CLI kept for the account goes, then the empty
		// directories are made again (the first account's; another's is made when used).
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

// readLine reads up to a newline, one byte at a time, so the rest stays for the CLI.
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

// cleanTmp removes the turns' temporary directories older than a day: a prompt file is left
// behind when the helper execs the CLI.
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

// credentials is where each CLI keeps its login, in its config directory.
func credentials(cli CLI) string {
	if cli == Claude {
		return ".credentials.json"
	}
	return "auth.json"
}

// helperCommand returns the command that runs req for acct through the helper: as the account's
// own Unix user in container mode. Its stdin starts with the request; extra follows it (the
// CLI's stdin), unless the caller wires stdin itself (then extra is nil and it must write
// header() first).
func (w *Worker) helperCommand(acct account, req helperRequest) (*exec.Cmd, []byte, error) {
	w.helperLaunches.Add(1)
	// No helper runs for a retired account, nor while its home is being removed: a removal's own
	// steps make none, apart from the browser's removal, a purge's step, which runs under the gate
	// it holds (browserHost.Purge) and may come after the account was retired.
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
	// Only the worker's own (tests'): a request never brings its own.
	req.Overrides = w.cfg.Overrides[req.CLI]
	req.Approve = w.approveCommand(req.CLI)
	req.AllowRoot = w.cfg.UnsafePrivileged
	if w.cfg.Isolation != nil && req.Sandbox == "" {
		// The OS boundary is ours; Codex's own sandbox can't start inside a container.
		req.Sandbox = "danger-full-access"
	}
	header, err := json.Marshal(req)
	if err != nil {
		return nil, nil, err
	}
	cmd := exec.Command(w.cfg.Helper, HelperArg) //nolint:gosec,noctx // G204: our own binary; stopped as a group by the caller
	cmd.Dir = "/"
	// The helper builds the CLI's environment itself; it needs none of ours.
	cmd.Env = []string{"PATH=" + w.cfg.Path}
	newGroup(cmd)
	if err := w.asUser(cmd, acct.home); err != nil {
		return nil, nil, err
	}
	return cmd, append(header, '\n'), nil
}

// unprivileged is the last step before the helper execs a CLI or an agent browser: they and
// every command they run never act as root (docs/agent-safety.md). It refuses to go on as
// root (unless the worker's development override allows it), and sets no_new_privs, which
// the exec keeps, so no setuid program can raise them.
func unprivileged(req helperRequest) error {
	if (os.Getuid() == 0 || os.Geteuid() == 0) && !req.AllowRoot {
		return errors.New("refusing to run an agent's program as root (docs/agent-safety.md)")
	}
	if err := noNewPrivs(); err != nil {
		return fmt.Errorf("setting no_new_privs: %w", err)
	}
	return nil
}
