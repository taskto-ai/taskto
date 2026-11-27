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

var smtpSecretEnv = []string{"TASKTO_SMTP_USERNAME", "TASKTO_SMTP_PASSWORD_FILE"}

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
		raw, err := os.ReadFile(cfg.SMTPPasswordFile)
		if err != nil {
			return nil, fmt.Errorf("smtp password file: %w", err)
		}
		password = strings.TrimRight(string(raw), "\r\n")
		if password == "" {
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

func helloName(publicURL string) string {
	if u, err := url.Parse(publicURL); err == nil && u.Hostname() != "" && net.ParseIP(u.Hostname()) == nil {
		return u.Hostname()
	}
	return ""
}
