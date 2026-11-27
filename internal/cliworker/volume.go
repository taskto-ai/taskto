package cliworker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"taskto.ai/taskto/internal/cliowner"
	"taskto.ai/taskto/internal/lockfile"
)

// The worker's volume (the layout's root, the cli-homes volume in container mode) holds the
// account records, the home counter and the homes. What keeps two accounts apart is that no home
// number, and so no uid, is ever given twice on it. So, before the worker uses the volume:
//
//   - it takes the volume's lock (.worker/lock, an exclusive flock held while it runs): a second
//     worker on the same volume doesn't start, and never touches the counter, the records or the
//     sockets of the first;
//   - it checks the counter against everything on the volume that names a home (the records, the
//     homes' directories, the trash): a counter that is missing, damaged or behind is moved past
//     the highest home seen, and two records with the same home stop the worker;
//   - with isolation, it pins the UID base to the volume (.worker/uid-base): a worker started with
//     another base would give a home's uid to another home, so it doesn't start.
//
// The metadata directories are made before any of this, each new one with its parent synced.

// ErrStaleBoot: the request comes from a server boot older than one the worker has seen since (a
// server that stopped, or one a restore replaced). A census or a deletion from it is ignored.
var ErrStaleBoot = errors.New("the request comes from an earlier start of the server")

// syncedForTest, in tests, is told each directory syncDir synced.
var syncedForTest func(path string)

// mkdirDurable makes dir and every missing parent (mode 700), each new one's parent synced, so
// the new entries are on disk before anything in them relies on them.
func mkdirDurable(dir string) error {
	if fi, err := os.Lstat(dir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("%s isn't a directory", dir)
		}
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(dir)
	if parent != dir {
		if err := mkdirDurable(parent); err != nil {
			return err
		}
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	return syncDir(parent)
}

// openVolume prepares the worker's own directories, takes the volume's lock, and checks the
// counter and, with isolation, the UID base. The lock is held until Close.
func (w *Worker) openVolume() error {
	for _, dir := range []string{w.stateDir(), w.accountsDir()} {
		if err := mkdirDurable(dir); err != nil {
			return err
		}
	}
	lock, err := lockfile.Lock(filepath.Join(w.stateDir(), "lock"))
	if err != nil {
		if errors.Is(err, lockfile.ErrLocked) {
			return fmt.Errorf("cliworker: another CLI worker uses %s: %w", w.cfg.Layout.Root, err)
		}
		return err
	}
	w.volumeLock = lock
	if err := w.checkCounter(); err != nil {
		return err
	}
	if iso := w.cfg.Isolation; iso != nil {
		if err := w.pinUIDBase(iso.UIDBase); err != nil {
			return err
		}
	}
	return w.loadBoots()
}

// homeOfName is the home number a name on the volume gives (h<home>, or a removal's in the trash,
// which starts with it), and whether it gives one.
func homeOfName(name string) (int64, bool) {
	rest, ok := strings.CutPrefix(name, "h")
	if !ok {
		return 0, false
	}
	if i := strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
		rest = rest[:i]
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	return n, err == nil && n >= firstHome && n < maxHome
}

// checkCounter makes sure the next home number is above every home on the volume, and keeps what
// it found in memory (w.nextFree, w.usedHomes) for every allocation after it: the counter file is
// then only ever moved on, never read back lower.
func (w *Worker) checkCounter() error {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	highest := int64(0)
	owner := map[int64]string{}
	used := map[int64]bool{}
	entries, err := os.ReadDir(w.accountsDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || !cliowner.IsIdentity(id) {
			continue
		}
		r, ok, err := w.record(id)
		if err != nil {
			return fmt.Errorf("cliworker: the record of an account: %w", err)
		}
		if !ok || r.Home == 0 {
			continue
		}
		if other, dup := owner[r.Home]; dup {
			return fmt.Errorf("cliworker: two accounts have home %d (%s… and %s…): the worker's volume is damaged; start the worker on a new volume", r.Home, other[:6], id[:6])
		}
		owner[r.Home] = id
		used[r.Home] = true
		highest = max(highest, r.Home)
	}
	for _, dir := range []string{w.cfg.Layout.Root, w.trashDir()} {
		names, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, n := range names {
			if home, ok := homeOfName(n.Name()); ok {
				used[home] = true
				highest = max(highest, home)
			}
		}
	}
	w.usedHomes = used
	next, ok := w.readCounter()
	if ok && next > highest {
		w.nextFree = next
		return nil
	}
	if highest == 0 && !ok {
		w.nextFree = firstHome
		return nil // a new volume: the counter is made by the first allocation
	}
	repaired := max(highest+1, int64(firstHome))
	if ok && next > repaired {
		repaired = next
	}
	slog.Warn("cliworker: the home counter was missing, damaged or behind the homes on the volume: moved past them",
		"counter", next, "highest_home", highest, "next", repaired)
	if err := writeDurably(w.stateDir(), w.counterFile(), []byte(strconv.FormatInt(repaired, 10)+"\n")); err != nil {
		return err
	}
	w.nextFree = repaired
	return nil
}

// readCounter is the counter's value, and whether it could be read as one.
func (w *Worker) readCounter() (int64, bool) {
	b, err := os.ReadFile(w.counterFile())
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil && n >= firstHome
}

func (w *Worker) uidBaseFile() string { return filepath.Join(w.stateDir(), "uid-base") }

// pinUIDBase records base as the volume's UID base the first time, and refuses another later:
// every account's uid is its home number plus the base it was given under. A volume whose base
// can't be rebuilt (homes given, no pin, and no record that says its uid) is refused too: a base
// it accepted then might give a home's uid to another home.
func (w *Worker) pinUIDBase(base int) error {
	b, err := os.ReadFile(w.uidBaseFile())
	switch {
	case err == nil:
		pinned, perr := strconv.Atoi(strings.TrimSpace(string(b)))
		if perr != nil {
			return errors.New("cliworker: the volume's UID base (.worker/uid-base) is damaged")
		}
		if pinned != base {
			return fmt.Errorf("cliworker: this volume's accounts run as uid %d plus their home number: start the worker with -uid-base %d, or on a new volume", pinned, pinned)
		}
		return nil
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	// Not pinned: the records that say their uid must agree with base, and with homes given at
	// least one must say it.
	recs, err := w.records()
	if err != nil {
		return err
	}
	homes, said := 0, 0
	for _, r := range recs {
		if r.Home == 0 {
			continue
		}
		homes++
		if r.UID == 0 {
			continue
		}
		said++
		if r.UID != int64(base)+r.Home {
			return fmt.Errorf("cliworker: this volume's accounts run under uid base %d: start the worker with -uid-base %d, or on a new volume", r.UID-r.Home, r.UID-r.Home)
		}
	}
	if homes > 0 && said == 0 {
		return errors.New("cliworker: this volume has homes but no record of the UID base they were given under (.worker/uid-base is gone): start the worker on a new volume")
	}
	return writeDurably(w.stateDir(), w.uidBaseFile(), []byte(strconv.Itoa(base)+"\n"))
}

// bootsFile keeps the server boots whose censuses the worker applied, in that order.
func (w *Worker) bootsFile() string { return filepath.Join(w.stateDir(), "boots.json") }

// maxBoots bounds the boots kept: a server start each, a few kilobytes for years of them.
const maxBoots = 4096

// bootEntry is a boot whose census the worker applied, with when its server read its database.
type bootEntry struct {
	Boot   string    `json:"boot"`
	Before time.Time `json:"before"`
}

type bootLog struct {
	// Boots is the boots, oldest first.
	Boots []bootEntry `json:"boots"`
}

func (w *Worker) loadBoots() error {
	b, err := os.ReadFile(w.bootsFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var l bootLog
	if err := json.Unmarshal(b, &l); err != nil {
		// It is written whole, by a rename: damaged is the disk's doing, and the order it kept is
		// what keeps an earlier server's census from retiring a later one's accounts.
		return fmt.Errorf("cliworker: the log of server boots (.worker/boots.json) is damaged; remove it to start it again: %w", err)
	}
	w.boots = l.Boots
	return nil
}

// Only a census registers a boot: a server's census is the first thing it sends, and the worker
// keeps the order it applied them in. No other request registers one, so a delayed request of an
// earlier server can't make its boot look like the latest. The caller holds w.censusMu, which every
// census and deletion takes, so nothing registers a boot between a decision and what it decides.

// latestBoot is the boot of the last census applied, "" before any.
func (w *Worker) latestBoot() string {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	if len(w.boots) == 0 {
		return ""
	}
	return w.boots[len(w.boots)-1].Boot
}

// knownBoot reports whether a census of boot was applied.
func (w *Worker) knownBoot(boot string) bool {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	for _, b := range w.boots {
		if b.Boot == boot {
			return true
		}
	}
	return false
}

// registerCensus takes c's boot as the latest, or refuses it with ErrStaleBoot: a boot whose
// census was applied before another's, or one the worker never heard from whose server read its
// database before the latest's did (an earlier server's census, delayed). The clock only ever
// makes the worker refuse a census, which removes nothing, never apply one.
func (w *Worker) registerCensus(c Census) error {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	for i, b := range w.boots {
		if b.Boot == c.Boot {
			if i == len(w.boots)-1 {
				return nil
			}
			return ErrStaleBoot
		}
	}
	if n := len(w.boots); n > 0 && !c.Before.After(w.boots[n-1].Before) {
		return ErrStaleBoot
	}
	boots := append(append([]bootEntry(nil), w.boots...), bootEntry{Boot: c.Boot, Before: c.Before})
	if len(boots) > maxBoots {
		boots = boots[len(boots)-maxBoots:]
	}
	b, err := json.Marshal(bootLog{Boots: boots})
	if err != nil {
		return err
	}
	if err := writeDurably(w.stateDir(), w.bootsFile(), b); err != nil {
		return err
	}
	w.boots = boots
	return nil
}

// deletionFence admits a deletion (Forget, a browser purge) only from the latest census's boot:
// one an earlier server sent, delayed past a restore that brought the account back, retires and
// removes nothing. Before any census, there is nothing to fence against. The caller holds
// w.censusMu.
func (w *Worker) deletionFence(boot string) error {
	latest := w.latestBoot()
	if latest == "" || boot == latest {
		return nil
	}
	return ErrStaleBoot
}

// FenceDeletion runs do, the agent browsers' part of a deleted account's purge, as Forget is run:
// one at a time with the censuses, only for the latest server boot, and with the account retired
// first, so a purge an earlier server sent can't stop or remove the browser of an account a
// restore brought back (browser.Config.Fence).
func (w *Worker) FenceDeletion(ctx context.Context, a cliowner.Account, do func() error) error {
	if !cliowner.IsIdentity(a.Identity) {
		return badRequest{errors.New("invalid account")}
	}
	w.censusMu.Lock()
	defer w.censusMu.Unlock()
	if err := w.deletionFence(a.Boot); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.retire(a.Identity, a.User); err != nil {
		return fmt.Errorf("recording that the account is gone: %w", err)
	}
	return do()
}
