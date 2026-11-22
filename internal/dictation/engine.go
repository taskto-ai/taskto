// Package dictation turns speech into text for the chat box (docs/dictation.md): the
// workspace's settings, the engines that transcribe (a hosted API with the workspace's key,
// the operator's local model, or an add-on the operator installed), and the knowledge
// base's glossary, which improves what they hear.
package dictation

import (
	"context"
	"errors"

	"taskto.ai/taskto/internal/apierr"
)

// Engine turns audio into text.
type Engine interface {
	// Transcribe returns the text of audio, which is at most MaxDuration long. An engine
	// that can't run now returns an *Unavailable.
	Transcribe(ctx context.Context, audio Audio, opts Options) (string, error)
}

// Options are what a transcription may use besides the audio.
type Options struct {
	// Language is an ISO 639-1 code, or "" for the engine to detect it.
	Language string
	// Terms are glossary terms to favour. Only engines that run on this server get them:
	// they are knowledge-base content.
	Terms []string
}

// Unavailable is an engine that can't transcribe now, and why, in words safe to show, with
// the API's code for them (and its params).
type Unavailable struct {
	Reason string
	Code   apierr.Code
	Params apierr.Params
}

func (u *Unavailable) Error() string { return u.Reason }

// unavailable reports whether err is an *Unavailable, and returns it.
func unavailable(err error) (*Unavailable, bool) {
	var u *Unavailable
	ok := errors.As(err, &u)
	return u, ok
}

// Local is the operator's local engine: a model the server runs itself, loaded for each
// dictation and freed after it.
type Local interface {
	Engine
	// Model is the operator's choice, such as "parakeet-v3".
	Model() string
	// State says whether the engine can run now.
	State() LocalState
	// Warm starts getting the model ready (downloading it) if it isn't, and returns at once.
	Warm()
	// Credits are the attributions the runtime and the model need.
	Credits() []Credit
}

// The values of LocalState.State.
const (
	LocalUnavailable   = "unavailable"    // not on this platform
	LocalNotDownloaded = "not_downloaded" // its files aren't here yet
	LocalDownloading   = "downloading"
	LocalReady         = "ready"
	LocalFailed        = "failed" // the last download failed; Warm tries again
)

// LocalState is the local engine's state, as the API shows it.
type LocalState struct {
	State string `json:"state"`
	// Progress is how much of the download is done, from 0 to 1, while downloading.
	Progress float64 `json:"progress,omitempty"`
	// Error says why, when unavailable or failed, in words safe to show; ErrorCode and
	// ErrorParams are its code, for the web app to translate (apierr).
	Error       string        `json:"error,omitempty"`
	ErrorCode   apierr.Code   `json:"error_code,omitempty"`
	ErrorParams apierr.Params `json:"error_params,omitempty"`
}

// Credit is an attribution the web app's About page shows.
type Credit struct {
	Name       string `json:"name"`
	By         string `json:"by"`
	URL        string `json:"url"`
	Licence    string `json:"licence"`
	LicenceURL string `json:"licence_url"`
	// Note says what it is used for, or what changed (such as quantization).
	Note string `json:"note,omitempty"`
}
