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
	if !errors.Is(modelOutcome(busy), cliaccounts.ErrModelBusy) || !errors.Is(modelOutcome(busy), busy) {
		t.Fatal("a busy answer is not the busy error")
	}
	for _, other := range []error{
		&agentrt.StatusError{Code: http.StatusConflict, Reason: "unknown_model"},
		&agentrt.StatusError{Code: http.StatusNotFound},
		&agentrt.StatusError{Code: http.StatusBadGateway},
		errors.New("closed"),
	} {
		if errors.Is(modelOutcome(other), cliaccounts.ErrModelBusy) {
			t.Fatalf("%v stops the hop", other)
		}
	}
	if modelOutcome(nil) != nil {
		t.Fatal("no error became one")
	}
}

func TestOnlyAnAnswerThatLeavesTheWriteUnknownIsUncertain(t *testing.T) {
	for name, err := range map[string]error{
		"a lost connection": errors.New("closed"),
		"a server error":    &agentrt.StatusError{Code: http.StatusInternalServerError},
		"a bad gateway":     &agentrt.StatusError{Code: http.StatusBadGateway},
	} {
		if !errors.Is(modelOutcome(err), cliaccounts.ErrModelUncertain) {
			t.Fatalf("%s is not uncertain", name)
		}
	}
	for name, err := range map[string]error{
		"an unknown model": &agentrt.StatusError{Code: http.StatusConflict, Reason: "unknown_model"},
		"a retired chat":   &agentrt.StatusError{Code: http.StatusNotFound},
		"a busy chat":      &agentrt.StatusError{Code: http.StatusConflict, Reason: "busy"},
	} {
		if errors.Is(modelOutcome(err), cliaccounts.ErrModelUncertain) {
			t.Fatalf("%s is uncertain, but the runtime answered", name)
		}
	}
}
