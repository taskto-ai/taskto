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

const KindSpeechToText = "speech-to-text"

type Addon struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Vendor      string `json:"vendor"`
	Kind        string `json:"kind"`
	LicenceName string `json:"licence_name"`
	LicenceURL  string `json:"licence_url"`
	Notice      string `json:"notice"`
	Attribution Credit `json:"attribution"`
	entry       string
}

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

func addonByID(id string) (Addon, bool) {
	i := slices.IndexFunc(Addons, func(a Addon) bool { return a.ID == id })
	if i < 0 {
		return Addon{}, false
	}
	return Addons[i], true
}

type Installed struct {
	Addon
	Path string
}

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

type addonEngine struct {
	Installed
	scratch string
	jobs    chan struct{}
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
