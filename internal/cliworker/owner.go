package cliworker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Who a user's directory belongs to. A user number names an account only within one history of
// the server's database: a restore goes back in it and gives the numbers after the backup again,
// while the worker's own directory (a volume of its own in container mode) is not part of the
// backup. So the worker never decides by number alone, nor by any clock: the server gives each
// account an identity that never changes and is never given to another (users.cli_identity), and
// the worker records it as the owner of the number's directory the first time the server
// introduces the user (Introduce), before anything of theirs is made. Nothing starts for a user
// without that record (ErrUnknownOwner), and a directory whose recorded owner isn't the account
// introduced is removed first, with everything of it, the way Forget removes one.
//
// A directory from before the worker recorded owners has none: Reconcile adopts it for the live
// user of its number only when the server says no restore happened since (Census.Restored), and
// removes it otherwise.

// ErrUnknownOwner: the server hasn't introduced the user to the worker (Introduce), so nothing
// is made or run for them yet.
var ErrUnknownOwner = errors.New("the CLI worker doesn't know this user's account yet")

// identityPattern is an account's identity and a server's boot: 32 lowercase hex digits.
var identityPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ownerRecord is who a user's directory belongs to: the account's identity, and the boot of the
// server that introduced it.
type ownerRecord struct {
	identity, boot string
}

func (w *Worker) ownerFile(user int64) string {
	return filepath.Join(w.stateDir(), "owner-u"+strconv.FormatInt(user, 10))
}

// owner reads user's owner record. A record that can't be read or makes no sense is an error:
// only its absence says the directory has no owner.
func (w *Worker) owner(user int64) (ownerRecord, bool, error) {
	b, err := os.ReadFile(w.ownerFile(user))
	if errors.Is(err, fs.ErrNotExist) {
		return ownerRecord{}, false, nil
	}
	if err != nil {
		return ownerRecord{}, false, err
	}
	identity, boot, ok := strings.Cut(strings.TrimSpace(string(b)), " ")
	if !ok || !identityPattern.MatchString(identity) || !identityPattern.MatchString(boot) {
		return ownerRecord{}, false, fmt.Errorf("the owner record of user %d is damaged", user)
	}
	return ownerRecord{identity: identity, boot: boot}, true, nil
}

// owned reports whether user has an owner record: things may start for them.
func (w *Worker) owned(user int64) bool {
	_, ok, err := w.owner(user)
	return ok && err == nil
}

// setOwner records r as the owner of user's directory, durably, before anything of theirs is made.
func (w *Worker) setOwner(user int64, r ownerRecord) error {
	if err := os.MkdirAll(w.stateDir(), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(w.stateDir(), ".owner-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) //nolint:errcheck // gone after the rename
	if _, err := tmp.WriteString(r.identity + " " + r.boot + "\n"); err != nil {
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
	if err := os.Rename(tmp.Name(), w.ownerFile(user)); err != nil {
		return err
	}
	return syncDir(w.stateDir())
}

func syncDir(path string) error {
	d, err := os.Open(path) //nolint:gosec // G304: the worker's own directories
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// Introduce tells the worker that user is the account with identity, for the server started as
// boot, before anything is asked of it for them. If the user's directory belongs to someone else
// (another identity, or none recorded: a history a restore went back from), it is removed first,
// the way Forget removes one; then the account is recorded as its owner. Introducing the owner
// again changes nothing. A forgotten user (a deleted account, or one whose old directory couldn't
// be removed yet) is ErrForgotten.
func (w *Worker) Introduce(ctx context.Context, user int64, identity, boot string) error {
	if user <= 0 || !identityPattern.MatchString(identity) || !identityPattern.MatchString(boot) {
		return badRequest{errors.New("invalid introduction")}
	}
	sc := scope{user: user, all: true}
	end, err := w.beginGate(ctx, sc)
	if err != nil {
		return err
	}
	defer end()
	if w.isForgotten(user) {
		return ErrForgotten
	}
	rec, ok, err := w.owner(user)
	if err != nil {
		return err
	}
	if ok && rec.identity == identity {
		return nil
	}
	if ok || w.hasDir(user) {
		if err := w.removeUser(ctx, user); err != nil {
			return err
		}
	}
	return w.setOwner(user, ownerRecord{identity: identity, boot: boot})
}

func (w *Worker) hasDir(user int64) bool {
	_, err := os.Lstat(w.cfg.Layout.User(user).Root)
	return !errors.Is(err, fs.ErrNotExist)
}

// removeUser removes everything the worker keeps for user, once the caller holds a gate that
// covers all their accounts (nothing new starts for them). In order:
//
//  1. their agent browsers are stopped and waited for: the profile is in the directory, and the
//     browser's processes aren't the ones quiesce stops;
//  2. what runs for them (turns, sign-ins, helpers, the process groups those started) is stopped
//     and waited for (quiesce), and with isolation every process of their Unix user, a process a
//     CLI detached into a group of its own too, so none of the number's is left for whoever gets
//     it next;
//  3. the directory is taken away from its path at once (detach), into the worker's own state
//     directory, where no user can reach it, and deleted there as the worker (removeTree);
//  4. the owner record, and what the worker remembers of them (their sign-ins and finished
//     turns, with the permission questions those held), go.
//
// On an error nothing of a later step is done; the caller keeps the user refused, and it can be
// tried again.
func (w *Worker) removeUser(ctx context.Context, user int64) error {
	// Nothing is done for a caller that has given up.
	if err := ctx.Err(); err != nil {
		return err
	}
	if b := w.browserStopper(); b != nil {
		if err := b.StopAll(ctx, user); err != nil {
			return fmt.Errorf("stopping the user's browsers: %w", err)
		}
	}
	if err := w.quiesce(ctx, scope{user: user, all: true}); err != nil {
		return err
	}
	if iso := w.cfg.Isolation; iso != nil {
		if err := killUID(iso.uidOf(user), uidWait); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	trash, err := w.detach(user)
	if err != nil {
		return fmt.Errorf("taking the user's directory away: %w", err)
	}
	// With isolation, the next helper for the number makes a new directory (ensureUser).
	w.dropUnixUser(user)
	if trash != "" {
		if err := removeTree(trash, w.cfg.Isolation != nil); err != nil {
			return fmt.Errorf("removing the user's old directory: %w", err)
		}
	}
	if err := os.Remove(w.ownerFile(user)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	w.dropState(user)
	return nil
}

// uidWait is how long a removal waits for the processes of a user's Unix user to end.
const uidWait = 5 * time.Second

// dropState lets go of what the worker remembers of user: their sign-ins and their turns, which
// quiesce has ended, with the permission questions they held.
func (w *Worker) dropState(user int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for k := range w.logins {
		if k.user == user {
			delete(w.logins, k)
		}
	}
	for id, t := range w.turns {
		if t.spec.User == user {
			delete(w.turns, id)
		}
	}
}

// trashDir is where detached directories wait to be deleted: in the worker's own state
// directory, which no user can enter.
func (w *Worker) trashDir() string { return filepath.Join(w.stateDir(), "trash") }

// detach takes user's directory away from its path into the trash, and returns where it went (""
// when there was none). With isolation the directory is the user's: the worker takes it over first
// (CAP_CHOWN), since moving a directory to another parent writes its "..". No process of the user's
// runs by then.
func (w *Worker) detach(user int64) (string, error) {
	root := w.cfg.Layout.User(user).Root
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(w.trashDir(), 0o700); err != nil {
		return "", err
	}
	if info.IsDir() {
		if err := takeOver(root, w.cfg.Isolation != nil); err != nil {
			return "", err
		}
	}
	var r [8]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	trash := filepath.Join(w.trashDir(), "u"+strconv.FormatInt(user, 10)+"-"+hex.EncodeToString(r[:]))
	if err := os.Rename(root, trash); err != nil {
		return "", err
	}
	if err := syncDir(w.cfg.Layout.Root); err != nil {
		return "", err
	}
	return trash, nil
}

// takeOver makes directory path the worker's own (with chown, when the users are Unix users of
// their own) and open to it alone. The caller checked that path is a directory in a directory
// only the worker writes, so nothing can have put a link in its place.
func takeOver(path string, chown bool) error {
	if chown {
		if err := os.Lchown(path, os.Getuid(), os.Getgid()); err != nil {
			return err
		}
	}
	return os.Chmod(path, 0o700) //nolint:gosec // G302: a directory needs x; 0700 is owner-only
}

// removeTree deletes path and everything under it, as the worker: each directory is taken over
// before it is read, so a read-only one (an archive's, a tool's) is no obstacle, and a link is
// removed, never followed. path is in a directory only the worker writes, and so, as it goes, is
// everything it looks at.
func removeTree(path string, chown bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.Remove(path)
	}
	if err := takeOver(path, chown); err != nil {
		return err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := removeTree(filepath.Join(path, e.Name()), chown); err != nil {
			return err
		}
	}
	return os.Remove(path)
}

// sweepTrash deletes the directories detached earlier that couldn't be deleted then, and returns
// the users of those it still can't delete.
func (w *Worker) sweepTrash() (map[int64]error, error) {
	entries, err := os.ReadDir(w.trashDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	failed := map[int64]error{}
	for _, e := range entries {
		if err := removeTree(filepath.Join(w.trashDir(), e.Name()), w.cfg.Isolation != nil); err != nil {
			id, _, _ := strings.Cut(strings.TrimPrefix(e.Name(), "u"), "-")
			user, perr := strconv.ParseInt(id, 10, 64)
			if perr != nil || user <= 0 {
				return nil, fmt.Errorf("removing %s from the trash: %w", e.Name(), err)
			}
			failed[user] = err
		}
	}
	return failed, nil
}
