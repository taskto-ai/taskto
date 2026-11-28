package search

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/knowledge"
	"taskto.ai/taskto/internal/providers"
)

const EmbedTimeout = 2 * time.Minute

type Options struct {
	HTTPClient  *http.Client
	Logger      *slog.Logger
	VectorCache int64
}

type Index struct {
	db        *sql.DB
	kb        *knowledge.Store
	providers *providers.Store
	client    *http.Client
	log       *slog.Logger
	now       func() time.Time
	vectors   *vectorCache

	batchFiles, batchBytes                                                 int
	onBatch                                                                func(files, bytes int)
	afterPending                                                           func()
	afterEndpoint, beforeStore, afterQueryEndpoint                         func()
	afterEmbedRequest, afterQueryEmbedded, beforeRelease, afterConsentBody func()
	afterCardSync                                                          func()
	afterPlacesRead, betweenPlaceChecks                                    func()
	afterRecordSync                                                        func()
	recordSyncLimit                                                        int
	conversationSyncLimit                                                  int
	conversationPassLimit                                                  int
	conversations                                                          atomic.Pointer[ConversationSource]

	locks    sync.Map
	syncWait time.Duration

	dirtyMu   sync.Mutex
	dirty     map[dirtyKey]access.Space
	syncWake  chan struct{}
	embedWake chan struct{}
	convWake  chan struct{}
}

func New(sqlDB *sql.DB, kb *knowledge.Store, prov *providers.Store, opts Options) *Index {
	x := &Index{
		db: sqlDB, kb: kb, providers: prov, client: opts.HTTPClient, log: opts.Logger, now: time.Now,
		vectors:    newVectorCache(opts.VectorCache),
		batchFiles: syncBatch, batchBytes: syncBatchBytes, syncWait: searchSyncWait, recordSyncLimit: recordSyncMax, conversationSyncLimit: conversationSyncMax, conversationPassLimit: conversationPassMax,
		dirty:    map[dirtyKey]access.Space{},
		syncWake: make(chan struct{}, 1), embedWake: make(chan struct{}, 1), convWake: make(chan struct{}, 1),
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

func (x *Index) Knowledge() *knowledge.Store { return x.kb }

func (x *Index) changed(sp access.Space) {
	x.dirtyMu.Lock()
	x.dirty[dirtyKey{sp.ID, sp.Generation}] = sp
	x.dirtyMu.Unlock()
	wake(x.syncWake)
}

type dirtyKey struct{ id, generation int64 }

func wake(c chan struct{}) {
	select {
	case c <- struct{}{}:
	default:
	}
}

const sweepEvery = time.Minute

func (x *Index) Start(ctx context.Context) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	wg.Go(func() { x.syncLoop(ctx) })
	wg.Go(func() { x.embedLoop(ctx) })
	wg.Go(func() { x.conversationLoop(ctx) })
	return func() {
		cancel()
		wg.Wait()
	}
}

func (x *Index) syncLoop(ctx context.Context) {
	if err := x.syncAll(ctx); err != nil && ctx.Err() == nil {
		x.log.Error("search: catching up at startup", "err", err)
	}
	if err := x.queueCards(ctx); err != nil && ctx.Err() == nil {
		x.log.Error("search: queueing cards at startup", "err", err)
	}
	if err := x.syncCards(ctx, ""); err != nil && ctx.Err() == nil {
		x.log.Error("search: indexing cards at startup", "err", err)
	}
	if err := x.queueRecords(ctx); err != nil && ctx.Err() == nil {
		x.log.Error("search: queueing app records at startup", "err", err)
	}
	if err := x.syncRecords(ctx, ""); err != nil && ctx.Err() == nil {
		x.log.Error("search: indexing app records at startup", "err", err)
	}
	if err := x.queueConversations(ctx); err != nil && ctx.Err() == nil {
		x.log.Error("search: queueing conversations at startup", "err", err)
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
		x.dirty = map[dirtyKey]access.Space{}
		x.dirtyMu.Unlock()
		for _, sp := range dirty {
			if _, err := x.Sync(ctx, sp); err != nil && ctx.Err() == nil {
				x.log.Error("search: syncing a space", "space", sp.ID, "err", err)
			}
		}
		if err := x.syncRecords(ctx, ""); err != nil && ctx.Err() == nil {
			x.log.Error("search: indexing app records", "err", err)
		}
	}
}

func (x *Index) syncAll(ctx context.Context) error {
	rows, err := x.db.QueryContext(ctx, `SELECT id, workspace_id, generation FROM spaces ORDER BY id`)
	if err != nil {
		return err
	}
	var spaces []access.Space
	for rows.Next() {
		var sp access.Space
		if err := rows.Scan(&sp.ID, &sp.WorkspaceID, &sp.Generation); err != nil {
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

func (x *Index) write(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	tx, err := x.db.BeginTx(context.WithoutCancel(ctx), nil)
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
	if err := ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

const timeFormat = "2006-01-02T15:04:05.000Z"
