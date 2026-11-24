package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"taskto.ai/taskto/internal/apierr"
	"taskto.ai/taskto/internal/auth"
	"taskto.ai/taskto/internal/egress"
	"taskto.ai/taskto/internal/mail"
)

// mailTestTimeout bounds a test send, on top of the sender's own.
const mailTestTimeout = 45 * time.Second

// mailRoutes mounts the admins' outgoing email routes (docs/api.md#outgoing-email). The
// SMTP server is the whole server's, set by its operator (docs/install.md#email), so any
// workspace's admins see it, as they see the backups. svc may be nil: not configured.
func mailRoutes(mux *http.ServeMux, accounts *auth.Service, svc *mail.Service) {
	mux.Handle("GET /api/workspaces/{workspace}/mail", accounts.RequireAdmin(mailStatus(svc)))
	mux.Handle("POST /api/workspaces/{workspace}/mail/test", accounts.RequireAdmin(mailTest(svc, accounts)))
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
			if info.PasswordSet {
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
// server to send mail to anyone else through this route.
func mailTest(svc *mail.Service, accounts *auth.Service) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !svc.Configured() {
			apierr.Write(w, http.StatusConflict, apierr.CodeMailNotConfigured,
				"outgoing email isn't set up: the server's operator sets the SMTP server (docs/install.md#email)")
			return
		}
		u, _ := auth.UserFrom(r.Context())
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
