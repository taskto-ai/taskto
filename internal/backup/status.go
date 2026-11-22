package backup

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

// StatusDir is the directory, in the data directory, where the backup schedule keeps its
// state: StatusName, the last backup's outcome, which is also the marker of the last
// success that retries go by, and lockName, held while a backup runs. A backup leaves it
// out: it describes this server's backups, not the data.
const (
	StatusDir  = "backup-schedule"
	StatusName = "status.json"
	lockName   = "lock"
)

// StatusPath is the status file of the data directory dataDir.
func StatusPath(dataDir string) string {
	return filepath.Join(dataDir, StatusDir, StatusName)
}

// Status is what the schedule knows of its backups, as it writes it to StatusPath for the
// server (Settings → About, /healthz/backup) and for outside monitoring. Times are UTC;
// a zero time is left out.
type Status struct {
	// At is the nightly time, "15:04" in UTC, and Retries and RetryEvery how a failed
	// night is retried.
	At                string `json:"at"`
	Retries           int    `json:"retries"`
	RetryEverySeconds int64  `json:"retry_every_seconds"`

	// LastSuccess is when the last backup that saved a snapshot ended, and LastSnapshot
	// that snapshot's ID.
	LastSuccess  time.Time `json:"last_success,omitzero"`
	LastSnapshot string    `json:"last_snapshot,omitempty"`
	// LastAttempt is when the last backup started, whatever came of it.
	LastAttempt time.Time `json:"last_attempt,omitzero"`
	// LastError is the last failed backup's error, and LastErrorAt when it failed. A later
	// success doesn't clear them: compare the times.
	LastError   string    `json:"last_error,omitempty"`
	LastErrorAt time.Time `json:"last_error_at,omitzero"`
	// Failures is how many backups failed for Night, the scheduled time they were for.
	Night    time.Time `json:"night,omitzero"`
	Failures int       `json:"failures,omitempty"`

	// NextAttempt is when the schedule starts its next backup.
	NextAttempt time.Time `json:"next_attempt,omitzero"`
	// DueBy is when the next success is due: the next night, once its retries have had
	// their time. A backup missing by then is a missed night (Health).
	DueBy time.Time `json:"due_by,omitzero"`
}

// Health sums up a Status for monitoring.
type Health string

const (
	// HealthNone: there is no status, so no schedule has run on this data directory.
	HealthNone Health = "none"
	// HealthPending: the schedule runs, and hasn't finished a backup yet.
	HealthPending Health = "pending"
	// HealthOK: the last backup succeeded, and the next isn't late.
	HealthOK Health = "ok"
	// HealthFailing: the last backup failed, and the night's retries still have time.
	HealthFailing Health = "failing"
	// HealthLate: no backup succeeded by DueBy: a night was missed.
	HealthLate Health = "late"
)

// Health is s's health at now.
func (s Status) Health(now time.Time) Health {
	switch {
	case !s.DueBy.IsZero() && now.After(s.DueBy):
		return HealthLate
	case !s.LastErrorAt.IsZero() && s.LastErrorAt.After(s.LastSuccess):
		return HealthFailing
	case s.LastSuccess.IsZero():
		return HealthPending
	}
	return HealthOK
}

// maxStatusSize bounds the status file a reader accepts.
const maxStatusSize = 1 << 20

// ReadStatus reads the status of the data directory dataDir. ok is false when there is
// none.
func ReadStatus(dataDir string) (s Status, ok bool, err error) {
	f, err := os.Open(StatusPath(dataDir)) // #nosec G304 -- a fixed name in the data directory.
	if errors.Is(err, fs.ErrNotExist) {
		return Status{}, false, nil
	}
	if err != nil {
		return Status{}, false, err
	}
	defer f.Close()
	if err := json.NewDecoder(io.LimitReader(f, maxStatusSize)).Decode(&s); err != nil {
		return Status{}, false, fmt.Errorf("the backup status %s: %w", StatusPath(dataDir), err)
	}
	return s, true, nil
}

// writeStatus replaces the status of dataDir in one step: a reader sees the old file or
// the new one, never part of one.
func writeStatus(dataDir string, s Status) error {
	dir := filepath.Join(dataDir, StatusDir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".status-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // gone once renamed
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), StatusPath(dataDir))
}
