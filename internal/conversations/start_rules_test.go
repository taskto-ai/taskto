package conversations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/agentrt"
)

type failingSubmit struct {
	*fakeRuntime
	fail int
}

func (f *failingSubmit) Submit(ctx context.Context, conv int64, in agentrt.Input) (int64, error) {
	if f.fail > 0 {
		f.fail--
		return 0, errors.New("the runtime dropped the submission")
	}
	return f.fakeRuntime.Submit(ctx, conv, in)
}

type lostAnswer struct {
	*fakeRuntime
	lose int
}

func (l *lostAnswer) Submit(ctx context.Context, conv int64, in agentrt.Input) (int64, error) {
	submission, err := l.fakeRuntime.Submit(ctx, conv, in)
	if err == nil && l.lose > 0 {
		l.lose--
		return 0, errors.New("the answer was lost")
	}
	return submission, err
}

func TestStartRefusesWhatIsNotAValidTurn(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	valid := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:30"}}
	cases := map[string]func(*Start){
		"unknown kind":     func(s *Start) { s.Origin.Kind = "user" },
		"empty kind":       func(s *Start) { s.Origin.Kind = "" },
		"empty key":        func(s *Start) { s.Origin.Key = "" },
		"key of 101 bytes": func(s *Start) { s.Origin.Key = strings.Repeat("k", 101) },
		"key with a tab":   func(s *Start) { s.Origin.Key = "a\tb" },
		"key with a DEL":   func(s *Start) { s.Origin.Key = "a\x7fb" },
		"key not ASCII":    func(s *Start) { s.Origin.Key = "chave-ç" },
		"blank prompt":     func(s *Start) { s.Prompt = " \n\t" },
		"prompt not UTF-8": func(s *Start) { s.Prompt = "a\xffb" },
		"provider only":    func(s *Start) { s.Model = agentrt.ModelRef{Provider: "script"} },
		"model id only":    func(s *Start) { s.Model = agentrt.ModelRef{ModelID: "script-1"} },
		"another workspace": func(s *Start) {
			s.Model = agentrt.ModelRef{Provider: fmt.Sprintf("ws%d-gateway", s.Workspace+1), ModelID: "x"}
		},
	}
	for name, change := range cases {
		st := valid
		change(&st)
		if _, err := e.svc.Start(context.Background(), st); err == nil {
			t.Errorf("%s: Start was admitted", name)
		}
	}
	if len(e.rt.created) != 0 || len(e.rt.inputs) != 0 {
		t.Fatalf("a refused start reached the runtime: created=%v inputs=%v", e.rt.created, e.rt.inputs)
	}
	var rows int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("a refused start left %d conversations, %v", rows, err)
	}
	edge := valid
	edge.Origin.Key = strings.Repeat("k", 100)
	if _, err := e.svc.Start(context.Background(), edge); err != nil {
		t.Fatalf("a key of 100 bytes was refused: %v", err)
	}
	edge.Origin.Key = " ~!"
	if _, err := e.svc.Start(context.Background(), edge); err != nil {
		t.Fatalf("a key of printable ASCII was refused: %v", err)
	}
}

func TestStartRetriedAfterALostSubmissionMakesNoSecondConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	flaky := &failingSubmit{fakeRuntime: e.rt, fail: 1}
	e.svc.agent = func(context.Context) (Runtime, error) { return flaky, nil }
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:40", RoutineID: 4}}
	if _, err := e.svc.Start(context.Background(), st); !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("the lost submission error = %v", err)
	}
	var rows int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE origin = ?`, st.Origin.Key).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the crashed start left %d conversations, %v", rows, err)
	}
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.rt.created) != 1 || len(e.rt.inputs[started.Conversation.agentID]) != 1 {
		t.Fatalf("created=%v inputs=%v", e.rt.created, e.rt.inputs)
	}
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM conversations WHERE origin = ?`, st.Origin.Key).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("the retry left %d conversations, %v", rows, err)
	}
}

func TestAnOriginKeyOfAnotherPersonOrAnotherSourceIsRefused(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	member := e.invite(owner, workspace, "member@example.com")
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:50", SpaceID: 1, AppID: 2}}
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	stolen := st
	stolen.User = member.id
	if _, err := e.svc.Start(context.Background(), stolen); !errors.Is(err, ErrOrigin) {
		t.Fatalf("another member's start with the key = %v", err)
	}
	elsewhere := e.workspace(owner)
	moved := st
	moved.Workspace = elsewhere
	if _, err := e.svc.Start(context.Background(), moved); !errors.Is(err, ErrOrigin) {
		t.Fatalf("a start for another workspace with the key = %v", err)
	}
	for name, change := range map[string]func(*Origin){
		"kind":    func(o *Origin) { o.Kind = "routine" },
		"space":   func(o *Origin) { o.SpaceID = 9 },
		"app":     func(o *Origin) { o.AppID = 9 },
		"routine": func(o *Origin) { o.RoutineID = 9 },
	} {
		other := st
		change(&other.Origin)
		if _, err := e.svc.Start(context.Background(), other); !errors.Is(err, ErrOrigin) {
			t.Errorf("a start with another %s and the same key = %v", name, err)
		}
	}
	if len(e.rt.created) != 1 || len(e.rt.inputs[started.Conversation.agentID]) != 1 {
		t.Fatalf("a refused start reached the runtime: created=%v inputs=%v", e.rt.created, e.rt.inputs)
	}
	if len(e.rt.inputs) != 1 {
		t.Fatalf("inputs = %v", e.rt.inputs)
	}
}

func TestALostRaceOnTheOriginKeyReturnsTheWinnerOrRefuses(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	member := e.invite(owner, workspace, "member@example.com")
	st := Start{Workspace: workspace, User: owner.id, Title: "First", Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:60", AppID: 2}}
	winner, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	loser := st
	loser.Title = "Second"
	got, err := e.svc.insertWithOrigin(context.Background(), loser, 999)
	if err != nil || got.ID != winner.Conversation.ID || got.Title != "First" {
		t.Fatalf("the lost race returned %+v, %v", got, err)
	}
	loser.User = member.id
	if _, err := e.svc.insertWithOrigin(context.Background(), loser, 999); !errors.Is(err, ErrOrigin) {
		t.Fatalf("a lost race against another person's conversation = %v", err)
	}
	var rows int
	if err := e.db.QueryRow(`SELECT COUNT(*) FROM conversations`).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("%d conversations, %v", rows, err)
	}
}

func TestOutcomeAndStopRefuseAConversationThatDoesNotExist(t *testing.T) {
	e := newEnv(t, false)
	e.setup()
	if _, err := e.svc.Outcome(context.Background(), 12345, 1, time.Millisecond); !errors.Is(err, errNotFound) {
		t.Fatalf("Outcome = %v", err)
	}
	if err := e.svc.Stop(context.Background(), 12345); !errors.Is(err, errNotFound) {
		t.Fatalf("Stop = %v", err)
	}
	off := newEnv(t, true)
	if _, err := off.svc.Outcome(context.Background(), 1, 1, time.Millisecond); !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("Outcome with the runtime off = %v", err)
	}
	if err := off.svc.Stop(context.Background(), 1); !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("Stop with the runtime off = %v", err)
	}
}

func TestOutcomeReportsARuntimeFailureAndACancelledCaller(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	started, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:70"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.rt.mu.Lock()
	e.rt.settle = make(chan struct{})
	e.rt.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := e.svc.Outcome(ctx, started.Conversation.ID, started.Submission, time.Second); !errors.Is(err, context.Canceled) || out.Settled {
		t.Fatalf("a cancelled caller got %+v, %v", out, err)
	}
	e.rt.mu.Lock()
	e.rt.settle = nil
	e.rt.waitErr = &agentrt.StatusError{Code: http.StatusNotFound}
	e.rt.mu.Unlock()
	if _, err := e.svc.Outcome(context.Background(), started.Conversation.ID, started.Submission, time.Second); !errors.Is(err, ErrGone) {
		t.Fatalf("a submission the runtime forgot = %v", err)
	}
	e.rt.mu.Lock()
	e.rt.waitErr = errors.New("the runtime stopped")
	e.rt.mu.Unlock()
	if _, err := e.svc.Outcome(context.Background(), started.Conversation.ID, started.Submission, time.Second); !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("a runtime that stopped = %v", err)
	}
}

func TestTheOriginIsShownWithoutItsKeyAndPeopleOwnConversationsHaveNone(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	started, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:80-secret", SpaceID: 3, RoutineID: 8},
	})
	if err != nil {
		t.Fatal(err)
	}
	own := owner.create(workspace, nil)
	one := owner.expect(http.StatusOK, http.MethodGet, path(workspace, fmt.Sprintf("/%d", started.Conversation.ID)), nil).Body.String()
	if strings.Contains(one, "routine-run:80-secret") || strings.Contains(one, "origin_kind") {
		t.Fatalf("the reply shows the private key or a column: %s", one)
	}
	got := decode[Conversation](t, owner.expect(http.StatusOK, http.MethodGet, path(workspace, fmt.Sprintf("/%d", started.Conversation.ID)), nil))
	if got.Origin == nil || *got.Origin != (ConversationOrigin{Kind: "routine", SpaceID: 3, RoutineID: 8}) {
		t.Fatalf("origin = %+v", got.Origin)
	}
	if raw := owner.expect(http.StatusOK, http.MethodGet, path(workspace, fmt.Sprintf("/%d", own.ID)), nil).Body.String(); !strings.Contains(raw, `"origin":null`) {
		t.Fatalf("a person's own conversation: %s", raw)
	}
	listed := decode[struct{ Conversations []Conversation }](t, owner.expect(http.StatusOK, http.MethodGet, path(workspace, ""), nil))
	kinds := map[int64]string{}
	for _, c := range listed.Conversations {
		if c.Origin != nil {
			kinds[c.ID] = c.Origin.Kind
		}
	}
	if len(kinds) != 1 || kinds[started.Conversation.ID] != "routine" {
		t.Fatalf("origins in the list = %v", kinds)
	}
}

func TestStartRetriedAfterALostAnswerSendsTheTurnOnce(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	lossy := &lostAnswer{fakeRuntime: e.rt, lose: 1}
	e.svc.agent = func(context.Context) (Runtime, error) { return lossy, nil }
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:90", AppID: 5}}
	if _, err := e.svc.Start(context.Background(), st); !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("the lost answer error = %v", err)
	}
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.rt.created) != 1 || len(e.rt.inputs[started.Conversation.agentID]) != 1 {
		t.Fatalf("the retry sent the turn again: created=%v inputs=%v", e.rt.created, e.rt.inputs)
	}
}

func TestOutcomeOfACallerWhoseTimeRanOutIsAnErrorNotAPendingTurn(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	started, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:91"},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.rt.mu.Lock()
	e.rt.settle = make(chan struct{})
	e.rt.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	out, err := e.svc.Outcome(ctx, started.Conversation.ID, started.Submission, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRuntimeDown) || out.Settled {
		t.Fatalf("a caller whose own time ran out got %+v, %v", out, err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := e.svc.Outcome(cancelled, started.Conversation.ID, started.Submission, time.Minute); !errors.Is(err, context.Canceled) || errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("a cancelled caller got %v", err)
	}
}

func TestAStartedTurnMovesItsConversationUpAndWatchesForTheEnd(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:95"}}
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the end of the turn to be watched", func() bool { return e.rt.waited.Load() > 0 })
	own := owner.create(workspace, nil)
	first := func() int64 {
		list := decode[struct{ Conversations []Conversation }](t, owner.expect(http.StatusOK, http.MethodGet, path(workspace, ""), nil))
		return list.Conversations[0].ID
	}
	if first() != own.ID {
		t.Fatalf("the newest conversation is not first")
	}
	time.Sleep(3 * time.Millisecond)
	e.rt.waited.Store(0)
	e.rt.mu.Lock()
	e.rt.settle = make(chan struct{})
	e.rt.mu.Unlock()
	if _, err := e.svc.Start(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	if first() != started.Conversation.ID {
		t.Fatalf("a retried start did not mark its conversation updated")
	}
}
