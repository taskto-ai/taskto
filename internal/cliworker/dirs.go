package cliworker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Layout is where the accounts' CLI directories are: Root/h<home>/ for each account's home number
// (accounts.go), mode 700.
type Layout struct{ Root string }

// Dirs are one user's CLI directories.
type Dirs struct {
	// Root is the user's directory, the parent of all the others.
	Root string
	// Home is $HOME for the CLIs and the commands they run.
	Home string
	// Tmp is $TMPDIR, and holds each turn's own temporary directory.
	Tmp string
	// Work holds a working directory per conversation.
	Work string
}

// Config is cli's config directory (CLAUDE_CONFIG_DIR, CODEX_HOME or GROK_HOME) for the first
// account, which holds its login and sessions.
func (d Dirs) Config(cli CLI) string { return d.ConfigOf(cli, "") }

// ConfigOf is cli's config directory for the account in slot: config/<cli> for the first,
// config/<cli>-<slot> for the others. slot must be Valid.
func (d Dirs) ConfigOf(cli CLI, slot Slot) string {
	name := string(cli)
	if slot != "" {
		name += "-" + string(slot)
	}
	return filepath.Join(d.Root, "config", name)
}

// Conversation is the working directory of a conversation's turns: fixed per conversation,
// since Claude Code finds a session by its working directory.
func (d Dirs) Conversation(id int64) string {
	return filepath.Join(d.Work, "c"+strconv.FormatInt(id, 10))
}

// Home returns the directories of the account with home number home, without creating them.
func (l Layout) Home(home int64) Dirs {
	root := filepath.Join(l.Root, "h"+strconv.FormatInt(home, 10))
	return Dirs{
		Root: root,
		Home: filepath.Join(root, "home"),
		Tmp:  filepath.Join(root, "tmp"),
		Work: filepath.Join(root, "work"),
	}
}

// legacy is the directory a worker from before homes made for user number user: Root/u<user>.
func (l Layout) legacy(user int64) string {
	return filepath.Join(l.Root, "u"+strconv.FormatInt(user, 10))
}

// Ensure creates home's directories (mode 700) and returns them. A directory that exists with
// a looser mode is tightened: these hold the account's logins.
func (l Layout) Ensure(home int64) (Dirs, error) { return l.EnsureSlot(home, "", "") }

// EnsureSlot is Ensure, and also creates cli's config directory for slot (none for an empty cli).
func (l Layout) EnsureSlot(home int64, cli CLI, slot Slot) (Dirs, error) {
	if home < firstHome || home >= maxHome {
		return Dirs{}, fmt.Errorf("invalid home %d", home)
	}
	if !slot.Valid() {
		return Dirs{}, fmt.Errorf("invalid account slot")
	}
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return Dirs{}, err
	}
	d := l.Home(home)
	dirs := []string{d.Root, d.Home, d.Tmp, d.Work, d.Config(Claude), d.Config(Codex), d.Config(Grok)}
	if cli.Valid() && slot != "" {
		dirs = append(dirs, d.ConfigOf(cli, slot))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Dirs{}, err
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: a directory needs x; 0700 is owner-only
			return Dirs{}, err
		}
	}
	return d, nil
}
