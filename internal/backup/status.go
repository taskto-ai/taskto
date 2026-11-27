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

const (
	StatusDir  = "backup-schedule"
	StatusName = "status.json"
	lockName   = "lock"
)

func StatusPath(dataDir string) string {
	return filepath.Join(dataDir, StatusDir, StatusName)
}

type Status struct {
	At                string `json:"at"`
	Retries           int    `json:"retries"`
	RetryEverySeconds int64  `json:"retry_every_seconds"`

	LastSuccess  time.Time `json:"last_success,omitzero"`
	LastSnapshot string    `json:"last_snapshot,omitempty"`
	LastAttempt  time.Time `json:"last_attempt,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
	Night        time.Time `json:"night,omitzero"`
	Failures     int       `json:"failures,omitempty"`

	NextAttempt time.Time `json:"next_attempt,omitzero"`
	DueBy       time.Time `json:"due_by,omitzero"`
}

type Health string

const (
	HealthNone    Health = "none"
	HealthPending Health = "pending"
	HealthOK      Health = "ok"
	HealthFailing Health = "failing"
	HealthLate    Health = "late"
)

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

const maxStatusSize = 1 << 20

func ReadStatus(dataDir string) (s Status, ok bool, err error) {
	f, err := os.Open(StatusPath(dataDir))
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
	defer os.Remove(f.Name())
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), StatusPath(dataDir))
}
