package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Feed writes the agent runtime's providers file from the database, and has the agent runtime
// reload it, so it always runs with the accounts as they are.
//
// In its directory (the agent runtime's, <data>/agent/) it writes:
//
//   - providers.json, mode 0600: the operator's providers (the -agent-providers file) first,
//     or the scripted offline model without one, then every account, workspaces' before
//     users'. The first model of the first provider is the agent runtime's default, so it is
//     never someone's account.
//   - keys/<provider>.key, mode 0600 in a 0700 directory: each account's API key, the only
//     place it is ever in clear. The agent runtime reads it on every request, so a new key
//     takes effect at once; files of keys no longer in use are removed.
//
// Each file is replaced in one rename, so the agent runtime never reads half of one.
type Feed struct {
	store    *Store
	dir      string
	instance []instanceProvider
	egress   Egress
	reload   func(context.Context) error
	log      *slog.Logger

	mu    sync.Mutex // one write at a time, so the files always match one read of the database
	retry chan struct{}
}

// FeedConfig configures a Feed.
type FeedConfig struct {
	// Dir is the agent runtime's directory, <data>/agent.
	Dir string
	// OperatorFile is the operator's own providers file (-agent-providers), or "". It is read
	// once, here.
	OperatorFile string
	// Reload asks the running agent runtime to read the providers file again. An error that
	// wraps ErrRefused means it refused the file; any other error, that it didn't answer.
	Reload func(context.Context) error
	// Egress is the accounts' egress policies, which the agent runtime enforces on its calls
	// to their endpoints. The operator's providers have none of the server's: the operator's
	// file sets them, if it wants.
	Egress Egress
	// CLI are the CLI workers to offer, after the operator's providers (none while CLI
	// workers are off).
	CLI []CLIProvider
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// CLIProvider is a CLI worker as the agent runtime and the model list see it: provider ID
// "cli-<cli>", and its models (the CLI's own model names; "default" leaves the CLI's choice).
type CLIProvider struct {
	CLI    string
	Name   string
	Models []ModelChoice
}

// instanceProvider is one of the operator's providers: as written, for the file, and what the
// model list shows of it.
type instanceProvider struct {
	raw    json.RawMessage
	choice Choice
}

// scriptProvider stands in for the operator's providers when there are none: the agent
// runtime's scripted offline model, as without any providers file.
var scriptProvider = json.RawMessage(`{"id":"script","kind":"script","name":"Scripted (offline)"}`)

// NewFeed returns a feed of store's accounts. It reads the operator's providers file, refusing
// one that isn't JSON or uses an account's ID; the agent runtime checks the rest.
func NewFeed(store *Store, cfg FeedConfig) (*Feed, error) {
	dir, err := filepath.Abs(cfg.Dir)
	if err != nil {
		return nil, err
	}
	f := &Feed{store: store, dir: dir, egress: cfg.Egress, reload: cfg.Reload, log: cfg.Logger, retry: make(chan struct{}, 1)}
	if f.log == nil {
		f.log = slog.Default()
	}
	raws := []json.RawMessage{scriptProvider}
	if cfg.OperatorFile != "" {
		if raws, err = readOperatorFile(cfg.OperatorFile); err != nil {
			return nil, err
		}
	}
	for i, raw := range raws {
		var p struct {
			ID     string `json:"id"`
			Kind   string `json:"kind"`
			Name   string `json:"name"`
			Models []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"models"`
		}
		// The agent runtime validates the file in full; only the ID matters here.
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, fmt.Errorf("providers file %s: providers[%d] is not an object", cfg.OperatorFile, i)
		}
		if reserved(p.ID) || strings.HasPrefix(p.ID, CLIProviderPrefix) {
			return nil, fmt.Errorf("providers file %s: providers[%d].id looks like a provider account's (ws<N>-… or u<N>-…), or a CLI worker's (cli-…), which the server assigns", cfg.OperatorFile, i)
		}
		c := Choice{Provider: p.ID, Name: p.Name, Scope: ScopeInstance, Models: []ModelChoice{}}
		if c.Name == "" {
			c.Name = p.ID
		}
		if p.Kind == "script" {
			c.Models = append(c.Models, ModelChoice{ID: "script-1", Name: "Scripted"})
		}
		for _, m := range p.Models {
			if m.Name == "" {
				m.Name = m.ID
			}
			c.Models = append(c.Models, ModelChoice(m))
		}
		f.instance = append(f.instance, instanceProvider{raw: raw, choice: c})
	}
	for _, p := range cfg.CLI {
		type model struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		entry := struct {
			ID     string  `json:"id"`
			Kind   string  `json:"kind"`
			Name   string  `json:"name"`
			CLI    string  `json:"cli"`
			Models []model `json:"models"`
		}{ID: CLIProviderPrefix + p.CLI, Kind: "cli", Name: p.Name, CLI: p.CLI}
		c := Choice{Provider: entry.ID, Name: p.Name, Scope: ScopeCLI, Models: append([]ModelChoice(nil), p.Models...)}
		for _, m := range p.Models {
			entry.Models = append(entry.Models, model(m))
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		f.instance = append(f.instance, instanceProvider{raw: raw, choice: c})
	}
	return f, nil
}

// readOperatorFile returns the provider entries of an operator's providers file. Its errors
// never quote the file, which could hold a misplaced key.
func readOperatorFile(path string) ([]json.RawMessage, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chooses the file
	if err != nil {
		return nil, fmt.Errorf("providers file: %w", err)
	}
	var file struct {
		Providers []json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("providers file %s: not valid JSON, or providers isn't a list", path)
	}
	if len(file.Providers) == 0 {
		return nil, fmt.Errorf("providers file %s: providers must be a non-empty list", path)
	}
	return file.Providers, nil
}

// Path is the providers file the agent runtime reads (TASKTO_AGENT_PROVIDERS).
func (f *Feed) Path() string { return filepath.Join(f.dir, "providers.json") }

func (f *Feed) keyDir() string { return filepath.Join(f.dir, "keys") }

// Instance returns the operator's providers, as the model list shows them.
func (f *Feed) Instance() []Choice {
	out := make([]Choice, len(f.instance))
	for i, p := range f.instance {
		out[i] = p.choice
	}
	return out
}

// sidecarProvider is an account in the agent runtime's schema (docs/agent-providers.md).
type sidecarProvider struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	BaseURL string          `json:"baseUrl"`
	APIKey  *keyFile        `json:"apiKey,omitempty"`
	Compat  json.RawMessage `json:"compat,omitempty"`
	Models  []sidecarModel  `json:"models"`
	// Egress is the account's egress policy (internal/egress), or nil for any.
	Egress []string `json:"egress,omitempty"`
}

type keyFile struct {
	File string `json:"file"`
}

type sidecarModel struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int64  `json:"contextWindow,omitempty"`
	MaxTokens     int64  `json:"maxTokens,omitempty"`
	Reasoning     bool   `json:"reasoning"`
}

// write writes the providers file and the key files from the database. It returns the key
// files no longer in use, which the caller removes once the agent runtime stops reading them.
func (f *Feed) write(ctx context.Context) (stale []string, err error) {
	recs, err := f.store.all(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range []string{f.dir, f.keyDir()} {
		if err := ownerOnlyDir(d); err != nil {
			return nil, err
		}
	}
	providers := make([]json.RawMessage, 0, len(f.instance)+len(recs))
	for _, p := range f.instance {
		providers = append(providers, p.raw)
	}
	keep := map[string]bool{}
	for _, rec := range recs {
		p := sidecarProvider{
			ID: rec.Provider, Kind: rec.Kind, Name: rec.DisplayName, BaseURL: rec.BaseURL, Compat: rec.Compat,
			Models: make([]sidecarModel, len(rec.Models)), Egress: f.egress.Scope(rec.Scope).Entries(),
		}
		for i, m := range rec.Models {
			p.Models[i] = sidecarModel(m)
		}
		key, err := f.store.apiKey(rec)
		if err != nil {
			// Left out rather than sent without its key. Only the cause is logged, never the value.
			f.log.Error("provider account left out of the agent runtime: its API key can't be decrypted (was server.key replaced?)",
				"account", rec.ID, "provider", rec.Provider)
			continue
		}
		if key != "" {
			path := filepath.Join(f.keyDir(), rec.Provider+".key")
			if err := writeFile(path, []byte(key)); err != nil {
				return nil, fmt.Errorf("provider %s: key file: %w", rec.Provider, err)
			}
			keep[filepath.Base(path)] = true
			p.APIKey = &keyFile{File: path}
		}
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, err
		}
		providers = append(providers, raw)
	}
	body, err := json.MarshalIndent(map[string]any{"providers": providers}, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := writeFile(f.Path(), append(body, '\n')); err != nil {
		return nil, fmt.Errorf("providers file: %w", err)
	}
	entries, err := os.ReadDir(f.keyDir())
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !keep[e.Name()] {
			stale = append(stale, filepath.Join(f.keyDir(), e.Name()))
		}
	}
	return stale, nil
}

// Write writes the files without reloading the agent runtime: for before it starts.
func (f *Feed) Write(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stale, err := f.write(ctx)
	if err != nil {
		return err
	}
	_ = removeAll(stale) // logged; the files are written again at every start and change
	return nil
}

// reloadTimeout bounds one reload request; it reads a few small files.
const reloadTimeout = 10 * time.Second

// ErrKeyNotRemoved is wrapped by Sync's error when a removed account's key file could not be
// deleted: the key is still on disk, so a caller that must know it is gone (the purge of a deleted
// account) retries.
var ErrKeyNotRemoved = errors.New("a removed provider account's key file could not be deleted")

// Sync writes the files and has the agent runtime reload them. If it doesn't answer (it is
// starting or restarting), Run retries until it does; a restarted agent runtime reads the
// new files anew anyway. A refused file is logged: the agent runtime keeps the providers it had.
// The error is that of a refused file, and of a removed account's key file that couldn't be
// deleted (ErrKeyNotRemoved): never nil while a removed key is still on disk.
func (f *Feed) Sync(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stale, err := f.write(ctx)
	if err != nil {
		return err
	}
	// Keys of removed accounts go once the agent runtime has stopped using them, and also if
	// it can't be told: a removed key must not stay on disk.
	reloaded := f.reloadLocked(ctx)
	return errors.Join(reloaded, removeAll(stale))
}

func (f *Feed) reloadLocked(ctx context.Context) error {
	if f.reload == nil {
		return nil
	}
	rctx, cancel := context.WithTimeout(ctx, reloadTimeout)
	defer cancel()
	err := f.reload(rctx)
	switch {
	case err == nil:
		return nil
	case refused(err):
		f.log.Error("the agent runtime refused the providers file and keeps its previous providers", "err", err)
		return err
	default:
		f.log.Warn("the agent runtime didn't take the new providers yet; retrying", "err", err)
		select {
		case f.retry <- struct{}{}:
		default:
		}
		return nil
	}
}

// ErrRefused is wrapped by a FeedConfig.Reload error when the agent runtime answered and
// refused the file, as opposed to not answering.
var ErrRefused = errors.New("the agent runtime refused the providers file")

func refused(err error) bool { return errors.Is(err, ErrRefused) }

// Run retries reloads that didn't reach the agent runtime, until ctx ends.
func (f *Feed) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.retry:
		}
		for wait := time.Second; ; wait = min(2*wait, 30*time.Second) {
			t := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			rctx, cancel := context.WithTimeout(ctx, reloadTimeout)
			err := f.reload(rctx)
			cancel()
			if err == nil {
				f.log.Info("the agent runtime took the new providers")
				break
			}
			if refused(err) {
				f.log.Error("the agent runtime refused the providers file and keeps its previous providers", "err", err)
				break
			}
		}
	}
}

// writeFile replaces path with data, mode 0600, in one rename.
func writeFile(path string, data []byte) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// removeAll deletes the files, logging and answering (ErrKeyNotRemoved) the ones it couldn't.
func removeAll(paths []string) error {
	var failed []error
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("could not remove an unused provider key file", "path", p, "err", err)
			failed = append(failed, ErrKeyNotRemoved)
		}
	}
	return errors.Join(failed...)
}

// ownerOnlyDir creates dir with mode 0700, or makes an existing one 0700.
func ownerOnlyDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if info.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700) //nolint:gosec // G302: a directory needs x; 0700 is owner-only
	}
	return nil
}

// Choice is one provider in a model list: what a conversation may pick.
type Choice struct {
	Provider string        `json:"provider"`
	Name     string        `json:"name"`
	Scope    Scope         `json:"scope"`
	Account  int64         `json:"account,omitempty"`
	Models   []ModelChoice `json:"models"`
}

// ModelChoice is one model of a Choice.
type ModelChoice struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func choiceOf(rec record) Choice {
	c := Choice{Provider: rec.Provider, Name: rec.DisplayName, Scope: rec.Scope, Account: rec.ID, Models: make([]ModelChoice, len(rec.Models))}
	for i, m := range rec.Models {
		c.Models[i] = ModelChoice{ID: m.ID, Name: m.Name}
	}
	return c
}
