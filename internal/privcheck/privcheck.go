// Package privcheck checks that agents can't run with more privilege than a plain user
// (docs/agent-safety.md): not as root, without a passwordless sudo or doas, and with no
// setuid or setgid program where a CLI worker's commands run.
//
// The server checks itself before it starts the agent sidecar or local CLI workers, which
// run as its own user; the container worker checks its image before it starts any CLI. A
// problem stops them, unless the development override (-unsafe-privileged-agents) is set,
// which turns it into a warning.
package privcheck

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// OverrideFlag is the development override's flag, also TASKTO_UNSAFE_PRIVILEGED_AGENTS.
const OverrideFlag = "unsafe-privileged-agents"

// Process describes the process to check. The zero value is this process, with its PATH.
type Process struct {
	// UID and EUID return the real and effective user IDs (default os.Getuid, os.Geteuid).
	UID, EUID func() int
	// Path is where sudo and doas are looked for (default $PATH).
	Path string
	// Timeout bounds each `sudo -n true` (default 5 s).
	Timeout time.Duration
}

// escalators are the programs that run a command as root, each checked with -n (never ask
// for a password): what an agent could do on its own.
var escalators = []string{"sudo", "doas"}

// Problems returns what lets this process act as root: it is root, or sudo or doas runs a
// command as root without asking for a password. None means it can't.
func (p Process) Problems(ctx context.Context) []string {
	uid, euid := os.Getuid, os.Geteuid
	if p.UID != nil {
		uid = p.UID
	}
	if p.EUID != nil {
		euid = p.EUID
	}
	path := p.Path
	if path == "" {
		path = os.Getenv("PATH")
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if uid() == 0 || euid() == 0 {
		// Root needs no sudo: asking would only slow the start down.
		return []string{"it runs as root (run it as an unprivileged account)"}
	}
	var problems []string
	for _, name := range escalators {
		for _, bin := range candidates(name, path) {
			if run(ctx, timeout, bin, "-n", "true") {
				problems = append(problems, fmt.Sprintf("`%s -n true` runs as root without a password (%s); if that is a cached password, run `%s -k`", name, bin, name))
				break
			}
			// A NOPASSWD rule for some commands only (apt, say) fails the probe above but still
			// escalates. sudo lists the rules without a password when at least one is NOPASSWD
			// (its default listpw=any).
			if name == "sudo" && run(ctx, timeout, bin, "-n", "-l") {
				problems = append(problems, fmt.Sprintf("`sudo -n -l` lists rules without a password (%s): some command runs as root without one; remove the NOPASSWD rules for this user", bin))
				break
			}
		}
	}
	return problems
}

// run reports whether bin with args succeeds, with an empty environment and no terminal:
// exactly what an agent's command would have.
func run(ctx context.Context, timeout time.Duration, bin string, args ...string) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: sudo or doas, fixed arguments
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd.Run() == nil
}

// standardDirs are where sudo and doas are installed: an agent's command can run one there by
// its full path, whatever PATH says.
var standardDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin", "/usr/local/sbin", "/opt/homebrew/bin"}

// candidates returns every executable called name in path and the standard directories, each
// once (by its resolved path).
func candidates(name, path string) []string {
	var out []string
	seen := map[string]bool{}
	for _, dir := range append(filepath.SplitList(path), standardDirs...) {
		bin := lookPath(name, dir)
		if bin == "" {
			continue
		}
		real, err := filepath.EvalSymlinks(bin)
		if err != nil {
			real = bin
		}
		if !seen[real] {
			seen[real] = true
			out = append(out, bin)
		}
	}
	return out
}

// lookPath finds an executable name in path, or returns "".
func lookPath(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		bin := filepath.Join(dir, name)
		if info, err := os.Stat(bin); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 { //nolint:gosec // G703: a PATH directory and a fixed name
			return bin
		}
	}
	return ""
}

// SetuidFiles returns the setuid and setgid files under root on root's own file system (like
// find -xdev), leaving out the directories in skip, and the directories it couldn't read,
// which could hide one. A CLI user could raise their privileges with one; the worker's image
// has none. At most limit files are returned (0: all), and more reports whether there were.
func SetuidFiles(root string, limit int, skip ...string) (found, unreadable []string, more bool, err error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, nil, false, err
	}
	dev, ok := deviceOf(info)
	if !ok {
		return nil, nil, false, errors.New("privcheck: no device numbers on this system")
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory that can't be read could hide one; one that vanished meanwhile can't.
			if !errors.Is(err, fs.ErrNotExist) {
				unreadable = append(unreadable, path)
			}
			if d != nil && d.IsDir() && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			for _, s := range skip {
				if path == s {
					return fs.SkipDir
				}
			}
			if path != root {
				if info, err := d.Info(); err == nil {
					if dv, ok := deviceOf(info); ok && dv != dev {
						return fs.SkipDir // another file system: /proc, a volume
					}
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		// A file that vanished in the meantime isn't one.
		if info, ierr := d.Info(); ierr == nil && info.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
			if limit > 0 && len(found) >= limit {
				more = true
				return fs.SkipAll
			}
			found = append(found, path)
		}
		return nil
	})
	return found, unreadable, more, err
}

// Enforce turns problems with what into an error, or, with the override, into a warning
// logged once and no error.
func Enforce(what string, problems []string, override bool) error {
	if len(problems) == 0 {
		return nil
	}
	msg := fmt.Sprintf("%s must not be able to act as root: %s", what, strings.Join(problems, "; "))
	if override {
		slog.Warn(msg + " (allowed by -" + OverrideFlag + ", for development only)")
		return nil
	}
	return fmt.Errorf("%s. See docs/agent-safety.md, or set -%s for development only", msg, OverrideFlag)
}
