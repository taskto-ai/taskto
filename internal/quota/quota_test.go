package quota

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/access"
	"taskto.ai/taskto/internal/apierr"
)

func TestErrorIsAnAPIAnswer(t *testing.T) {
	var err error = &Error{Scope: ScopeUser, Limit: 10<<20 + 1, Used: 5}
	if !errors.Is(err, ErrExceeded) {
		t.Fatal("an *Error is not ErrExceeded")
	}
	var q *Error
	if !errors.As(err, &q) {
		t.Fatal("errors.As")
	}
	a := q.API()
	if a.Status != http.StatusInsufficientStorage || a.Code != apierr.CodeQuotaUser || a.Params["max_mib"] != int64(11) {
		t.Fatalf("API() = %+v, want 507 quota_user with the quota rounded up to 11 MiB", a)
	}
	if got := (&Error{Scope: ScopeWorkspace, Limit: 1 << 20}).API(); got.Code != apierr.CodeQuotaWorkspace {
		t.Fatalf("workspace code = %q", got.Code)
	}
}

// Without a quota, or for a write that adds nothing, the check answers before it reads anything.
func TestCheckNeedsNoDatabaseWhenThereIsNothingToCheck(t *testing.T) {
	sp := access.Space{ID: 1, WorkspaceID: 1}
	for name, s := range map[string]*Service{
		"no quotas":     New(nil, Limits{}, nil),
		"adds nothing":  New(nil, Limits{User: 1, Workspace: 1}, nil),
		"frees a space": New(nil, Limits{User: 1, Workspace: 1}, nil),
	} {
		adding := int64(10)
		if name != "no quotas" {
			adding = -5
		}
		if err := s.CheckDB(context.Background(), sp, adding); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := New(nil, Limits{User: 1}, nil).CheckDB(context.Background(), sp, 0); err != nil {
		t.Errorf("zero bytes: %v", err)
	}
}
