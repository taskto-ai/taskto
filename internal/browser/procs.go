package browser

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Paths is one user's browser directory and what is in it. Only that user's processes (in
// container mode, their Unix user) create or open anything here.
type Paths struct{ Root string }

// Home is $HOME for the browser's processes: Chromium keeps its NSS database and caches there.
func (p Paths) Home() string { return filepath.Join(p.Root, "home") }

// Tmp is $TMPDIR.
func (p Paths) Tmp() string { return filepath.Join(p.Root, "tmp") }

// Profile is the personal browser's profile, kept between runs.
func (p Paths) Profile() string { return filepath.Join(p.Root, "profile") }

// Testing is the testing browser's profile, made empty each time it starts.
func (p Paths) Testing() string { return filepath.Join(p.Root, "testing") }

// Run holds the display's cookie and the VNC socket.
func (p Paths) Run() string { return filepath.Join(p.Root, "run") }

// Xauthority is the display's cookie file: the X server refuses a client without it.
func (p Paths) Xauthority() string { return filepath.Join(p.Run(), "xauthority") }

// VNCSocket is where x11vnc serves the personal browser's display.
func (p Paths) VNCSocket() string { return filepath.Join(p.Run(), "vnc.sock") }

// Program is one of the processes of a browser.
type Program string

// The programs.
const (
	Xvfb     Program = "xvfb"
	Chromium Program = "chromium"
	X11vnc   Program = "x11vnc"
)

// Valid reports whether p is one of the programs.
func (p Program) Valid() bool { return p == Xvfb || p == Chromium || p == X11vnc }

// Screen is the virtual display's size, and the personal browser's window.
const (
	ScreenWidth  = 1280
	ScreenHeight = 800
)

// ProcSpec says which process of a browser to start.
type ProcSpec struct {
	Program Program `json:"program"`
	Kind    Kind    `json:"kind"`
	// Display is the X display, for Chromium (personal) and x11vnc.
	Display int `json:"display,omitempty"`
	// NoSandbox turns off Chromium's sandbox, which can't start in an unprivileged container
	// (docs/agent-browser.md).
	NoSandbox bool `json:"no_sandbox,omitempty"`
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

// Prepare makes the directories and files spec's process needs in p, mode 700 and 600: for
// Xvfb a new cookie; for the personal Chromium, the profile with any stale lock removed (a
// recreated container is another host to Chromium, which then refuses the profile: there is
// only ever one personal browser per user, so a lock left is always stale); for the testing
// Chromium, an empty profile. It runs as the user (the helper, in container mode).
func Prepare(p Paths, spec ProcSpec) error {
	if err := spec.check(); err != nil {
		return err
	}
	for _, dir := range []string{p.Root, p.Home(), p.Tmp(), p.Run()} {
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

// Clean removes the testing browser's profile, when it stops.
func Clean(p Paths) error { return os.RemoveAll(p.Testing()) }

// ownerOnlyDir makes dir (mode 700), or tightens it if it is there.
func ownerOnlyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.Chmod(dir, 0o700) //nolint:gosec // G302: a directory needs x; 0700 is owner-only
}

// writeXauthority writes a new MIT-MAGIC-COOKIE-1 for any display on any host (FamilyWild,
// no display number), mode 600: what Xvfb's -auth reads, and its clients find through
// XAUTHORITY.
func writeXauthority(path string) error {
	cookie := make([]byte, 16)
	if _, err := rand.Read(cookie); err != nil {
		return err
	}
	field := func(b []byte) []byte { return append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...) } //nolint:gosec // short constant fields
	// FamilyWild, then no address and no display number: any host, any display.
	entry := binary.BigEndian.AppendUint16(nil, 0xffff)
	entry = append(entry, field(nil)...)
	entry = append(entry, field(nil)...)
	entry = append(entry, field([]byte("MIT-MAGIC-COOKIE-1"))...)
	entry = append(entry, field(cookie)...)
	tmp := path + ".tmp"
	_ = os.Remove(tmp)
	if err := os.WriteFile(tmp, entry, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Command returns the command line, environment and working directory of spec's process in
// p, with bin the program's executable and path $PATH. Chromium reads DevTools commands on
// fd 3 and writes on fd 4; Xvfb writes its display number on fd 3. The environment is built
// from nothing.
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
		// -displayfd picks a free display and says which; -nolisten local drops the abstract
		// socket, which has no permissions, and -auth refuses a client without the cookie.
		argv = []string{bin, "-displayfd", "3", "-auth", p.Xauthority(), "-nolisten", "tcp", "-nolisten", "local",
			"-screen", "0", fmt.Sprintf("%dx%dx24", ScreenWidth, ScreenHeight)}
	case X11vnc:
		// Only the Unix socket: -rfbport 0 alone leaves TCP 5900 open over IPv6 (x11vnc
		// 0.9.17), where any user could connect. No password: the socket's directory is
		// owner-only. -norc, -nocmds and -noremote: nothing but serving the display.
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

// readDisplay reads the display number Xvfb writes on -displayfd.
func readDisplay(line string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || n < 0 || n > 65535 {
		return 0, fmt.Errorf("the virtual display reported %q", strings.TrimSpace(line))
	}
	return n, nil
}
