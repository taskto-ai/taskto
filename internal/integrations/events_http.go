package integrations

import (
	"database/sql"
	"net/http"
	"strconv"

	"taskto.ai/taskto/internal/apierr"
)

// The admin's view of an integration's events (docs/api.md#integration-events): what the webhook
// route stored and what the worker made of it, and a retry for the events that failed. An event has
// no record data in it, no body and no token of ours: its kind, its amount, its state and the code
// that says why it is not applied. An admin sees that a payment of 120.00 EUR was not applied and
// why; which app or record it was for is not shown, since an admin has no read access to a private
// app's records.

// EventView is a stored event as the admins are shown it.
type EventView struct {
	ID int64 `json:"id"`
	// Kind is the normalized event (payment.succeeded...), Source where it came from (webhook).
	Kind   string `json:"kind"`
	Source string `json:"source"`
	// State is pending, running, applied, ignored or failed. Code is why an ignored or failed event
	// was not applied (the codes of integration_event_*), "" for one that was.
	State string `json:"state"`
	Code  string `json:"code,omitempty"`
	// AmountMinor is in the currency's smallest unit; of a refund, what has been refunded so far.
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
	// Livemode says whether the event is of live money.
	Livemode    bool    `json:"livemode"`
	Attempts    int     `json:"attempts"`
	OccurredAt  string  `json:"occurred_at"`
	ReceivedAt  string  `json:"received_at"`
	ProcessedAt *string `json:"processed_at"`
	// Retriable is whether a retry is possible: a failed event, once what is wrong is fixed.
	Retriable bool `json:"retriable"`
}

// EventList is a page of events, newest first. NextBefore is the id to give as before for the next
// page, 0 when there is none.
type EventList struct {
	Events     []EventView `json:"events" apitypes:"nonnull"`
	NextBefore int64       `json:"next_before"`
}

const (
	defaultEventPage = 50
	maxEventPage     = 100
)

var eventStates = map[string]bool{EventPending: true, EventRunning: true, EventApplied: true, EventIgnored: true, EventFailed: true}

// handleEvents lists the integration's events: ?state= narrows them to one state, ?limit= bounds the
// page and ?before= continues after an earlier page.
func (s *Service) handleEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// The integration must be this workspace's: another workspace's id is not found, as everywhere.
	if _, err := s.store.getSnapshot(ctx, workspaceOf(r), id); err != nil {
		fail(w, r, err)
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if state != "" && !eventStates[state] {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "state is one of pending, running, applied, ignored, failed")
		return
	}
	limit := defaultEventPage
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxEventPage {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "limit is a number from 1 to "+strconv.Itoa(maxEventPage))
			return
		}
		limit = n
	}
	var before int64
	if v := q.Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "before is an event's id")
			return
		}
		before = n
	}
	query := `SELECT id, kind, source, state, coalesce(error_code, ''), amount_minor, currency, livemode, attempts, occurred_at, received_at, processed_at
		FROM integration_events WHERE integration_id = ?`
	args := []any{id}
	if state != "" {
		query += ` AND state = ?`
		args = append(args, state)
	}
	if before > 0 {
		query += ` AND id < ?`
		args = append(args, before)
	}
	query += ` ORDER BY id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		serverError(w, r, err)
		return
	}
	defer rows.Close()
	out := EventList{Events: []EventView{}}
	for rows.Next() {
		var v EventView
		if err := rows.Scan(&v.ID, &v.Kind, &v.Source, &v.State, &v.Code, &v.AmountMinor, &v.Currency, &v.Livemode, &v.Attempts, &v.OccurredAt, &v.ReceivedAt, &v.ProcessedAt); err != nil {
			serverError(w, r, err)
			return
		}
		v.Retriable = v.State == EventFailed
		out.Events = append(out.Events, v)
	}
	if err := rows.Err(); err != nil {
		serverError(w, r, err)
		return
	}
	if len(out.Events) > limit {
		out.Events = out.Events[:limit]
		out.NextBefore = out.Events[limit-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

// handleRetryEvent puts a failed event back to wait for the worker, with its tries counted anew: for
// once what was wrong (an approval, a switched-off integration, an app that was offline) is fixed.
// Only a failed event of this integration, in this workspace, is retried; applying it again is safe
// whatever happened to it before (apply.go).
func (s *Service) handleRetryEvent(w http.ResponseWriter, r *http.Request) {
	id, ok := integrationID(w, r)
	if !ok {
		return
	}
	event, err := strconv.ParseInt(r.PathValue("event"), 10, 64)
	if err != nil {
		writeError(w, http.StatusNotFound, apierr.CodeIntegrationEventNotFound, "not found")
		return
	}
	ctx := r.Context()
	if _, err := s.store.getSnapshot(ctx, workspaceOf(r), id); err != nil {
		fail(w, r, err)
		return
	}
	at := s.store.stamp(s.now())
	res, err := s.store.db.ExecContext(ctx, `UPDATE integration_events
		SET state = 'pending', attempts = 0, lease_until = NULL, next_attempt_at = ?, error_code = NULL, error_params = NULL, processed_at = NULL
		WHERE id = ? AND integration_id = ? AND state = 'failed'`, at, event, id)
	if err != nil {
		serverError(w, r, err)
		return
	}
	if n, err := res.RowsAffected(); err != nil {
		serverError(w, r, err)
		return
	} else if n == 0 {
		// Not this integration's, or not failed: which of the two is not told apart for an id that is
		// not its own.
		var state string
		err := s.store.db.QueryRowContext(ctx, `SELECT state FROM integration_events WHERE id = ? AND integration_id = ?`, event, id).Scan(&state)
		switch {
		case err == sql.ErrNoRows:
			writeError(w, http.StatusNotFound, apierr.CodeIntegrationEventNotFound, "not found")
		case err != nil:
			serverError(w, r, err)
		default:
			writeError(w, http.StatusConflict, apierr.CodeIntegrationEventNotRetriable, "only a failed event is retried")
		}
		return
	}
	s.poke()
	var v EventView
	err = s.store.db.QueryRowContext(ctx, `SELECT id, kind, source, state, coalesce(error_code, ''), amount_minor, currency, livemode, attempts, occurred_at, received_at, processed_at
		FROM integration_events WHERE id = ?`, event).
		Scan(&v.ID, &v.Kind, &v.Source, &v.State, &v.Code, &v.AmountMinor, &v.Currency, &v.Livemode, &v.Attempts, &v.OccurredAt, &v.ReceivedAt, &v.ProcessedAt)
	if err != nil {
		serverError(w, r, err)
		return
	}
	v.Retriable = v.State == EventFailed
	writeJSON(w, http.StatusOK, v)
}
