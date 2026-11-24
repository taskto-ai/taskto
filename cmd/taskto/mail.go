package main

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"taskto.ai/taskto/internal/config"
	"taskto.ai/taskto/internal/mail"
)

// smtpSecretEnv are the variables that name the SMTP account. They are removed from this
// process's environment once read, so no child process (the agent sidecar, the CLI workers,
// the browsers) inherits them: an agent never sees where the server's mail account is.
var smtpSecretEnv = []string{"TASKTO_SMTP_USERNAME", "TASKTO_SMTP_PASSWORD_FILE"}

// loadMail makes the server's outgoing email from the operator's SMTP settings, or returns
// nil when there are none (no -smtp-host). It reads the password file (the password is never a
// setting, never in the environment, and kept only in memory) and removes smtpSecretEnv from
// the environment. A mistake stops the server at startup, naming the setting and never a value.
func loadMail(cfg config.Config, unsetenv func(string) error) (*mail.Service, error) {
	defer func() {
		for _, name := range smtpSecretEnv {
			_ = unsetenv(name)
		}
	}()
	if cfg.SMTPHost == "" {
		return nil, nil
	}
	password := ""
	if cfg.SMTPPasswordFile != "" {
		raw, err := os.ReadFile(cfg.SMTPPasswordFile) // #nosec G304 G703 -- the operator names the file.
		if err != nil {
			// The error names the file, which the operator chose; never its contents.
			return nil, fmt.Errorf("smtp password file: %w", err)
		}
		password = strings.TrimRight(string(raw), "\r\n")
		if password == "" {
			// Not the path: the setting is named, and a value of a setting that holds a
			// password's place never goes into a log line or an error.
			return nil, errors.New("the smtp password file (smtp_password_file) is empty")
		}
	}
	sender, err := mail.NewSMTP(mail.SMTPConfig{
		Host: cfg.SMTPHost, Port: cfg.SMTPPortNumber(), Security: mail.Security(cfg.SMTPSecurityMode()),
		From: cfg.SMTPFrom, Username: cfg.SMTPUsername, Password: password,
		Egress: cfg.SMTPEgressPolicy(), Hello: helloName(cfg.PublicURL),
	})
	if err != nil {
		return nil, fmt.Errorf("smtp: %w", err)
	}
	return mail.NewService(sender, sender.Info()), nil
}

// helloName is the name the server gives in EHLO: its public address's host, when it has one.
func helloName(publicURL string) string {
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" && net.ParseIP(u.Hostname()) == nil {
		return u.Hostname()
	}
	return ""
}
