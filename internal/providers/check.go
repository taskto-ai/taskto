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
	// checkTimeout bounds a test of an account, connecting included.
	checkTimeout = 10 * time.Second
	// egressCheckTimeout bounds resolving a base URL's host when an account is saved.
	egressCheckTimeout = 5 * time.Second
	// maxModelsReply is the largest model list read from an endpoint, in bytes.
	maxModelsReply = 4 << 20
)

// Check is the answer to a test of an account: whether its endpoint answered with its list
// of models, with the account's key, and which of the account's models it didn't list.
// Error never carries what the endpoint said, which could echo the key. Code and Params are
// Error's, for the web app to translate (apierr).
type Check struct {
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	Code    apierr.Code   `json:"code,omitempty"`
	Params  apierr.Params `json:"params,omitempty"`
	Missing []string      `json:"missing,omitempty"`
}

// failed is a Check that failed with code.
func failed(code apierr.Code, msg string, params apierr.Params) Check {
	return Check{Error: msg, Code: code, Params: params}
}

// checkClient is the client of account tests under policy. It doesn't follow redirects: a
// redirect is an answer to report, and following one would send the key on to wherever it
// points.
func checkClient(policy egress.Policy) *http.Client { return policy.Client(checkTimeout) }

// Egress is the egress policies of provider accounts: the server's and the agent runtime's
// calls to an account's endpoint connect only to addresses its scope's policy allows.
type Egress struct {
	Workspace egress.Policy // of workspaces' accounts, which admins add
	Personal  egress.Policy // of users' own accounts, which any member adds
}

func (e Egress) of(o owner) egress.Policy {
	if o.workspace != 0 {
		return e.Workspace
	}
	return e.Personal
}

// Scope returns the policy of accounts of scope.
func (e Egress) Scope(scope Scope) egress.Policy {
	if scope == ScopeWorkspace {
		return e.Workspace
	}
	return e.Personal
}

// endpoint returns o's account id and its endpoint, with the key in clear.
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

// handleCheck tests an account: GET {base_url}/models, OpenAI's way, with its key. The
// endpoint is the one the account's owner set, which the agent runtime calls anyway; only
// the outcome comes back, never the reply.
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
		// Refused before connecting: the key went nowhere.
		return failed(apierr.CodeEgressRefused, refused.Error(), apierr.Params{"host": refused.Host})
	}
	if err != nil {
		// The error names the URL, which is the account's, without a key; it isn't passed on
		// all the same, only that there was no answer.
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
