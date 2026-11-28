package browser

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type Paths struct {
	Root  string
	Short string
}

func HomePaths(home string) Paths {
	return Paths{Root: filepath.Join(home, "browser"), Short: filepath.Join(home, "r")}
}

func (p Paths) Home() string { return filepath.Join(p.Root, "home") }

func (p Paths) Tmp() string {
	if p.Short != "" {
		return p.Short
	}
	return filepath.Join(p.Root, "tmp")
}

func (p Paths) Profile() string { return filepath.Join(p.Root, "profile") }

func (p Paths) Testing() string { return filepath.Join(p.Root, "testing") }

func (p Paths) Run() string {
	if p.Short != "" {
		return p.Short
	}
	return filepath.Join(p.Root, "run")
}

func (p Paths) Xauthority() string { return filepath.Join(p.Run(), "xauthority") }

func (p Paths) VNCSocket() string { return filepath.Join(p.Run(), "vnc.sock") }

type Program string

const (
	Xvfb     Program = "xvfb"
	Chromium Program = "chromium"
	X11vnc   Program = "x11vnc"
)

func (p Program) Valid() bool { return p == Xvfb || p == Chromium || p == X11vnc }

const (
	ScreenWidth  = 1280
	ScreenHeight = 800
)

type ProcSpec struct {
	Program   Program `json:"program"`
	Kind      Kind    `json:"kind"`
	Display   int     `json:"display,omitempty"`
	NoSandbox bool    `json:"no_sandbox,omitempty"`
}

func (s ProcSpec) check() error {
	switch {
	case !s.Program.Valid():
		return fmt.Errorf("unknown program %q", s.Program)
	case !s.Kind.Valid():
		return fmt.Errorf("unknown browser %q", s.Kind)
	case s.Kind == Testing && s.Program != Chromium:
		return errors.New("the testing browser is Chromium alone")
	case s.Display < 0 || s.Display > 65535:
		return errors.New("invalid display")
	}
	return nil
}

func Prepare(p Paths, spec ProcSpec) error {
	if err := spec.check(); err != nil {
		return err
	}
	for _, dir := range []string{p.Root, p.Home(), p.Run(), p.Tmp()} {
		if err := ownerOnlyDir(dir); err != nil {
			return err
		}
	}
	switch {
	case spec.Program == Xvfb:
		return writeXauthority(p.Xauthority())
	case spec.Program == Chromium && spec.Kind == Personal:
		if err := ownerOnlyDir(p.Profile()); err != nil {
			return err
		}
		for _, name := range []string{"SingletonLock", "SingletonSocket", "SingletonCookie"} {
			if err := os.Remove(filepath.Join(p.Profile(), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	case spec.Program == Chromium && spec.Kind == Testing:
		if err := os.RemoveAll(p.Testing()); err != nil {
			return err
		}
		return ownerOnlyDir(p.Testing())
	}
	return nil
}

func Clean(p Paths) error { return removeAll(p.Testing()) }

func Purge(p Paths) error {
	if p.Short != "" {
		if err := removeAll(p.Short); err != nil {
			return err
		}
	}
	return removeAll(p.Root)
}

func removeAll(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	if info.Mode().Perm()&0o700 != 0o700 {
		if err := os.Chmod(path, 0o700); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeAll(filepath.Join(path, e.Name())); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

func ownerOnlyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700)
}

func writeXauthority(path string) error {
	cookie := make([]byte, 16)
	if _, err := rand.Read(cookie); err != nil {
		return err
	}
	entry := binary.BigEndian.AppendUint16(nil, 0xffff)
	for _, field := range [][]byte{nil, nil, []byte("MIT-MAGIC-COOKIE-1"), cookie} {
		size := len(field)
		if size > math.MaxUint16 {
			return errors.New("xauthority: a field is too long")
		}
		entry = binary.BigEndian.AppendUint16(entry, uint16(size))
		entry = append(entry, field...)
	}
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, entry, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func Command(p Paths, spec ProcSpec, bin, path string) (argv, env []string, err error) {
	if err := spec.check(); err != nil {
		return nil, nil, err
	}
	env = []string{
		"PATH=" + path,
		"HOME=" + p.Home(),
		"TMPDIR=" + p.Tmp(),
		"LANG=C.UTF-8",
	}
	display := ":" + strconv.Itoa(spec.Display)
	switch spec.Program {
	case Xvfb:
		argv = []string{bin, "-displayfd", "3", "-auth", p.Xauthority(), "-nolisten", "tcp", "-nolisten", "local",
			"-screen", "0", fmt.Sprintf("%dx%dx24", ScreenWidth, ScreenHeight)}
	case X11vnc:
		argv = []string{bin, "-display", display, "-auth", p.Xauthority(), "-unixsock", p.VNCSocket(),
			"-rfbport", "0", "-rfbportv6", "-1", "-noipv6", "-forever", "-shared", "-nopw",
			"-norc", "-nocmds", "-noremote", "-xkb", "-quiet"}
		env = append(env, "DISPLAY="+display, "XAUTHORITY="+p.Xauthority())
	case Chromium:
		profile := p.Profile()
		if spec.Kind == Testing {
			profile = p.Testing()
		}
		argv = []string{bin,
			"--user-data-dir=" + profile,
			"--remote-debugging-pipe",
			"--no-first-run",
			"--no-default-browser-check",
			"--password-store=basic",
			"--disable-dev-shm-usage",
		}
		if spec.NoSandbox {
			argv = append(argv, "--no-sandbox")
		}
		if spec.Kind == Testing {
			argv = append(argv, "--headless", fmt.Sprintf("--window-size=%d,%d", ScreenWidth, ScreenHeight))
		} else {
			argv = append(argv, "--window-position=0,0", fmt.Sprintf("--window-size=%d,%d", ScreenWidth, ScreenHeight))
			env = append(env, "DISPLAY="+display, "XAUTHORITY="+p.Xauthority())
		}
		argv = append(argv, "about:blank")
	}
	return argv, env, nil
}

func readDisplay(line string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 0 || n > 65535 {
		return 0, fmt.Errorf("the virtual display reported %q", strings.TrimSpace(line))
	}
	return n, nil
}
