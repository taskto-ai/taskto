package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/mail"
)

// mailTestTimeout bounds a test send, on top of the sender's own.
const mailTestTimeout = 45 * time.Second

const (
	// defaultMailTestCooldown is how long a person waits between test sends, whatever
	// workspace they are an admin of: the route sends real mail through the operator's relay.
	defaultMailTestCooldown = 30 * time.Second
	// mailTestConcurrency is how many test sends the whole server makes at once.
	mailTestConcurrency = 3
	// busyRetryAfter is what a person is told to wait when the server's test sends are all busy.
	busyRetryAfter = 5 * time.Second
)

// mailTestGate limits the test route: one send per person per cooldown (across workspaces:
// the key is the user), and at most mailTestConcurrency at once on the server. It is in
// memory; a restart forgets it, which is fine for a courtesy to the operator's relay.
type mailTestGate struct {
	cooldown time.Duration
	now      func() time.Time

	mu      sync.Mutex
	last    map[int64]time.Time
	running int
}

func newMailTestGate(cooldown time.Duration, now func() time.Time) *mailTestGate {
	if cooldown == 0 {
		cooldown = defaultMailTestCooldown
	}
	return &mailTestGate{cooldown: cooldown, now: now, last: map[int64]time.Time{}}
}

// enter lets user send, and returns the function that ends the send. If user sent a test less
// than a cooldown ago, or the server's test sends are all busy, it returns false and how long
// to wait. A refused attempt doesn't start a new cooldown.
func (g *mailTestGate) enter(user int64) (leave func(), ok bool, wait time.Duration) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if at, seen := g.last[user]; seen && now.Sub(at) < g.cooldown {
		return nil, false, g.cooldown - now.Sub(at)
	}
	if g.running >= mailTestConcurrency {
		return nil, false, busyRetryAfter
	}
	for u, at := range g.last { // the ones that ended their cooldown no longer count
		if now.Sub(at) >= g.cooldown {
			delete(g.last, u)
		}
	}
	g.last[user] = now
	g.running++
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.running--
			g.mu.Unlock()
		})
	}, true, 0
}

// mailRoutes mounts the admins' outgoing email routes (docs/api.md#outgoing-email). The
// SMTP server is the whole server's, set by its operator (docs/install.md#email), so any
// workspace's admins see it, as they see the backups. svc may be nil: not configured.
func mailRoutes(mux *http.ServeMux, accounts *auth.Service, svc *mail.Service, gate *mailTestGate) {
	mux.Handle("GET /api/workspaces/{workspace}/mail", accounts.RequireAdmin(mailStatus(svc)))
	mux.Handle("POST /api/workspaces/{workspace}/mail/test", accounts.RequireAdmin(mailTest(svc, accounts, gate)))
}

// mailStatus is GET /api/workspaces/{workspace}/mail: whether the server can send email, and
// where. It never says the user name or the password, only whether the server signs in.
func mailStatus(svc *mail.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := struct {
			Configured bool   `json:"configured"`
			Host       string `json:"host,omitempty"`
			Port       int    `json:"port,omitempty"`
			Security   string `json:"security,omitempty"`
			From       string `json:"from,omitempty"`
			// Password is "set" or "not set", never the password.
			Password string `json:"password,omitempty"`
		}{}
		if svc.Configured() {
			info := svc.Info()
			reply.Configured, reply.Host, reply.Port, reply.Security, reply.From = true, info.Host, info.Port, info.Security, info.From
			reply.Password = "not set"
			if info.SignsIn {
				reply.Password = "set"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(reply) //nolint:gosec // G117: "password" is "set" or "not set", never the password
	})
}

// mailTest is POST /api/workspaces/{workspace}/mail/test: sends a test message to the
// caller's own address, in their language. Only to themselves: an admin can't use the
// server to send mail to anyone else through this route. It is limited (mailTestGate): 429
// with Retry-After when the caller sent a test a moment ago or the server is busy with them.
func mailTest(svc *mail.Service, accounts *auth.Service, gate *mailTestGate) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !svc.Configured() {
			apierr.Write(w, http.StatusConflict, apierr.CodeMailNotConfigured,
				"outgoing email isn't set up: the server's operator sets the SMTP server (docs/install.md#email)")
			return
		}
		u, _ := auth.UserFrom(r.Context())
		leave, ok, wait := gate.enter(u.ID)
		if !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait.Seconds()))))
			apierr.Write(w, http.StatusTooManyRequests, apierr.CodeMailTestRateLimited,
				"too many test emails: try again in a little while")
			return
		}
		defer leave()
		ctx, cancel := context.WithTimeout(r.Context(), mailTestTimeout)
		defer cancel()
		err := svc.SendTemplate(ctx, u.Email, accounts.Language(r, ""), "test", map[string]string{"name": u.Name})
		if err != nil {
			writeMailError(w, r, svc, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]string{"sent_to": u.Email})
	})
}

// writeMailError answers a failed send. The server's own reply can name hosts and
// addresses, so it goes to the log; the answer is a code the web app translates.
func writeMailError(w http.ResponseWriter, r *http.Request, svc *mail.Service, err error) {
	var se *mail.SendError
	if !errors.As(err, &se) {
		slog.ErrorContext(r.Context(), "mail: the test email couldn't be made", "err", err)
		apierr.Internal(w)
		return
	}
	// SendError holds the stage and the reply's code, never the relay's text.
	slog.WarnContext(r.Context(), "mail: the test email failed", "stage", se.Stage, "smtp_code", se.Code, "err", err)
	host := svc.Info().Host
	if refused := egress.Refusal(err); refused != nil {
		e := refused.APIError("")
		e.Status = http.StatusBadGateway
		apierr.WriteError(w, e)
		return
	}
	switch se.Stage {
	case mail.StageConnect:
		apierr.Write(w, http.StatusBadGateway, apierr.CodeMailConnectFailed,
			"the SMTP server didn't answer", apierr.Params{"host": host})
	case mail.StageTLS:
		apierr.Write(w, http.StatusBadGateway, apierr.CodeMailTLSFailed,
			"the connection to the SMTP server couldn't be made secure")
	case mail.StageAuth:
		apierr.Write(w, http.StatusBadGateway, apierr.CodeMailAuthFailed,
			"the SMTP server refused the sign-in")
	case mail.StageRejected:
		apierr.Write(w, http.StatusBadGateway, apierr.CodeMailRejected,
			"the SMTP server refused the message (reply "+strconv.Itoa(se.Code)+")", apierr.Params{"smtp_code": se.Code})
	default:
		apierr.Write(w, http.StatusBadGateway, apierr.CodeMailFailed, "the message couldn't be sent")
	}
}
