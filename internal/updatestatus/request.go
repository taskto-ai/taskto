package updatestatus

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// An operator's "Update now" is a file in a directory of its own, which the host's updater
// (deploy/taskto-update.sh, `taskto-update --requested`, started by a systemd path unit) takes and
// removes. The server never runs the updater and never gives it anything to run: the request
// carries no version, no repository and no flag. The host installs exactly what the nightly run
// would take, on the configured channel, and keeps every gate of that run (the hold, a release
// that needs a person, an update that was stopped).
//
// The directory is the one place the server may write of the updater's (compose.yaml mounts it
// read-write, deploy/safety-check.sh allows that and nothing else), so the host treats whatever it
// holds as hostile: only the file's existence is the signal, its content is never read into an
// argument, and a link, a directory or a large file there is removed and ignored.

// RequestFile is the request in the request directory. The host's path unit watches it.
const RequestFile = "update-now"

// ExpireAfter is how long a request waits before the server stops counting it as pending: a
// request the host never took (its path unit is off) mustn't block the button for good. The
// host's own wait for a run in progress is shorter.
const ExpireAfter = 3 * time.Hour

// Errors of Request.
var (
	// ErrNoDir: the server was not told where requests go, or the directory isn't there or isn't
	// writable for it.
	ErrNoDir = errors.New("updatestatus: no request directory")
	// ErrPending: a request is waiting for the host to take it.
	ErrPending = errors.New("updatestatus: an update is already requested")
)

// Pending reports the time of the request that waits in dir, if one does. A link or a directory in
// its place counts too: the host removes whatever is there. A request older than ExpireAfter is not
// pending.
func Pending(dir string, now time.Time) (since time.Time, pending bool, err error) {
	if dir == "" {
		return time.Time{}, false, nil
	}
	info, err := os.Lstat(filepath.Join(dir, RequestFile))
	if errors.Is(err, fs.ErrNotExist) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	if now.Sub(info.ModTime()) > ExpireAfter {
		return time.Time{}, false, nil
	}
	return info.ModTime().UTC(), true, nil
}

// Prepared is a request that is written but not yet there for the host to see: its content sits
// in a temporary file the path unit doesn't watch. The caller records the request (the audit
// event, in its transaction), and only when that is committed does Publish make it visible, so the
// host can't act on a request that has no record. Discard drops it.
type Prepared struct {
	dir, tmp string
	now      time.Time
}

// Prepare checks that no request waits and writes the next one's content to a temporary file in
// dir. The content is a fixed line, the time and a random nonce, which the host checks against a
// strict pattern and uses for nothing but telling one request from another. It holds no secret.
func Prepare(dir string, now time.Time) (*Prepared, error) {
	if dir == "" {
		return nil, ErrNoDir
	}
	final := filepath.Join(dir, RequestFile)
	if _, pending, err := Pending(dir, now); err != nil {
		return nil, err
	} else if pending {
		return nil, ErrPending
	}
	// A request nobody took in ExpireAfter, or a leftover of a crash, goes.
	if info, err := os.Lstat(final); err == nil && now.Sub(info.ModTime()) > ExpireAfter {
		_ = os.Remove(final)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, ".request-*")
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
		// The mount isn't there, or isn't the server's to write: an install from before the button.
		return nil, ErrNoDir
	}
	if err != nil {
		return nil, fmt.Errorf("updatestatus: %w", err)
	}
	_, err = fmt.Fprintf(tmp, "%s %s\n", now.UTC().Format(time.RFC3339), hex.EncodeToString(nonce[:]))
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return nil, fmt.Errorf("updatestatus: %w", err)
	}
	return &Prepared{dir: dir, tmp: tmp.Name(), now: now}, nil
}

// Publish makes the request visible to the host, atomically and without replacing one that waits:
// a hard link fails when the name is taken, where a rename would replace it, so two presses at once
// make one request. The temporary file goes either way.
func (p *Prepared) Publish() error {
	defer os.Remove(p.tmp)
	if err := os.Link(p.tmp, filepath.Join(p.dir, RequestFile)); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return ErrPending
		}
		return fmt.Errorf("updatestatus: %w", err)
	}
	sweep(p.dir, p.now)
	return nil
}

// Discard drops a request that wasn't published.
func (p *Prepared) Discard() { _ = os.Remove(p.tmp) }

// Request prepares and publishes a request at once, for a caller with nothing to record first.
func Request(dir string, now time.Time) error {
	p, err := Prepare(dir, now)
	if err != nil {
		return err
	}
	return p.Publish()
}

// sweep removes the temporary files of requests that were interrupted.
func sweep(dir string, now time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".request-") {
			continue
		}
		if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}
