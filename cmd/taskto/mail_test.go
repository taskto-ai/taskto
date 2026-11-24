package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"taskto.ai/taskto/internal/config"
)

func smtpConfig(t *testing.T, password string) config.Config {
	t.Helper()
	file := filepath.Join(t.TempDir(), "smtp-password")
	if err := os.WriteFile(file, []byte(password), 0o600); err != nil {
		t.Fatal(err)
	}
	return config.Config{SMTPHost: "smtp.example.com", SMTPFrom: "taskto@example.com", SMTPUsername: "taskto", SMTPPasswordFile: file}
}

func TestLoadMailReadsThePasswordFromItsFile(t *testing.T) {
	var unset []string
	svc, err := loadMail(smtpConfig(t, "pw-from-file\n"), func(n string) error { unset = append(unset, n); return nil })
	if err != nil {
		t.Fatal(err)
	}
	info := svc.Info()
	if !svc.Configured() || info.Host != "smtp.example.com" || info.Port != 587 || info.Security != "starttls" || !info.PasswordSet {
		t.Fatalf("%+v", info)
	}
	// The variables that name the account are removed from the environment, set or not.
	if strings.Join(unset, ",") != "TASKTO_SMTP_USERNAME,TASKTO_SMTP_PASSWORD_FILE" {
		t.Fatalf("unset = %v", unset)
	}
}

func TestLoadMailWithoutAHostIsOff(t *testing.T) {
	svc, err := loadMail(config.Config{}, func(string) error { return nil })
	if err != nil || svc.Configured() {
		t.Fatalf("svc %v, err %v", svc, err)
	}
}

func TestLoadMailErrorsNameTheFileNeverThePassword(t *testing.T) {
	noop := func(string) error { return nil }
	cfg := smtpConfig(t, "\n")
	if _, err := loadMail(cfg, noop); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("an empty file: %v", err)
	}
	cfg.SMTPPasswordFile = filepath.Join(t.TempDir(), "missing")
	if _, err := loadMail(cfg, noop); err == nil || !strings.Contains(err.Error(), "smtp password file") {
		t.Fatalf("a missing file: %v", err)
	}
	// A password that fails the sender's own checks is never in the message.
	cfg = smtpConfig(t, "super-secret-value")
	cfg.SMTPSecurity = "none"
	_, err := loadMail(cfg, noop)
	if err == nil || strings.Contains(err.Error(), "super-secret-value") {
		t.Fatalf("err = %v", err)
	}
}

func TestHelloName(t *testing.T) {
	for in, want := range map[string]string{"https://tasks.example.com": "tasks.example.com", "": "", "http://127.0.0.1:8080": ""} {
		if got := helloName(in); got != want {
			t.Errorf("helloName(%q) = %q, want %q", in, got, want)
		}
	}
}
