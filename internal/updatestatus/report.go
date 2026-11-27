package updatestatus

import (
	"os"
	"time"
)

// Why the operator can't press "Update now" now (RequestState.Reason).
const (
	// ReasonNotConfigured: the server was not given a request directory (it isn't a Compose
	// install, or an install from before the button).
	ReasonNotConfigured = "not_configured"
	// ReasonUnsupported: the host can't take a request: its updater predates the button, its
	// request directory or path unit isn't in place (deploy/INSTALL.md step 16), or it has
	// written no status yet.
	ReasonUnsupported = "unsupported"
	// ReasonOff: the updater's channel is off.
	ReasonOff = "off"
	// ReasonPending: a request waits for the host to take it.
	ReasonPending = "pending"
	// ReasonRunning: a run is changing the install.
	ReasonRunning = "running"
)

// RequestState is whether the operator may press "Update now", and if not, why.
type RequestState struct {
	Available bool `json:"available"`
	// Reason is one of the Reason constants when Available is false.
	Reason string `json:"reason,omitempty"`
	// PendingSince is when the request that waits was made.
	PendingSince time.Time `json:"pending_since,omitzero"`
}

// Report is the reply of GET /api/workspaces/{workspace}/updates: the version this server runs,
// what the host's updater last wrote, and whether "Update now" can be pressed.
type Report struct {
	// Configured is whether the server was told where the status is.
	Configured     bool   `json:"configured"`
	RunningVersion string `json:"running_version"`
	// ExpectsStatus is set when the server is a release (or a commit's build) and the status is
	// still missing, which only an updater of the host that predates it explains.
	ExpectsStatus bool `json:"expects_status,omitempty"`
	// Stale says the status wasn't written for days.
	Stale  bool    `json:"stale,omitempty"`
	Status *Status `json:"status"`
	// Request is "Update now"'s state.
	Request RequestState `json:"request"`
	// NotesURL is where to read what the available version brings: its release's page on the
	// release channel, or on main the comparison of the version on record with it. Empty when
	// nothing is available, or the updater named no repository. The server builds it, from the
	// checked repository and versions only.
	NotesURL string `json:"notes_url,omitempty"`
}

// NotesURL is the page that says what a, available on top of current, brings.
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

// BuildReport reads the status in statusDir and the request in requestDir and says what the page
// shows. It reads only; an unreadable status is an error.
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
	// A directory that isn't there is an install from before the button (the mount, or the
	// directory itself, is missing): whatever the status says.
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
		// The updater hasn't written for days: its timer, and likely its path unit, aren't on.
		return RequestState{Reason: ReasonUnsupported}
	}
	return RequestState{Available: true}
}

// Requested is the reply of POST /api/workspaces/{workspace}/updates/request.
type Requested struct {
	// RequestedAt is when the server made the request; the page waits for a run the updater
	// started after it.
	RequestedAt time.Time `json:"requested_at"`
}
