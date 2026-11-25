package testprovider

import (
	"net/http"
	"strconv"
	"time"
)

// Fault is a failure the fake injects into the next matching requests: Stripe being down, rate
// limiting, refusing, or going silent. Build one with the constructors (ServerError,
// RateLimited, Timeout, ConnectionReset, Refusal, Unauthorized) and adjust it with the methods;
// every method returns a changed copy.
//
// By default a fault strikes before the operation runs: nothing is created and nothing is
// remembered under the request's idempotency key (Stripe doesn't save a result for a request
// that was refused before it began, such as a rate-limited one). AfterExecuting makes it strike
// after: the operation took effect, but the client sees the failure. That is the case that
// matters for idempotency: a retry with the same key must find the object, not make another.
// An error answer is then saved under the key as Stripe does for a 500; a timeout or a dropped
// connection saves the success.
type Fault struct {
	// Status is the HTTP status of the answer (500 when 0). It is unused with Hang and Drop.
	Status int
	// Type, Code, Message and Param fill the error object of the answer.
	Type, Code, Message, Param string
	// RetryAfter sets the Retry-After header (whole seconds), as a 429 does.
	RetryAfter time.Duration
	// Hang never answers: the client's own timeout ends the request.
	Hang bool
	// Drop closes the connection without an answer.
	Drop bool
	// After makes the fault strike after the operation ran.
	After bool

	method, path string
	times        int // 0 means once; negative means forever
}

// ServerError is Stripe's 500.
func ServerError() Fault {
	return Fault{Status: http.StatusInternalServerError, Type: "api_error",
		Message: "An unknown error occurred"}
}

// Unavailable is a 503.
func Unavailable() Fault {
	return Fault{Status: http.StatusServiceUnavailable, Type: "api_error",
		Message: "The service is temporarily unavailable"}
}

// RateLimited is a 429, with Retry-After set from retryAfter when it is positive.
func RateLimited(retryAfter time.Duration) Fault {
	return Fault{Status: http.StatusTooManyRequests, Type: "invalid_request_error", Code: "rate_limit",
		Message: "Too many requests hit the API too quickly.", RetryAfter: retryAfter}
}

// Timeout never answers.
func Timeout() Fault { return Fault{Hang: true} }

// ConnectionReset closes the connection without an answer.
func ConnectionReset() Fault { return Fault{Drop: true} }

// Refusal is a 400 refusing the request with an error code, such as "amount_too_small" or
// "currency_not_supported".
func Refusal(code string) Fault {
	return Fault{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: code,
		Message: "The request was refused."}
}

// Unauthorized is the 401 of a key that was revoked.
func Unauthorized() Fault {
	return Fault{Status: http.StatusUnauthorized, Type: "invalid_request_error",
		Message: "Invalid API Key provided: sk_test_****"}
}

// AfterExecuting makes the fault strike after the operation took effect.
func (f Fault) AfterExecuting() Fault { f.After = true; return f }

// Times makes the fault strike n times (the default is once).
func (f Fault) Times(n int) Fault { f.times = n; return f }

// Forever makes the fault strike on every matching request until ClearFaults.
func (f Fault) Forever() Fault { f.times = -1; return f }

// On restricts the fault to requests of a method (empty: any) whose path starts with prefix.
func (f Fault) On(method, pathPrefix string) Fault {
	f.method, f.path = method, pathPrefix
	return f
}

func (f Fault) matches(r *http.Request) bool {
	if f.method != "" && f.method != r.Method {
		return false
	}
	return len(r.URL.Path) >= len(f.path) && r.URL.Path[:len(f.path)] == f.path
}

// reply is the fault's answer.
func (f Fault) reply() reply {
	status := f.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	typ := f.Type
	if typ == "" {
		typ = "api_error"
	}
	rp := errorReply(status, typ, f.Code, f.Message, f.Param)
	if f.RetryAfter > 0 {
		rp.header.Set("Retry-After", strconv.Itoa(int((f.RetryAfter+time.Second-1)/time.Second)))
	}
	if status == http.StatusTooManyRequests || status >= 500 {
		rp.header.Set("Stripe-Should-Retry", "true")
	}
	rp.drop, rp.hang = f.Drop, f.Hang
	return rp
}

// Fail queues a fault. Faults are used in the order queued, each by the requests it matches.
func (s *Stripe) Fail(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.times == 0 {
		f.times = 1
	}
	s.faults = append(s.faults, &f)
}

// ClearFaults drops the faults still queued.
func (s *Stripe) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

// takeFault returns the first queued fault matching r, using up one of its strikes.
func (s *Stripe) takeFault(r *http.Request) *Fault {
	for i, f := range s.faults {
		if !f.matches(r) {
			continue
		}
		taken := *f
		if f.times > 0 {
			f.times--
			if f.times == 0 {
				s.faults = append(s.faults[:i], s.faults[i+1:]...)
			}
		}
		return &taken
	}
	return nil
}
