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

const mailTestTimeout = 45 * time.Second

const (
	defaultMailTestCooldown = 30 * time.Second
	mailTestConcurrency     = 3
	busyRetryAfter          = 5 * time.Second
)

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
	for u, at := range g.last {
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

func mailRoutes(mux *http.ServeMux, accounts *auth.Service, svc *mail.Service, gate *mailTestGate) {
	mux.Handle("GET /api/workspaces/{workspace}/mail", accounts.RequireOperator(mailStatus(svc, accounts)))
	mux.Handle("POST /api/workspaces/{workspace}/mail/test", accounts.RequireOperator(mailTest(svc, accounts, gate)))
}

func mailStatus(svc *mail.Service, accounts *auth.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reply := struct {
			Configured bool   `json:"configured"`
			SendsLinks bool   `json:"sends_links"`
			Host       string `json:"host,omitempty"`
			Port       int    `json:"port,omitempty"`
			Security   string `json:"security,omitempty"`
			From       string `json:"from,omitempty"`
			Password   string `json:"password,omitempty"`
		}{SendsLinks: accounts.EmailLinks()}
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
		_ = json.NewEncoder(w).Encode(reply)
	})
}

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

func writeMailError(w http.ResponseWriter, r *http.Request, svc *mail.Service, err error) {
	var se *mail.SendError
	if !errors.As(err, &se) {
		slog.ErrorContext(r.Context(), "mail: the test email couldn't be made", "err", err)
		apierr.Internal(w)
		return
	}
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
