package cliworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/cliowner"
)

const firstHome = 1 << 30

var ErrUnknownAccount = errors.New("the CLI worker doesn't know this account")

var ErrFull = errors.New("the CLI worker has given out every home number: see docs/cli-workers.md")

type accountRecord struct {
	Home         int64     `json:"home"`
	UID          int64     `json:"uid,omitempty"`
	User         int64     `json:"user"`
	Installation string    `json:"installation,omitempty"`
	Boot         string    `json:"boot"`
	Gen          int64     `json:"gen,omitempty"`
	Retired      bool      `json:"retired,omitempty"`
	RetiredAt    time.Time `json:"retired_at,omitzero"`
	RetiredGen   int64     `json:"retired_gen,omitempty"`
	Cleaned      bool      `json:"cleaned,omitempty"`
	Adopting     int64     `json:"adopting,omitempty"`
}

type account struct {
	id   string
	user int64
	home int64
}

func (w *Worker) accountsDir() string { return filepath.Join(w.stateDir(), "accounts") }

func (w *Worker) accountFile(identity string) string {
	if !cliowner.IsIdentity(identity) || strings.Contains(identity, "..") || strings.ContainsRune(identity, filepath.Separator) {
		identity = "invalid"
	}
	return filepath.Join(w.accountsDir(), identity+".json")
}

func (w *Worker) counterFile() string { return filepath.Join(w.stateDir(), "next-home") }

func (w *Worker) record(identity string) (accountRecord, bool, error) {
	if r, ok := w.accounts[identity]; ok {
		return r, true, nil
	}
	b, err := os.ReadFile(w.accountFile(identity))
	if errors.Is(err, fs.ErrNotExist) {
		return accountRecord{}, false, nil
	}
	if err != nil {
		return accountRecord{}, false, err
	}
	var r accountRecord
	if err := json.Unmarshal(b, &r); err != nil || (r.Home < firstHome && (!r.Retired || r.Home != 0)) || r.Home >= maxHome ||
		!validSeq(r.Gen) || !validSeq(r.RetiredGen) {
		return accountRecord{}, false, fmt.Errorf("the record of an account is damaged")
	}
	w.accounts[identity] = r
	return r, true, nil
}

func (w *Worker) putRecord(identity string, r accountRecord) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := writeDurably(w.accountsDir(), w.accountFile(identity), b); err != nil {
		return err
	}
	w.accounts[identity] = r
	return nil
}

func writeDurably(dir, path string, b []byte) error {
	if err := mkdirDurable(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

var faultForTest func(op, path string) error

func fault(op, path string) error {
	if f := faultForTest; f != nil {
		return f(op, path)
	}
	return nil
}

func lstat(path string) (fs.FileInfo, error) {
	if err := fault("lstat", path); err != nil {
		return nil, err
	}
	return os.Lstat(path)
}

func syncDir(path string) error {
	if err := fault("sync", path); err != nil {
		return err
	}
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return err
	}
	if f := syncedForTest; f != nil {
		f(path)
	}
	return nil
}

func (w *Worker) nextHome() (int64, error) {
	home := max(w.nextFree, int64(firstHome))
	if n, ok := w.readCounter(); ok {
		home = max(home, n)
	}
	for ; home < maxHome; home++ {
		if w.usedHomes[home] {
			continue
		}
		_, err := lstat(w.cfg.Layout.Home(home).Root)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return 0, err
		}
	}
	if home >= maxHome {
		return 0, ErrFull
	}
	if err := writeDurably(w.stateDir(), w.counterFile(), []byte(strconv.FormatInt(home+1, 10)+"\n")); err != nil {
		return 0, err
	}
	w.nextFree = home + 1
	if w.usedHomes == nil {
		w.usedHomes = map[int64]bool{}
	}
	w.usedHomes[home] = true
	if hook := w.allocateHookForTest; hook != nil {
		if err := hook(); err != nil {
			return 0, err
		}
	}
	return home, nil
}

func (w *Worker) giveHome(r *accountRecord) error {
	home, err := w.nextHome()
	if err != nil {
		return err
	}
	r.Home = home
	if iso := w.cfg.Isolation; iso != nil {
		r.UID = int64(iso.UIDBase) + home
	}
	return nil
}

func (w *Worker) resolve(a cliowner.Account, make bool) (account, error) {
	if !a.Valid() {
		return account{}, badRequest{errors.New("invalid account")}
	}
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	r, ok, err := w.record(a.Identity)
	if err != nil {
		return account{}, err
	}
	if !ok {
		if !make {
			return account{}, ErrUnknownAccount
		}
		w.bootsMu.Lock()
		err := ErrAuthorityLost
		if w.authorityLost == "" {
			err = w.sameInstallation(a.Installation, a.Installation != "")
		}
		w.bootsMu.Unlock()
		if err != nil {
			return account{}, err
		}
		r = accountRecord{User: a.User, Boot: a.Boot, Installation: a.Installation, Gen: w.latestSeq()}
		if err := w.giveHome(&r); err != nil {
			return account{}, err
		}
		if w.cfg.AdoptLegacy {
			switch _, err := lstat(w.cfg.Layout.legacy(a.User)); {
			case err == nil:
				r.Adopting = a.User
			case !errors.Is(err, fs.ErrNotExist):
				return account{}, err
			}
		}
		if err := w.putRecord(a.Identity, r); err != nil {
			return account{}, err
		}
	}
	if r.Retired {
		return account{}, ErrForgotten
	}
	if iso := w.cfg.Isolation; iso != nil && r.UID != 0 && r.UID != int64(iso.UIDBase)+r.Home {
		return account{}, errors.New("the account's uid isn't its home's under this UID base")
	}
	if r.Adopting > 0 {
		if err := w.adopt(a.Identity, r); err != nil {
			return account{}, fmt.Errorf("moving the account's directory: %w", err)
		}
	}
	return account{id: a.Identity, user: a.User, home: r.Home}, nil
}

func (w *Worker) adopt(identity string, r accountRecord) error {
	from, to := w.cfg.Layout.legacy(r.Adopting), w.cfg.Layout.Home(r.Home).Root
	switch _, err := lstat(from); {
	case err == nil:
		switch _, err := lstat(to); {
		case errors.Is(err, fs.ErrNotExist):
			if err := os.Rename(from, to); err != nil {
				return err
			}
		case err != nil:
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := syncDir(w.cfg.Layout.Root); err != nil {
		return err
	}
	if hook := w.adoptHookForTest; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	r.Adopting = 0
	return w.putRecord(identity, r)
}

func (w *Worker) retire(identity string, user int64) error {
	_, err := w.retireNow(identity, user)
	return err
}

func (w *Worker) retireNow(identity string, user int64) (already bool, err error) {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	r, ok, err := w.record(identity)
	if err != nil {
		return false, err
	}
	if !ok {
		r = accountRecord{User: user, Gen: w.latestSeq()}
	}
	if r.Retired {
		return true, nil
	}
	r.Retired, r.RetiredAt, r.RetiredGen = true, time.Now(), w.latestSeq()
	return false, w.putRecord(identity, r)
}

func (w *Worker) markCleaned(identity string) error {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	r, ok, err := w.record(identity)
	if err != nil || !ok || !r.Retired || r.Cleaned {
		return err
	}
	r.Cleaned = true
	return w.putRecord(identity, r)
}

func (w *Worker) retired(identity string) bool {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	r, ok, err := w.record(identity)
	return err != nil || (ok && r.Retired)
}

func (w *Worker) records() (map[string]accountRecord, error) {
	entries, err := os.ReadDir(w.accountsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	out := map[string]accountRecord{}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !cliowner.IsIdentity(id) {
			continue
		}
		r, ok, err := w.record(id)
		if err != nil {
			return nil, err
		}
		if ok {
			out[id] = r
		}
	}
	return out, nil
}

const maxHome = 1 << 31

func (a account) public() cliowner.Account { return cliowner.Account{User: a.user, Identity: a.id} }

func (w *Worker) find(identity string) (accountRecord, bool, error) {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	return w.record(identity)
}

func (w *Worker) unretire(identity string, seq int64) error {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	r, ok, err := w.record(identity)
	if err != nil || !ok || !r.Retired || r.RetiredGen >= seq {
		return err
	}
	r.Retired, r.RetiredAt, r.RetiredGen, r.Cleaned = false, time.Time{}, 0, false
	if r.Home == 0 {
		if err := w.giveHome(&r); err != nil {
			return err
		}
	}
	return w.putRecord(identity, r)
}

func (w *Worker) AccountDirs(a cliowner.Account, cli CLI, slot Slot) (Dirs, error) {
	if w.cfg.Isolation != nil {
		return Dirs{}, errors.New("an isolated account's directories are made by its own Unix user")
	}
	acct, err := w.resolve(a, true)
	if err != nil {
		return Dirs{}, err
	}
	return w.cfg.Layout.EnsureSlot(acct.home, cli, slot)
}

func (w *Worker) HomeDir(identity string) (string, bool) {
	home, ok := w.HomeOf(identity)
	if !ok {
		return "", false
	}
	return w.cfg.Layout.Home(home).Root, true
}

func (w *Worker) HomeOf(identity string) (int64, bool) {
	r, ok, err := w.find(identity)
	if err != nil || !ok || r.Home == 0 {
		return 0, false
	}
	return r.Home, true
}
