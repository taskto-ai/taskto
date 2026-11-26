package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/integrations"
	"taskto.ai/taskto/internal/integrations/testprovider"
)

// The isolation scenarios of payments, through the real server (payments_e2e_test.go has its
// fixtures): what an event, an integration, an approval or a space may and may not reach. In each, the
// thing that must not cross is shown to be there, so the test proves something: the record that is left
// alone is one a payment would have changed.

// lastEffect is the newest entry of the apps' outbox: its state and its code.
func (p *payStack) lastEffect() (state, code string) {
	p.t.Helper()
	return p.text(`SELECT state FROM app_effects ORDER BY id DESC LIMIT 1`), p.text(`SELECT coalesce(error_code, '') FROM app_effects ORDER BY id DESC LIMIT 1`)
}

// failsWith waits for the newest entry to fail with the code.
func (p *payStack) failsWith(code string) {
	p.t.Helper()
	p.eventually("the entry fails with "+code, func() bool {
		state, got := p.lastEffect()
		return state == "failed" && got == code
	})
}

// payAndDeliver pays the session at the fake and has the provider's events delivered to the integration.
func (p *payStack) payAndDeliver(sh *shop, session string) {
	p.t.Helper()
	evs, err := p.fake.Pay(session)
	if err != nil {
		p.t.Fatal(err)
	}
	p.deliver(sh, evs)
}

// tokenOf is the reference token of the session's payment link: what the provider echoes back.
func (p *payStack) tokenOf(session string) string {
	p.t.Helper()
	s, ok := p.fake.Session(session)
	if !ok || s.ClientReferenceID == nil {
		p.t.Fatalf("no session %s with a reference", session)
	}
	return *s.ClientReferenceID
}

// ----- another workspace -----

func TestAnotherWorkspacesIntegrationAndAdminsReachNothing(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	before := s.record(s.bob, s.app, record)

	// Alice administers workspace two too, and connects her own integration there.
	two := s.connect(s.alice, s.two, "other", keyB, hookW2)

	// The provider's event for workspace one's payment, delivered to workspace two's route with its own
	// secret: authentic there, and it names nothing there. It is ignored, and workspace one's invoice
	// stays as it is.
	s.deliver(two, []testprovider.Event{s.completed(session, nil)})
	events := s.settled(s.alice, two, 1)
	if e := events[0]; e.State != integrations.EventIgnored || e.Code != "integration_event_ref_unknown" {
		t.Fatalf("event = %+v", e)
	}
	if after := s.record(s.bob, s.app, record); after.Revision != before.Revision || after.Data["status"] != "sent" {
		t.Fatalf("another workspace's integration changed the record: %+v", after)
	}
	if n := len(s.eventsOf(s.alice, s.in)); n != 0 {
		t.Fatalf("workspace one's integration got %d events", n)
	}
	// One workspace's secret opens the other's route no more than a wrong one: the plain 404.
	for name, d := range map[string]*shop{
		"workspace one's secret on two's route": {hook: two.hook, secret: hookA},
		"workspace two's secret on one's route": {hook: s.in.hook, secret: hookW2},
	} {
		got, err := s.sender(d).Send(context.Background(), s.completed(session, nil))
		if err != nil || got.Status != http.StatusNotFound {
			t.Errorf("%s: %v %d", name, err, got.Status)
		}
	}
	if n := len(s.eventsOf(s.alice, s.in)); n != 0 {
		t.Fatalf("a forged delivery was stored (%d events)", n)
	}

	// The routes: workspace one's integration, approval and events don't exist in workspace two, and the
	// ones of workspace two don't exist in one, for an admin of both.
	approval := s.app.approval
	for name, req := range map[string]struct {
		method, path string
		body         any
	}{
		"get":           {http.MethodGet, s.ws(s.two, fmt.Sprintf("integrations/%d", s.in.id)), nil},
		"patch":         {http.MethodPatch, s.ws(s.two, fmt.Sprintf("integrations/%d", s.in.id)), map[string]any{"disabled": true}},
		"delete":        {http.MethodDelete, s.ws(s.two, fmt.Sprintf("integrations/%d", s.in.id)), nil},
		"spaces":        {http.MethodPut, s.ws(s.two, fmt.Sprintf("integrations/%d/spaces", s.in.id)), map[string]any{"spaces": []int64{s.alicePrivate2}}},
		"check":         {http.MethodPost, s.ws(s.two, fmt.Sprintf("integrations/%d/check", s.in.id)), map[string]any{}},
		"events":        {http.MethodGet, s.ws(s.two, fmt.Sprintf("integrations/%d/events", s.in.id)), nil},
		"unsolved":      {http.MethodGet, s.ws(s.two, fmt.Sprintf("integrations/%d/payment-links/unresolved", s.in.id)), nil},
		"approve":       {http.MethodPost, s.ws(s.two, fmt.Sprintf("integration-approvals/%d/revoke", approval)), map[string]any{}},
		"the other way": {http.MethodGet, s.ws(s.one, fmt.Sprintf("integrations/%d", two.id)), nil},
	} {
		if status, code := s.code(s.alice.cookie, req.method, req.path, req.body); status != http.StatusNotFound {
			t.Errorf("%s: %d %s", name, status, code)
		}
	}
	// Nothing was changed by them.
	var got integrations.Integration
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, fmt.Sprintf("integrations/%d", s.in.id)), nil, &got)
	if got.Disabled {
		t.Fatal("workspace two's route disabled workspace one's integration")
	}
	// The lists are each workspace's own.
	var list struct {
		Integrations []integrations.Integration `json:"integrations"`
	}
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.two, "integrations"), nil, &list)
	if len(list.Integrations) != 1 || list.Integrations[0].ID != two.id {
		t.Fatalf("workspace two lists %+v", list.Integrations)
	}
}

// Who may do what: the admin's routes are for admins, with a session. A member, a person of another
// workspace and an access token are refused every one.
func TestOnlyAnAdminWithASessionMayConnectAndDecide(t *testing.T) {
	s := newInvoicing(t)
	_, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	_ = session
	token, _ := s.newToken(s.alice.cookie, s.one, "agent")

	type req struct {
		method, path string
		body         any
	}
	in := fmt.Sprintf("integrations/%d", s.in.id)
	routes := map[string]req{
		"list":      {http.MethodGet, s.ws(s.one, "integrations"), nil},
		"providers": {http.MethodGet, s.ws(s.one, "integration-providers"), nil},
		"create": {http.MethodPost, s.ws(s.one, "integrations"), map[string]any{"provider": "stripe", "name": "x",
			"secrets": map[string]string{"api_key": keyB, "webhook_secret": hookB}}},
		"get":      {http.MethodGet, s.ws(s.one, in), nil},
		"disable":  {http.MethodPatch, s.ws(s.one, in), map[string]any{"disabled": true}},
		"delete":   {http.MethodDelete, s.ws(s.one, in), nil},
		"spaces":   {http.MethodPut, s.ws(s.one, in+"/spaces"), map[string]any{"spaces": []int64{s.team, s.carol.private}}},
		"check":    {http.MethodPost, s.ws(s.one, in+"/check"), map[string]any{}},
		"events":   {http.MethodGet, s.ws(s.one, in+"/events"), nil},
		"queue":    {http.MethodGet, s.ws(s.one, "integration-approvals"), nil},
		"approve":  {http.MethodPost, s.ws(s.one, fmt.Sprintf("integration-approvals/%d/approve", s.app.approval)), map[string]any{"integration_id": s.in.id}},
		"reject":   {http.MethodPost, s.ws(s.one, fmt.Sprintf("integration-approvals/%d/reject", s.app.approval)), map[string]any{}},
		"revoke":   {http.MethodPost, s.ws(s.one, fmt.Sprintf("integration-approvals/%d/revoke", s.app.approval)), map[string]any{}},
		"unlinked": {http.MethodGet, s.ws(s.one, in+"/payment-links/unresolved"), nil},
	}
	for name, r := range routes {
		// A member of the workspace who isn't an admin: forbidden.
		for who, cookie := range map[string]string{"bob": s.bob.cookie, "carol": s.carol.cookie} {
			if status, code := s.code(cookie, r.method, r.path, r.body); status != http.StatusForbidden {
				t.Errorf("%s as %s: %d %s", name, who, status, code)
			}
		}
		// An access token is not a session: refused, whoever's it is.
		if rec := s.request(r.method, r.path, r.body, bearer(token)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"token_refused"`) {
			t.Errorf("%s with an access token: %d %s", name, rec.Code, rec.Body)
		}
		// Nobody at all.
		if rec := s.request(r.method, r.path, r.body, sameOrigin("")); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s with no session: %d", name, rec.Code)
		}
	}
	// Nothing of what they tried took effect.
	var got integrations.Integration
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, in), nil, &got)
	if got.Disabled || len(got.Spaces) != 1 || got.Spaces[0] != s.team {
		t.Fatalf("integration = %+v", got)
	}
	var st struct {
		Slots []struct{ Status string } `json:"slots"`
	}
	s.mustCall(http.StatusOK, s.bob.cookie, http.MethodGet, s.appPath(s.app.space, fmt.Sprintf("/%d/integrations", s.app.app)), nil, &st)
	if len(st.Slots) != 1 || st.Slots[0].Status != "approved" {
		t.Fatalf("the approval is %+v", st.Slots)
	}
}

// ----- a private space -----

// An app in a space the admin can't open is approved without opening it, and what the admin sees of it
// is its name, its space and its contract: never its records, and never a way to its events' records.
func TestAnAppInAPrivateSpaceStaysClosedToAnAdminAndToOthers(t *testing.T) {
	s := newInvoicing(t)

	// Carol builds the app in her private space, where nobody else is: the integration isn't allowed
	// there, and the admin who approves says it is (allow_space), without being able to open the space.
	inv := s.draft(s.carol, s.carol.private, "Carol's invoices", payInvoiceApp)
	s.askApproval(s.carol, &inv)
	if status, code := s.code(s.alice.cookie, http.MethodPost, s.ws(s.one, fmt.Sprintf("integration-approvals/%d/approve", inv.approval)),
		map[string]any{"integration_id": s.in.id}); status != http.StatusConflict || code != "app_integration_space_not_allowed" {
		t.Fatalf("approving where the integration isn't allowed: %d %s", status, code)
	}
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodPost, s.ws(s.one, fmt.Sprintf("integration-approvals/%d/approve", inv.approval)),
		map[string]any{"integration_id": s.in.id, "allow_space": true}, nil)
	s.publish(s.carol, inv)

	// Nobody else can reach the app, its integrations or its records: bob, who is in other spaces; and
	// alice, the admin, who decided on it. The space doesn't exist for them.
	for name, who := range map[string]person{"bob": s.bob, "alice": s.alice} {
		for what, path := range map[string]string{
			"the app":                  s.appPath(inv.space, fmt.Sprintf("/%d", inv.app)),
			"its records":              s.appPath(inv.space, fmt.Sprintf("/%d/records", inv.app)),
			"its slots":                s.appPath(inv.space, fmt.Sprintf("/%d/integrations", inv.app)),
			"its effects":              s.appPath(inv.space, fmt.Sprintf("/%d/effects", inv.app)),
			"the space's integrations": fmt.Sprintf("/api/workspaces/%d/spaces/%d/integrations", s.one, inv.space),
		} {
			if status, code := s.code(who.cookie, http.MethodGet, path, nil); status != http.StatusNotFound || code != "space_not_found" {
				t.Errorf("%s reads %s of a private space: %d %s", name, what, status, code)
			}
		}
		if got := s.send(who, inv, 1); got != http.StatusNotFound {
			t.Errorf("%s runs an action of a private space's app: %d", name, got)
		}
		if status, _ := s.code(who.cookie, http.MethodPost, s.appPath(inv.space, fmt.Sprintf("/%d/integrations/payments/request", inv.app)), map[string]any{"contract_hash": "x"}); status != http.StatusNotFound {
			t.Errorf("%s asks for an approval in a private space: %d", name, status)
		}
	}
	// The same for carol's Secret space and the app of one in it that someone else isn't a member of.
	if status, _ := s.code(s.bob.cookie, http.MethodPost, s.appPath(s.secret, ""), map[string]any{"name": "Sneaky", "definition": rawDefinition(payInvoiceApp)}); status != http.StatusNotFound {
		t.Errorf("bob makes an app in a space he isn't in: %d", status)
	}

	// What the admin sees: the approval, with the app's name and its space, and the integration's own
	// answers hold no space of a person's private one but a count.
	var queue struct {
		Approvals []struct {
			AppName   string `json:"app_name"`
			SpaceKind string `json:"space_kind"`
			Status    string `json:"status"`
		} `json:"approvals"`
	}
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, "integration-approvals"), nil, &queue)
	var got integrations.Integration
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, fmt.Sprintf("integrations/%d", s.in.id)), nil, &got)
	for _, sp := range append(append([]int64{}, got.Spaces...), got.ApprovalSpaces...) {
		if sp == s.carol.private {
			t.Fatalf("the admin is shown carol's private space %d: %+v", sp, got)
		}
	}
	if got.OtherSpaces+len(got.Spaces)+len(got.ApprovalSpaces) < 2 {
		t.Fatalf("the integration's spaces: %+v", got)
	}

	// Carol sends an invoice from her app: the link is made, and the payment is applied to her record by
	// the integration. The admin, who runs the integration, is told of the event, and has no way to the
	// record through it.
	record, session := s.sentInvoice(s.carol, inv, "C-1", 75)
	s.payAndDeliver(s.in, session)
	events := s.settled(s.alice, s.in, 1)
	if events[0].State != integrations.EventApplied {
		t.Fatalf("event = %+v", events[0])
	}
	if s.record(s.carol, inv, record).Data["status"] != "paid" {
		t.Fatal("carol's invoice isn't paid")
	}
	// The events' answer is its closed fields, with nothing of the app, the record or the reference.
	rec := s.request(http.MethodGet, s.ws(s.one, fmt.Sprintf("integrations/%d/events", s.in.id)), nil, sameOrigin(s.alice.cookie))
	for _, leak := range []string{"Carol", "C-1", "app_id", "record", "token", s.tokenOf(session), session} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Errorf("the admin's event list holds %q: %s", leak, rec.Body)
		}
	}
	// And the Team's app, which the same integration serves, is not carol's.
	if n := len(s.records(s.bob, s.app)); n != 0 {
		t.Fatalf("the team's app has %d records", n)
	}
}

// ----- an event of one integration or app never moves another's record -----

func TestAnEventNeverMovesARecordOfAnotherIntegrationOrApp(t *testing.T) {
	s := newInvoicing(t)
	b := s.connect(s.alice, s.one, "second", keyB, hookB)
	s.allowSpaces(s.alice, b, s.team)
	appB := s.build(s.bob, s.alice, b, s.team, "Invoices B", payInvoiceApp)
	appC := s.build(s.bob, s.alice, s.in, s.team, "Invoices C", payInvoiceApp)

	recA, sessA := s.sentInvoice(s.bob, s.app, "A-1", 50)
	recC, sessC := s.sentInvoice(s.bob, appC, "C-1", 50)
	// The fake accepts one key at a time: the second integration's link is made under its own.
	s.fake.SetAPIKey(keyB)
	recB, sessB := s.sentInvoice(s.bob, appB, "B-1", 50)
	if n := s.refRows(s.one); n != 3 {
		t.Fatalf("%d refs", n)
	}
	revA, revB, revC := s.record(s.bob, s.app, recA).Revision, s.record(s.bob, appB, recB).Revision, s.record(s.bob, appC, recC).Revision

	// The first integration's webhook told of the second integration's payment (its token, signed with
	// the first's own secret) names nothing of the first's, and the other way round.
	s.deliver(s.in, []testprovider.Event{s.completed(sessB, nil)})
	s.deliver(b, []testprovider.Event{s.completed(sessA, nil)})
	for _, sh := range []*shop{s.in, b} {
		events := s.settled(s.alice, sh, 1)
		if e := events[0]; e.State != integrations.EventIgnored || e.Code != "integration_event_ref_unknown" {
			t.Fatalf("event of integration %d = %+v", sh.id, e)
		}
	}
	for name, got := range map[string][2]int64{
		"A": {s.record(s.bob, s.app, recA).Revision, revA}, "B": {s.record(s.bob, appB, recB).Revision, revB}, "C": {s.record(s.bob, appC, recC).Revision, revC},
	} {
		if got[0] != got[1] {
			t.Fatalf("invoice %s was changed by another integration's event", name)
		}
	}

	// A real payment of the first app's invoice moves that record and no other, though the integration,
	// the space and the definition are the same as the third app's.
	s.payAndDeliver(s.in, sessA)
	s.eventually("the first invoice is paid", func() bool { return s.record(s.bob, s.app, recA).Data["status"] == "paid" })
	if s.record(s.bob, appC, recC).Data["status"] != "sent" || s.record(s.bob, appB, recB).Data["status"] != "sent" {
		t.Fatal("a payment moved another app's invoice")
	}
	// And the payment of the third's moves only the third's.
	s.payAndDeliver(s.in, sessC)
	s.eventually("the third invoice is paid", func() bool { return s.record(s.bob, appC, recC).Data["status"] == "paid" })
	if s.record(s.bob, appB, recB).Data["status"] != "sent" || s.record(s.bob, appB, recB).Revision != revB {
		t.Fatal("a payment moved another integration's invoice")
	}
	// Its own integration's route does move it.
	s.payAndDeliver(b, sessB)
	s.eventually("the second invoice is paid", func() bool { return s.record(s.bob, appB, recB).Data["status"] == "paid" })
}

// ----- a revoked approval -----

func TestARevokedApprovalAppliesNoPaymentAndMakesNoLink(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	before := s.record(s.bob, s.app, record)

	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodPost, s.ws(s.one, fmt.Sprintf("integration-approvals/%d/revoke", s.app.approval)), map[string]any{}, nil)

	// The payment arrives after the revocation: it is stored, not applied, failed with its reason, left
	// for the admin to retry, and told to them. The money isn't dropped and the record isn't touched.
	s.payAndDeliver(s.in, session)
	events := s.settled(s.alice, s.in, 1)
	if e := events[0]; e.State != integrations.EventFailed || e.Code != "integration_event_unavailable" || !e.Retriable {
		t.Fatalf("event = %+v", e)
	}
	if after := s.record(s.bob, s.app, record); after.Revision != before.Revision || after.Data["status"] != "sent" {
		t.Fatalf("a revoked approval's payment changed the record: %+v", after)
	}
	if n := s.scalar(`SELECT count(*) FROM notifications WHERE kind = 'integration_event' AND user_id = ?`, s.alice.id); n != 1 {
		t.Fatalf("the admin has %d notices", n)
	}

	// A new invoice sent after the revocation makes no link: the entry fails, the provider isn't called.
	posts := len(s.sessionPosts())
	record2 := s.draftInvoice(s.bob, s.app, "A-2", 60)
	if got := s.send(s.bob, s.app, record2); got != http.StatusOK {
		t.Fatalf("send = %d", got)
	}
	s.failsWith("integration_unavailable")
	if n := len(s.sessionPosts()); n != posts {
		t.Fatalf("a revoked approval made %d calls to the provider", n-posts)
	}
	if url, _ := s.record(s.bob, s.app, record2).Data["pay_url"].(string); url != "" {
		t.Fatalf("a link was written: %q", url)
	}

	// Approved again, the failed event is retried by the admin and applies: nothing was lost.
	s.askApproval(s.bob, &s.app)
	s.approve(s.alice, s.in, s.app)
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodPost, s.ws(s.one, fmt.Sprintf("integrations/%d/events/%d/retry", s.in.id, events[0].ID)), map[string]any{}, nil)
	s.eventually("the retried payment is applied", func() bool { return s.record(s.bob, s.app, record).Data["status"] == "paid" })
}

// ----- a disabled integration -----

func TestADisabledIntegrationTakesNoEventAndMakesNoLink(t *testing.T) {
	s := newInvoicing(t)
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	before := s.record(s.bob, s.app, record)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}

	patch := func(disabled bool) {
		s.mustCall(http.StatusOK, s.alice.cookie, http.MethodPatch, s.ws(s.one, fmt.Sprintf("integrations/%d", s.in.id)), map[string]any{"disabled": disabled}, nil)
	}
	patch(true)

	// Its route is the plain 404 of an id that doesn't exist, whatever the signature, and stores nothing.
	unknown, err := s.sender(&shop{hook: strings.Repeat("a", 32), secret: hookA}).Send(context.Background(), evs[len(evs)-1])
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range evs {
		d, err := s.sender(s.in).Send(context.Background(), ev)
		if err != nil || d.Status != http.StatusNotFound || string(d.Response) != string(unknown.Response) {
			t.Fatalf("%s to a disabled integration: %v %d %s", ev.Type, err, d.Status, d.Response)
		}
	}
	if n := len(s.eventsOf(s.alice, s.in)); n != 0 {
		t.Fatalf("a disabled integration stored %d events", n)
	}
	// A new invoice makes no link.
	posts := len(s.sessionPosts())
	record2 := s.draftInvoice(s.bob, s.app, "A-2", 60)
	if got := s.send(s.bob, s.app, record2); got != http.StatusOK {
		t.Fatalf("send = %d", got)
	}
	s.failsWith("integration_unavailable")
	if n := len(s.sessionPosts()); n != posts {
		t.Fatalf("a disabled integration made %d calls to the provider", n-posts)
	}
	if after := s.record(s.bob, s.app, record); after.Revision != before.Revision {
		t.Fatal("a disabled integration changed a record")
	}

	// Switched on again, the provider's retry of the same events is accepted and applied: no payment was
	// lost to the pause.
	patch(false)
	s.deliver(s.in, evs)
	s.eventually("the payment is applied", func() bool { return s.record(s.bob, s.app, record).Data["status"] == "paid" })
}

// ----- test mode and live mode -----

func TestATestModeEventNeverMovesALiveReference(t *testing.T) {
	s := newPayStack(t)
	s.fake.SetAPIKey(keyLv) // from now on the provider's objects are live money
	live := s.connect(s.alice, s.one, "live", keyLv, hookLv)
	var got integrations.Integration
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, fmt.Sprintf("integrations/%d", live.id)), nil, &got)
	if got.Mode != "live" {
		t.Fatalf("mode = %q", got.Mode)
	}
	s.allowSpaces(s.alice, live, s.team)
	app := s.build(s.bob, s.alice, live, s.team, "Invoices", payInvoiceApp)
	record, session := s.sentInvoice(s.bob, app, "A-1", 50)
	if !strings.HasPrefix(session, "cs_live_") {
		t.Fatalf("the session %q isn't live", session)
	}
	before := s.record(s.bob, app, record)

	// A test-mode event with the live reference's token, with a signature that verifies (the same
	// secret): answered 200 as every event of a type the integration doesn't act on, and not stored.
	test := s.completed(session, func(o map[string]any) { o["livemode"] = false })
	test.Livemode = false
	s.deliver(live, []testprovider.Event{test})
	if n := len(s.eventsOf(s.alice, live)); n != 0 {
		t.Fatalf("a test-mode event of a live integration was stored (%d events)", n)
	}
	if after := s.record(s.bob, app, record); after.Revision != before.Revision || after.Data["status"] != "sent" {
		t.Fatalf("a test-mode event moved a live reference: %+v", after)
	}

	// The live payment itself does.
	s.payAndDeliver(live, session)
	events := s.settled(s.alice, live, 1)
	if e := events[0]; e.State != integrations.EventApplied || !e.Livemode {
		t.Fatalf("event = %+v", e)
	}
	if s.record(s.bob, app, record).Data["status"] != "paid" {
		t.Fatal("the live payment wasn't applied")
	}
}

// ----- what a definition can't do -----

func TestADefinitionCannotSteerMoneyOrWriteWhatWasNotApproved(t *testing.T) {
	s := newInvoicing(t)

	// No address, header, redirect or secret in a definition: it is refused when it is saved.
	for name, tc := range map[string]struct{ from, to string }{
		"a url in a slot":          {`"kind": "payments", "on"`, `"kind": "payments", "url": "https://example.com", "on"`},
		"a header in a handler":    {`"payment.expired": {"set"`, `"payment.expired": {"headers": {"a": "b"}, "set"`},
		"a redirect in the effect": {`"id_field": "payment_id"}]`, `"id_field": "payment_id", "success_url": "https://example.com"}]`},
		"a url in the effect":      {`"id_field": "payment_id"}]`, `"id_field": "payment_id", "url": "https://example.com"}]`},
		"a secret in the effect":   {`"id_field": "payment_id"}]`, `"id_field": "payment_id", "api_key": "x"}]`},
		"a provider in a slot":     {`"kind": "payments", "on"`, `"kind": "payments", "provider": "stripe", "on"`},
		"a limit in a slot":        {`"kind": "payments", "on"`, `"kind": "payments", "max_amount": 5, "on"`},
	} {
		// The definition has no "payment.expired" handler: the header goes in one that does.
		def := strings.Replace(payInvoiceApp, `"payment.succeeded"`, `"payment.expired": {"set": {"status": {"value": "overdue"}}}, "payment.succeeded"`, 1)
		if !strings.Contains(def, tc.from) {
			t.Fatalf("%s: the definition has no %q", name, tc.from)
		}
		def = strings.Replace(def, tc.from, tc.to, 1)
		status, code := s.code(s.bob.cookie, http.MethodPost, s.appPath(s.team, ""), map[string]any{"name": "Steer", "definition": rawDefinition(def)})
		if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
			t.Errorf("%s: %d %s", name, status, code)
		}
	}
	// None of them is an app.
	if n := s.scalar(`SELECT count(*) FROM apps WHERE name = 'Steer'`); n != 0 {
		t.Fatalf("%d apps were made from refused definitions", n)
	}

	// A change of what a payment writes is a new contract: it can't be published until an admin approves
	// it, and until then the live release's contract is the authority, so a payment writes only what was
	// approved.
	wider := strings.Replace(payInvoiceApp, `"paid_at": {"from": "occurred_at"}`, `"paid_at": {"from": "occurred_at"}, "customer": {"value": "Paid"}`, 1)
	var a struct{ Version int64 }
	app := s.appPath(s.team, fmt.Sprintf("/%d", s.app.app))
	s.mustCall(http.StatusOK, s.bob.cookie, http.MethodGet, app, nil, &a)
	s.mustCall(http.StatusOK, s.bob.cookie, http.MethodPatch, app, map[string]any{"version": a.Version, "definition": rawDefinition(wider)}, &a)
	s.mustCall(http.StatusOK, s.bob.cookie, http.MethodGet, app, nil, &a)
	publishWider := func(when string) {
		t.Helper()
		status, code := s.code(s.bob.cookie, http.MethodPost, s.appPath(s.team, fmt.Sprintf("/%d/publish", s.app.app)), map[string]any{"version": a.Version, "acknowledge": []any{}})
		if status != http.StatusConflict || code != "app_integration_unapproved" {
			t.Fatalf("publishing a contract %s: %d %s", when, status, code)
		}
	}
	publishWider("nobody asked approval for")
	wide := s.app
	s.askApproval(s.bob, &wide)
	publishWider("the admin hasn't decided on")
	record, session := s.sentInvoice(s.bob, s.app, "A-1", 50)
	s.payAndDeliver(s.in, session)
	s.eventually("the invoice is paid", func() bool { return s.record(s.bob, s.app, record).Data["status"] == "paid" })
	if got := s.record(s.bob, s.app, record).Data["customer"]; got != "Acme" {
		t.Fatalf("a payment wrote a field that was not approved: customer = %v", got)
	}
}

// The payee is the credential's, the amount has the admin's ceiling and the currency the admin's list:
// an app can't ask for more, in another currency or for nothing.
func TestAnAppCannotAskForMoreThanTheAdminAllowed(t *testing.T) {
	s := newInvoicing(t)
	for name, tc := range map[string]struct {
		total    any
		currency string
		code     string
	}{
		"above the ceiling": {5000, "EUR", "integration_amount_above_max"},
		"another currency":  {50, "USD", "integration_currency_refused"},
		"too many decimals": {0.001, "EUR", "integration_amount_refused"},
		"nothing":           {0, "EUR", "integration_amount_refused"},
		"a sum of floats":   {floatNoise(), "EUR", "integration_amount_refused"},
	} {
		posts := len(s.sessionPosts())
		if got := s.act(s.bob, s.app, "", "add", map[string]any{"number": name, "total": tc.total, "currency": tc.currency, "status": "draft"}); got != http.StatusOK {
			t.Fatalf("%s: add = %d", name, got)
		}
		var id int64
		for _, r := range s.records(s.bob, s.app) {
			if r.Data["number"] == name {
				id = r.ID
			}
		}
		if got := s.send(s.bob, s.app, id); got != http.StatusOK {
			t.Fatalf("%s: send = %d", name, got)
		}
		s.failsWith(tc.code)
		if n := len(s.sessionPosts()); n != posts {
			t.Fatalf("%s: %d calls to the provider", name, n-posts)
		}
	}
}

// ----- the secret -----

// The keys and secrets are write-only: no answer, no log line and no row of the database holds them
// after they were saved, whether the provider answers or fails.
func TestASecretIsNeverInAnAnswerALogOrARow(t *testing.T) {
	s := newInvoicing(t)

	// Everything an admin and a member can ask, including a change of the secrets and the connection check.
	in := fmt.Sprintf("integrations/%d", s.in.id)
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, "integrations"), nil, nil)
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, in), nil, nil)
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodPost, s.ws(s.one, in+"/check"), map[string]any{}, nil)
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, "integration-approvals"), nil, nil)
	s.mustCall(http.StatusOK, s.bob.cookie, http.MethodGet, fmt.Sprintf("/api/workspaces/%d/spaces/%d/integrations", s.one, s.team), nil, nil)

	// A provider that refuses the key, that fails and that never answered: each ends an entry with a code.
	s.fake.Fail(testprovider.Unauthorized().Times(1))
	record := s.draftInvoice(s.bob, s.app, "A-1", 50)
	if got := s.send(s.bob, s.app, record); got != http.StatusOK {
		t.Fatalf("send = %d", got)
	}
	s.failsWith("integration_credentials_refused")
	s.fake.ClearFaults()
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodPost, s.ws(s.one, in+"/check"), map[string]any{}, nil)

	// A rotation of the webhook secret keeps the old one for an overlap, and shows neither.
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodPatch, s.ws(s.one, in), map[string]any{"secrets": map[string]string{"webhook_secret": hookB}}, nil)
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, in), nil, nil)
	_, session := s.sentInvoice(s.bob, s.app, "A-2", 60)
	evs, err := s.fake.Pay(session)
	if err != nil {
		t.Fatal(err)
	}
	s.deliver(&shop{hook: s.in.hook, secret: hookB, ws: s.one, id: s.in.id}, evs)
	s.settled(s.alice, s.in, 1)
	s.eventsOf(s.alice, s.in)
	// A forged delivery too, and a wrong-secret one.
	if d, err := s.sender(&shop{hook: s.in.hook, secret: keyA}).Send(context.Background(), evs[len(evs)-1]); err != nil || d.Status != http.StatusNotFound {
		t.Fatalf("a delivery signed with the API key: %v %d", err, d.Status)
	}

	answers := s.seen.all()
	if len(answers) < 10 {
		t.Fatalf("only %d answers were kept", len(answers))
	}
	logs := s.logs.String()
	for name, secret := range map[string]string{"the API key": keyA, "the first webhook secret": hookA, "the second": hookB} {
		for _, a := range answers {
			if strings.Contains(a, secret) {
				t.Errorf("an answer holds %s: %.300s", name, a)
			}
		}
		if strings.Contains(logs, secret) {
			t.Errorf("the log holds %s", name)
		}
		// The sealed value isn't the secret: it is in no column of any table.
		if n := s.dbHolds(secret); n != 0 {
			t.Errorf("%s is in %d cells of the database, unsealed", name, n)
		}
		// And the secret's own middle, in case it was cut or quoted.
		if mid := secret[len(secret)/2-4 : len(secret)/2+4]; strings.Contains(logs, mid) && strings.Contains(logs, "api_key") {
			t.Errorf("the log holds a piece of %s", name)
		}
	}
	// The answers show them as "set", never a value.
	var got integrations.Integration
	s.mustCall(http.StatusOK, s.alice.cookie, http.MethodGet, s.ws(s.one, in), nil, &got)
	if got.Secrets["api_key"] != "set" || got.Secrets["webhook_secret"] != "set" {
		t.Fatalf("secrets = %v", got.Secrets)
	}
	// A control: the scan of the answers does find what an answer holds.
	if !strings.Contains(strings.Join(answers, "\n"), "\"shop\"") {
		t.Fatal("the answers kept don't hold the integration's name: the search proves nothing")
	}
}

// floatNoise is 0.30000000000000004: a sum of floats at run time (as a constant it would be exactly 0.3).
func floatNoise() float64 {
	a, b := 0.1, 0.2
	return a + b
}
