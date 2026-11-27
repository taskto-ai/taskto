package dictation

import (
	"context"
	"errors"

	"taskto.ai/taskto/internal/apierr"
)

type Engine interface {
	Transcribe(ctx context.Context, audio Audio, opts Options) (string, error)
}

type Options struct {
	Language string
	Terms    []string
}

type Unavailable struct {
	Reason string
	Code   apierr.Code
	Params apierr.Params
}

func (u *Unavailable) Error() string { return u.Reason }

func unavailable(err error) (*Unavailable, bool) {
	var u *Unavailable
	ok := errors.As(err, &u)
	return u, ok
}

type Local interface {
	Engine
	Model() string
	State() LocalState
	Warm()
	Credits() []Credit
}

const (
	LocalUnavailable   = "unavailable"
	LocalNotDownloaded = "not_downloaded"
	LocalDownloading   = "downloading"
	LocalReady         = "ready"
	LocalFailed        = "failed"
)

type LocalState struct {
	State       string        `json:"state"`
	Progress    float64       `json:"progress,omitempty"`
	Error       string        `json:"error,omitempty"`
	ErrorCode   apierr.Code   `json:"error_code,omitempty"`
	ErrorParams apierr.Params `json:"error_params,omitempty"`
}

type Credit struct {
	Name       string `json:"name"`
	By         string `json:"by"`
	URL        string `json:"url"`
	Licence    string `json:"licence"`
	LicenceURL string `json:"licence_url"`
	Note       string `json:"note,omitempty"`
}
