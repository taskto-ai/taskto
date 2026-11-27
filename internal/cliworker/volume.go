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

var ErrStaleBoot = errors.New("the request comes from an earlier start of the server")

var syncedForTest func(path string)

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

type Authority struct {
	Lost         string `json:"lost,omitempty"`
	Installation string `json:"installation,omitempty"`
}

func (w *Worker) Authority() Authority {
	return Authority{Lost: w.AuthorityLost(), Installation: w.Installation()}
}

func (w *Worker) Installation() string {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	return w.installation
}

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
		return nil
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

func (w *Worker) readCounter() (int64, bool) {
	b, err := os.ReadFile(w.counterFile())
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, err == nil && n >= firstHome && n <= maxHome
}

func (w *Worker) uidBaseFile() string { return filepath.Join(w.stateDir(), "uid-base") }

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
	if (homes > 0 || len(w.usedHomes) > 0) && said == 0 {
		return errors.New("cliworker: this volume has homes but no record of the UID base they were given under (.worker/uid-base is gone): start the worker on a new volume")
	}
	return writeDurably(w.stateDir(), w.uidBaseFile(), []byte(strconv.Itoa(base)+"\n"))
}

var trustHookForTest func(Layout)

var ErrAuthorityLost = cliowner.ErrAuthorityLost

var ErrCensusNeeded = cliowner.ErrCensusNeeded

func (w *Worker) bootsFile() string { return filepath.Join(w.stateDir(), "boots.json") }

func (w *Worker) bootSeqFile() string { return filepath.Join(w.stateDir(), "boot-seq") }

func (w *Worker) latestBootFile() string { return filepath.Join(w.stateDir(), "latest-boot") }

func (w *Worker) authorityLostFile() string { return filepath.Join(w.stateDir(), "authority-lost") }

type bootEntry struct {
	Boot string `json:"boot"`
	Seq  int64  `json:"seq"`
}

type bootLog struct {
	Boots []bootEntry `json:"boots"`
	Count int         `json:"count"`
	Sum   string      `json:"sum"`
}

func historySum(boots []bootEntry) string {
	b, _ := json.Marshal(boots)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type bootState struct {
	boots []bootEntry
	seq   int64
	lost  string
}

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

func readIfThere(path string) ([]byte, bool, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	return b, err == nil, err
}

func (w *Worker) loadBoots() error {
	st, err := readBootState(w)
	if err != nil {
		return err
	}
	w.boots, w.bootSeq = st.boots, st.seq
	if st.lost != "" && w.cfg.OneServer {
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

const maxBootSeq = 1 << 40

func validSeq(n int64) bool { return n >= 0 && n <= maxBootSeq }

func (w *Worker) readSeq(path string) (n int64, there, valid bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false, false
	}
	n, err = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return n, true, err == nil && validSeq(n)
}

func (w *Worker) AuthorityLost() string {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	return w.authorityLost
}

func (w *Worker) latestSeq() int64 {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	return w.bootSeq
}

func (w *Worker) latestBoot() string {
	w.bootsMu.Lock()
	defer w.bootsMu.Unlock()
	if n := len(w.boots); n > 0 {
		return w.boots[n-1].Boot
	}
	return ""
}

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

type reservation struct {
	seq     int64
	waiting int
}

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
		return 0, ErrStaleBoot
	}
	if !validSeq(n) {
		return 0, errors.New("cliworker: no boot number is left")
	}
	return n, w.register(c.Boot, n)
}

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
	if _, there, err := readIfThere(w.installationFile()); err != nil {
		return err
	} else if !there && installation == "" {
		return badRequest{errors.New("this volume has no installation: name the server's (-installation, as Settings → About shows it and the server logs it)")}
	}
	st, err := readBootState(w)
	if err != nil {
		return err
	}
	w.boots, w.bootSeq = st.boots, st.seq
	if n := len(w.boots); n > 0 {
		w.bootSeq = max(w.bootSeq, w.boots[n-1].Seq)
	}
	if !validSeq(w.bootSeq + 1) {
		return errors.New("the volume's boot numbers are damaged beyond repair: start the worker on a new volume")
	}
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

func (w *Worker) FenceDeletion(ctx context.Context, a cliowner.Account, do func() error) error {
	if !cliowner.IsIdentity(a.Identity) {
		return badRequest{errors.New("invalid account")}
	}
	w.censusMu.Lock()
	defer w.censusMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.deletionFence(a); err != nil {
		return err
	}
	if err := w.retire(a.Identity, a.User); err != nil {
		return fmt.Errorf("recording that the account is gone: %w", err)
	}
	return do()
}
