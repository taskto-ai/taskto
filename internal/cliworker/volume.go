package cliworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// checkCounter makes sure the next home number is above every home on the volume.
func (w *Worker) checkCounter() error {
	w.accountsMu.Lock()
	defer w.accountsMu.Unlock()
	highest := int64(0)
	owner := map[int64]string{}
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
				highest = max(highest, home)
			}
		}
	}
	next, ok := w.readCounter()
	if ok && next > highest {
		return nil
	}
	if highest == 0 && !ok {
		return nil // a new volume: the counter is made by the first allocation
	}
	repaired := max(highest+1, int64(firstHome))
	if ok && next > repaired {
		repaired = next
	}
	slog.Warn("cliworker: the home counter was missing, damaged or behind the homes on the volume: moved past them",
		"counter", next, "highest_home", highest, "next", repaired)
	return writeDurably(w.stateDir(), w.counterFile(), []byte(strconv.FormatInt(repaired, 10)+"\n"))
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
// every account's uid is its home number plus the base it was given under.
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
	// Not pinned yet: the records that say their uid must agree with base.
	recs, err := w.records()
	if err != nil {
		return err
	}
	for _, r := range recs {
		if r.UID != 0 && r.UID != int64(base)+r.Home {
			return fmt.Errorf("cliworker: this volume's accounts run under uid base %d: start the worker with -uid-base %d, or on a new volume", r.UID-r.Home, r.UID-r.Home)
		}
	}
	return writeDurably(w.stateDir(), w.uidBaseFile(), []byte(strconv.Itoa(base)+"\n"))
}

// bootsFile keeps the server boots the worker has seen, in the order it first saw them.
func (w *Worker) bootsFile() string { return filepath.Join(w.stateDir(), "boots.json") }

// maxBoots bounds the boots kept: the last ones are all that ever matter.
const maxBoots = 64

type bootLog struct {
	// Seen is the boots, oldest first.
	Seen []string `json:"seen"`
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
	w.boots = l.Seen
	return nil
}

// seeBoot records boot as seen, if it is new, and reports whether it is the latest: a boot first
// seen before another is a server's that started before it.
func (w *Worker) seeBoot(boot string) (latest bool, err error) {
	if boot == "" {
		return true, nil // a request from before boots were sent: nothing to order
	}
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	for i, b := range w.boots {
		if b == boot {
			return i == len(w.boots)-1, nil
		}
	}
	seen := append(append([]string(nil), w.boots...), boot)
	if len(seen) > maxBoots {
		seen = seen[len(seen)-maxBoots:]
	}
	b, err := json.Marshal(bootLog{Seen: seen})
	if err != nil {
		return false, err
	}
	if err := writeDurably(w.stateDir(), w.bootsFile(), b); err != nil {
		return false, err
	}
	w.boots = seen
	return true, nil
}
