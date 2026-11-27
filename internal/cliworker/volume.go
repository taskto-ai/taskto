package cliworker

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
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
	if err := w.loadInstallation(); err != nil {
		return err
	}
	return w.loadBoots()
}

// The worker's volume belongs to one taskto installation (the server's database's, which a restore
// keeps and another data directory doesn't share): the first census pins it, durably
// (.worker/installation), and from then on a census, a deletion or a new account of another
// installation is refused (ErrOtherInstallation) and registers nothing. Two servers of separate
// data directories (a move to a new host, a second install) can then never both act on one
// volume; moving it to another installation is an operator's handover (TrustServer). Boot order
// only has to tell apart the boots of one installation, which one server holds at a time.

// ErrOtherInstallation is the worker's answer to a census, a deletion or a new account from a
// server of another installation than the volume's.
var ErrOtherInstallation = cliowner.ErrOtherInstallation

func (w *Worker) installationFile() string { return filepath.Join(w.stateDir(), "installation") }

func (w *Worker) loadInstallation() error {
	b, there, err := readIfThere(w.installationFile())
	if err != nil || !there {
		return err
	}
	inst := strings.TrimSpace(string(b))
	if !cliowner.IsIdentity(inst) {
		return errors.New("cliworker: the volume's installation (.worker/installation) is damaged: an operator hands the volume over (taskto cli-worker trust-server -installation, docs/cli-workers.md)")
	}
	w.installation = inst
	return nil
}

// Authority is what the worker says of its authority (GET /authority): why it lost it, "" when it
// didn't, and the installation its volume belongs to, "" before the first census.
type Authority struct {
	Lost         string `json:"lost,omitempty"`
	Installation string `json:"installation,omitempty"`
}

// Authority is the worker's authority, for the server's operators.
func (w *Worker) Authority() Authority {
	return Authority{Lost: w.AuthorityLost(), Installation: w.Installation()}
}

// Installation is the installation the volume belongs to, "" before the first census.
func (w *Worker) Installation() string {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	return w.installation
}

// sameInstallation refuses inst unless it is the volume's (or the volume has none yet, and pin
// is set: then it becomes the volume's, unless the worker lost its authority, when only the
// operator's handover names it). The caller holds w.bootsMu.
func (w *Worker) sameInstallation(inst string, pin bool) error {
	switch {
	case w.installation != "" && inst == w.installation:
		return nil
	case w.installation != "":
		return ErrOtherInstallation
	case !pin:
		return nil
	case w.authorityLost != "":
		return ErrAuthorityLost
	case !cliowner.IsIdentity(inst):
		return badRequest{errors.New("a census names its installation")}
	}
	if err := writeDurably(w.stateDir(), w.installationFile(), []byte(inst+"\n")); err != nil {
		return err
	}
	w.installation = inst
	return nil
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
	// The next home number is firstHome up to maxHome (every number given): anything else is a
	// damaged counter, rebuilt from the records and the homes.
	return n, err == nil && n >= firstHome && n <= maxHome
}

func (w *Worker) uidBaseFile() string { return filepath.Join(w.stateDir(), "uid-base") }

// pinUIDBase records base as the volume's UID base the first time, and refuses another later:
// every account's uid is its home number plus the base it was given under. A volume whose base
// can't be rebuilt (homes given, no pin, and no record that says its uid) is refused too: a base
// it accepted then might give a home's uid to another home.
func (w *Worker) pinUIDBase(base int) error {
	if err := w.checkLegacyOwners(base); err != nil {
		return err
	}
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
	// A home's directory with no record (its record lost) is a home given under some base too.
	if (homes > 0 || len(w.usedHomes) > 0) && said == 0 {
		return errors.New("cliworker: this volume has homes but no record of the UID base they were given under (.worker/uid-base is gone): start the worker on a new volume")
	}
	return writeDurably(w.stateDir(), w.uidBaseFile(), []byte(strconv.Itoa(base)+"\n"))
}

// Server boots are ordered by the worker alone, never by a clock: the first census of a boot it
// applies gives the boot the next number of a counter that only goes up (.worker/boot-seq, moved
// on durably before the boot is used), and a boot with a higher number is a later server. The
// order lives on the worker's volume, so a restore of the server's data can't take it back. Only
// a census registers a boot: a delayed request of an earlier server can't make its boot the
// latest. Every account record keeps the number of the latest boot when it was made (Gen) and,
// once retired, when it was (RetiredGen): a census retires only accounts made before it was
// registered, and takes back only retirements made before it.
//
// The whole history of boots is kept, never cut (.worker/boots.json, the latest last, written
// whole by a rename; .worker/latest-boot is a copy of the latest, written after it), so a boot the
// worker can't place is a server it never heard from: a later one. If the history is lost or
// damaged, or disagrees with the records, the worker can no longer tell the latest server from an
// earlier one still trying: it is then in the authority-lost state (.worker/authority-lost, kept
// until an operator hands it over). It refuses every census and every deletion, the agent
// browsers' purge too (ErrAuthorityLost), and goes on with everything else for the accounts it
// has. Only an operator leaves the state, by naming the current server's boot with the worker
// stopped (TrustServer, `taskto cli-worker trust-server`): never the first server that sends a
// census.

// trustHookForTest, in tests, runs between a handover's writes and its reading them back.
var trustHookForTest func(Layout)

// ErrAuthorityLost is the worker's answer to a census or a deletion while it can't tell which
// server is the current one (its history of boots was lost): an operator hands it over
// (docs/cli-workers.md). It is cliowner's, so the agent browsers' API carries it too.
var ErrAuthorityLost = cliowner.ErrAuthorityLost

// ErrCensusNeeded is the worker's answer to a deletion before its volume's first census.
var ErrCensusNeeded = cliowner.ErrCensusNeeded

// bootsFile is the history of boots, the latest last.
func (w *Worker) bootsFile() string { return filepath.Join(w.stateDir(), "boots.json") }

// bootSeqFile is the boot counter: the highest number given.
func (w *Worker) bootSeqFile() string { return filepath.Join(w.stateDir(), "boot-seq") }

// latestBootFile is a copy of the latest boot, with its number: its loss with the history's tells
// a volume that had boots from a new one.
func (w *Worker) latestBootFile() string { return filepath.Join(w.stateDir(), "latest-boot") }

// authorityLostFile marks the authority-lost state: it stays until an operator hands the worker
// over (TrustServer).
func (w *Worker) authorityLostFile() string { return filepath.Join(w.stateDir(), "authority-lost") }

// bootEntry is a registered boot, with its number.
type bootEntry struct {
	Boot string `json:"boot"`
	Seq  int64  `json:"seq"`
}

type bootLog struct {
	// Boots is the boots, oldest first.
	Boots []bootEntry `json:"boots"`
	// Count and Sum make the history's completeness part of it: how many boots it has, and the
	// sha256 of their JSON. A history that lost an entry, or had one changed, doesn't match them.
	Count int    `json:"count"`
	Sum   string `json:"sum"`
}

// historySum is the sha256 of boots' JSON, in hex.
func historySum(boots []bootEntry) string {
	b, _ := json.Marshal(boots) //nolint:errchkjson // strings and numbers only
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// bootState is what the worker's volume says of the server boots: the history, the counter, and
// whether the worker can trust them (lost: why not).
type bootState struct {
	boots []bootEntry
	seq   int64
	lost  string
}

// readBootState reads the boots of the volume under layout. A volume with nothing of boots (a new
// one) is not lost; one with a history that is missing, damaged, behind its records or apart from
// its copy of the latest, or with the authority-lost mark, is.
func readBootState(w *Worker) (bootState, error) {
	var st bootState
	if _, err := os.Lstat(w.authorityLostFile()); err == nil {
		st.lost = "an operator hasn't handed the worker over since it lost its history of boots"
	} else if !errors.Is(err, fs.ErrNotExist) {
		return st, err
	}
	hist, haveHist, err := readIfThere(w.bootsFile())
	if err != nil {
		return st, err
	}
	latestRaw, haveLatest, err := readIfThere(w.latestBootFile())
	if err != nil {
		return st, err
	}
	seq, seqThere, seqValid := w.readSeq(w.bootSeqFile())
	if seqThere && !seqValid {
		st.lost = cmp.Or(st.lost, "its boot counter (.worker/boot-seq) is damaged")
		seq = 0
	}
	recs, err := w.records()
	if err != nil {
		return st, err
	}
	var gen int64
	for _, r := range recs {
		gen = max(gen, r.Gen, r.RetiredGen)
	}
	st.seq = max(seq, gen)
	fresh := !haveHist && !haveLatest && !seqThere && gen == 0
	// A volume with boots, accounts or homes on it is some installation's: without its binding,
	// the worker can't tell whose, and the first census to come would take it.
	if _, bound, err := readIfThere(w.installationFile()); err != nil {
		return st, err
	} else if !bound && (!fresh || len(recs) > 0 || w.hasHomes()) {
		st.lost = cmp.Or(st.lost, "its installation (.worker/installation) is gone from a volume that has boots, accounts or homes")
	}
	switch {
	case fresh:
		return st, nil
	case !haveHist:
		st.lost = cmp.Or(st.lost, "its history of boots (.worker/boots.json) is gone")
		return st, nil
	}
	l, damaged := decodeHistory(hist)
	if damaged != "" {
		st.lost = cmp.Or(st.lost, damaged)
		return st, nil
	}
	st.boots = l.Boots
	last := l.Boots[len(l.Boots)-1]
	st.seq = max(st.seq, last.Seq)
	if gen > last.Seq {
		st.lost = cmp.Or(st.lost, "its history of boots is behind its accounts' records")
	}
	if haveLatest {
		var e bootEntry
		if json.Unmarshal(latestRaw, &e) != nil || !slices.Contains(l.Boots, e) {
			st.lost = cmp.Or(st.lost, "its copy of the latest boot (.worker/latest-boot) isn't one of its history")
		}
	}
	return st, nil
}

// decodeHistory reads a history of boots, or says why it can't be trusted.
func decodeHistory(b []byte) (bootLog, string) {
	var l bootLog
	switch err := json.Unmarshal(b, &l); {
	case err != nil:
		return l, "its history of boots (.worker/boots.json) is damaged: " + err.Error()
	case len(l.Boots) == 0:
		return l, "its history of boots (.worker/boots.json) is empty"
	case l.Count != len(l.Boots) || l.Sum != historySum(l.Boots):
		return l, "its history of boots (.worker/boots.json) isn't whole: its count or its sum doesn't match its boots"
	}
	seen := map[string]bool{}
	for i, e := range l.Boots {
		switch {
		case !cliowner.IsIdentity(e.Boot) || e.Seq <= 0 || !validSeq(e.Seq):
			return l, "its history of boots (.worker/boots.json) has an entry that isn't a boot"
		case seen[e.Boot]:
			return l, "its history of boots (.worker/boots.json) has a boot twice"
		case i > 0 && e.Seq <= l.Boots[i-1].Seq:
			return l, "its history of boots (.worker/boots.json) isn't in order"
		}
		seen[e.Boot] = true
	}
	return l, ""
}

// hasHomes reports whether the layout's root has a home's directory.
func (w *Worker) hasHomes() bool {
	names, err := os.ReadDir(w.cfg.Layout.Root)
	if err != nil {
		return false
	}
	for _, n := range names {
		if _, ok := homeOfName(n.Name()); ok {
			return true
		}
	}
	return false
}

// readIfThere reads path, and says whether it was there: only its absence isn't an error.
func readIfThere(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the worker's own files, in its own directory
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

// loadBoots reads the boots at start. A lost state is recorded (the mark) and said loudly; a copy
// of the latest that is behind the history (a stop between the two writes) or gone is written
// again, the history being whole.
func (w *Worker) loadBoots() error {
	st, err := readBootState(w)
	if err != nil {
		return err
	}
	w.boots, w.bootSeq = st.boots, st.seq
	if st.lost != "" && w.cfg.OneServer {
		// One server only, and it is this one: its next census sets the history again.
		slog.Warn("cliworker: the history of server boots was lost: the next census sets it again (one server only)", "why", st.lost)
		if err := os.Remove(w.authorityLostFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		st.lost = ""
	}
	if st.lost != "" {
		w.authorityLost = st.lost
		if err := writeDurably(w.stateDir(), w.authorityLostFile(), []byte(st.lost+"\n")); err != nil {
			return err
		}
		slog.Error("cliworker: AUTHORITY LOST: this worker can't tell the current server from an earlier one, so it refuses every census and every deletion until an operator hands it over (taskto cli-worker trust-server, docs/cli-workers.md)", "why", st.lost)
		return nil
	}
	if n := len(w.boots); n > 0 {
		if err := w.writeLatest(w.boots[n-1]); err != nil {
			return err
		}
	}
	if n, there, valid := w.readSeq(w.bootSeqFile()); there && !valid || n < w.bootSeq {
		return writeDurably(w.stateDir(), w.bootSeqFile(), []byte(strconv.FormatInt(w.bootSeq, 10)+"\n"))
	}
	return nil
}

func (w *Worker) writeLatest(e bootEntry) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return writeDurably(w.stateDir(), w.latestBootFile(), b)
}

// maxBootSeq bounds the boot numbers: a server start each, so never reached; a number above it is
// a damaged counter or record, and adding one to it can't overflow.
const maxBootSeq = 1 << 40

// validSeq reports whether n can be a boot number (0: none yet).
func validSeq(n int64) bool { return n >= 0 && n <= maxBootSeq }

// readSeq is the counter at path, whether it is there, and whether it is a boot number.
func (w *Worker) readSeq(path string) (n int64, there, valid bool) {
	b, err := os.ReadFile(path) //nolint:gosec // G304: the worker's own counter, in its own directory
	if err != nil {
		return 0, false, false
	}
	n, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, true, err == nil && validSeq(n)
}

// AuthorityLost says why the worker is in the authority-lost state, "" when it isn't.
func (w *Worker) AuthorityLost() string {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	return w.authorityLost
}

// latestSeq is the number of the latest registered boot (0 before any): a new record's Gen.
func (w *Worker) latestSeq() int64 {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	return w.bootSeq
}

// latestBoot is the latest registered boot, "" before any.
func (w *Worker) latestBoot() string {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	if n := len(w.boots); n > 0 {
		return w.boots[n-1].Boot
	}
	return ""
}

// reserveBoot is the number boot's census takes as it arrives, before it waits for the census
// lock: the boot's own if it is registered, the one a census of it that is waiting already took,
// or the next. Numbers given this way only go up; one a census never registers (it was refused,
// or its caller went) is skipped.
func (w *Worker) reserveBoot(boot string) int64 {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	for _, b := range w.boots {
		if b.Boot == boot {
			return b.Seq
		}
	}
	if r, ok := w.reserved[boot]; ok {
		r.waiting++
		w.reserved[boot] = r
		return r.seq
	}
	w.reservedTop = max(w.reservedTop, w.bootSeq) + 1
	if w.reserved == nil {
		w.reserved = map[string]reservation{}
	}
	w.reserved[boot] = reservation{seq: w.reservedTop, waiting: 1}
	return w.reservedTop
}

// unreserveBoot lets go of a census's reservation once it is done.
func (w *Worker) unreserveBoot(boot string) {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	if r, ok := w.reserved[boot]; ok {
		if r.waiting--; r.waiting <= 0 {
			delete(w.reserved, boot)
		} else {
			w.reserved[boot] = r
		}
	}
}

// reservation is a number a waiting census of a boot took, and how many censuses of it wait.
type reservation struct {
	seq     int64
	waiting int
}

// registerCensus registers c's boot with n, the number its census took as it arrived
// (reserveBoot), or refuses it: ErrAuthorityLost while the worker can't tell the current server,
// ErrStaleBoot for a boot registered before the latest one, or whose census arrived after a later
// one was registered. It returns the boot's number. The caller holds w.censusMu.
func (w *Worker) registerCensus(c Census, n int64) (int64, error) {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	if w.authorityLost != "" {
		return 0, ErrAuthorityLost
	}
	if err := w.sameInstallation(c.Installation, true); err != nil {
		return 0, err
	}
	for i, b := range w.boots {
		if b.Boot == c.Boot {
			if i == len(w.boots)-1 {
				return b.Seq, nil
			}
			return 0, ErrStaleBoot
		}
	}
	if n <= w.bootSeq {
		return 0, ErrStaleBoot // a census that arrived after this one was registered first
	}
	if !validSeq(n) {
		return 0, errors.New("cliworker: no boot number is left")
	}
	return n, w.register(c.Boot, n)
}

// register makes boot, with number seq, the latest: the counter first (a number is never given
// twice, a stop in between only skipping one), then the history, whole, then its copy of the
// latest. The caller holds w.bootsMu.
func (w *Worker) register(boot string, seq int64) error {
	if err := writeDurably(w.stateDir(), w.bootSeqFile(), []byte(strconv.FormatInt(seq, 10)+"\n")); err != nil {
		return err
	}
	w.bootSeq = seq
	latest := bootEntry{Boot: boot, Seq: seq}
	boots := append(append([]bootEntry(nil), w.boots...), latest)
	b, err := json.Marshal(bootLog{Boots: boots, Count: len(boots), Sum: historySum(boots)})
	if err != nil {
		return err
	}
	if err := writeDurably(w.stateDir(), w.bootsFile(), b); err != nil {
		return err
	}
	w.boots = boots
	return w.writeLatest(latest)
}

// deletionFence admits a deletion (Forget, a browser purge) only from the latest registered boot:
// one an earlier server sent, delayed past a restore that brought the account back, retires and
// removes nothing (ErrStaleBoot). Before the volume's first census a deletion waits for it
// (ErrCensusNeeded) and writes nothing: a record it made on a volume not yet pinned to an
// installation would read, at the worker's next start, as a used volume that lost its binding, and
// a census read before it could take its retirement back. In the authority-lost state every
// deletion is refused (ErrAuthorityLost). The caller holds w.censusMu.
func (w *Worker) deletionFence(a cliowner.Account) error {
	if w.AuthorityLost() != "" {
		return ErrAuthorityLost
	}
	w.bootsMu.Lock()
	err := w.sameInstallation(a.Installation, false)
	w.bootsMu.Unlock()
	if err != nil {
		return err
	}
	if w.latestSeq() == 0 {
		return ErrCensusNeeded
	}
	if a.Boot == w.latestBoot() {
		return nil
	}
	return ErrStaleBoot
}

// TrustServer hands the worker's volume under layout over to the server with boot, ending the
// authority-lost state, and, with installation, to that installation (a move to another data
// directory): an operator's step (`taskto cli-worker trust-server`), with the worker stopped (it
// takes the volume's lock) and every server but the current one stopped too. The boot is
// registered as the latest, after everything the volume names (the history that is left, its
// counter, its records), and the mark is removed. A boot the history no longer has is, from then
// on, a server the worker never heard from: that is why every other server must be stopped.
func TrustServer(layout Layout, boot, installation string) error {
	if !cliowner.IsIdentity(boot) {
		return badRequest{errors.New("invalid boot: 32 hexadecimal characters, as the server logs it")}
	}
	if installation != "" && !cliowner.IsIdentity(installation) {
		return badRequest{errors.New("invalid installation: 32 hexadecimal characters, as the server logs it")}
	}
	w := &Worker{cfg: Config{Layout: layout}, accounts: map[string]accountRecord{}}
	for _, dir := range []string{w.stateDir(), w.accountsDir()} {
		if err := mkdirDurable(dir); err != nil {
			return err
		}
	}
	lock, err := lockfile.Lock(filepath.Join(w.stateDir(), "lock"))
	if err != nil {
		if errors.Is(err, lockfile.ErrLocked) {
			return fmt.Errorf("the CLI worker is running on %s: stop it first: %w", layout.Root, err)
		}
		return err
	}
	defer lock.Close()
	// A volume whose installation binding is gone (or never was, with boots or accounts on it) is
	// handed over to a named installation only: the operator says whose it is.
	if _, there, err := readIfThere(w.installationFile()); err != nil {
		return err
	} else if !there && installation == "" {
		return badRequest{errors.New("this volume has no installation: name the server's (-installation, as Settings → About shows it and the server logs it)")}
	}
	st, err := readBootState(w)
	if err != nil {
		return err
	}
	// The counter is made again from what the volume names, whatever it says itself: the history
	// that is left and the records (readBootState leaves a damaged counter out).
	w.boots, w.bootSeq = st.boots, st.seq
	if n := len(w.boots); n > 0 {
		w.bootSeq = max(w.bootSeq, w.boots[n-1].Seq)
	}
	if !validSeq(w.bootSeq + 1) {
		return errors.New("the volume's boot numbers are damaged beyond repair: start the worker on a new volume")
	}
	// What is left of the history stays, but not the boot being trusted: it is the latest now.
	w.boots = slices.DeleteFunc(w.boots, func(e bootEntry) bool { return e.Boot == boot })
	w.bootsMu.Lock()
	err = w.register(boot, w.bootSeq+1)
	w.bootsMu.Unlock()
	if err != nil {
		return err
	}
	if installation != "" {
		if err := writeDurably(w.stateDir(), w.installationFile(), []byte(installation+"\n")); err != nil {
			return err
		}
	}
	if err := os.Remove(w.authorityLostFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := syncDir(w.stateDir()); err != nil {
		return err
	}
	if trustHookForTest != nil {
		trustHookForTest(layout)
	}
	// Read back as a starting worker reads it: success only if that worker trusts this boot.
	check := &Worker{cfg: Config{Layout: layout}, accounts: map[string]accountRecord{}}
	got, err := readBootState(check)
	if err != nil {
		return err
	}
	if err := check.loadInstallation(); err != nil {
		return err
	}
	switch n := len(got.boots); {
	case got.lost != "":
		return fmt.Errorf("the handover didn't take: the worker would still not trust it (%s)", got.lost)
	case n == 0 || got.boots[n-1].Boot != boot:
		return errors.New("the handover didn't take: the latest boot isn't the one handed over to")
	case check.installation == "" || (installation != "" && check.installation != installation):
		return errors.New("the handover didn't take: the volume's installation isn't the one handed over to")
	}
	slog.Info("cliworker: handed over to the server with this boot", "boot", boot, "installation", check.installation, "seq", w.bootSeq)
	return nil
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
	if err := ctx.Err(); err != nil {
		return err // its caller has gone while it waited: nothing is done
	}
	if err := w.deletionFence(a); err != nil {
		return err
	}
	if err := w.retire(a.Identity, a.User); err != nil {
		return fmt.Errorf("recording that the account is gone: %w", err)
	}
	return do()
}
