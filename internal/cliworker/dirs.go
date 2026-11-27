package cliworker

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Layout struct{ Root string }

type Dirs struct {
	Root string
	Home string
	Tmp  string
	Work string
}

func (d Dirs) Config(cli CLI) string { return d.ConfigOf(cli, "") }

func (d Dirs) ConfigOf(cli CLI, slot Slot) string {
	name := string(cli)
	if slot != "" {
		name += "-" + string(slot)
	}
	return filepath.Join(d.Root, "config", name)
}

func (d Dirs) Conversation(id int64) string {
	return filepath.Join(d.Work, "c"+strconv.FormatInt(id, 10))
}

func (l Layout) User(user int64) Dirs {
	root := filepath.Join(l.Root, "u"+strconv.FormatInt(user, 10))
	return Dirs{
		Root: root,
		Home: filepath.Join(root, "home"),
		Tmp:  filepath.Join(root, "tmp"),
		Work: filepath.Join(root, "work"),
	}
}

func (l Layout) Ensure(user int64) (Dirs, error) { return l.EnsureSlot(user, "", "") }

func (l Layout) EnsureSlot(user int64, cli CLI, slot Slot) (Dirs, error) {
	if user <= 0 {
		return Dirs{}, fmt.Errorf("invalid user %d", user)
	}
	if !slot.Valid() {
		return Dirs{}, fmt.Errorf("invalid account slot")
	}
	if err := os.MkdirAll(l.Root, 0o700); err != nil {
		return Dirs{}, err
	}
	d := l.User(user)
	dirs := []string{d.Root, d.Home, d.Tmp, d.Work, d.Config(Claude), d.Config(Codex), d.Config(Grok)}
	if cli.Valid() && slot != "" {
		dirs = append(dirs, d.ConfigOf(cli, slot))
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Dirs{}, err
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return Dirs{}, err
		}
	}
	return d, nil
}
