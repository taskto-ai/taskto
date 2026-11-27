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

// Everything the worker keeps for an account is under its identity, never its number: a restore of
// the server's data gives numbers again, while the worker's own directory (a volume of its own in
// container mode) is not part of the backup. The first time the worker sees an identity it gives
// it a home number from a counter that only goes up (homeCounter), recorded with the identity
// (accountRecord) before anything of the account's is made: its directory is Root/h<home>, and with
// isolation its Unix user is UIDBase plus the home number. So an old account's files, temporary
// files, sockets and processes all belong to a home and a uid no later account is ever given, and
// a request names the account it is for: one of an account that is gone can only ever reach that
// account's own home, which is retired (refused for good, until a census says it is live again).
//
// Removing a retired account's files and processes is tidying up, not what keeps accounts apart: it
// is bounded, and tried again at the next start when it doesn't finish.

// firstHome is the first home number: above every user number a worker before homes accepted
// (below 2^30), so no home's uid is a uid an older worker gave a user (their temporary files may
// still be there).
const firstHome = 1 << 30

// ErrUnknownAccount: the worker has no record of the account the request names, and the request
// isn't one that makes it (a lookup of a sign-in or a turn).
var ErrUnknownAccount = errors.New("the CLI worker doesn't know this account")

// ErrFull: every home number is taken (an install that made over a billion accounts on one
// worker volume). Accounts the worker has go on; a new one is refused.
var ErrFull = errors.New("the CLI worker has given out every home number: see docs/cli-workers.md")

// accountRecord is what the worker keeps of an account, in .worker/accounts/<identity>.json.
type accountRecord struct {
	Home int64 `json:"home"`
	// UID is the account's Unix user with isolation (the UID base it was given under plus Home),
	// so a worker started with another base is refused (pinUIDBase).
	UID int64 `json:"uid,omitempty"`
	// User is the number the account had when the worker first saw it, for logs and adoption.
	User int64 `json:"user"`
	// Boot is the server boot whose request made the record (Reconcile keeps an account made since
	// the census it is given), and Gen the number of the latest registered boot then (volume.go): a
	// census retires only accounts made before it was registered.
	Boot string `json:"boot"`
	Gen  int64  `json:"gen,omitempty"`
	// Retired is set when the account is gone (Forget, Reconcile), with when: nothing runs for it
	// until a census says it is live again, and its files are removed.
	Retired   bool      `json:"retired,omitempty"`
	RetiredAt time.Time `json:"retired_at,omitzero"`
	// RetiredGen is the number of the latest registered boot when it was retired: a census takes
	// back only a retirement made before it was registered (a deletion since is one it can't know
	// of). RetiredAt is for people to read; nothing is decided by it.
	RetiredGen int64 `json:"retired_gen,omitempty"`
	// Cleaned says the removal of the retired account completed once (its work stopped and waited
	// for, its home removed): written after it, so a census may leave an account alone only then.
	Cleaned bool `json:"cleaned,omitempty"`
	// Adopting is the number of a directory from before homes (Root/u<n>) being moved to this home
	// (Config.AdoptLegacy): set before the move, cleared after it, so a stop in between is finished
	// by the next use.
	Adopting int64 `json:"adopting,omitempty"`
}

// account is a request's account as the worker resolved it.
type account struct {
	id   string // the identity
	user int64
	home int64
}

func (w *Worker) accountsDir() string { return filepath.Join(w.stateDir(), "accounts") }

// accountFile is identity's record. Every caller has checked the identity (cliowner.IsIdentity:
// 32 lowercase hex digits); one that isn't names no file of an account.
func (w *Worker) accountFile(identity string) string {
	if !cliowner.IsIdentity(identity) || strings.Contains(identity, "..") || strings.ContainsRune(identity, filepath.Separator) {
		identity = "invalid"
	}
	return filepath.Join(w.accountsDir(), identity+".json")
}

func (w *Worker) counterFile() string { return filepath.Join(w.stateDir(), "next-home") }

// record reads identity's record, from memory once read. A record that can't be read is an error:
// only its absence says the worker doesn't know the account. The caller holds w.accountsMu.
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
	// A retired account the worker never gave a home (a deletion of one it hadn't seen) has none.
	if err := json.Unmarshal(b, &r); err != nil || (r.Home < firstHome && (!r.Retired || r.Home != 0)) || r.Home >= maxHome {
		return accountRecord{}, false, fmt.Errorf("the record of an account is damaged")
	}
	w.accounts[identity] = r
	return r, true, nil
}

// putRecord writes identity's record durably (the file and its directory synced) before it is used.
// The caller holds w.accountsMu.
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

// writeDurably replaces path, in dir, with b: a new file synced, renamed in place, and the
// directory synced, so the new content is on disk before anything relies on it.
func writeDurably(dir, path string, b []byte) error {
	if err := mkdirDurable(dir); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
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

func syncDir(path string) error {
	d, err := os.Open(path) //nolint:gosec // G304: the worker's own directories
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

// nextHome takes the next home number: the counter is moved on, on disk, before the number is
// used, so a number is never given twice, a crash in between only skipping one. The number is the
// highest of what the worker found at start and has given since (w.nextFree) and the counter file,
// so a file that is lost or put back while the worker runs never sends it lower; a home the volume
// has (a record, retired or not, or a directory) is skipped whatever they say. The caller holds
// w.accountsMu.
func (w *Worker) nextHome() (int64, error) {
	home := max(w.nextFree, int64(firstHome))
	if n, ok := w.readCounter(); ok {
		home = max(home, n)
	}
	for ; home < maxHome; home++ {
		if w.usedHomes[home] {
			continue
		}
		if _, err := os.Lstat(w.cfg.Layout.Home(home).Root); errors.Is(err, fs.ErrNotExist) {
			break
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

// giveHome gives r the next home number, and with isolation its uid under the volume's UID base:
// every account that gets a home gets it here (a new one, or one a census brings back that never
// had one). The caller holds w.accountsMu.
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

// resolve is the account a request names: its record, or, with make, a new one, given the next
// home number. A retired account is ErrForgotten; one the worker doesn't know, without make,
// ErrUnknownAccount.
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
		r = accountRecord{User: a.User, Boot: a.Boot, Gen: w.latestSeq()}
		if err := w.giveHome(&r); err != nil {
			return account{}, err
		}
		if w.cfg.AdoptLegacy {
			if _, err := os.Lstat(w.cfg.Layout.legacy(a.User)); err == nil {
				r.Adopting = a.User
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

// adopt moves a directory from before homes (Root/u<n>, local mode: the server's backups hold it
// with the database, so it is the account's of its number) to the account's home, once: the
// record says so before the move, and stops saying so after it. The caller holds w.accountsMu.
func (w *Worker) adopt(identity string, r accountRecord) error {
	from, to := w.cfg.Layout.legacy(r.Adopting), w.cfg.Layout.Home(r.Home).Root
	if _, err := os.Lstat(from); err == nil {
		if _, err := os.Lstat(to); errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(from, to); err != nil {
				return err
			}
			if err := syncDir(w.cfg.Layout.Root); err != nil {
				return err
			}
		}
	}
	if hook := w.adoptHookForTest; hook != nil {
		if err := hook(); err != nil {
			return err
		}
	}
	r.Adopting = 0
	return w.putRecord(identity, r)
}

// retire records identity, user's, as gone, durably: nothing runs for it until a census says it
// is live again. Its record stays, so its home number is never given again; one the worker had
// none of gets one with no home.
func (w *Worker) retire(identity string, user int64) error {
	_, err := w.retireNow(identity, user)
	return err
}

// retireNow is retire, and reports whether the account was retired already.
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

// markCleaned records that the removal of identity's retired account completed (tidy, Forget).
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

// retired reports whether identity is retired. A record that can't be read counts as retired: only
// its absence or its saying so tells.
func (w *Worker) retired(identity string) bool {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	r, ok, err := w.record(identity)
	return err != nil || (ok && r.Retired)
}

// records lists every account the worker has a record of.
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

// maxHome bounds the home numbers, so that UIDBase plus any of them stays a valid uid below the
// 32-bit nobody (New checks UIDBase against it): 2^30 accounts from firstHome.
const maxHome = 1 << 31

// public is the account as the browsers and the server name it.
func (a account) public() cliowner.Account { return cliowner.Account{User: a.user, Identity: a.id} }

// find is identity's record, retired or not.
func (w *Worker) find(identity string) (accountRecord, bool, error) {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	return w.record(identity)
}

// unretire takes back identity's retirement, made before before: a census says the account is
// live again (a restore of the server's data brought it back). A retirement made after it is a
// deletion the census can't know of, and stays.
func (w *Worker) unretire(identity string, seq int64) error {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	r, ok, err := w.record(identity)
	if err != nil || !ok || !r.Retired || r.RetiredGen >= seq {
		return err
	}
	r.Retired, r.RetiredAt, r.RetiredGen, r.Cleaned = false, time.Time{}, 0, false
	if r.Home == 0 {
		// Retired before the worker gave it a home: it gets one now, as any account does.
		if err := w.giveHome(&r); err != nil {
			return err
		}
	}
	return w.putRecord(identity, r)
}

// AccountDirs returns a's directories, made (mode 700) with cli's config directory for slot (none
// for an empty cli), the account given a home if the worker has none: where local mode's browsers
// keep its profile (browser.LocalHost). Only without isolation: with it, an account's directories
// are made by its own Unix user.
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

// HomeDir is the directory of identity's home, retired or not, if the worker gave it one: for
// removing what is left of a retired account (a purge), never made.
func (w *Worker) HomeDir(identity string) (string, bool) {
	home, ok := w.HomeOf(identity)
	if !ok {
		return "", false
	}
	return w.cfg.Layout.Home(home).Root, true
}

// HomeOf is the home number of identity's account, retired or not, if the worker has one.
func (w *Worker) HomeOf(identity string) (int64, bool) {
	r, ok, err := w.find(identity)
	if err != nil || !ok || r.Home == 0 {
		return 0, false
	}
	return r.Home, true
}
