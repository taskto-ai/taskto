package integrations

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/integrations/testprovider"
)

// shareSecret gives integration in the database a webhook secret directly, as a database from
// before the route refused reuse can hold one: it is sealed for the integration with no check and
// no fingerprint, and the server then gives it the one it gives secrets at start.
func (r *rig) shareSecret(in Integration, id, secret string) {
	r.t.Helper()
	ctx := context.Background()
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		r.t.Fatal(err)
	}
	ref := sealRef{workspace: r.ws, id: in.ID, publicID: id}
	if err := r.store.seal(ctx, tx, ref, SecretWebhookSecret, secret, nil); err != nil {
		r.t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integration_secrets SET fingerprint = NULL WHERE integration_id = ?`, in.ID); err != nil {
		r.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		r.t.Fatal(err)
	}
	if err := r.store.BackfillFingerprints(ctx); err != nil {
		r.t.Fatal(err)
	}
}

func (r *rig) checkOf(id int64) CheckResult {
	r.t.Helper()
	rec := r.owner.do(http.MethodPost, wsPath(r.ws, fmt.Sprintf("integrations/%d/check", id)), nil)
	expect(r.t, rec, http.StatusOK)
	if strings.Contains(rec.Body.String(), "stripe") || strings.Contains(rec.Body.String(), "second") {
		r.t.Errorf("a check names an integration: %s", rec.Body.String())
	}
	return decode[CheckResult](r.t, rec)
}

// A webhook secret two integrations hold (an old install can have it) verifies a delivery for
// neither, whichever workspace they are in: the delivery stores nothing and is the plain 404, the
// check tells admins without saying whose it is, and once one of them rotates the other still refuses
// (the secret is spent) until it has a fresh one too.
func TestADuplicatedWebhookSecretVerifiesNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, ws2 := r.otherWorkspace(r.owner)
	// The second integration is in another workspace, as the worst case is.
	rec := r.owner.do(http.MethodPost, wsPath(ws2, "integrations"), stripeBody("elsewhere"))
	expect(t, rec, http.StatusCreated)
	second := decode[Integration](t, rec)
	secondID := strings.TrimPrefix(*second.WebhookURL, hookOrigin+HookPrefix)
	third, _, thirdSend := r.integration(stripeBody("third"), hookFor("third"))

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.store.seal(ctx, tx, sealRef{workspace: ws2, id: second.ID, publicID: secondID}, SecretWebhookSecret, stripeHook, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE integration_secrets SET fingerprint = NULL`); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var unfingerprinted int
	if err := r.db.QueryRow(`SELECT count(*) FROM integration_secrets WHERE fingerprint IS NULL AND name = 'webhook_secret'`).Scan(&unfingerprinted); err != nil || unfingerprinted == 0 {
		t.Fatalf("no secret without a fingerprint to fill in (%d, %v)", unfingerprinted, err)
	}
	if err := r.store.BackfillFingerprints(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.db.QueryRow(`SELECT count(*) FROM integration_secrets WHERE fingerprint IS NULL AND name = 'webhook_secret'`).Scan(&unfingerprinted); err != nil || unfingerprinted != 0 {
		t.Fatalf("%d secrets still without a fingerprint after the backfill (%v)", unfingerprinted, err)
	}

	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()
	secondSend := r.sender(secondID, stripeHook)
	for label, send := range map[string]*testprovider.Sender{"first": r.send, "second": secondSend} {
		d, err := send.Send(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		isNotFound(t, "a delivery signed with a secret two integrations hold, to the "+label, d.Status, d.Response)
	}
	if n := r.total(); n != 0 {
		t.Fatalf("%d events stored for a shared secret, want none", n)
	}
	if at := r.lastEvent(r.in.ID); at.Valid {
		t.Fatal("a refused delivery noted an event on the integration")
	}
	// Every other secret still works.
	if d, err := thirdSend.Send(ctx, ev); err != nil || d.Status != http.StatusOK {
		t.Fatalf("an integration with a secret of its own = %v, %v", d.Status, err)
	}

	// The check says so, to the admin of each, without saying whose.
	if got := r.checkOf(r.in.ID); got.OK || got.Code != "integration_webhook_secret_shared" || got.Params["secret"] != SecretWebhookSecret {
		t.Fatalf("check of an integration with a shared secret = %+v", got)
	}
	rec = r.owner.do(http.MethodPost, wsPath(ws2, fmt.Sprintf("integrations/%d/check", second.ID)), nil)
	if got := decode[CheckResult](t, rec); got.OK || got.Code != "integration_webhook_secret_shared" {
		t.Fatalf("check of the other integration = %+v", got)
	}
	if got := r.checkOf(third.ID); !got.OK {
		t.Fatalf("check of an integration with a secret of its own = %+v", got)
	}

	// One of them rotates. The secret was found shared, so it is spent for both: the other
	// integration, which now is the only one that holds it, still refuses, and says so.
	expect(t, r.owner.do(http.MethodPatch, wsPath(ws2, fmt.Sprintf("integrations/%d", second.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)
	var previous int
	if err := r.db.QueryRow(`SELECT count(*) FROM integration_secrets WHERE integration_id = ? AND name = 'webhook_secret_previous'`,
		second.ID).Scan(&previous); err != nil || previous != 0 {
		t.Fatalf("a shared secret was kept as the previous one (%d, %v)", previous, err)
	}
	if d, err := r.send.Send(ctx, ev); err != nil {
		t.Fatal(err)
	} else {
		isNotFound(t, "the first integration after the other rotated, still with the shared secret", d.Status, d.Response)
	}
	if n := len(r.rows(r.in.ID)); n != 0 {
		t.Fatalf("%d events stored with a secret that was shared", n)
	}
	if got := r.checkOf(r.in.ID); got.OK || got.Code != "integration_webhook_secret_shared" {
		t.Fatalf("check of the first integration after the other rotated = %+v", got)
	}
	rotated := r.sender(secondID, stripeHookNew)
	if d, err := rotated.Send(ctx, ev); err != nil || d.Status != http.StatusOK {
		t.Fatalf("the rotated integration with its new secret = %v, %v", d.Status, err)
	}
	if d, err := secondSend.Send(ctx, ev); err != nil {
		t.Fatal(err)
	} else {
		isNotFound(t, "the rotated integration with its old secret", d.Status, d.Response)
	}
	// Nobody may be given the spent secret again, even the integration that still holds it.
	expectCode(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHook}}), http.StatusConflict, "integration_webhook_secret_reused")
	expectCode(t, r.owner.do(http.MethodPatch, wsPath(ws2, fmt.Sprintf("integrations/%d", second.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHook}}), http.StatusConflict, "integration_webhook_secret_reused")
	expectCode(t, r.owner.do(http.MethodPost, wsPath(r.ws, "integrations"), withHook(stripeBody("again"), stripeHook)),
		http.StatusConflict, "integration_webhook_secret_reused")

	// The first one gets a fresh secret from its provider too: now it works, with that one only.
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": hookFor("fresh")}}), http.StatusOK)
	if err := r.db.QueryRow(`SELECT count(*) FROM integration_secrets WHERE integration_id = ? AND name = 'webhook_secret_previous'`,
		r.in.ID).Scan(&previous); err != nil || previous != 0 {
		t.Fatalf("a spent secret was kept as the previous one (%d, %v)", previous, err)
	}
	if d, err := r.send.Send(ctx, ev); err != nil {
		t.Fatal(err)
	} else {
		isNotFound(t, "the first integration with the spent secret after its rotation", d.Status, d.Response)
	}
	if d, err := r.sender(strings.TrimPrefix(*r.in.WebhookURL, hookOrigin+HookPrefix), hookFor("fresh")).Send(ctx, ev); err != nil || d.Status != http.StatusOK {
		t.Fatalf("the first integration with its fresh secret = %v, %v", d.Status, err)
	}
	if n := len(r.rows(r.in.ID)); n != 1 {
		t.Fatalf("%d events stored for the first integration, want 1", n)
	}
	if got := r.checkOf(r.in.ID); !got.OK {
		t.Fatalf("check after both rotated = %+v", got)
	}
}

// Deleting one of two integrations that share a secret doesn't make the survivor the only holder:
// it keeps refusing until it has a fresh secret.
func TestASecretFoundSharedStaysSpentWhenAHolderIsRemoved(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	other, otherID, _ := r.integration(stripeBody("other"), hookFor("other"))
	r.shareSecret(other, otherID, stripeHook)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()
	expect(t, r.owner.do(http.MethodDelete, wsPath(r.ws, fmt.Sprintf("integrations/%d", other.ID)), nil), http.StatusNoContent)
	if d, err := r.send.Send(ctx, ev); err != nil {
		t.Fatal(err)
	} else {
		isNotFound(t, "the survivor of a removed holder", d.Status, d.Response)
	}
	if got := r.checkOf(r.in.ID); got.OK || got.Code != "integration_webhook_secret_shared" {
		t.Fatalf("check of the survivor = %+v", got)
	}
}

// A database that isn't started again since it was found to hold a secret twice is still safe: the
// first change to either integration records the secret before it takes a copy away.
func TestARotationRecordsASharedSecretNotYetFound(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	other, otherID, _ := r.integration(stripeBody("other"), hookFor("other"))
	r.shareSecret(other, otherID, stripeHook)
	if _, err := r.db.Exec(`DELETE FROM integration_shared_secrets`); err != nil {
		t.Fatal(err)
	}
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", other.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	if d, err := r.send.Send(ctx, r.completed()); err != nil {
		t.Fatal(err)
	} else {
		isNotFound(t, "the integration still holding a secret found shared at the other's rotation", d.Status, d.Response)
	}
}

// The backfill that every create and rotation runs reads only the rows that wait for a fingerprint,
// through their partial index, and not the table.
func TestTheBackfillUsesItsPartialIndex(t *testing.T) {
	r := newRig(t)
	rows, err := r.db.Query(`EXPLAIN QUERY PLAN ` + backfillQuery)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, "\n")
	if !strings.Contains(joined, "integration_secrets_unfingerprinted") || strings.Contains(joined, "SCAN s") && !strings.Contains(joined, "USING") {
		t.Fatalf("the backfill's plan doesn't use its index:\n%s", joined)
	}
	// Nothing waits for a fingerprint, so it fills nothing in.
	tx, err := r.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if n, err := r.store.backfillFingerprints(context.Background(), tx); err != nil || n != 0 {
		t.Fatalf("backfill with nothing missing = %d, %v", n, err)
	}
}

// A secret that is a previous one of an integration, still in its overlap, is held too: a second
// integration given it directly can't verify with it, nor can the first.
func TestADuplicatedPreviousWebhookSecretVerifiesNothing(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)
	other, otherID, _ := r.integration(stripeBody("other"), hookFor("other"))
	// The first one's replaced secret is in its overlap: it still verifies there.
	s, _ := r.session(2500, "eur")
	r.must(r.fake.Pay(s.ID))
	ev := r.completed()
	if d, err := r.send.Send(ctx, ev); err != nil || d.Status != http.StatusOK {
		t.Fatalf("the replaced secret inside the overlap = %v, %v", d.Status, err)
	}
	r.shareSecret(other, otherID, stripeHook)
	otherSend := r.sender(otherID, stripeHook)
	for label, send := range map[string]*testprovider.Sender{"first": r.send, "other": otherSend} {
		d, err := send.Send(ctx, ev)
		if err != nil {
			t.Fatal(err)
		}
		isNotFound(t, "the shared previous secret, to the "+label, d.Status, d.Response)
	}
	// The current secret of the first integration is its own.
	if d, err := r.sender(strings.TrimPrefix(*r.in.WebhookURL, hookOrigin+HookPrefix), stripeHookNew).Send(ctx, ev); err != nil || d.Status != http.StatusOK {
		t.Fatalf("the first integration's current secret = %v, %v", d.Status, err)
	}
	if got := r.checkOf(r.in.ID); got.Code != "integration_webhook_secret_shared" || got.Params["secret"] != SecretWebhookSecretPrevious {
		t.Fatalf("check of an integration whose previous secret is shared = %+v", got)
	}
}

// The fingerprint beside a webhook secret is a keyed MAC of it: not the secret, not a plain
// hash, and the same for the same secret under the same key.
func TestWebhookSecretFingerprintsAreKeyed(t *testing.T) {
	r := newRig(t)
	var fp []byte
	if err := r.db.QueryRow(`SELECT fingerprint FROM integration_secrets WHERE integration_id = ? AND name = 'webhook_secret'`, r.in.ID).Scan(&fp); err != nil {
		t.Fatal(err)
	}
	if string(fp) != string(r.store.webhookFingerprint(stripeHook)) {
		t.Fatal("the stored fingerprint isn't the keyed MAC of the secret")
	}
	if strings.Contains(string(fp), stripeHook) {
		t.Fatal("the fingerprint holds the secret")
	}
	var n int
	if err := r.db.QueryRow(`SELECT count(*) FROM integration_secrets WHERE name = 'api_key' AND fingerprint IS NOT NULL`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("%d API keys have a fingerprint (%v)", n, err)
	}
}

// Whatever the integration behind a delivery's id, a well-formed signature costs the same number
// of MACs over the body, so the work says nothing of which ids exist or what they hold.
func TestEveryWellFormedSignatureCostsTheSameMACs(t *testing.T) {
	body := []byte(`{"id":"evt_1","object":"event"}`)
	good := sig(stripeHook, hookNow, string(body))
	wrong := sig(stripeHookNew, hookNow, string(body))
	var count int
	macHook = func() { count++ }
	t.Cleanup(func() { macHook = nil })
	cases := []struct {
		name    string
		h       http.Header
		secrets WebhookSecrets
		want    int
	}{
		{"both secrets, wrong signature", wrong, WebhookSecrets{Current: Secret(stripeHook + "x"), Previous: Secret(stripeHook + "y")}, 2},
		{"both secrets, valid signature", good, WebhookSecrets{Current: Secret(stripeHook), Previous: Secret(stripeHookNew)}, 2},
		{"previous matches", good, WebhookSecrets{Current: Secret(stripeHookNew), Previous: Secret(stripeHook)}, 2},
		{"one secret, wrong signature", wrong, WebhookSecrets{Current: Secret(stripeHook)}, 2},
		{"only a previous one", wrong, WebhookSecrets{Previous: Secret(stripeHook)}, 2},
		{"no secret", wrong, WebhookSecrets{}, 2},
		{"a header that is malformed", http.Header{StripeSignatureHeader: {"t=1"}}, WebhookSecrets{Current: Secret(stripeHook)}, 0},
	}
	for _, c := range cases {
		count = 0
		_ = stripe{}.VerifyWebhook(c.h, body, c.secrets, hookNow)
		if count != c.want {
			t.Errorf("%s: %d MACs, want %d", c.name, count, c.want)
		}
	}
	// A slot with no secret never matches, even a signature made with the empty key.
	if err := (stripe{}).VerifyWebhook(sig("", hookNow, string(body)), body, WebhookSecrets{}, hookNow); err == nil {
		t.Error("a delivery verified with no secret")
	}
}

// The same through the route: an id in a rotation's overlap, one with a single secret, a disabled
// one, one whose secret is shared, one that doesn't exist and one that isn't an id at all each run
// the same number of MACs for a well-formed signature that doesn't verify.
func TestTheRouteDoesTheSameMACsForEveryID(t *testing.T) {
	r := newRig(t)
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", r.in.ID)),
		map[string]any{"secrets": map[string]string{"webhook_secret": stripeHookNew}}), http.StatusOK)
	_, single, _ := r.integration(stripeBody("single"), hookFor("single"))
	off, offID, _ := r.integration(stripeBody("off"), hookFor("off"))
	expect(t, r.owner.do(http.MethodPatch, wsPath(r.ws, fmt.Sprintf("integrations/%d", off.ID)), map[string]any{"disabled": true}), http.StatusOK)
	dup, dupID, _ := r.integration(stripeBody("dup"), hookFor("dup"))
	r.shareSecret(dup, dupID, hookFor("single"))

	body := []byte(`{"id":"evt_1","object":"event"}`)
	h := sig(stripeHookNew+"-not-it", r.clock.Now(), string(body))
	var count int
	macHook = func() { count++ }
	t.Cleanup(func() { macHook = nil })
	ids := map[string]string{
		"overlap":    strings.TrimPrefix(*r.in.WebhookURL, hookOrigin+HookPrefix),
		"single":     single,
		"disabled":   offID,
		"shared":     dupID,
		"unknown":    strings.Repeat("0", 32),
		"not an id":  "nope",
		"too long":   strings.Repeat("a", 200),
		"upper case": strings.ToUpper(single),
	}
	for name, id := range ids {
		count = 0
		rec := r.post(HookPrefix+id, body, h, "")
		isNotFound(t, name, rec.Code, rec.Body.Bytes())
		if count != verifyKeys {
			t.Errorf("%s: %d MACs, want %d", name, count, verifyKeys)
		}
	}
}

// A secret saved before fingerprints existed is still refused for a second integration: the write
// gives it its fingerprint first, so reuse is found whether or not the server restarted since.
func TestAnOldSecretWithoutAFingerprintIsStillRefused(t *testing.T) {
	r := newRig(t)
	if _, err := r.db.Exec(`UPDATE integration_secrets SET fingerprint = NULL`); err != nil {
		t.Fatal(err)
	}
	expectCode(t, r.owner.do(http.MethodPost, wsPath(r.ws, "integrations"), withHook(stripeBody("second"), stripeHook)),
		http.StatusConflict, "integration_webhook_secret_reused")
}

// The padding key of an empty slot does the work of a key and authenticates nothing: a delivery
// signed with it, were it known, is refused, with one secret configured or none, while the same
// number of MACs are computed.
func TestThePaddingKeyAuthenticatesNothing(t *testing.T) {
	body := []byte(`{"id":"evt_1","object":"event"}`)
	known := "padding-key-known-to-the-test"
	old := verifyPad
	verifyPad = []byte(known)
	t.Cleanup(func() { verifyPad = old })
	var count int
	macHook = func() { count++ }
	t.Cleanup(func() { macHook = nil })
	forged := sig(known, hookNow, string(body))
	for name, secrets := range map[string]WebhookSecrets{
		"no secret":       {},
		"one secret":      {Current: Secret(stripeHook)},
		"only a previous": {Previous: Secret(stripeHook)},
		"two secrets":     {Current: Secret(stripeHook), Previous: Secret(stripeHookNew)},
	} {
		count = 0
		if err := (stripe{}).VerifyWebhook(forged, body, secrets, hookNow); err == nil {
			t.Errorf("%s: a delivery signed with the padding key verified", name)
		}
		if count != verifyKeys {
			t.Errorf("%s: %d MACs, want %d", name, count, verifyKeys)
		}
	}
	// The configured secrets still verify.
	if err := (stripe{}).VerifyWebhook(sig(stripeHook, hookNow, string(body)), body, WebhookSecrets{Current: Secret(stripeHook)}, hookNow); err != nil {
		t.Errorf("a delivery signed with the configured secret = %v", err)
	}
}
