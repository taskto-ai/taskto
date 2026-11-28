package main

import (
	"errors"
	"net/http"
	"testing"

	"taskto.ai/taskto/internal/agentrt"
	"taskto.ai/taskto/internal/cliaccounts"
)

func TestOnlyTheRuntimesBusyAnswerStopsAModelHop(t *testing.T) {
	busy := &agentrt.StatusError{Code: http.StatusConflict, Reason: "busy"}
	if !errors.Is(modelBusy(busy), cliaccounts.ErrModelBusy) || !errors.Is(modelBusy(busy), busy) {
		t.Fatal("a busy answer is not the busy error")
	}
	for _, other := range []error{
		&agentrt.StatusError{Code: http.StatusConflict, Reason: "unknown_model"},
		&agentrt.StatusError{Code: http.StatusNotFound},
		errors.New("closed"),
	} {
		if errors.Is(modelBusy(other), cliaccounts.ErrModelBusy) {
			t.Fatalf("%v stops the hop", other)
		}
	}
	if modelBusy(nil) != nil {
		t.Fatal("no error became one")
	}
}
