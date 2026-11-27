package integrations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/apps"
	"taskto.ai/taskto/internal/integrations/testprovider"
	"taskto.ai/taskto/internal/paylink"
)

func retrieval(r *http.Request) bool {
	return r.URL.RawQuery == "" && !strings.HasSuffix(r.URL.Path, "/line_items")
}

func (p *pay) confirm(effect int64) (int, string) {
	p.t.Helper()
	rec := p.owner.do(http.MethodPost, p.confirmPath(effect, fmt.Sprintf("%d/confirm-no-link", effect)), p.checkedBody(effect))
	if rec.Code == http.StatusNoContent {
		return rec.Code, ""
	}
	return rec.Code, decode[struct {
		Code string `json:"code"`
	}](p.t, rec).Code
}

func (p *pay) unresolvedList() []PaymentLinkOutcome {
	p.t.Helper()
	rec := p.owner.do(http.MethodGet, p.confirmPath(0, "unresolved"), nil)
	expect(p.t, rec, http.StatusOK)
	return decode[PaymentLinkOutcomes](p.t, rec).PaymentLinks
}

func (p *pay) refusedConfirmation(e entry, keySeq int) {
	p.t.Helper()
	if status, code := p.confirm(e.id); status != http.StatusConflict || code != string(apierr.CodeIntegrationLinkNotConfirmable) {
		p.t.Fatalf("confirm = %d %q, want 409 %s", status, code, apierr.CodeIntegrationLinkNotConfirmable)
	}
	list := p.unresolvedList()
	if len(list) != 1 || list[0].EffectID != e.id || list[0].CanConfirm {
		p.t.Fatalf("list = %+v: a ref with something seen is offered for confirmation", list)
	}
	if r := p.refs(); len(r) != 1 || r[0].keySeq != keySeq {
		p.t.Fatalf("refs = %+v: the ref moved on", r)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'abandoned'`); n != 0 {
		p.t.Fatalf("%d keys abandoned by a refused confirmation", n)
	}
	if n := p.count(`SELECT count(*) FROM audit_events WHERE action = 'integration_new_link_allowed'`); n != 0 {
		p.t.Fatalf("%d audit events for a refused confirmation", n)
	}
	if es := p.entries(); es[0].state != apps.StateFailed {
		p.t.Fatalf("entry = %+v: queued again", es[0])
	}
}

func TestAnAdminCannotConfirmNoLinkForAKeyWhoseObjectWasSeen(t *testing.T) {
	for name, shape := range linkShapes {
		for _, how := range []string{"an answer named it", "a lookup found it"} {
			t.Run(name+"/"+how, func(t *testing.T) {
				p := newPay(t, shape.public)
				down := testprovider.Unavailable().Forever().On(http.MethodGet, shape.path+"/").When(retrieval)
				if how == "an answer named it" {
					record := p.invoice(10, "EUR")
					p.fake.Fail(down)
					p.startWorker()
					p.send(record, "op-seen-0001")
					p.waitsUnknown()
					if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE object_id IS NOT NULL AND outcome IS NULL`); n != 1 {
						t.Fatal("the answer's object was not kept on its key")
					}
				} else {
					p.unknownOutcome(shape, "op-seen-0002")
					p.fake.Fail(down)
					p.appear(shape.path, 0)
					p.later(time.Minute)
					p.waitFor("the lookup finds it and can't read its state", func() bool {
						for _, r := range p.fake.Requests() {
							if r.Method == http.MethodGet && strings.HasPrefix(r.Path, shape.path+"/") && len(r.Query) == 0 {
								return true
							}
						}
						return false
					})
					p.waitsUnknown()
				}
				p.later(7 * time.Hour)
				e := p.settled(apps.StateFailed)
				if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
					t.Fatalf("entry = %+v", e)
				}
				p.refusedConfirmation(e, 0)
				if keys := p.keysUsed(shape.path); len(keys) != 1 {
					t.Fatalf("calls under %d keys", len(keys))
				}

				p.fake.ClearFaults()
				expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, e.id)), map[string]any{}), http.StatusOK)
				p.settled(apps.StateDone)
				if n := p.objects(shape.path); n != 1 {
					t.Fatalf("%d objects at Stripe, want 1", n)
				}
				if r := p.refs(); len(r) != 1 || r[0].object != p.objectID(shape.path) || r[0].keySeq != 0 {
					t.Fatalf("refs = %+v", r)
				}
			})
		}
	}
}

func TestAnAdminCannotConfirmNoLinkAfterAKeyOfTheRefWasResolved(t *testing.T) {
	ctx := context.Background()
	p := newPay(t, publicURL)
	record := p.invoice(10, "EUR")
	req := p.direct(record, "op-seen-0003")
	p.fake.Fail(testprovider.ConnectionReset().AfterExecuting().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); err == nil {
		t.Fatal("the lost answer was no error")
	}
	if _, err := p.fake.Expire(p.fake.Sessions()[0].ID); err != nil {
		t.Fatal(err)
	}
	p.fake.Fail(testprovider.ServerError().KeptUnderKey().Forever().On(http.MethodPost, "/v1/checkout/sessions"))
	if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationLinkLookupPending) {
		t.Fatalf("err = %v", err)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE seq = 0 AND outcome = 'expired'`); n != 1 {
		t.Fatal("the first key was not resolved as expired")
	}
	p.later(7 * time.Hour)
	if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationOutcomeUnknown) {
		t.Fatalf("err = %v", err)
	}
	p.exec(`UPDATE app_effects SET state = 'failed', error_code = ?`, string(apierr.CodeIntegrationOutcomeUnknown))
	e := p.entries()[0]
	if status, code := p.confirm(e.id); status != http.StatusConflict || code != string(apierr.CodeIntegrationLinkNotConfirmable) {
		t.Fatalf("confirm = %d %q", status, code)
	}
	if list := p.unresolvedList(); len(list) != 1 || list[0].CanConfirm {
		t.Fatalf("list = %+v", list)
	}
	if r := p.refs(); r[0].keySeq != 1 {
		t.Fatalf("refs = %+v", r)
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'abandoned'`); n != 0 {
		t.Fatalf("%d keys abandoned", n)
	}
	if keys := p.keysUsed("/v1/checkout/sessions"); len(keys) != 2 {
		t.Fatalf("calls under %d keys, want 2", len(keys))
	}
}

func TestARefNeverMovesOnFromAnAbandonedKeyWithAnObjectOnRecord(t *testing.T) {
	for name, shape := range linkShapes {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			p := newPay(t, shape.public)
			record := p.invoice(10, "EUR")
			req := p.direct(record, "op-seen-0004")
			p.fake.Fail(testprovider.Unavailable().On(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationLinkLookupPending) {
				t.Fatalf("err = %v", err)
			}
			p.exec(`UPDATE integration_ref_attempts SET outcome = 'abandoned', object_id = 'cs_test_seen', url = 'https://checkout.stripe.com/c/pay/cs_test_seen'`)
			before := len(p.fake.RequestsTo(http.MethodPost, shape.path))
			if _, err := p.svc.OpenPaymentLink(ctx, req); !hasCode(err, apierr.CodeIntegrationOutcomeUnknown) {
				t.Fatalf("err = %v", err)
			}
			if n := len(p.fake.RequestsTo(http.MethodPost, shape.path)); n != before {
				t.Fatalf("%d calls after the abandoned key, want none", n-before)
			}
			if r := p.refs(); r[0].keySeq != 0 {
				t.Fatalf("refs = %+v: moved on", r)
			}
		})
	}
}

func hasCode(err error, code apierr.Code) bool {
	var pe *paylink.Error
	return errors.As(err, &pe) && pe.Code == code
}

func (p *pay) others(path string, n int) {
	p.t.Helper()
	first := p.fake.RequestsTo(http.MethodPost, path)[0].Form
	for i := range n {
		form := url.Values{}
		for k, v := range first {
			form[k] = append([]string(nil), v...)
		}
		other := fmt.Sprintf("%032x", i+1)
		if form.Has("client_reference_id") {
			form.Set("client_reference_id", other)
		} else {
			form.Set("metadata["+stripeRefMetadata+"]", other)
		}
		p.mustPost(path, form)
	}
}

func TestEveryFailedReadOfTheReconciliationStaysOnItsSchedule(t *testing.T) {
	type read struct {
		setup  func(p *pay, path string)
		fault  func(path string) testprovider.Fault
		reads  func(p *pay, path string) int
		hosted bool
	}
	gets := func(p *pay, path string) int { return len(p.fake.RequestsTo(http.MethodGet, path)) }
	getsUnder := func(p *pay, prefix string, cond func(testprovider.Request) bool) int {
		n := 0
		for _, r := range p.fake.Requests() {
			if r.Method == http.MethodGet && strings.HasPrefix(r.Path, prefix) && cond(r) {
				n++
			}
		}
		return n
	}
	reads := map[string]read{
		"the account": {
			fault: func(string) testprovider.Fault { return testprovider.Fault{}.On(http.MethodGet, "/v1/account") },
			reads: func(p *pay, _ string) int { return gets(p, "/v1/account") },
		},
		"the lookup": {
			fault: func(path string) testprovider.Fault { return testprovider.Fault{}.On(http.MethodGet, path) },
			reads: func(p *pay, path string) int { return gets(p, path) },
		},
		"the lookup's next page": {
			setup: func(p *pay, path string) { p.others(path, findPageSize+1) },
			fault: func(path string) testprovider.Fault {
				return testprovider.Fault{}.On(http.MethodGet, path).When(func(r *http.Request) bool { return r.URL.Query().Has("starting_after") })
			},
			reads: func(p *pay, path string) int {
				return getsUnder(p, path, func(r testprovider.Request) bool { return r.Path == path && r.Query.Has("starting_after") })
			},
		},
		"a link's line items": {
			hosted: true,
			setup:  func(p *pay, path string) { p.appear(path, 0) },
			fault: func(path string) testprovider.Fault {
				return testprovider.Fault{}.On(http.MethodGet, path+"/").When(func(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/line_items") })
			},
			reads: func(p *pay, path string) int {
				return getsUnder(p, path+"/", func(r testprovider.Request) bool { return strings.HasSuffix(r.Path, "/line_items") })
			},
		},
		"the object's state": {
			setup: func(p *pay, path string) { p.appear(path, 0) },
			fault: func(path string) testprovider.Fault {
				return testprovider.Fault{}.On(http.MethodGet, path+"/").When(retrieval)
			},
			reads: func(p *pay, path string) int {
				return getsUnder(p, path+"/", func(r testprovider.Request) bool {
					return len(r.Query) == 0 && !strings.HasSuffix(r.Path, "/line_items")
				})
			},
		},
	}
	failures := map[string]func(testprovider.Fault) testprovider.Fault{
		"the key refused": func(f testprovider.Fault) testprovider.Fault {
			u := testprovider.Unauthorized()
			f.Status, f.Type, f.Code, f.Message = u.Status, u.Type, u.Code, u.Message
			return f.Forever()
		},
		"the provider down": func(f testprovider.Fault) testprovider.Fault {
			u := testprovider.Unavailable()
			f.Status, f.Type, f.Code, f.Message = u.Status, u.Type, u.Code, u.Message
			return f.Forever()
		},
	}
	for shapeName, shape := range linkShapes {
		for readName, rd := range reads {
			if rd.hosted && shape.public != "" {
				continue
			}
			for failName, failure := range failures {
				t.Run(shapeName+"/"+readName+"/"+failName, func(t *testing.T) {
					p := newPay(t, shape.public)
					p.unknownOutcome(shape, "op-read-0001")
					if rd.setup != nil {
						rd.setup(p, shape.path)
					}
					objects := p.objects(shape.path)
					p.fake.Fail(failure(rd.fault(shape.path)))
					for i := range 3 {
						before := rd.reads(p, shape.path)
						p.later(31 * time.Minute)
						p.waitFor(fmt.Sprintf("try %d reads and waits", i), func() bool {
							es := p.entries()
							return rd.reads(p, shape.path) > before && es[0].state == "pending" && es[0].code == string(apierr.CodeIntegrationLinkLookupPending)
						})
						if es := p.entries(); es[0].attempts != 0 {
							t.Fatalf("try %d used up the entry's tries: %+v", i, es[0])
						}
					}
					p.later(7 * time.Hour)
					e := p.settled(apps.StateFailed)
					if e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
						t.Fatalf("entry = %+v, want %s", e, apierr.CodeIntegrationOutcomeUnknown)
					}
					if keys := p.keysUsed(shape.path); len(keys) != 1 {
						t.Fatalf("calls under %d keys, want the first only", len(keys))
					}
					if n := p.objects(shape.path); n != objects {
						t.Fatalf("%d objects at Stripe, want %d", n, objects)
					}
				})
			}
		}
	}
}

func TestAnAdminsConfirmationIsBoundToTheAttemptTheyChecked(t *testing.T) {
	shape := linkShapes["a checkout session"]
	p := newPay(t, shape.public)
	p.unknownOutcome(shape, "op-bound-0001")
	p.later(time.Minute)
	p.looked(shape.path, 1)
	p.later(7 * time.Hour)
	e := p.settled(apps.StateFailed)
	list := p.unresolvedList()
	if len(list) != 1 || !list[0].CanConfirm || list[0].AttemptSeq != 0 || list[0].AttemptSends < 1 {
		t.Fatalf("list = %+v", list)
	}
	checked := list[0]

	expect(t, p.owner.do(http.MethodPost, p.appsPath(fmt.Sprintf("/%d/effects/%d/retry", p.app.ID, e.id)), map[string]any{}), http.StatusOK)
	p.waitFor("another call under the key", func() bool {
		return p.count(`SELECT sends FROM integration_ref_attempts WHERE seq = 0`) > checked.AttemptSends
	})
	if e = p.settled(apps.StateFailed); e.code != string(apierr.CodeIntegrationOutcomeUnknown) {
		t.Fatalf("entry = %+v", e)
	}

	confirm := func(seq, sends any) (int, string) {
		body := map[string]any{}
		if seq != nil {
			body["attempt_seq"] = seq
		}
		if sends != nil {
			body["attempt_sends"] = sends
		}
		rec := p.owner.do(http.MethodPost, p.confirmPath(e.id, fmt.Sprintf("%d/confirm-no-link", e.id)), body)
		if rec.Code == http.StatusNoContent {
			return rec.Code, ""
		}
		return rec.Code, decode[struct {
			Code string `json:"code"`
		}](t, rec).Code
	}
	for name, c := range map[string][2]any{
		"what the admin checked": {checked.AttemptSeq, checked.AttemptSends},
		"no attempt named":       {nil, nil},
		"another attempt":        {checked.AttemptSeq + 1, checked.AttemptSends + 1},
	} {
		if status, code := confirm(c[0], c[1]); status != http.StatusConflict || code != string(apierr.CodeIntegrationLinkAttemptChanged) {
			t.Fatalf("%s: confirm = %d %q", name, status, code)
		}
	}
	if n := p.count(`SELECT count(*) FROM integration_ref_attempts WHERE outcome = 'abandoned'`); n != 0 {
		t.Fatalf("%d keys abandoned by a stale confirmation", n)
	}
	if r := p.refs(); len(r) != 1 || r[0].keySeq != 0 {
		t.Fatalf("refs = %+v: the ref moved on", r)
	}

	now := p.unresolvedList()
	if len(now) != 1 || now[0].AttemptSeq != 0 || now[0].AttemptSends != checked.AttemptSends+1 {
		t.Fatalf("list = %+v", now)
	}
	if status, code := confirm(now[0].AttemptSeq, now[0].AttemptSends); status != http.StatusNoContent {
		t.Fatalf("confirm = %d %q", status, code)
	}
}

func (p *pay) checkedAttempt(effect int64) CheckedAttempt {
	p.t.Helper()
	var seq, sends int
	err := p.db.QueryRow(`SELECT k.seq, k.sends FROM integration_refs r
		JOIN integration_ref_attempts k ON k.ref_id = r.id AND k.seq = r.key_seq WHERE r.effect_id = ?`, effect).Scan(&seq, &sends)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckedAttempt{Seq: &seq, Sends: &sends}
	}
	if err != nil {
		p.t.Fatal(err)
	}
	return CheckedAttempt{Seq: &seq, Sends: &sends}
}

func (p *pay) checkedBody(effect int64) map[string]any {
	p.t.Helper()
	c := p.checkedAttempt(effect)
	return map[string]any{"attempt_seq": *c.Seq, "attempt_sends": *c.Sends}
}
