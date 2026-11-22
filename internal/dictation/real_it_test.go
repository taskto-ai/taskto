package dictation

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRealLocalModels downloads the pinned runtimes and models for this platform (about
// 700 MB) and transcribes a real recording with each. It runs only with
// TASKTO_DICTATION_IT=1, never in CI by default. TASKTO_DICTATION_IT_CACHE keeps the files
// between runs.
func TestRealLocalModels(t *testing.T) {
	if os.Getenv("TASKTO_DICTATION_IT") != "1" {
		t.Skip("set TASKTO_DICTATION_IT=1 to download the real models and run them")
	}
	dir := os.Getenv("TASKTO_DICTATION_IT_CACHE")
	if dir == "" {
		dir = t.TempDir()
	}
	// sherpa-onnx's English test clip of Parakeet v3, at the pinned commit.
	clip := fetchClip(t, "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/ed5d745623472f97cfc2e6c2916ee248798fc5a8/test_wavs/en.wav",
		"148b936b43ce7c546a866e64da059f0458aee2d65e617f16e9d94f06e8d99ed6")
	// Ten copies with a second of silence between them: past one piece, so it is split.
	var long Audio
	for range 10 {
		long.Samples = append(long.Samples, clip.Samples...)
		long.Samples = append(long.Samples, make([]int16, SampleRate)...)
	}

	for _, model := range Models {
		t.Run(model, func(t *testing.T) {
			e, err := NewLocal(model, dir)
			if err != nil {
				t.Fatal(err)
			}
			if e.State().State == LocalUnavailable {
				t.Skipf("no build for this platform: %s", e.State().Error)
			}
			e.Warm()
			deadline := time.Now().Add(downloadTimeout)
			for st := e.State(); st.State != LocalReady; st = e.State() {
				if st.State == LocalFailed || time.Now().After(deadline) {
					t.Fatalf("download: %+v", st)
				}
				time.Sleep(time.Second)
			}
			started := time.Now()
			text, err := e.Transcribe(ctx, clip, Options{Language: "en", Terms: []string{"Kennedy"}})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %q in %v", model, text, time.Since(started))
			if !strings.Contains(strings.ToLower(text), "ask not what your country can do for you") {
				t.Fatalf("text = %q", text)
			}
			started = time.Now()
			text, err = e.Transcribe(ctx, long, Options{Language: "en"})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: %v of audio in %v", model, long.Duration(), time.Since(started))
			if n := strings.Count(strings.ToLower(text), "your country"); n < 18 {
				t.Fatalf("%d of 20 \"your country\" in %q", n, text)
			}
		})
	}
}

func fetchClip(t *testing.T, url, sha string) Audio {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // a pinned test file
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(b); hex.EncodeToString(sum[:]) != sha {
		t.Fatalf("%s: unexpected sha256", url)
	}
	// The clip is a canonical 24 kHz WAV: resample it to 16 kHz as the web app would.
	rate := int(binary.LittleEndian.Uint32(b[24:28]))
	in := make([]int16, (len(b)-44)/2)
	for i := range in {
		in[i] = int16(binary.LittleEndian.Uint16(b[44+2*i:])) //nolint:gosec // PCM
	}
	out := make([]int16, len(in)*SampleRate/rate)
	for i := range out {
		pos := float64(i) * float64(rate) / SampleRate
		j := int(pos)
		frac := pos - float64(j)
		next := in[min(j+1, len(in)-1)]
		out[i] = int16(float64(in[j])*(1-frac) + float64(next)*frac)
	}
	return Audio{Samples: slices.Clip(out)}
}
