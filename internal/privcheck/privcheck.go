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

const OverrideFlag = "unsafe-privileged-agents"

type Process struct {
	UID, EUID func() int
	Path      string
	Timeout   time.Duration
}

var escalators = []string{"sudo", "doas"}

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
		return []string{"it runs as root (run it as an unprivileged account)"}
	}
	var problems []string
	for _, name := range escalators {
		for _, bin := range candidates(name, path) {
			if run(ctx, timeout, bin, "-n", "true") {
				problems = append(problems, fmt.Sprintf("`%s -n true` runs as root without a password (%s); if that is a cached password, run `%s -k`", name, bin, name))
				break
			}
			if name == "sudo" && run(ctx, timeout, bin, "-n", "-l") {
				problems = append(problems, fmt.Sprintf("`sudo -n -l` lists rules without a password (%s): some command runs as root without one; remove the NOPASSWD rules for this user", bin))
				break
			}
		}
	}
	return problems
}

func run(ctx context.Context, timeout time.Duration, bin string, args ...string) bool {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	return cmd.Run() == nil
}

var standardDirs = []string{"/usr/bin", "/bin", "/usr/sbin", "/sbin", "/usr/local/bin", "/usr/local/sbin", "/opt/homebrew/bin"}

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

func lookPath(name, path string) string {
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		bin := filepath.Join(dir, name)
		if info, err := os.Stat(bin); err == nil && info.Mode().IsRegular() && info.Mode()&0o111 != 0 {
			return bin
		}
	}
	return ""
}

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
						return fs.SkipDir
					}
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
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
