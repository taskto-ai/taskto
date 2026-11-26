package dictation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFiles serves files by path, and counts the requests for each.
type fakeFiles struct {
	*httptest.Server
	mu    sync.Mutex
	files map[string][]byte
	hits  map[string]int
}

func newFakeFiles(t *testing.T, files map[string][]byte) *fakeFiles {
	f := &fakeFiles{files: files, hits: map[string]int{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		b, ok := f.files[r.URL.Path]
		f.hits[r.URL.Path]++
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeFiles) set(p string, b []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.files[p] = b
}

func (f *fakeFiles) download(p, dest string, members map[string]string) download {
	b := f.files[p]
	sum := sha256.Sum256(b)
	return download{url: f.URL + p, sha256: hex.EncodeToString(sum[:]), size: int64(len(b)), dest: dest, members: members}
}

const testPlatform = "testos/testarch"

func readFixture(t *testing.T, name string) []byte {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sherpaSpec is a model like Parakeet's, served by f: the runtime in an archive, and the
// model's files.
func sherpaSpec(t *testing.T, f *fakeFiles) spec {
	f.set("/runtime.tar.bz2", readFixture(t, "runtime.tar.bz2"))
	for _, name := range []string{"tokens.txt", "encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx"} {
		f.set("/"+name, []byte("fake "+name))
	}
	s := spec{id: "fake-parakeet", kind: runSherpa, maxPiece: 30, credits: []Credit{{Name: "Fake"}}, runtime: map[string][]download{
		testPlatform: {f.download("/runtime.tar.bz2", "", map[string]string{
			"bin/sherpa-onnx-offline": "bin/sherpa-onnx-offline", "lib/libonnxruntime.so": "lib/libonnxruntime.so",
		})},
	}}
	for _, name := range []string{"tokens.txt", "encoder.int8.onnx", "decoder.int8.onnx", "joiner.int8.onnx"} {
		s.model = append(s.model, f.download("/"+name, name, nil))
	}
	return s
}

func waitState(t *testing.T, e Local, want string) LocalState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		st := e.State()
		if st.State == want {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("state = %+v, want %s", st, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLocalDownloadsVerifiesAndRuns(t *testing.T) {
	if !groupsSupported {
		t.Skip("local engines need Unix")
	}
	t.Setenv("TASKTO_TEST_SECRET", "the runtime must not see this")
	f := newFakeFiles(t, map[string][]byte{})
	s := sherpaSpec(t, f)
	dir := t.TempDir()
	e := newLocal(s, dir, testPlatform, f.Client())
	if st := e.State(); st.State != LocalNotDownloaded {
		t.Fatalf("state = %+v", st)
	}
	// Not ready: a transcription starts the download and says so.
	_, err := e.Transcribe(ctx, tone(SampleRate, nil), Options{})
	if u, ok := unavailable(err); !ok || !strings.Contains(u.Reason, "downloading") {
		t.Fatalf("err = %v", err)
	}
	waitState(t, e, LocalReady)

	root := filepath.Join(dir, s.id)
	for _, name := range []string{"bin/sherpa-onnx-offline", "lib/libonnxruntime.so", "tokens.txt", "encoder.int8.onnx", readyFile} {
		if _, err := os.Stat(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	// Only the named members are extracted.
	if _, err := os.Stat(filepath.Join(root, "include")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("include extracted: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(root, "bin/sherpa-onnx-offline")); fi.Mode().Perm() != 0o700 {
		t.Fatalf("the runtime's mode is %v", fi.Mode())
	}
	if fi, _ := os.Stat(filepath.Join(root, "tokens.txt")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("a model file's mode is %v", fi.Mode())
	}

	// 70 s of audio is three pieces of at most 30 s, in one run.
	text, err := e.Transcribe(ctx, tone(SampleRate*70, nil), Options{Terms: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	if text != "piece 1 piece 2 piece 3" {
		t.Fatalf("text = %q", text)
	}
	// Each job's folder is gone afterwards.
	if jobs, _ := filepath.Glob(filepath.Join(dir, "job-*")); len(jobs) != 0 {
		t.Fatalf("left behind: %v", jobs)
	}

	// Another engine on the same folder (a restart) is ready without downloading again.
	hits := f.hits["/encoder.int8.onnx"]
	if e2 := newLocal(s, dir, testPlatform, f.Client()); e2.State().State != LocalReady || f.hits["/encoder.int8.onnx"] != hits {
		t.Fatalf("after a restart: %+v", e2.State())
	}
	if c := e.Credits(); len(c) != 1 || e.Model() != "fake-parakeet" {
		t.Fatalf("credits %v, model %q", c, e.Model())
	}
}

func TestLocalRefusesAFileWithTheWrongHash(t *testing.T) {
	if !groupsSupported {
		t.Skip("local engines need Unix")
	}
	f := newFakeFiles(t, map[string][]byte{})
	s := sherpaSpec(t, f)
	f.set("/encoder.int8.onnx", []byte("fake encoder.int8.onnx, but changed")) // same name, other bytes
	dir := t.TempDir()
	e := newLocal(s, dir, testPlatform, f.Client())
	e.Warm()
	st := waitState(t, e, LocalFailed)
	if !strings.Contains(st.Error, "failed") {
		t.Fatalf("state = %+v", st)
	}
	if _, err := os.Stat(filepath.Join(dir, s.id)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed download left the model's folder: %v", err)
	}
	// The right file comes back: the next Warm tries again, and keeps what the failed one
	// had already verified.
	tokens := f.hits["/tokens.txt"]
	f.set("/encoder.int8.onnx", []byte("fake encoder.int8.onnx"))
	e.Warm()
	waitState(t, e, LocalReady)
	if f.hits["/tokens.txt"] != tokens {
		t.Fatal("a file the failed attempt verified was downloaded again")
	}
	for _, leftover := range []string{".partial", ".previous", ".old"} {
		if _, err := os.Stat(filepath.Join(dir, s.id+leftover)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s left behind: %v", leftover, err)
		}
	}
}

func TestLocalReusesFilesAlreadyThere(t *testing.T) {
	if !groupsSupported {
		t.Skip("local engines need Unix")
	}
	f := newFakeFiles(t, map[string][]byte{})
	s := sherpaSpec(t, f)
	dir := t.TempDir()
	// The operator copied the big model file in by hand; the server didn't download it.
	root := filepath.Join(dir, s.id)
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "encoder.int8.onnx"), []byte("fake encoder.int8.onnx"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newLocal(s, dir, testPlatform, f.Client())
	e.Warm()
	waitState(t, e, LocalReady)
	if n := f.hits["/encoder.int8.onnx"]; n != 0 {
		t.Fatalf("the encoder was downloaded %d times", n)
	}
	if n := f.hits["/tokens.txt"]; n != 1 {
		t.Fatalf("tokens.txt was downloaded %d times", n)
	}
}

func TestLocalWithoutABuildForThisPlatform(t *testing.T) {
	f := newFakeFiles(t, map[string][]byte{})
	e := newLocal(sherpaSpec(t, f), t.TempDir(), "plan9/mips", f.Client())
	st := e.State()
	if st.State != LocalUnavailable || st.Error == "" {
		t.Fatalf("state = %+v", st)
	}
	e.Warm()
	if e.State().State != LocalUnavailable {
		t.Fatal("Warm changed an unavailable engine")
	}
	if _, err := e.Transcribe(ctx, tone(100, nil), Options{}); err == nil {
		t.Fatal("no error")
	}
	if _, err := NewLocal("no-such-model", t.TempDir()); err == nil {
		t.Fatal("an unknown model was accepted")
	}
}

// cactusScript is a fake cactus: it answers its arguments, so the test sees them.
const cactusScript = `#!/bin/sh
args="$*"
kw=""
while [ $# -gt 0 ]; do
  case "$1" in
    --keywords) kw=$(tr '\n' ',' < "$2"); shift ;;
  esac
  shift
done
printf '{"text":"%s kw=%s","language":"en"}\n' "$(echo "$args" | sed 's/[^ ]*\/\([^ \/]*\)/\1/g')" "$kw"
`

func TestLocalCactus(t *testing.T) {
	if !groupsSupported {
		t.Skip("local engines need Unix")
	}
	f := newFakeFiles(t, map[string][]byte{"/cactus": []byte(cactusScript), "/whistle.cact": []byte("weights")})
	s := spec{id: "fake-whistle", kind: runCactus, maxPiece: 30, runtime: map[string][]download{
		testPlatform: {f.download("/cactus", "bin/cactus", nil)},
	}, model: []download{f.download("/whistle.cact", "whistle.cact", nil)}}
	e := newLocal(s, t.TempDir(), testPlatform, f.Client())
	e.Warm()
	waitState(t, e, LocalReady)

	// A known language goes through, with the glossary as keywords.
	text, err := e.Transcribe(ctx, tone(SampleRate, nil), Options{Language: "de", Terms: []string{"Wurst", "taskto"}})
	if err != nil {
		t.Fatal(err)
	}
	if want := "--model whistle.cact --language de --keywords keywords.txt --audio piece-000.wav kw=Wurst,taskto,"; text != want {
		t.Fatalf("text = %q, want %q", text, want)
	}
	// One Whistle doesn't know is left to detect; 40 s is two runs.
	text, err = e.Transcribe(ctx, tone(SampleRate*40, nil), Options{Language: "pt"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "--language") || strings.Count(text, "--audio piece-") != 2 {
		t.Fatalf("text = %q", text)
	}
}

func TestRunStopsTheWholeGroupOnTimeout(t *testing.T) {
	if !groupsSupported {
		t.Skip("process groups need Unix")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "slow")
	// A child that would outlive its parent, if the group weren't killed.
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\nsleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := run(c, command{argv: []string{script}, dir: dir})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(started); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

func TestRunCapsAndReportsFailures(t *testing.T) {
	if !groupsSupported {
		t.Skip("process groups need Unix")
	}
	dir := t.TempDir()
	fail := filepath.Join(dir, "fail")
	if err := os.WriteFile(fail, []byte("#!/bin/sh\necho one >&2\necho the reason >&2\nexit 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, command{argv: []string{fail}, dir: dir}); err == nil || !strings.Contains(err.Error(), "the reason") {
		t.Fatalf("err = %v", err)
	}
	loud := filepath.Join(dir, "loud")
	if err := os.WriteFile(loud, []byte("#!/bin/sh\nhead -c 9000000 /dev/zero\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := run(ctx, command{argv: []string{loud}, dir: dir}); err == nil || !strings.Contains(err.Error(), "over") {
		t.Fatalf("err = %v", err)
	}
}
