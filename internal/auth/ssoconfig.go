package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

type SSOProvider struct {
	ID           string
	Name         string
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
	TrustEmail   bool
}

var providerID = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

type secretRef struct {
	File string `json:"file"`
	Env  string `json:"env"`
}

type providerEntry struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Issuer       string     `json:"issuer"`
	ClientID     string     `json:"client_id"`
	ClientSecret *secretRef `json:"client_secret"`
	Scopes       []string   `json:"scopes"`
	TrustEmail   bool       `json:"trust_email"`
}

func LoadSSOProviders(path string, getenv func(string) string) ([]SSOProvider, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("sso providers: %w", err)
	}
	var doc struct {
		Providers []providerEntry `json:"providers"`
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("sso providers %s: %s", path, jsonProblem(err))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("sso providers %s: one JSON object expected", path)
	}
	if len(doc.Providers) == 0 {
		return nil, fmt.Errorf("sso providers %s: providers is empty", path)
	}
	var out []SSOProvider
	for i, e := range doc.Providers {
		p, err := e.provider(getenv)
		if err != nil {
			return nil, fmt.Errorf("sso providers %s: providers[%d]%w", path, i, err)
		}
		if slices.ContainsFunc(out, func(o SSOProvider) bool { return o.ID == p.ID }) {
			return nil, fmt.Errorf("sso providers %s: providers[%d].id: used twice", path, i)
		}
		out = append(out, p)
	}
	return out, nil
}

func jsonProblem(err error) string {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax):
		return fmt.Sprintf("not valid JSON, at byte %d", syntax.Offset)
	case errors.As(err, &typ):
		return fmt.Sprintf("%s: wrong type, want %s", typ.Field, typ.Type)
	case strings.HasPrefix(err.Error(), "json: unknown field"):
		return err.Error()
	}
	return "not valid JSON"
}

func (e providerEntry) provider(getenv func(string) string) (SSOProvider, error) {
	p := SSOProvider{ID: e.ID, Name: strings.TrimSpace(e.Name), Issuer: e.Issuer, ClientID: e.ClientID,
		Scopes: e.Scopes, TrustEmail: e.TrustEmail}
	switch {
	case !providerID.MatchString(e.ID):
		return p, errors.New(".id: want 1 to 32 characters of a-z, 0-9 and -")
	case p.Name == "" || len(p.Name) > 64:
		return p, errors.New(".name: want 1 to 64 characters")
	case !validIssuer(e.Issuer):
		return p, errors.New(".issuer: want an https URL (http only for localhost) without a query or fragment")
	case e.ClientID == "":
		return p, errors.New(".client_id: required")
	case e.ClientSecret == nil || (e.ClientSecret.File == "") == (e.ClientSecret.Env == ""):
		return p, errors.New(`.client_secret: want exactly one of {"file": "/absolute/path"} or {"env": "VAR"}`)
	}
	if len(p.Scopes) == 0 {
		p.Scopes = []string{"openid", "email", "profile"}
	}
	if !slices.Contains(p.Scopes, "openid") {
		return p, errors.New(`.scopes: must include "openid"`)
	}
	for _, s := range p.Scopes {
		if s == "" || strings.ContainsAny(s, " \t\r\n") {
			return p, errors.New(".scopes: a scope is a word without spaces")
		}
	}
	if f := e.ClientSecret.File; f != "" {
		if !filepath.IsAbs(f) {
			return p, errors.New(".client_secret.file: want an absolute path")
		}
		b, err := os.ReadFile(filepath.Clean(f))
		if err != nil {
			return p, fmt.Errorf(".client_secret.file: %w", err)
		}
		p.ClientSecret = strings.TrimSpace(string(b))
	} else {
		p.ClientSecret = strings.TrimSpace(getenv(e.ClientSecret.Env))
	}
	if p.ClientSecret == "" {
		return p, errors.New(".client_secret: the file or variable is empty")
	}
	return p, nil
}

func validIssuer(v string) bool {
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Hostname()))
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
