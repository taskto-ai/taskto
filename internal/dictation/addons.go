package dictation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

// Add-ons are programs the operator installs, from vendors whose licences keep them out of
// this repository and its image. TaskTo only knows of them: their name, the notice to show
// before an admin turns one on, and the attribution to show while it is on. The operator
// installs one at <addons_dir>/<id>/<entry>, and the server finds it at start.
//
// Desert Ant's other models (Redact, Schemer, …) would be more entries, of other kinds.

// KindSpeechToText is an add-on that is a dictation engine. Its entry, "transcribe", reads
// a WAV (16-bit PCM, mono, 16 kHz) on standard input, takes "--language <code>" when the
// language is known, and writes {"text": "…"} on standard output.
const KindSpeechToText = "speech-to-text"

// Addon is an add-on TaskTo knows of.
type Addon struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Vendor      string `json:"vendor"`
	Kind        string `json:"kind"`
	LicenceName string `json:"licence_name"`
	LicenceURL  string `json:"licence_url"`
	// Notice is what an admin must read and confirm before turning it on.
	Notice string `json:"notice"`
	// Attribution is the credit shown while it is on.
	Attribution Credit `json:"attribution"`
	entry       string // the program's name in its folder
}

// Addons are the add-ons TaskTo knows of.
var Addons = []Addon{{
	ID: "desertant-voz", Name: "Voz", Vendor: "Desert Ant Labs", Kind: KindSpeechToText,
	LicenceName: "Desert Ant Labs Source-Available License 1.1", LicenceURL: "https://license.desertant.com/",
	Notice: "Voz is source-available, not open source, and the operator installs it on this server. " +
		"Its licence check contacts Desert Ant Labs every time the model loads, which is every dictation, and it can't be turned off. " +
		"Each server counts as an install under the licence. " +
		"The check sends an install ID, the app's identifier, the model and SDK versions and the platform; never audio or text. " +
		"Speech stays on this server.",
	Attribution: Credit{
		Name: "Voz", By: "Desert Ant Labs", URL: "https://desertant.com",
		Licence: "Desert Ant Labs Source-Available License 1.1", LicenceURL: "https://license.desertant.com/",
		Note: "Speech recognition: Voz, powered by Desert Ant Labs.",
	},
	entry: "transcribe",
}}

// addonByID is the known add-on id, if any.
func addonByID(id string) (Addon, bool) {
	i := slices.IndexFunc(Addons, func(a Addon) bool { return a.ID == id })
	if i < 0 {
		return Addon{}, false
	}
	return Addons[i], true
}

// Installed is an add-on the operator installed.
type Installed struct {
	Addon
	Path string // its entry program
}

// DetectAddons finds the known add-ons installed under dir: an executable regular file at
// dir/<id>/<entry>.
func DetectAddons(dir string) []Installed {
	var found []Installed
	for _, a := range Addons {
		p := filepath.Join(dir, a.ID, a.entry)
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 == 0 {
			continue
		}
		found = append(found, Installed{Addon: a, Path: p})
	}
	return found
}

// addonEngine runs a speech-to-text add-on, one dictation at a time.
type addonEngine struct {
	Installed
	scratch string        // where each run gets a folder of its own
	jobs    chan struct{} // shared by every workspace using it
}

func (a *addonEngine) Transcribe(ctx context.Context, audio Audio, opts Options) (string, error) {
	select {
	case a.jobs <- struct{}{}:
		defer func() { <-a.jobs }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := os.MkdirAll(a.scratch, 0o700); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(a.scratch, a.ID+"-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	argv := []string{a.Path}
	if opts.Language != "" {
		argv = append(argv, "--language", opts.Language)
	}
	out, err := run(ctx, command{argv: argv, dir: work, stdin: bytes.NewReader(audio.WAV())})
	if err != nil {
		return "", err
	}
	var r struct {
		Text *string `json:"text"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil || r.Text == nil {
		return "", fmt.Errorf("%s: its output isn't {\"text\": …}", a.ID)
	}
	return *r.Text, nil
}
