package testprovider

import (
	"cmp"
	"encoding/json"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// eventRetention is how far back GET /v1/events reaches.
const eventRetention = 30 * 24 * time.Hour

// emit records an event of type typ about obj, created at time at. The caller holds s.mu.
func (s *Stripe) emit(typ string, obj any, at time.Time) *Event {
	raw, err := json.Marshal(obj)
	if err != nil {
		panic("testprovider: event object doesn't marshal: " + err.Error())
	}
	ev := &Event{
		ID: "evt_" + randomID(24), Object: "event", APIVersion: s.version, Created: at.Unix(),
		Data: EventData{Object: raw}, Livemode: s.live, PendingWebhooks: 1, Type: typ,
	}
	s.events = append(s.events, ev)
	return ev
}

// Events returns every event the fake has made, oldest first, as copies.
func (s *Stripe) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	for i, e := range s.events {
		out[i] = e.clone()
	}
	// Oldest first by creation time: a session that expired by the clock is recorded when it is
	// noticed, which may be after a later-made event. Equal times keep the order of making.
	slices.SortStableFunc(out, func(a, b Event) int { return cmp.Compare(a.Created, b.Created) })
	return out
}

// EventsOfType returns the events of one type, oldest first.
func (s *Stripe) EventsOfType(typ string) []Event {
	var out []Event
	for _, e := range s.Events() {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

var eventListParams = compileAll([]string{
	`type`, `types\[\d*\]`, `limit`, `starting_after`, `ending_before`,
	`created`, `created\[(gt|gte|lt|lte)\]`,
})

func (s *Stripe) listEvents(_ *http.Request, form url.Values, _ string) reply {
	if rp := unknownParam(form, eventListParams); rp != nil {
		return *rp
	}
	var types []string // names that must match exactly: Stripe's `types` takes specific event names
	for k, v := range form {
		if strings.HasPrefix(k, "types[") {
			types = append(types, v...)
		}
	}
	var pattern string // `type` takes a name or a prefix followed by *
	if form.Get("type") != "" {
		if len(types) > 0 {
			return invalid("", "You may pass one of `type` and `types`, not both.", "type")
		}
		pattern = form.Get("type")
	}
	if len(types) > 20 {
		return invalid("", "You can pass at most 20 types.", "types")
	}
	bound := func(name string) (int64, bool, *reply) {
		v := form.Get("created[" + name + "]")
		if v == "" {
			return 0, false, nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0, false, badInteger("created["+name+"]", v)
		}
		return n, true, nil
	}
	var gt, gte, lt, lte int64
	var hasGT, hasGTE, hasLT, hasLTE bool
	for _, b := range []struct {
		name string
		v    *int64
		has  *bool
	}{{"gt", &gt, &hasGT}, {"gte", &gte, &hasGTE}, {"lt", &lt, &hasLT}, {"lte", &lte, &hasLTE}} {
		var rp *reply
		if *b.v, *b.has, rp = bound(b.name); rp != nil {
			return *rp
		}
	}
	var exact int64
	var hasExact bool
	if v := form.Get("created"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return *badInteger("created", v)
		}
		exact, hasExact = n, true
	}

	floor := s.clock.Now().Add(-eventRetention).Unix()
	var eligible []*Event
	for i := len(s.events) - 1; i >= 0; i-- {
		e := s.events[i]
		switch {
		case e.Created < floor,
			len(types) > 0 && !slices.Contains(types, e.Type),
			pattern != "" && !matchesPattern(pattern, e.Type),
			hasGT && e.Created <= gt, hasGTE && e.Created < gte,
			hasLT && e.Created >= lt, hasLTE && e.Created > lte,
			hasExact && e.Created != exact:
			continue
		}
		eligible = append(eligible, e)
	}
	// Newest first by creation time; events made at the same time keep the later one first. A
	// session that expired by the clock is recorded when it is noticed, not when it expired, so
	// the order of making isn't always the order of time.
	slices.SortStableFunc(eligible, func(a, b *Event) int { return cmp.Compare(b.Created, a.Created) })
	byID := make(map[string]*Event, len(eligible))
	ids := make([]string, 0, len(eligible))
	for _, e := range eligible {
		byID[e.ID] = e
		ids = append(ids, e.ID)
	}
	page, more, rp := paginate(ids, form, "event")
	if rp != nil {
		return *rp
	}
	data := make([]*Event, 0, len(page))
	for _, id := range page {
		data = append(data, byID[id])
	}
	return jsonReply(http.StatusOK, listOf("/v1/events", more, data))
}

// matchesPattern is Stripe's `type` filter: an exact name, or a prefix followed by a * wildcard.
func matchesPattern(pattern, typ string) bool {
	if pattern == typ {
		return true
	}
	prefix, ok := strings.CutSuffix(pattern, "*")
	return ok && strings.HasPrefix(typ, prefix)
}

func (s *Stripe) retrieveEvent(_ *http.Request, form url.Values, id string) reply {
	if rp := unknownParam(form, nil); rp != nil {
		return *rp
	}
	floor := s.clock.Now().Add(-eventRetention).Unix()
	for _, e := range s.events {
		if e.ID == id && e.Created >= floor {
			return jsonReply(http.StatusOK, e)
		}
	}
	return noSuch("event", id, "id")
}

// balance is the one harmless authenticated call a connection check makes.
func (s *Stripe) balance(_ *http.Request, form url.Values, _ string) reply {
	if rp := unknownParam(form, nil); rp != nil {
		return *rp
	}
	money := []map[string]any{{"amount": 0, "currency": "usd", "source_types": map[string]int{"card": 0}}}
	return jsonReply(http.StatusOK, map[string]any{
		"object": "balance", "available": money, "pending": money, "livemode": s.live,
	})
}

// retrieveAccount is the account of the key: Stripe's "Retrieve account" with no id, which answers
// the account the key belongs to (https://docs.stripe.com/api/accounts/retrieve).
func (s *Stripe) retrieveAccount(_ *http.Request, form url.Values, _ string) reply {
	if rp := unknownParam(form, nil); rp != nil {
		return *rp
	}
	return jsonReply(http.StatusOK, map[string]any{"object": "account", "id": s.account})
}
