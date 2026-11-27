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

const FileName = "status.json"

const StaleAfter = 72 * time.Hour

const maxSize = 1 << 20

const Unknown = "unknown"

var results = setOf("updated", "up-to-date", "skipped", "failed", "rolled-back", "rollback-failed",
	"stopped", "stop-failed", "blocked", "interrupted", "cleared", "refused", "ignored")

const CodeOther = "other"

var codes = setOf(CodeOther, "off", "hold", "manual-upgrade", "github", "not-published", "not-newer",
	"version-mismatch", "server-down", "unsafe-settings", "disk", "registry", "backup", "bad-setting",
	"firewall", "settings", "blocked", "stopped", "stop-failed", "rollback-failed", "interrupted",
	"rolled-back", "unrecorded-failure", "rate-limited", "bad-request")

var channels = setOf("release", "main", "off")

func setOf(v ...string) map[string]bool {
	m := make(map[string]bool, len(v))
	for _, s := range v {
		m[s] = true
	}
	return m
}

var versionRE = regexp.MustCompile(`^(v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)|[0-9a-f]{40})$`)

func IsVersion(v string) bool { return len(v) <= 100 && versionRE.MatchString(v) }

var (
	shaRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	repoRE = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)
)

func IsSHA(v string) bool { return shaRE.MatchString(v) }

func isRepo(r string) bool { return len(r) <= 200 && repoRE.MatchString(r) }

type Available struct {
	Version       string    `json:"version"`
	SHA           string    `json:"sha,omitempty"`
	Published     time.Time `json:"published,omitzero"`
	ManualUpgrade bool      `json:"manual_upgrade"`
	Repo          string    `json:"repo,omitempty"`
}

type Run struct {
	At        time.Time `json:"at,omitzero"`
	Finished  time.Time `json:"finished,omitzero"`
	Result    string    `json:"result"`
	From      string    `json:"from,omitempty"`
	To        string    `json:"to,omitempty"`
	Code      string    `json:"code,omitempty"`
	BackedUp  bool      `json:"backed_up,omitempty"`
	Requested bool      `json:"requested,omitempty"`
}

type Running struct {
	Since     time.Time `json:"since"`
	To        string    `json:"to,omitempty"`
	Requested bool      `json:"requested,omitempty"`
}

const RunningMaxAge = 3 * time.Hour

type RequestRun struct {
	At       time.Time `json:"at,omitzero"`
	Finished time.Time `json:"finished,omitzero"`
	Result   string    `json:"result"`
	Code     string    `json:"code,omitempty"`
}

type Requests struct {
	Supported bool        `json:"supported"`
	Last      *RequestRun `json:"last"`
}

const (
	KindBlocked       = "blocked"
	KindManualUpgrade = "manual-upgrade"
)

type NeedsPerson struct {
	Kind string `json:"kind"`
	Code string `json:"code,omitempty"`
}

type Status struct {
	Version         int          `json:"version"`
	Checked         time.Time    `json:"checked,omitzero"`
	Channel         string       `json:"channel"`
	Hold            bool         `json:"hold,omitempty"`
	RecordedVersion string       `json:"recorded_version,omitempty"`
	LastRun         *Run         `json:"last_run"`
	LastChange      *Run         `json:"last_change"`
	NeedsPerson     *NeedsPerson `json:"needs_person"`
	Running         *Running     `json:"running"`
	Requests        *Requests    `json:"requests"`
	Looked          time.Time    `json:"looked,omitzero"`
	Available       *Available   `json:"available"`
}

func (s Status) Stale(now time.Time) bool {
	return s.Checked.IsZero() || now.Sub(s.Checked) > StaleAfter
}

func Path(dir string) string { return filepath.Join(dir, FileName) }

func Read(dir string) (s Status, ok bool, err error) {
	if dir == "" {
		return Status{}, false, nil
	}
	f, err := os.Open(Path(dir))
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
	if r := s.Running; r != nil {
		r.To = version(r.To)
		if r.Since.IsZero() {
			s.Running = nil
		}
	}
	if q := s.Requests; q != nil && q.Last != nil {
		if !results[q.Last.Result] {
			q.Last.Result = Unknown
		}
		if q.Last.Code != "" && !codes[q.Last.Code] {
			q.Last.Code = CodeOther
		}
	}
	if a := s.Available; a != nil {
		a.Version = version(a.Version)
		if !IsSHA(a.SHA) {
			a.SHA = ""
		}
		if IsSHA(a.Version) {
			a.SHA = a.Version
		}
		if !isRepo(a.Repo) {
			a.Repo = ""
		}
		if a.Version == "" || a.Version == s.RecordedVersion {
			s.Available = nil
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
