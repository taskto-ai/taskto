package updatestatus

import (
	"os"
	"time"
)

const (
	ReasonNotConfigured = "not_configured"
	ReasonUnsupported   = "unsupported"
	ReasonOff           = "off"
	ReasonPending       = "pending"
	ReasonRunning       = "running"
)

type RequestState struct {
	Available    bool      `json:"available"`
	Reason       string    `json:"reason,omitempty"`
	PendingSince time.Time `json:"pending_since,omitzero"`
}

type Report struct {
	Configured     bool         `json:"configured"`
	RunningVersion string       `json:"running_version"`
	ExpectsStatus  bool         `json:"expects_status,omitempty"`
	Stale          bool         `json:"stale,omitempty"`
	Status         *Status      `json:"status"`
	Request        RequestState `json:"request"`
	NotesURL       string       `json:"notes_url,omitempty"`
}

func NotesURL(a *Available, current string) string {
	if a == nil || a.Repo == "" || !IsVersion(a.Version) || !isRepo(a.Repo) {
		return ""
	}
	base := "https://github.com/" + a.Repo
	if !IsSHA(a.Version) {
		return base + "/releases/tag/" + a.Version
	}
	if IsVersion(current) && current != a.Version {
		return base + "/compare/" + current + "..." + a.Version
	}
	return base + "/commit/" + a.Version
}

func BuildReport(version, statusDir, requestDir string, now time.Time) (Report, error) {
	st, ok, err := Read(statusDir)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Configured: statusDir != "", RunningVersion: version}
	if ok {
		if st.Running != nil && now.Sub(st.Running.Since) > RunningMaxAge {
			st.Running = nil
		}
		rep.Status, rep.Stale = &st, st.Stale(now)
		current := st.RecordedVersion
		if current == "" {
			current = version
		}
		rep.NotesURL = NotesURL(st.Available, current)
	} else {
		rep.ExpectsStatus = rep.Configured && IsVersion(version)
	}
	since, pending, err := Pending(requestDir, now)
	if err != nil {
		return Report{}, err
	}
	rep.Request = requestState(rep.Status, rep.Stale, requestDir, since, pending)
	if info, err := os.Stat(requestDir); requestDir != "" && (err != nil || !info.IsDir()) {
		rep.Request = RequestState{Reason: ReasonUnsupported}
	}
	return rep, nil
}

func requestState(st *Status, stale bool, requestDir string, since time.Time, pending bool) RequestState {
	switch {
	case requestDir == "":
		return RequestState{Reason: ReasonNotConfigured}
	case st == nil || st.Requests == nil || !st.Requests.Supported:
		return RequestState{Reason: ReasonUnsupported}
	case st.Channel == "off":
		return RequestState{Reason: ReasonOff}
	case pending:
		return RequestState{Reason: ReasonPending, PendingSince: since}
	case st.Running != nil:
		return RequestState{Reason: ReasonRunning}
	case stale:
		return RequestState{Reason: ReasonUnsupported}
	}
	return RequestState{Available: true}
}

type Requested struct {
	RequestedAt time.Time `json:"requested_at"`
}
