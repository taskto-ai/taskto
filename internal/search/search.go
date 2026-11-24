// Package search is the knowledge base's search index: full-text search (SQLite FTS5) over
// the path, title and text of every space's documents, and, in a workspace that has chosen
// an embeddings model, semantic search over their chunks, the two ranked together.
//
// Git is the truth. Each space's index records the commit of main it reflects and catches
// up from how main differs from it (knowledge.Store.Changes): after every change
// (knowledge.Store.OnChange, in the background), at startup, and before every search, so
// neither a crash nor a change made outside the server leaves results behind main.
//
// Isolation: the index is per space, and every query names the spaces it may look in: the
// one access.Checker.RequireSpace let the caller read, or the caller's
// access.Checker.Readable spaces for a workspace-wide search. Indexing reads every space,
// as the server; searching never reads a space the caller can't.
//
// Board cards are searched too (cards.go), by full text only and apart from documents. The
// boards are their truth, as git is the documents': a search first indexes again the cards
// of the spaces it looks in that changed, and the same isolation holds.
//
// Embeddings come from the workspace's choice of one of its provider accounts (an
// OpenAI-compatible /embeddings endpoint, such as Ollama's), and are made in the
// background: a change never waits for them. Vectors are stored as BLOBs and compared in
// Go, since the pure-Go SQLite driver can't load sqlite-vec, from an in-memory cache of
// them (vectors.go).
package search

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/providers"
)

// EmbedTimeout bounds each call to an embeddings endpoint of the default client.
const EmbedTimeout = 2 * time.Minute

// Options configure an Index. Zero values take the defaults.
type Options struct {
	// HTTPClient calls the embeddings endpoints; its timeout bounds each call. The default
	// allows any address and follows no redirect; the server passes the workspaces' accounts'
	// egress policy's (egress.Policy.Client).
	HTTPClient *http.Client
	// Logger defaults to slog.Default().
	Logger *slog.Logger
	// VectorCache is the most memory, in bytes, the vector cache may hold: 0 means
	// DefaultVectorCache, and a negative one turns the cache off.
	VectorCache int64
}

// Index is the search index of every space's knowledge base.
type Index struct {
	db        *sql.DB
	kb        *knowledge.Store
	providers *providers.Store
	client    *http.Client
	log       *slog.Logger
	now       func() time.Time
	vectors   *vectorCache // nil if off

	// A sync's batch limits, syncBatch and syncBatchBytes; tests lower them, and onBatch,
	// when set, hears of each batch's files and bytes of text.
	batchFiles, batchBytes int
	onBatch                func(files, bytes int)

	locks sync.Map // space ID → chan struct{} of one slot: one sync of a space at a time
	// syncWait is how long a search waits for a sync of a space already running before it
	// answers from what is indexed.
	syncWait time.Duration

	// The spaces changed since the sync worker last looked, and its wake-up. A change only
	// adds its space here: it never waits for the index.
	dirtyMu   sync.Mutex
	dirty     map[int64]access.Space
	syncWake  chan struct{}
	embedWake chan struct{}
}

// New returns the index over kb's repositories, kept in sqlDB, which must be migrated. Its
// embeddings use providers' workspace accounts. It hears of every change to kb from now on;
// Start runs its background work.
func New(sqlDB *sql.DB, kb *knowledge.Store, prov *providers.Store, opts Options) *Index {
	x := &Index{
		db: sqlDB, kb: kb, providers: prov, client: opts.HTTPClient, log: opts.Logger, now: time.Now,
		vectors:    newVectorCache(opts.VectorCache),
		batchFiles: syncBatch, batchBytes: syncBatchBytes, syncWait: searchSyncWait,
		dirty:    map[int64]access.Space{},
		syncWake: make(chan struct{}, 1), embedWake: make(chan struct{}, 1),
	}
	if x.client == nil {
		x.client = egress.Any.Client(EmbedTimeout)
	}
	if x.log == nil {
		x.log = slog.Default()
	}
	kb.OnChange(x.changed)
	return x
}

// Knowledge is the knowledge base the index keeps up with.
func (x *Index) Knowledge() *knowledge.Store { return x.kb }

// changed is the knowledge base's OnChange: the space is synced in the background.
func (x *Index) changed(sp access.Space) {
	x.dirtyMu.Lock()
	x.dirty[sp.ID] = sp
	x.dirtyMu.Unlock()
	wake(x.syncWake)
}

// wake signals a worker without waiting: signals sent while it is busy add up to one.
func wake(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

// sweepEvery is how often the workers look for work nobody signalled: embeddings due for
// a retry, and anything a signal was lost for.
const sweepEvery = time.Minute

// Start runs the index's background work until ctx is done or the returned function is
// called, which waits for it to stop: first syncing every space, then the spaces that
// change, and embedding documents in workspaces with an embeddings model.
func (x *Index) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { x.syncLoop(ctx) })
	wg.Go(func() { x.embedLoop(ctx) })
	return func() {
		cancel()
		wg.Wait()
	}
}

func (x *Index) syncLoop(ctx context.Context) {
	// At startup, every space: a crash, or a change made while the server was down, can
	// have left any of them behind.
	if err := x.syncAll(ctx); err != nil && ctx.Err() == nil {
		x.log.Error("search: catching up at startup", "err", err)
	}
	// And every card: the boards' cards are indexed as a search needs them, and this does
	// it once for all before anyone searches.
	if err := x.syncCards(ctx, ""); err != nil && ctx.Err() == nil {
		x.log.Error("search: indexing cards at startup", "err", err)
	}
	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-x.syncWake:
		case <-tick.C:
		}
		x.dirtyMu.Lock()
		dirty := x.dirty
		x.dirty = map[int64]access.Space{}
		x.dirtyMu.Unlock()
		for _, sp := range dirty {
			if _, err := x.Sync(ctx, sp); err != nil && ctx.Err() == nil {
				x.log.Error("search: syncing a space", "space", sp.ID, "err", err)
			}
		}
	}
}

// syncAll syncs every space.
func (x *Index) syncAll(ctx context.Context) error {
	rows, err := x.db.QueryContext(ctx, `SELECT id, workspace_id FROM spaces ORDER BY id`)
	if err != nil {
		return err
	}
	var spaces []access.Space
	for rows.Next() {
		var sp access.Space
		if err := rows.Scan(&sp.ID, &sp.WorkspaceID); err != nil {
			_ = rows.Close()
			return err
		}
		spaces = append(spaces, sp)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var errs []error
	for _, sp := range spaces {
		if _, err := x.Sync(ctx, sp); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (x *Index) embedLoop(ctx context.Context) {
	tick := time.NewTicker(sweepEvery)
	defer tick.Stop()
	for {
		for {
			n, err := x.embedPending(ctx)
			if err != nil && ctx.Err() == nil {
				x.log.Error("search: embedding documents", "err", err)
			}
			if n == 0 || err != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-x.embedWake:
		case <-tick.C:
		}
	}
}

// lock locks the space's sync, waiting at most until ctx is done, and returns its unlock.
func (x *Index) lock(ctx context.Context, spaceID int64) (unlock func(), err error) {
	m, _ := x.locks.LoadOrStore(spaceID, make(chan struct{}, 1))
	slot := m.(chan struct{})
	select {
	case slot <- struct{}{}:
		return func() { <-slot }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// write runs fn in a transaction. Transactions take the database's write lock when they
// begin (see package db): keep them short, and never call the network inside one.
func (x *Index) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := x.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rbErr := tx.Rollback(); !errors.Is(rbErr, sql.ErrTxDone) {
				err = errors.Join(err, rbErr)
			}
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// timeFormat matches strftime('%Y-%m-%dT%H:%M:%fZ'), the schema's format for times.
const timeFormat = "2006-01-02T15:04:05.000Z"
