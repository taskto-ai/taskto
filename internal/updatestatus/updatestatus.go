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
	"regexp"
	"time"
)

// FileName is the status file in the directory the updater writes to and the server reads.
const FileName = "status.json"

// StaleAfter is how long after it was written a status is called stale. The updater runs every
// night and writes the file on every run (a run that does nothing too), so a status older than
// this means the updater isn't running.
const StaleAfter = 72 * time.Hour

// maxSize bounds the file a reader accepts.
const maxSize = 1 << 20

// The updater publishes structured status only: results and reasons from the closed lists
// below, versions, and yes or no. What a person needs to know beyond that (a backup's path, a
// registry's host, a volume's name, where the updater keeps its record) stays on the host, in
// its journal, and the web app says where to look. The reader enforces it again, whatever the
// file holds: a word that is not on a list becomes Unknown or CodeOther, a version that isn't
// one is dropped.

// Unknown is the result of a run whose result isn't one the reader knows.
const Unknown = "unknown"

var results = setOf("updated", "up-to-date", "skipped", "failed", "rolled-back", "rollback-failed",
	"stopped", "stop-failed", "blocked", "interrupted", "cleared", "refused")

// CodeOther is the reason code of a reason the updater couldn't put in any other word.
const CodeOther = "other"

// codes are the reasons the updater may give (deploy/taskto-update.sh's reason_code and
// blocked_code), each translated by the web app (settings.updates.reasons.<code>).
var codes = setOf(CodeOther, "off", "hold", "manual-upgrade", "github", "not-published", "not-newer",
	"version-mismatch", "server-down", "unsafe-settings", "disk", "registry", "backup", "bad-setting",
	"firewall", "settings", "blocked", "stopped", "stop-failed", "rollback-failed", "interrupted",
	"rolled-back", "unrecorded-failure")

var channels = setOf("release", "main", "off")

func setOf(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

// versionRE is a release (vMAJOR.MINOR.PATCH, no leading zeros) or a full commit SHA, the only
// versions there are.
var versionRE = regexp.MustCompile(`^(v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)|[0-9a-f]{40})$`)

// IsVersion reports whether v is a release version or a full commit SHA.
func IsVersion(v string) bool { return len(v) <= 100 && versionRE.MatchString(v) }

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
	// Code says why, as one word of a closed list (empty after a success), and BackedUp whether
	// the run made a backup before it changed anything. Where it is stays on the host.
	Code     string `json:"code,omitempty"`
	BackedUp bool   `json:"backed_up,omitempty"`
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

// NeedsPerson is the reason the install waits for a person, as a kind and a code.
type NeedsPerson struct {
	Kind string `json:"kind"`
	Code string `json:"code,omitempty"`
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
	s.sanitize()
	return s, true, nil
}

// sanitize keeps only what the closed lists and the version's grammar allow, whatever the file
// holds.
func (s *Status) sanitize() {
	if !channels[s.Channel] {
		s.Channel = ""
	}
	s.RecordedVersion = version(s.RecordedVersion)
	for _, r := range []*Run{s.LastRun, s.LastChange} {
		if r == nil {
			continue
		}
		if !results[r.Result] {
			r.Result = Unknown
		}
		r.From, r.To = version(r.From), version(r.To)
		if r.Code != "" && !codes[r.Code] {
			r.Code = CodeOther
		}
	}
	if n := s.NeedsPerson; n != nil {
		if n.Kind != KindBlocked && n.Kind != KindManualUpgrade {
			s.NeedsPerson = nil
		} else if n.Code != "" && !codes[n.Code] {
			n.Code = CodeOther
		}
	}
}

func version(v string) string {
	if IsVersion(v) {
		return v
	}
	return ""
}
