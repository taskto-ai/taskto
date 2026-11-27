package integrations

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/integrations/testprovider"
)

type trapStripe struct {
	stripe
	before func()
}

func (p trapStripe) VerifyWebhook(h http.Header, body []byte, secrets WebhookSecrets, now time.Time) error {
	if p.before != nil {
		p.before()
	}
	return p.stripe.VerifyWebhook(h, body, secrets, now)
}

func trap(t *testing.T, before func()) {
	t.Helper()
	for i, p := range registry {
		if p.ID() == "stripe" {
			registry[i] = trapStripe{before: before}
			t.Cleanup(func() { registry[i] = stripe{} })
			return
		}
	}
	t.Fatal("no stripe in the registry")
}

type countingReceiver struct {
	Receiver
	calls   int
	decoyed int
	decoy   Secret
}

func (c *countingReceiver) VerifyWebhook(h http.Header, body []byte, secrets WebhookSecrets, now time.Time) error {
	c.calls++
	if secrets.Current == c.decoy {
		c.decoyed++
	}
	return c.Receiver.VerifyWebhook(h, body, secrets, now)
}

func TestUnsignedFloodsDoNotUseUpAnotherClientsQuota(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()

	from := func(i int) string { return fmt.Sprintf("198.51.%d.%d:4000", i/250, i%250+1) }
	for i := range 3 * hookIDAttempts {
		if rec := r.post(HookPrefix+r.id, []byte("{}"), nil, from(i)); rec.Code != http.StatusNotFound {
			t.Fatalf("unsigned request %d = %d, want 404", i, rec.Code)
		}
	}
	for i := range hookIDAttempts {
		h := http.Header{StripeSignatureHeader: {testprovider.Sign(testprovider.OtherWebhookSecret, r.clock.Now(), ev.Body())}}
		if rec := r.post(HookPrefix+r.id, ev.Body(), h, from(i)); rec.Code != http.StatusNotFound {
			t.Fatalf("badly signed request %d = %d, want 404", i, rec.Code)
		}
	}
	d, err := r.send.Send(context.Background(), ev)
	if err != nil || d.Status != http.StatusOK {
		t.Fatalf("a valid delivery after the floods = %d %s, %v", d.Status, d.Response, err)
	}
	if n := len(r.rows(r.in.ID)); n != 1 {
		t.Fatalf("%d events stored, want 1", n)
	}
}

func TestAWebhookSecretBelongsToOneIntegration(t *testing.T) {
	r := newRig(t)
	_, ws2 := r.otherWorkspace(r.owner)
	notNamed := func(label string, body string) {
		t.Helper()
		if strings.Contains(body, r.id) || strings.Contains(body, r.in.Name) || strings.Contains(body, r.in.DisplayName) {
			t.Errorf("%s: the refusal names the holder: %s", label, body)
		}
	}

	same := r.owner.do(http.MethodPost, wsPath(r.ws, "integrations"), withHook(stripeBody("second"), stripeHook))
	expectCode(t, same, http.StatusConflict, "integration_webhook_secret_reused")
	notNamed("same workspace", same.Body.String())
	other := r.owner.do(http.MethodPost, wsPath(ws2, "integrations"), withHook(stripeBody("elsewhere"), stripeHook))
	expectCode(t, other, http.StatusConflict, "integration_webhook_secret_reused")
	notNamed("another workspace", other.Body.String())
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM integrations`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d integrations after refused creates, want 1 (%v)", n, err)
	}

	second := r.create(r.owner, r.ws, stripeBody("second"))
	path := wsPath(r.ws, fmt.Sprintf("integrations/%d", second.ID))
	rec := r.owner.do(http.MethodPatch, path, map[string]any{"secrets": map[string]string{"webhook_secret": stripeHook}})
	expectCode(t, rec, http.StatusConflict, "integration_webhook_secret_reused")
	notNamed("a change", rec.Body.String())
	creds, err := r.store.credentials(context.Background(), r.ws, second.ID)
	if err != nil || creds.WebhookSecret.Reveal() != hookFor("second") || creds.WebhookSecretPrevious != "" {
		t.Fatalf("a refused change altered the secrets: %v", err)
	}

	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)
	expectCode(t, r.owner.do(http.MethodPatch, path, map[string]any{"secrets": map[string]string{"webhook_secret": stripeHook}}),
		http.StatusConflict, "integration_webhook_secret_reused")
	expectCode(t, r.owner.do(http.MethodPatch, path, map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}),
		http.StatusConflict, "integration_webhook_secret_reused")
	r.clock.Advance(previousOverlap + time.Minute)
	expect(t, r.owner.do(http.MethodPatch, path, map[string]any{"secrets": map[string]string{"webhook_secret": stripeHook}}), http.StatusOK)

	expect(t, r.owner.do(http.MethodPatch, path, map[string]any{"secrets": map[string]string{"webhook_secret": stripeHook}}), http.StatusOK)

	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	secondID := ""
	if err := r.db.QueryRow(`SELECT public_id FROM integrations WHERE id = ?`, second.ID).Scan(&secondID); err != nil {
		t.Fatal(err)
	}
	d, err := r.sender(secondID, stripeHookNew).Send(context.Background(), r.completed())
	if err != nil {
		t.Fatal(err)
	}
	isNotFound(t, "the first's secret against the second", d.Status, d.Response)
}

func TestADeliveryInFlightWhenTheIntegrationChanges(t *testing.T) {
	changes := map[string]func(r *rig){
		"disabled": func(r *rig) {
			expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)), map[string]any{"disabled": true}), http.StatusOK)
		},
		"the secret replaced": func(r *rig) {
			expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
				map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)
		},
		"the mode changed": func(r *rig) {
			expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
				map[string]any{"secrets": map[string]string{"api_key": stripeLiveKey}}), http.StatusOK)
		},
		"removed": func(r *rig) {
			expect(t, r.owner.do(http.MethodDelete, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)), nil), http.StatusNoContent)
		},
	}
	deliveries := map[string]func(r *rig) testprovider.Event{
		"an event to store": func(r *rig) testprovider.Event {
			s, _ := r.session(2500, "eur")
			r.must(r.fake.Pay(s.ID))
			return r.completed()
		},
		"an event that is only noted": func(r *rig) testprovider.Event {
			return testprovider.NewEvent("customer.created", map[string]any{"id": "cus_1", "object": "customer"}, r.clock.Now())
		},
	}
	for change, apply := range changes {
		for delivery, makeEvent := range deliveries {
			t.Run(change+", "+delivery, func(t *testing.T) {
				r := newRig(t)
				ev := makeEvent(r)
				armed := true
				trap(t, func() {
					if armed {
						armed = false
						apply(r)
					}
				})
				d, err := r.send.Send(context.Background(), ev)
				if err != nil {
					t.Fatal(err)
				}
				isNotFound(t, "a delivery overtaken by a change", d.Status, d.Response)
				if armed {
					t.Fatal("the change never ran: the delivery didn't reach the verification")
				}
				if n := r.total(); n != 0 {
					t.Fatalf("%d events stored for a delivery that was in flight during the change", n)
				}
				if change != "removed" && r.lastEvent(r.in.ID).Valid {
					t.Error("last_event_at moved for a delivery that was in flight during the change")
				}
			})
		}
	}

	r := newRig(t)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	trap(t, func() {})
	if d, err := r.send.Send(context.Background(), r.completed()); err != nil || d.Status != http.StatusOK {
		t.Fatalf("an unchanged integration = %d, %v", d.Status, err)
	}
	if len(r.rows(r.in.ID)) != 1 || !r.lastEvent(r.in.ID).Valid {
		t.Fatal("an unchanged integration stored nothing")
	}
}

func TestRefusalsDoTheSameWorkAndLeaveNoLog(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()

	noSecret := stripeBody("nosecret")
	noSecret["secrets"] = map[string]string{"api_key": stripeTestKey}
	_, noSecretID, _ := r.integration(noSecret, stripeHook)
	_, squareID, _ := r.integration(squareBody("squareshop"), stripeHook)
	broken, brokenID, _ := r.integration(stripeBody("broken"), hookFor("broken"))
	if _, err := r.db.Exec(`UPDATE integration_secrets SET sealed = x'00' WHERE integration_id = ? AND name = 'webhook_secret'`, broken.ID); err != nil {
		t.Fatal(err)
	}
	off, offID, _ := r.integration(stripeBody("off"), hookFor("off"))
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", off.ID)), map[string]any{"disabled": true}), http.StatusOK)

	counter := &countingReceiver{Receiver: decoyReceiver, decoy: r.svc.decoy.Current}
	prevDecoy := decoyReceiver
	decoyReceiver = counter
	t.Cleanup(func() { decoyReceiver = prevDecoy })
	real := 0
	trap(t, func() { real++ })

	type refusal struct {
		name  string
		id    string
		key   string
		decoy bool
	}
	cases := []refusal{
		{"unknown id", token(), stripeHook, true},
		{"id of another shape", "not-an-id", stripeHook, true},
		{"uppercase id", strings.ToUpper(r.id), stripeHook, true},
		{"no webhook secret", noSecretID, stripeHook, true},
		{"a provider without webhooks", squareID, stripeHook, true},
		{"secrets that can't be opened", brokenID, hookFor("broken"), true},
		{"disabled", offID, hookFor("off"), false},
		{"bad signature", r.id, testprovider.OtherWebhookSecret, false},
		{"valid signature, stale", r.id, stripeHook, false},
	}
	before := len(r.logs.String())
	for _, c := range cases {
		counter.calls, counter.decoyed, real = 0, 0, 0
		snd := r.sender(c.id, c.key)
		opts := []testprovider.SignOption{}
		if c.name == "valid signature, stale" {
			opts = append(opts, testprovider.OldBy(time.Hour))
		}
		d, err := snd.Send(context.Background(), ev, opts...)
		if err != nil {
			t.Fatal(err)
		}
		isNotFound(t, c.name, d.Status, d.Response)
		if counter.calls+real != 1 {
			t.Errorf("%s: %d verifications, want exactly one", c.name, counter.calls+real)
		}
		if c.decoy && counter.decoyed != 1 {
			t.Errorf("%s: not verified against the server's own secret", c.name)
		}
		if !c.decoy && counter.decoyed != 0 {
			t.Errorf("%s: verified against the decoy, but has secrets of its own", c.name)
		}
	}
	if n := r.total(); n != 0 {
		t.Fatalf("%d events stored from refusals", n)
	}
	if got := r.logs.String()[before:]; got != "" {
		t.Errorf("a refusal wrote to the log: %q", got)
	}
}

func TestAStalledBodyIsCutOff(t *testing.T) {
	old := hookReadTimeout
	hookReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookReadTimeout = old })
	r := newRig(t)
	srv := httptest.NewServer(r.h)
	defer srv.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST %s%s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", HookPrefix, r.id)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no answer for a stalled body within the deadline: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a stalled body = %d, want 400", resp.StatusCode)
	}
	if r.total() != 0 {
		t.Fatal("a stalled body stored an event")
	}
}

func TestAnIntegrationRemovedDuringLookupGetsTheDecoy(t *testing.T) {
	r := newRig(t)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()
	var removed int
	r.svc.afterLookup = func() {
		removed++
		expect(t, r.owner.do(http.MethodDelete, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)), nil), http.StatusNoContent)
	}
	d, err := r.send.Send(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("the seam ran %d times, want once: the removal didn't land between the reads", removed)
	}
	isNotFound(t, "a removed integration", d.Status, d.Response)
	if r.total() != 0 {
		t.Fatal("a delivery to a removed integration stored an event")
	}
}

func TestAStalledBodyOfARefusedRequestIsCutOff(t *testing.T) {
	old := hookReadTimeout
	hookReadTimeout = 200 * time.Millisecond
	t.Cleanup(func() { hookReadTimeout = old })
	r := newRig(t)
	srv := httptest.NewServer(r.h)
	defer srv.Close()
	addr := strings.TrimPrefix(srv.URL, "http://")
	for range hookIPAttempts {
		resp, err := http.Post(srv.URL+HookPrefix+r.id, "application/json", strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST %s%s HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", HookPrefix, r.id)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("an address over its quota = %d, want 429", resp.StatusCode)
	}
	start := time.Now()
	if _, err := br.ReadByte(); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("the connection of a refused request with a stalled body stayed open (%v, %v)", err, time.Since(start))
	}
}
