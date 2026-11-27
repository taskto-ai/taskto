package testprovider

import (
	"net/http"
	"slices"
	"strconv"
	"time"
)

type Fault struct {
	Status                     int
	Type, Code, Message, Param string
	RetryAfter                 time.Duration
	Hang                       bool
	Drop                       bool
	After                      bool
	Kept                       bool

	method, path string
	when         func(*http.Request) bool
	times        int
}

func ServerError() Fault {
	return Fault{Status: http.StatusInternalServerError, Type: "api_error",
		Message: "An unknown error occurred"}
}

func Unavailable() Fault {
	return Fault{Status: http.StatusServiceUnavailable, Type: "api_error",
		Message: "The service is temporarily unavailable"}
}

func RateLimited(retryAfter time.Duration) Fault {
	return Fault{Status: http.StatusTooManyRequests, Type: "invalid_request_error", Code: "rate_limit",
		Message: "Too many requests hit the API too quickly.", RetryAfter: retryAfter}
}

func Timeout() Fault { return Fault{Hang: true} }

func ConnectionReset() Fault { return Fault{Drop: true} }

func Refusal(code string) Fault {
	return Fault{Status: http.StatusBadRequest, Type: "invalid_request_error", Code: code,
		Message: "The request was refused."}
}

func Unauthorized() Fault {
	return Fault{Status: http.StatusUnauthorized, Type: "invalid_request_error",
		Message: "Invalid API Key provided: sk_test_****"}
}

func KeyInUse() Fault {
	return Fault{Status: http.StatusConflict, Type: "invalid_request_error", Code: "idempotency_key_in_use",
		Message: "There is currently another in-progress request using this idempotency key."}
}

func LockTimeout() Fault {
	return Fault{Status: http.StatusTooManyRequests, Type: "invalid_request_error", Code: "lock_timeout",
		Message: "This object cannot be accessed right now because another API request is using it."}
}

func (f Fault) KeptUnderKey() Fault { f.Kept = true; return f }

func (f Fault) AfterExecuting() Fault { f.After = true; return f }

func (f Fault) Times(n int) Fault { f.times = n; return f }

func (f Fault) Forever() Fault { f.times = -1; return f }

func (f Fault) On(method, pathPrefix string) Fault {
	f.method, f.path = method, pathPrefix
	return f
}

func (f Fault) When(cond func(*http.Request) bool) Fault {
	f.when = cond
	return f
}

func (f Fault) matches(r *http.Request) bool {
	if f.method != "" && f.method != r.Method {
		return false
	}
	if f.when != nil && !f.when(r) {
		return false
	}
	return len(r.URL.Path) >= len(f.path) && r.URL.Path[:len(f.path)] == f.path
}

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

func (s *Stripe) Fail(f Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f.times == 0 {
		f.times = 1
	}
	s.faults = append(s.faults, &f)
}

func (s *Stripe) ClearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = nil
}

func (s *Stripe) peekFault(r *http.Request) *Fault {
	for _, f := range s.faults {
		if f.matches(r) {
			return f
		}
	}
	return nil
}

func (s *Stripe) useFault(f *Fault) {
	if f.times <= 0 {
		return
	}
	f.times--
	if f.times == 0 {
		if i := slices.Index(s.faults, f); i >= 0 {
			s.faults = slices.Delete(s.faults, i, i+1)
		}
	}
}
