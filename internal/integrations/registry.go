package integrations

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"taskto.ai/taskto/internal/apierr"
)

const KindPayments = "payments"

const (
	ModeTest = "test"
	ModeLive = "live"
)

const (
	SecretAPIKey                = "api_key"
	SecretWebhookSecret         = "webhook_secret"
	SecretWebhookSecretPrevious = "webhook_secret_previous"
)

type SecretSpec struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

type SettingSpec struct {
	Name     string   `json:"name"`
	Options  []string `json:"options" apitypes:"nonnull"`
	Required bool     `json:"required"`
}

type Provider interface {
	ID() string
	Kinds() []string
	Secrets() []SecretSpec
	Settings() []SettingSpec
	CheckSecret(name, value string) error
	Mode(settings map[string]string, apiKey string) string
	Hosts(settings map[string]string) []string
	APIVersion() string
}

var settingRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

var registry = []Provider{stripe{}, square{}}

func Providers() []Provider { return slices.Clone(registry) }

func Lookup(id string) Provider {
	for _, p := range registry {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

func secretSpec(p Provider, name string) (SecretSpec, bool) {
	for _, s := range p.Secrets() {
		if s.Name == name {
			return s, true
		}
	}
	return SecretSpec{}, false
}

func invalidSecret(name string) *apierr.Error {
	return apierr.New(http.StatusBadRequest, apierr.CodeIntegrationSecretInvalid,
		fmt.Sprintf("%s isn't a valid credential for this provider", name), apierr.Params{"secret": name})
}

func checkSettings(p Provider, settings map[string]string) *apierr.Error {
	bad := func(name string) *apierr.Error {
		return apierr.New(http.StatusBadRequest, apierr.CodeIntegrationSettingInvalid,
			fmt.Sprintf("setting %s is missing or not valid", name), apierr.Params{"setting": name})
	}
	known := map[string]SettingSpec{}
	for _, spec := range p.Settings() {
		known[spec.Name] = spec
	}
	for name := range settings {
		if _, ok := known[name]; !ok {
			return bad("unknown")
		}
	}
	for name, spec := range known {
		v, given := settings[name]
		switch {
		case !given && spec.Required:
			return bad(name)
		case !given:
		case len(spec.Options) > 0 && !slices.Contains(spec.Options, v):
			return bad(name)
		case len(spec.Options) == 0 && !settingRe.MatchString(v):
			return bad(name)
		}
	}
	return nil
}

type stripe struct{}

var (
	stripeKeyRe     = regexp.MustCompile(`^(sk|rk)_(test|live)_[A-Za-z0-9]{8,250}$`)
	stripeWebhookRe = regexp.MustCompile(`^whsec_[A-Za-z0-9+/=_-]{8,250}$`)
)

func (stripe) ID() string      { return "stripe" }
func (stripe) Kinds() []string { return []string{KindPayments} }
func (stripe) Secrets() []SecretSpec {
	return []SecretSpec{{Name: SecretAPIKey, Required: true}, {Name: SecretWebhookSecret}}
}
func (stripe) Settings() []SettingSpec { return []SettingSpec{} }

const StripeAPIVersion = "2026-11-18.endive"

func (stripe) Hosts(map[string]string) []string { return []string{"api.stripe.com"} }
func (stripe) APIVersion() string               { return StripeAPIVersion }

func (stripe) CheckSecret(name, value string) error {
	switch {
	case name == SecretAPIKey && stripeKeyRe.MatchString(value):
		return nil
	case name == SecretWebhookSecret && stripeWebhookRe.MatchString(value):
		return nil
	}
	return invalidSecret(name)
}

func (stripe) Mode(_ map[string]string, apiKey string) string {
	switch {
	case strings.Contains(apiKey, "_test_"):
		return ModeTest
	case strings.Contains(apiKey, "_live_"):
		return ModeLive
	}
	return ""
}

type square struct{}

var (
	squareTokenRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{16,255}$`)
	squareWebhookRe = regexp.MustCompile(`^[A-Za-z0-9+/=_-]{8,255}$`)
)

func (square) ID() string      { return "square" }
func (square) Kinds() []string { return []string{KindPayments} }
func (square) Secrets() []SecretSpec {
	return []SecretSpec{{Name: SecretAPIKey, Required: true}, {Name: SecretWebhookSecret}}
}
func (square) Settings() []SettingSpec {
	return []SettingSpec{
		{Name: "environment", Options: []string{"sandbox", "production"}, Required: true},
		{Name: "location_id", Options: []string{}, Required: true},
	}
}

func (square) Hosts(settings map[string]string) []string {
	if settings["environment"] == "production" {
		return []string{"connect.squareup.com"}
	}
	return []string{"connect.squareupsandbox.com"}
}

func (square) CheckSecret(name, value string) error {
	switch {
	case name == SecretAPIKey && squareTokenRe.MatchString(value):
		return nil
	case name == SecretWebhookSecret && squareWebhookRe.MatchString(value):
		return nil
	}
	return invalidSecret(name)
}

func (square) Mode(settings map[string]string, _ string) string {
	if settings["environment"] == "production" {
		return ModeLive
	}
	return ModeTest
}
