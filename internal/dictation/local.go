package dictation

import (
	"archive/tar"
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"taskto.ai/taskto/internal/apierr"
)

const readyFile = ".ready"

const downloadTimeout = 2 * time.Hour

type localEngine struct {
	spec     spec
	files    []download
	dir      string
	client   *http.Client
	platform string

	mu    sync.Mutex
	state LocalState
	done  atomic.Int64
	total int64
	jobs  chan struct{}
}

func NewLocal(model, dir string) (Local, error) {
	s, ok := specs[model]
	if !ok {
		return nil, fmt.Errorf("dictation: unknown local model %q (one of %s)", model, strings.Join(Models, ", "))
	}
	return newLocal(s, dir, platform(), downloadClient()), nil
}

func downloadClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	t.ResponseHeaderTimeout = time.Minute
	return &http.Client{Transport: t}
}

func newLocal(s spec, dir, platform string, client *http.Client) *localEngine {
	e := &localEngine{
		spec: s, files: s.files(platform), dir: dir, client: client, platform: platform,
		jobs: make(chan struct{}, 1),
	}
	for _, f := range e.files {
		e.total += f.size
	}
	switch {
	case e.files == nil || !groupsSupported:
		e.state = LocalState{
			State: LocalUnavailable, Error: fmt.Sprintf("the local speech model %s has no build for %s", s.id, platform),
			ErrorCode: apierr.CodeDictationLocalNoBuild, ErrorParams: apierr.Params{"model": s.id, "platform": platform},
		}
	case e.ready():
		e.state = LocalState{State: LocalReady}
	default:
		e.state = LocalState{State: LocalNotDownloaded}
	}
	return e
}

func (e *localEngine) Model() string     { return e.spec.id }
func (e *localEngine) Credits() []Credit { return slices.Clone(e.spec.credits) }

func (e *localEngine) root() string { return filepath.Join(e.dir, e.spec.id) }

func (e *localEngine) ready() bool {
	b, err := os.ReadFile(filepath.Join(e.root(), readyFile))
	return err == nil && strings.TrimSpace(string(b)) == fingerprint(e.files)
}

func (e *localEngine) State() LocalState {
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.state
	if st.State == LocalDownloading && e.total > 0 {
		st.Progress = min(float64(e.done.Load())/float64(e.total), 0.99)
	}
	return st
}

func (e *localEngine) Warm() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.state.State != LocalNotDownloaded && e.state.State != LocalFailed {
		return
	}
	e.state = LocalState{State: LocalDownloading}
	e.done.Store(0)
	go e.fetch()
}

func (e *localEngine) fetch() {
	ctx, cancel := context.WithTimeout(context.Background(), downloadTimeout)
	defer cancel()
	started := time.Now()
	slog.Info("dictation: downloading the local speech model", "model", e.spec.id, "bytes", e.total, "dir", e.root())
	err := e.download(ctx)
	e.mu.Lock()
	defer e.mu.Unlock()
	if err != nil {
		slog.Error("dictation: downloading the local speech model failed", "model", e.spec.id, "err", err)
		e.state = LocalState{
			State: LocalFailed, Error: "downloading the local speech model failed; the server log says why, and the next dictation tries again",
			ErrorCode: apierr.CodeDictationLocalFailed,
		}
		return
	}
	slog.Info("dictation: the local speech model is ready", "model", e.spec.id, "took", time.Since(started).Round(time.Second))
	e.state = LocalState{State: LocalReady}
}

func (e *localEngine) download(ctx context.Context) error {
	root := e.root()
	partial, previous, old := root+".partial", root+".previous", root+".old"
	if _, err := os.Stat(partial); err == nil {
		if err := os.RemoveAll(previous); err != nil {
			return err
		}
		if err := os.Rename(partial, previous); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(partial, 0o700); err != nil {
		return err
	}
	for _, f := range e.files {
		if err := e.fetchOne(ctx, f, []string{root, previous}, partial); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(partial, readyFile), []byte(fingerprint(e.files)+"\n"), 0o600); err != nil {
		return err
	}
	if err := os.RemoveAll(old); err != nil {
		return err
	}
	hadRoot := os.Rename(root, old) == nil
	if err := os.Rename(partial, root); err != nil {
		if hadRoot {
			_ = os.Rename(old, root)
		}
		return err
	}
	_ = os.RemoveAll(old)
	_ = os.RemoveAll(previous)
	return nil
}

func (e *localEngine) fetchOne(ctx context.Context, f download, sources []string, dst string) error {
	if f.dest != "" {
		to := filepath.Join(dst, filepath.FromSlash(f.dest))
		for _, src := range sources {
			if reuse(f, filepath.Join(src, filepath.FromSlash(f.dest)), to) {
				e.done.Add(f.size)
				return nil
			}
		}
	}
	tmp, err := os.CreateTemp(dst, ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.url, nil)
	if err != nil {
		return err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		if ue := (*url.Error)(nil); errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s: %w", f.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", f.url, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h, progress{&e.done}), io.LimitReader(resp.Body, f.size+1))
	if err != nil {
		return fmt.Errorf("%s: %w", f.url, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); n != f.size || got != f.sha256 {
		return fmt.Errorf("%s: got %d bytes with sha256 %s, want %d bytes with sha256 %s", f.url, n, got, f.size, f.sha256)
	}
	if f.members != nil {
		if _, err := tmp.Seek(0, io.SeekStart); err != nil {
			return err
		}
		return extract(tmp, f.members, dst)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return place(tmp.Name(), filepath.Join(dst, filepath.FromSlash(f.dest)))
}

type progress struct{ n *atomic.Int64 }

func (p progress) Write(b []byte) (int, error) {
	p.n.Add(int64(len(b)))
	return len(b), nil
}

func reuse(f download, from, to string) bool {
	in, err := os.Open(filepath.Clean(from))
	if err != nil {
		return false
	}
	defer in.Close()
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() != f.size {
		return false
	}
	h := sha256.New()
	if _, err := io.Copy(h, in); err != nil || hex.EncodeToString(h.Sum(nil)) != f.sha256 {
		return false
	}
	return place(from, to) == nil
}

func place(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	mode := os.FileMode(0o600)
	if strings.HasPrefix(filepath.Base(filepath.Dir(to)), "bin") {
		mode = 0o700
	}
	if err := os.Chmod(from, mode); err != nil {
		return err
	}
	return os.Rename(from, to)
}

func extract(r io.Reader, members map[string]string, dst string) error {
	want := map[string]string{}
	for dest, member := range members {
		want[member] = dest
	}
	tr := tar.NewReader(bzip2.NewReader(r))
	for len(want) > 0 {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		_, inner, ok := strings.Cut(path.Clean(hdr.Name), "/")
		dest, wanted := want[inner]
		if !ok || !wanted || hdr.Typeflag != tar.TypeReg {
			continue
		}
		tmp, err := os.CreateTemp(dst, ".extract-*")
		if err != nil {
			return err
		}
		_, err = io.Copy(tmp, io.LimitReader(tr, hdr.Size))
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = place(tmp.Name(), filepath.Join(dst, filepath.FromSlash(dest)))
		}
		if err != nil {
			_ = os.Remove(tmp.Name())
			return err
		}
		delete(want, inner)
	}
	if len(want) > 0 {
		return fmt.Errorf("the archive lacks %d of its expected files", len(want))
	}
	return nil
}

func (e *localEngine) Transcribe(ctx context.Context, audio Audio, opts Options) (string, error) {
	if st := e.State(); st.State != LocalReady {
		e.Warm()
		return "", localReason(e.State())
	}
	select {
	case e.jobs <- struct{}{}:
		defer func() { <-e.jobs }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		return "", err
	}
	work, err := os.MkdirTemp(e.dir, "job-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	pieces := audio.Split(time.Duration(e.spec.maxPiece) * time.Second)
	files := make([]string, len(pieces))
	for i, p := range pieces {
		files[i] = filepath.Join(work, fmt.Sprintf("piece-%03d.wav", i))
		if err := os.WriteFile(files[i], p.WAV(), 0o600); err != nil {
			return "", err
		}
	}
	var texts []string
	switch e.spec.kind {
	case runSherpa:
		texts, err = e.runSherpa(ctx, work, files)
	case runCactus:
		texts, err = e.runCactus(ctx, work, files, opts)
	default:
		err = fmt.Errorf("unknown runtime kind %q", e.spec.kind)
	}
	if err != nil {
		return "", err
	}
	return joinTexts(texts), nil
}

func joinTexts(texts []string) string {
	var out []string
	for _, t := range texts {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, " ")
}

func (e *localEngine) runSherpa(ctx context.Context, work string, files []string) ([]string, error) {
	root := e.root()
	argv := []string{
		filepath.Join(root, "bin", "sherpa-onnx-offline"),
		"--tokens=" + filepath.Join(root, "tokens.txt"),
		"--encoder=" + filepath.Join(root, "encoder.int8.onnx"),
		"--decoder=" + filepath.Join(root, "decoder.int8.onnx"),
		"--joiner=" + filepath.Join(root, "joiner.int8.onnx"),
		"--model-type=nemo_transducer",
		"--num-threads=2",
	}
	out, err := run(ctx, command{argv: append(argv, files...), dir: work})
	if err != nil {
		return nil, err
	}
	var texts []string
	for line := range bytes.Lines(out) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("{")) {
			continue
		}
		var r struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("sherpa-onnx-offline: unexpected output: %w", err)
		}
		texts = append(texts, r.Text)
	}
	if len(texts) != len(files) {
		return nil, fmt.Errorf("sherpa-onnx-offline: %d results for %d pieces", len(texts), len(files))
	}
	return texts, nil
}

func (e *localEngine) runCactus(ctx context.Context, work string, files []string, opts Options) ([]string, error) {
	root := e.root()
	base := []string{filepath.Join(root, "bin", "cactus"), "--model", filepath.Join(root, "whistle.cact")}
	if slices.Contains(whistleLanguages, opts.Language) {
		base = append(base, "--language", opts.Language)
	}
	if len(opts.Terms) > 0 {
		keywords := filepath.Join(work, "keywords.txt")
		if err := os.WriteFile(keywords, []byte(strings.Join(opts.Terms, "\n")+"\n"), 0o600); err != nil {
			return nil, err
		}
		base = append(base, "--keywords", keywords)
	}
	texts := make([]string, 0, len(files))
	for _, f := range files {
		out, err := run(ctx, command{argv: append(slices.Clone(base), "--audio", f), dir: work})
		if err != nil {
			return nil, err
		}
		var r struct {
			Text *string `json:"text"`
		}
		if err := json.Unmarshal(bytes.TrimSpace(out), &r); err != nil || r.Text == nil {
			return nil, fmt.Errorf("cactus: unexpected output")
		}
		texts = append(texts, *r.Text)
	}
	return texts, nil
}
