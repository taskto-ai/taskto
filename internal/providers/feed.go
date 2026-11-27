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

type Feed struct {
	store    *Store
	dir      string
	instance []instanceProvider
	egress   Egress
	reload   func(context.Context) error
	log      *slog.Logger

	mu    sync.Mutex
	retry chan struct{}
}

type FeedConfig struct {
	Dir          string
	OperatorFile string
	Reload       func(context.Context) error
	Egress       Egress
	CLI          []CLIProvider
	Logger       *slog.Logger
}

type CLIProvider struct {
	CLI    string
	Name   string
	Models []ModelChoice
}

type instanceProvider struct {
	raw    json.RawMessage
	choice Choice
}

var scriptProvider = json.RawMessage(`{"id":"script","kind":"script","name":"Scripted (offline)"}`)

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

func readOperatorFile(path string) ([]json.RawMessage, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
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

func (f *Feed) Path() string { return filepath.Join(f.dir, "providers.json") }

func (f *Feed) keyDir() string { return filepath.Join(f.dir, "keys") }

func (f *Feed) Instance() []Choice {
	out := make([]Choice, len(f.instance))
	for i, p := range f.instance {
		out[i] = p.choice
	}
	return out
}

type sidecarProvider struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	BaseURL string          `json:"baseUrl"`
	APIKey  *keyFile        `json:"apiKey,omitempty"`
	Compat  json.RawMessage `json:"compat,omitempty"`
	Models  []sidecarModel  `json:"models"`
	Egress  []string        `json:"egress,omitempty"`
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

func (f *Feed) Write(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stale, err := f.write(ctx)
	if err != nil {
		return err
	}
	_ = removeAll(stale)
	return nil
}

const reloadTimeout = 10 * time.Second

var ErrKeyNotRemoved = errors.New("a removed provider account's key file could not be deleted")

func (f *Feed) Sync(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	stale, err := f.write(ctx)
	if err != nil {
		return err
	}
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

var ErrRefused = errors.New("the agent runtime refused the providers file")

func refused(err error) bool { return errors.Is(err, ErrRefused) }

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
		return os.Chmod(dir, 0o700)
	}
	return nil
}

type Choice struct {
	Provider string        `json:"provider"`
	Name     string        `json:"name"`
	Scope    Scope         `json:"scope"`
	Account  int64         `json:"account,omitempty"`
	Models   []ModelChoice `json:"models"`
}

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
