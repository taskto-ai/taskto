package conversations

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/access"
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
		"unknown kind":         func(s *Start) { s.Origin.Kind = "user" },
		"empty kind":           func(s *Start) { s.Origin.Kind = "" },
		"empty key":            func(s *Start) { s.Origin.Key = "" },
		"key of 101 bytes":     func(s *Start) { s.Origin.Key = strings.Repeat("k", 101) },
		"key with a tab":       func(s *Start) { s.Origin.Key = "a\tb" },
		"key with a DEL":       func(s *Start) { s.Origin.Key = "a\x7fb" },
		"key not ASCII":        func(s *Start) { s.Origin.Key = "chave-ç" },
		"blank prompt":         func(s *Start) { s.Prompt = " \n\t" },
		"title of 201":         func(s *Start) { s.Title = strings.Repeat("x", 201) },
		"title with a newline": func(s *Start) { s.Title = "a\nb" },
		"title with a tab":     func(s *Start) { s.Title = "a\tb" },
		"title with a NUL":     func(s *Start) { s.Title = "a\x00b" },
		"title not UTF-8":      func(s *Start) { s.Title = "a\xffb" },
		"prompt not UTF-8":     func(s *Start) { s.Prompt = "a\xffb" },
		"provider only":        func(s *Start) { s.Model = agentrt.ModelRef{Provider: "script"} },
		"model id only":        func(s *Start) { s.Model = agentrt.ModelRef{ModelID: "script-1"} },
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

func TestStartTitleFollowsTheConversationRouteRule(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:31"}}
	st.Title = "  " + strings.Repeat("é", 200) + "  "
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatalf("a title of 200 characters was refused: %v", err)
	}
	if want := strings.Repeat("é", 200); started.Conversation.Title != want {
		t.Fatalf("title = %q, want it trimmed", started.Conversation.Title)
	}
	var stored string
	if err := e.db.QueryRow(`SELECT title FROM conversations WHERE id = ?`, started.Conversation.ID).Scan(&stored); err != nil || stored != started.Conversation.Title {
		t.Fatalf("stored title = %q, %v", stored, err)
	}
	over := st
	over.Origin.Key = "app-effect:32"
	over.Title = strings.Repeat("é", 201)
	if _, err := e.svc.Start(context.Background(), over); err == nil {
		t.Fatal("a title of 201 characters was admitted")
	}
	if len(e.rt.created) != 1 {
		t.Fatalf("a refused title reached the runtime: created=%v", e.rt.created)
	}
}

func TestStartChecksTheOwnerAndTheRowAgainJustBeforeTheSubmit(t *testing.T) {
	cases := map[string]struct {
		change func(e *env, owner *client, workspace int64, started *Conversation)
		want   func(error) bool
	}{
		"the owner is disabled": {
			change: func(e *env, owner *client, workspace int64, _ *Conversation) {
				if _, err := e.db.Exec(`UPDATE users SET disabled_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, owner.id); err != nil {
					t.Fatal(err)
				}
			},
			want: func(err error) bool { return errors.Is(err, access.ErrDisabled) },
		},
		"the owner is removed from the workspace": {
			change: func(e *env, owner *client, workspace int64, _ *Conversation) {
				if _, err := e.db.Exec(`DELETE FROM memberships WHERE workspace_id = ? AND user_id = ?`, workspace, owner.id); err != nil {
					t.Fatal(err)
				}
			},
			want: func(err error) bool { return errors.Is(err, ErrNotMember) },
		},
		"the conversation is archived": {
			change: func(e *env, owner *client, workspace int64, _ *Conversation) {
				if _, err := e.db.Exec(`UPDATE conversations SET archived_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE origin = 'app-effect:50'`); err != nil {
					t.Fatal(err)
				}
			},
			want: func(err error) bool { return errors.Is(err, ErrArchived) },
		},
		"the conversation row is purged": {
			change: func(e *env, owner *client, workspace int64, _ *Conversation) {
				if _, err := DeleteOwned(context.Background(), e.db, owner.id); err != nil {
					t.Fatal(err)
				}
			},
			want: func(err error) bool { return errors.Is(err, errNotFound) },
		},
		"another conversation has the row's id": {
			change: func(e *env, owner *client, workspace int64, _ *Conversation) {
				var id int64
				if err := e.db.QueryRow(`SELECT id FROM conversations WHERE origin = 'app-effect:50'`).Scan(&id); err != nil {
					t.Fatal(err)
				}
				if _, err := e.db.Exec(`DELETE FROM conversations WHERE id = ?`, id); err != nil {
					t.Fatal(err)
				}
				if _, err := e.db.Exec(`INSERT INTO conversations (id, workspace_id, owner_id, agent_id, title, origin, origin_kind) VALUES (?, ?, ?, 999, '', 'app-effect:other', 'app')`, id, workspace, owner.id); err != nil {
					t.Fatal(err)
				}
			},
			want: func(err error) bool { return errors.Is(err, errNotFound) },
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, false)
			owner, workspace := e.setup()
			armed := true
			e.svc.userLanguage = func(context.Context, int64) string {
				if armed {
					armed = false
					tc.change(e, owner, workspace, nil)
				}
				return ""
			}
			st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:50", AppID: 5}}
			_, err := e.svc.Start(context.Background(), st)
			if !tc.want(err) {
				t.Fatalf("Start = %v", err)
			}
			if submitted := len(e.rt.inputs[101]) + len(e.rt.inputs[100]); submitted != 0 {
				t.Fatalf("a turn was submitted for an ineligible owner or a changed row: %v", e.rt.inputs)
			}
		})
	}
}

type purgingSubmit struct {
	*fakeRuntime
	purge func()
}

func (p *purgingSubmit) Submit(ctx context.Context, conv int64, in agentrt.Input) (int64, error) {
	p.purge()
	return p.fakeRuntime.Submit(ctx, conv, in)
}

func TestStartReportsNoSuccessWhenTheRowIsPurgedDuringTheSubmit(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	e.svc.agent = func(context.Context) (Runtime, error) {
		return &purgingSubmit{fakeRuntime: e.rt, purge: func() {
			if _, err := DeleteOwned(context.Background(), e.db, owner.id); err != nil {
				t.Fatal(err)
			}
		}}, nil
	}
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:53", AppID: 5}}
	if started, err := e.svc.Start(context.Background(), st); !errors.Is(err, errNotFound) {
		t.Fatalf("Start = %+v, %v; want errNotFound for a purged row", started, err)
	}
}

func TestADisabledOwnersStartWaitsAndRunsOnceWhenEnabled(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	armed := true
	e.svc.userLanguage = func(context.Context, int64) string {
		if armed {
			armed = false
			if _, err := e.db.Exec(`UPDATE users SET disabled_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE id = ?`, owner.id); err != nil {
				t.Fatal(err)
			}
		}
		return ""
	}
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:51", RoutineID: 1}}
	if _, err := e.svc.Start(context.Background(), st); !errors.Is(err, access.ErrDisabled) {
		t.Fatalf("Start = %v", err)
	}
	if _, err := e.db.Exec(`UPDATE users SET disabled_at = NULL WHERE id = ?`, owner.id); err != nil {
		t.Fatal(err)
	}
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	if len(e.rt.created) != 1 || len(e.rt.inputs[started.Conversation.agentID]) != 1 {
		t.Fatalf("created=%v inputs=%v", e.rt.created, e.rt.inputs)
	}
}

func TestARetryAfterALostSubmissionIsRefusedOnceTheOwnerArchivedTheConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	flaky := &failingSubmit{fakeRuntime: e.rt, fail: 1}
	e.svc.agent = func(context.Context) (Runtime, error) { return flaky, nil }
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "routine", Key: "routine-run:52", RoutineID: 4}}
	if _, err := e.svc.Start(context.Background(), st); !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("the lost submission error = %v", err)
	}
	var id int64
	if err := e.db.QueryRow(`SELECT id FROM conversations WHERE origin = ?`, st.Origin.Key).Scan(&id); err != nil {
		t.Fatal(err)
	}
	one := path(workspace, fmt.Sprintf("/%d", id))
	owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"archived": true})
	if _, err := e.svc.Start(context.Background(), st); !errors.Is(err, ErrArchived) {
		t.Fatalf("a retry into an archived conversation = %v", err)
	}
	if got := len(e.rt.inputs); got != 0 {
		t.Fatalf("an archived conversation received a turn: %v", e.rt.inputs)
	}
	owner.expect(http.StatusOK, http.MethodPatch, one, map[string]any{"archived": false})
	started, err := e.svc.Start(context.Background(), st)
	if err != nil || started.Conversation.ID != id || len(e.rt.inputs[started.Conversation.agentID]) != 1 {
		t.Fatalf("the retry after unarchiving = %+v, %v, inputs=%v", started, err, e.rt.inputs)
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
	missing := Ref{Workspace: 1, User: 1, Conversation: 12345, OriginKey: "app-effect:1"}
	if _, err := e.svc.Outcome(context.Background(), missing, 1, time.Millisecond); !errors.Is(err, errNotFound) {
		t.Fatalf("Outcome = %v", err)
	}
	if err := e.svc.Stop(context.Background(), missing); !errors.Is(err, errNotFound) {
		t.Fatalf("Stop = %v", err)
	}
	off := newEnv(t, true)
	if _, err := off.svc.Outcome(context.Background(), Ref{}, 1, time.Millisecond); !errors.Is(err, ErrRuntimeDown) {
		t.Fatalf("Outcome with the runtime off = %v", err)
	}
	if err := off.svc.Stop(context.Background(), Ref{}); !errors.Is(err, ErrRuntimeDown) {
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
	if out, err := e.svc.Outcome(ctx, refOf(owner, workspace, "routine-run:70", started.Conversation), started.Submission, time.Second); !errors.Is(err, context.Canceled) || out.Settled {
		t.Fatalf("a cancelled caller got %+v, %v", out, err)
	}
	e.rt.mu.Lock()
	e.rt.settle = nil
	e.rt.waitErr = &agentrt.StatusError{Code: http.StatusNotFound}
	e.rt.mu.Unlock()
	if _, err := e.svc.Outcome(context.Background(), refOf(owner, workspace, "routine-run:70", started.Conversation), started.Submission, time.Second); !errors.Is(err, ErrGone) {
		t.Fatalf("a submission the runtime forgot = %v", err)
	}
	e.rt.mu.Lock()
	e.rt.waitErr = errors.New("the runtime stopped")
	e.rt.mu.Unlock()
	if _, err := e.svc.Outcome(context.Background(), refOf(owner, workspace, "routine-run:70", started.Conversation), started.Submission, time.Second); !errors.Is(err, ErrRuntimeDown) {
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
	out, err := e.svc.Outcome(ctx, refOf(owner, workspace, "routine-run:91", started.Conversation), started.Submission, time.Minute)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrRuntimeDown) || out.Settled {
		t.Fatalf("a caller whose own time ran out got %+v, %v", out, err)
	}
	cancelled, stop := context.WithCancel(context.Background())
	stop()
	if _, err := e.svc.Outcome(cancelled, refOf(owner, workspace, "routine-run:91", started.Conversation), started.Submission, time.Minute); !errors.Is(err, context.Canceled) || errors.Is(err, ErrRuntimeDown) {
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

func refOf(owner *client, workspace int64, key string, c Conversation) Ref {
	return Ref{Workspace: workspace, User: owner.id, Conversation: c.ID, OriginKey: key}
}

func TestStopAndOutcomeOfAReusedConversationIDTouchNothingOfTheNewConversation(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	member := e.invite(owner, workspace, "member@example.com")
	old, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:300"},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldRef := refOf(owner, workspace, "app-effect:300", old.Conversation)
	if _, err := e.db.Exec(`DELETE FROM conversations WHERE id = ?`, old.Conversation.ID); err != nil {
		t.Fatal(err)
	}
	reused, err := e.svc.Start(context.Background(), Start{
		Workspace: workspace, User: member.id, Prompt: "Private work", Origin: Origin{Kind: "app", Key: "app-effect:301"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if reused.Conversation.ID != old.Conversation.ID {
		t.Fatalf("the id was not reused: %d and %d", old.Conversation.ID, reused.Conversation.ID)
	}
	e.rt.mu.Lock()
	e.rt.replies[reused.Conversation.agentID] = []agentrt.Message{{Role: "assistant", Text: "the other person's answer"}}
	e.rt.mu.Unlock()
	if err := e.svc.Stop(context.Background(), oldRef); !errors.Is(err, errNotFound) {
		t.Fatalf("Stop of the old id = %v", err)
	}
	if got := e.rt.aborts[reused.Conversation.agentID]; got != 0 {
		t.Fatalf("the old id aborted the new conversation: %v", e.rt.aborts)
	}
	out, err := e.svc.Outcome(context.Background(), oldRef, old.Submission, time.Second)
	if !errors.Is(err, errNotFound) || out.Reply != "" {
		t.Fatalf("Outcome of the old id = %+v, %v", out, err)
	}
	byOwner := oldRef
	byOwner.User = member.id
	byOwner.OriginKey = "app-effect:301"
	if err := e.svc.Stop(context.Background(), byOwner); err != nil {
		t.Fatalf("Stop by the owner of the new conversation = %v", err)
	}
	if e.rt.aborts[reused.Conversation.agentID] != 1 {
		t.Fatalf("aborts = %v", e.rt.aborts)
	}
	for name, change := range map[string]func(*Ref){
		"owner":     func(r *Ref) { r.User = owner.id },
		"workspace": func(r *Ref) { r.Workspace = e.workspace(owner) },
		"key":       func(r *Ref) { r.OriginKey = "app-effect:302" },
	} {
		wrong := byOwner
		change(&wrong)
		if err := e.svc.Stop(context.Background(), wrong); !errors.Is(err, errNotFound) {
			t.Errorf("Stop with another %s = %v", name, err)
		}
		if _, err := e.svc.Outcome(context.Background(), wrong, reused.Submission, time.Second); !errors.Is(err, errNotFound) {
			t.Errorf("Outcome with another %s = %v", name, err)
		}
	}
	if e.rt.aborts[reused.Conversation.agentID] != 1 {
		t.Fatalf("a wrong reference aborted the conversation: %v", e.rt.aborts)
	}
}

func TestOutcomeRefusesASubmissionThatIsNotTheConversationsOwn(t *testing.T) {
	e := newEnv(t, false)
	owner, workspace := e.setup()
	st := Start{Workspace: workspace, User: owner.id, Prompt: "Do work", Origin: Origin{Kind: "app", Key: "app-effect:310"}}
	started, err := e.svc.Start(context.Background(), st)
	if err != nil {
		t.Fatal(err)
	}
	other := st
	other.Origin.Key = "app-effect:311"
	foreign, err := e.svc.Start(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	ref := refOf(owner, workspace, "app-effect:310", started.Conversation)
	if _, err := e.svc.Outcome(context.Background(), ref, started.Submission+foreign.Submission, time.Second); !errors.Is(err, errNotFound) {
		t.Fatalf("Outcome with a submission of no conversation = %v", err)
	}
	if _, err := e.svc.Outcome(context.Background(), ref, 0, time.Second); !errors.Is(err, errNotFound) {
		t.Fatalf("Outcome with no submission = %v", err)
	}
	if _, err := e.svc.Outcome(context.Background(), ref, started.Submission, time.Second); err != nil {
		t.Fatalf("Outcome with its own submission = %v", err)
	}
}
