package conversations

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/apierr"
)

const maxNoteChars = 8000

type NoteReply struct {
	SubmissionID int64 `json:"submission_id"`
}

func (s *Service) handleNote(w http.ResponseWriter, r *http.Request) {
	c, ok := s.writable(w, r)
	if !ok {
		return
	}
	var req struct {
		Content   string `json:"content"`
		RequestID string `json:"request_id"`
	}
	if !readJSON(w, r, &req) {
		return
	}
	if strings.TrimSpace(req.Content) == "" || utf8.RuneCountInString(req.Content) > maxNoteChars {
		writeError(w, http.StatusBadRequest, apierr.CodeConversationNoteInvalid,
			fmt.Sprintf("a note must have 1 to %d characters", maxNoteChars), apierr.Params{"max": maxNoteChars})
		return
	}
	if len(req.RequestID) > maxRequestIDLen || !printableASCII(req.RequestID) {
		writeError(w, http.StatusBadRequest, apierr.CodeBadRequest, "request_id must be up to 128 printable ASCII characters")
		return
	}
	rt, ok := s.runtime(w, r)
	if !ok {
		return
	}
	if !s.stillWritable(w, r) {
		return
	}
	user, _ := caller(r)
	sub, err := rt.Note(r.Context(), c.agentID, agentrt.NoteInput{Text: req.Content, Author: user, RequestID: req.RequestID})
	if err != nil {
		conversationError(w, r, c, err)
		return
	}
	if err := s.touch(context.WithoutCancel(r.Context()), c.ID, c.agentID); err != nil {
		slog.WarnContext(r.Context(), "conversations: marking a conversation updated", "conversation", c.ID, "err", err)
	}
	writeJSON(w, http.StatusAccepted, NoteReply{SubmissionID: sub})
}
