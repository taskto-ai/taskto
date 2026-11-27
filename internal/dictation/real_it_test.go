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

func TestRealLocalModels(t *testing.T) {
	if os.Getenv("TASKTO_DICTATION_IT") != "1" {
		t.Skip("set TASKTO_DICTATION_IT=1 to download the real models and run them")
	}
	dir := os.Getenv("TASKTO_DICTATION_IT_CACHE")
	if dir == "" {
		dir = t.TempDir()
	}
	clip := fetchClip(t, "https://huggingface.co/csukuangfj/sherpa-onnx-nemo-parakeet-tdt-0.6b-v3-int8/resolve/ed5d745623472f97cfc2e6c2916ee248798fc5a8/test_wavs/en.wav",
		"148b936b43ce7c546a866e64da059f0458aee2d65e617f16e9d94f06e8d99ed6")
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
	resp, err := http.Get(url)
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
	rate := int(binary.LittleEndian.Uint32(b[24:28]))
	in := make([]int16, (len(b)-44)/2)
	if _, err := binary.Decode(b[44:44+2*len(in)], binary.LittleEndian, in); err != nil {
		t.Fatal(err)
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
