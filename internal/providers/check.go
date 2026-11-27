package providers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
)

const (
	checkTimeout       = 10 * time.Second
	egressCheckTimeout = 5 * time.Second
	maxModelsReply     = 4 << 20
)

type Check struct {
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	Code    apierr.Code   `json:"code,omitempty"`
	Params  apierr.Params `json:"params,omitempty"`
	Missing []string      `json:"missing,omitempty"`
}

func failed(code apierr.Code, msg string, params apierr.Params) Check {
	return Check{Error: msg, Code: code, Params: params}
}

func checkClient(policy egress.Policy) *http.Client { return policy.Client(checkTimeout) }

type Egress struct {
	Workspace egress.Policy
	Personal  egress.Policy
}

func (e Egress) of(o owner) egress.Policy {
	if o.workspace != 0 {
		return e.Workspace
	}
	return e.Personal
}

func (e Egress) Scope(scope Scope) egress.Policy {
	if scope == ScopeWorkspace {
		return e.Workspace
	}
	return e.Personal
}

func (s *Store) endpoint(ctx context.Context, o owner, id int64) (record, Endpoint, error) {
	rec, err := scan(s.db.QueryRowContext(ctx, `SELECT `+columns+` FROM provider_accounts WHERE id = ? AND `+ownerCond,
		append([]any{id}, o.args()...)...))
	if errors.Is(err, sql.ErrNoRows) {
		return record{}, Endpoint{}, errNotFound
	}
	if err != nil {
		return record{}, Endpoint{}, err
	}
	key, err := s.apiKey(rec)
	if err != nil {
		return record{}, Endpoint{}, err
	}
	return rec, Endpoint{Provider: rec.Provider, BaseURL: rec.BaseURL, APIKey: key}, nil
}

func (s *Service) handleCheck(ownerOf func(*http.Request) owner) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := accountID(w, r)
		if !ok {
			return
		}
		rec, ep, err := s.store.endpoint(r.Context(), ownerOf(r), id)
		if err != nil {
			fail(w, r, err)
			return
		}
		check := s.check(r.Context(), s.client(ownerOf(r)), ep, rec.Models)
		u, _ := auth.UserFrom(r.Context())
		slog.InfoContext(r.Context(), "provider account tested", "account", rec.ID, "provider", ep, "by", u.ID, "ok", check.OK)
		writeJSON(w, http.StatusOK, check)
	}
}

func (s *Service) check(ctx context.Context, client *http.Client, ep Endpoint, models []Model) Check {
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ep.BaseURL+"/models", nil)
	if err != nil {
		return failed(apierr.CodeEndpointUnreachable, "the base URL can't be called", nil)
	}
	if ep.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+ep.APIKey)
	}
	resp, err := client.Do(req)
	if refused := egress.Refusal(err); refused != nil {
		return failed(apierr.CodeEgressRefused, refused.Error(), apierr.Params{"host": refused.Host})
	}
	if err != nil {
		return failed(apierr.CodeEndpointUnreachable, "no answer from the endpoint", nil)
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return failed(apierr.CodeEndpointKeyRefused, "the endpoint answered "+strconv.Itoa(resp.StatusCode)+": check the API key",
			apierr.Params{"status": resp.StatusCode})
	case resp.StatusCode != http.StatusOK:
		return failed(apierr.CodeEndpointStatus, "the endpoint answered "+strconv.Itoa(resp.StatusCode)+" to GET /models",
			apierr.Params{"status": resp.StatusCode})
	}
	var reply struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxModelsReply)).Decode(&reply); err != nil || reply.Data == nil {
		return failed(apierr.CodeEndpointNoModels, "the endpoint's answer to GET /models isn't a list of models", nil)
	}
	listed := make(map[string]bool, len(reply.Data))
	for _, m := range reply.Data {
		listed[m.ID] = true
	}
	check := Check{OK: true}
	for _, m := range models {
		if !listed[m.ID] {
			check.Missing = append(check.Missing, m.ID)
		}
	}
	return check
}
