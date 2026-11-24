// Package updatestatus reads what the host's updater (deploy/taskto-update.sh) says about the
// automatic updates, for the admins who see it in Settings → About.
//
// The updater runs on the host, outside the server, and the server never gains power over it:
// no Docker socket, nothing it runs. The updater writes a copy of its status, made for this, into
// a directory of its own, which compose.yaml mounts into the server read-only; this package only
// reads that one file, as internal/backup reads the backup schedule's.
package updatestatus

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// FileName is the status file in the directory the updater writes to and the server reads.
const FileName = "status.json"

// StaleAfter is how long after it was written a status is called stale. The updater runs every
// night and writes the file on every run (a run that does nothing too), so a status older than
// this means the updater isn't running.
const StaleAfter = 72 * time.Hour

// maxSize bounds the file a reader accepts; maxText bounds each text in it that people read.
const (
	maxSize = 1 << 20
	maxText = 4000
)

// Run is one run of the updater, as it recorded it.
type Run struct {
	// At is when the run started and Finished when it ended.
	At       time.Time `json:"at,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	// Result is what came of it: updated, up-to-date, skipped, failed, rolled-back,
	// rollback-failed, stopped, stop-failed, blocked, interrupted or cleared.
	Result string `json:"result"`
	// From and To are the versions it started on and chose; To is empty when it chose none.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// Reason says why, in the updater's words (it is empty after a success), and Backup is
	// the file it backed up to before an update, on the host.
	Reason string `json:"reason,omitempty"`
	Backup string `json:"backup,omitempty"`
}

// NeedsPerson kinds.
const (
	// KindBlocked: an update stopped, was interrupted, or the server isn't the version on
	// record; nothing runs until a person looks and runs `taskto-update --clear-block`.
	KindBlocked = "blocked"
	// KindManualUpgrade: the newest release (or the worker's firewall it changes) needs
	// steps by hand; follow its notes, then `taskto-update --to VERSION`.
	KindManualUpgrade = "manual-upgrade"
)

// NeedsPerson is the reason the install waits for a person.
type NeedsPerson struct {
	Kind   string `json:"kind"`
	Reason string `json:"reason,omitempty"`
}

// Status is the updater's status as the server reads it. Times are UTC.
type Status struct {
	// Version is the file's format, 1.
	Version int `json:"version"`
	// Checked is when the updater last wrote the file, on any run.
	Checked time.Time `json:"checked,omitzero"`
	// Channel is the updater's channel (release, main or off) and Hold whether
	// /etc/taskto/update.hold stops its automatic runs.
	Channel string `json:"channel"`
	Hold    bool   `json:"hold,omitempty"`
	// RecordedVersion is the version the install's .env says: the version on record.
	RecordedVersion string `json:"recorded_version,omitempty"`
	// LastRun is the last run that wrote a status, and LastChange the last that changed the
	// install (an update, or a failure of one), which a later quiet run doesn't replace.
	LastRun    *Run `json:"last_run"`
	LastChange *Run `json:"last_change"`
	// NeedsPerson is set while the install waits for a person.
	NeedsPerson *NeedsPerson `json:"needs_person"`
}

// Stale reports whether s was last written too long before now.
func (s Status) Stale(now time.Time) bool {
	return s.Checked.IsZero() || now.Sub(s.Checked) > StaleAfter
}

// Path is the status file in the directory dir.
func Path(dir string) string { return filepath.Join(dir, FileName) }

// Read reads the status in dir. ok is false when dir is empty (no updater is set up for this
// server) or has no status yet. A file that isn't a status is an error.
func Read(dir string) (s Status, ok bool, err error) {
	if dir == "" {
		return Status{}, false, nil
	}
	f, err := os.Open(Path(dir)) // #nosec G304 -- a fixed name in the directory the operator set.
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return Status{}, false, fmt.Errorf("the update status %s isn't a regular file", Path(dir))
	}
	if err := json.NewDecoder(io.LimitReader(f, maxSize)).Decode(&s); err != nil {
		return Status{}, false, fmt.Errorf("the update status %s: %w", Path(dir), err)
	}
	if s.Version != 1 {
		return Status{}, false, fmt.Errorf("the update status %s has format %d, not 1", Path(dir), s.Version)
	}
	s.clip()
	return s, true, nil
}

// clip bounds the texts a person reads, whatever the file holds.
func (s *Status) clip() {
	for _, r := range []*Run{s.LastRun, s.LastChange} {
		if r != nil {
			r.Reason, r.Backup = clip(r.Reason), clip(r.Backup)
		}
	}
	if s.NeedsPerson != nil {
		s.NeedsPerson.Reason = clip(s.NeedsPerson.Reason)
	}
}

func clip(v string) string {
	if len(v) <= maxText {
		return v
	}
	// Cut on a rune boundary.
	r := []rune(v[:maxText])
	return string(r[:len(r)-1]) + "…"
}
