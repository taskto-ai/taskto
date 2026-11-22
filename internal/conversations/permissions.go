package conversations

import (
	"context"
	"errors"
	"net/http"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/cliworker"
)

// Permission questions (docs/agent-safety.md): a conversation on a CLI worker asks its owner
// before the CLI runs a command, edits a file or fetches a page. The owner, and only the
// owner, lists the questions waiting in their conversation and answers them:
//
//	GET  /api/workspaces/{workspace}/conversations/{conversation}/cli-permissions      → {"requests": […]}
//	POST /api/workspaces/{workspace}/conversations/{conversation}/cli-permissions/{id} {allow} → 204

// CLIQuestions is the CLI workers' side of the questions (cliaccounts.Service).
type CLIQuestions interface {
	Questions(ctx context.Context, user, conversation int64) ([]cliworker.PermissionRequest, error)
	Answer(ctx context.Context, user, conversation int64, id string, allow bool) error
}

// SetCLIQuestions sets where the questions come from; without it a conversation has none.
func (s *Service) SetCLIQuestions(q CLIQuestions) { s.questions = q }

func (s *Service) handleQuestions(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	requests := []cliworker.PermissionRequest{}
	if s.questions != nil {
		user, _ := caller(r)
		qs, err := s.questions.Questions(r.Context(), user, c.ID)
		if err != nil {
			serverError(w, r, err)
			return
		}
		requests = append(requests, qs...)
	}
	writeJSON(w, http.StatusOK, map[string]any{"requests": requests})
}

func (s *Service) handleAnswer(w http.ResponseWriter, r *http.Request) {
	c, ok := s.conversationInPath(w, r)
	if !ok {
		return
	}
	var req struct {
		Allow *bool `json:"allow"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if req.Allow == nil {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "allow (true or false) is required")
		return
	}
	id := r.PathValue("id")
	if s.questions == nil {
		writeError(w, http.StatusNotFound, apierr.CodeCLIPermissionNotFound, cliworker.ErrNoPermission.Error())
		return
	}
	user, _ := caller(r)
	err := s.questions.Answer(r.Context(), user, c.ID, id, *req.Allow)
	switch {
	case errors.Is(err, cliworker.ErrNoPermission):
		writeError(w, http.StatusNotFound, apierr.CodeCLIPermissionNotFound, err.Error())
	case err != nil:
		serverError(w, r, err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}
