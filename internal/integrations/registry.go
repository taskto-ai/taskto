package integrations

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"taskto.ai/taskto/internal/apierr"
)

// KindPayments is the first kind of integration: taking a payment through a provider's hosted
// checkout. A kind is a closed class of operations an app may use (docs/integrations.md).
const KindPayments = "payments"

// The modes of an integration: whether its credentials move real money.
const (
	ModeTest = "test"
	ModeLive = "live"
)

// Names of the secrets an integration holds.
const (
	SecretAPIKey                = "api_key"
	SecretWebhookSecret         = "webhook_secret"
	SecretWebhookSecretPrevious = "webhook_secret_previous"
)

// SecretSpec describes one secret a provider needs, for the form that asks for it. What a
// person is told it is called is the web app's (settings.integrations.secret.<name>).
type SecretSpec struct {
	Name string `json:"name"`
	// Required secrets must be given when the integration is made. A webhook secret is not: an
	// install without a public address reconciles payments by polling and never receives one.
	Required bool `json:"required"`
}

// SettingSpec describes one non-secret setting a provider needs. Its values are short
// identifiers, never a URL or free text: a setting can't point the server's calls anywhere.
type SettingSpec struct {
	Name string `json:"name"`
	// Options are the values of a choice; empty for an identifier of the provider's.
	Options  []string `json:"options" apitypes:"nonnull"`
	Required bool     `json:"required"`
}

// Provider is an entry of the registry: what the server knows of one outside service. The
// registry is closed: it is this file's list, not data, so a provider can't be added by an
// operator, an admin or an app (docs/integrations.md#providers). The calls to the provider
// (links, retrieval, webhooks) arrive with the cards that need them; this is what the store
// needs: what the connection holds and how its shape is checked, without asking the provider.
type Provider interface {
	// ID is the registry's id, stored with the integration and recorded in the audit log.
	ID() string
	// Kinds are the kinds of integration the provider implements.
	Kinds() []string
	// Secrets and Settings say what a connection holds.
	Secrets() []SecretSpec
	Settings() []SettingSpec
	// CheckSecret checks the shape of a secret's value, such as a key's prefix, so a publishable
	// key pasted in place of a secret one is refused. Its error never quotes the value.
	CheckSecret(name, value string) error
	// Mode reports whether the credentials are for testing or for real money, from the
	// settings and the API key (which is empty when it isn't being changed: the answer is then
	// "", for "unchanged").
	Mode(settings map[string]string, apiKey string) string
	// Hosts are the API hosts the adapter owns, constants of the adapter (Square's depend on the
	// environment setting, which is a choice, never free text). The request builder refuses any
	// other host (Client).
	Hosts(settings map[string]string) []string
	// APIVersion is the provider's API version the adapter is written against and sends with
	// every call; "" until the adapter makes its first call.
	APIVersion() string
}

// settingRe is what every setting's value, other than a choice, must match: an identifier. A
// URL, a host or a sentence never does.
var settingRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// registry is the closed list of providers.
var registry = []Provider{stripe{}, square{}}

// Providers lists the registered providers, in the order the registry has them.
func Providers() []Provider { return slices.Clone(registry) }

// Lookup returns the provider with id, or nil.
func Lookup(id string) Provider {
	for _, p := range registry {
		if p.ID() == id {
			return p
		}
	}
	return nil
}

// secretSpec returns the spec of the provider's secret called name.
func secretSpec(p Provider, name string) (SecretSpec, bool) {
	for _, s := range p.Secrets() {
		if s.Name == name {
			return s, true
		}
	}
	return SecretSpec{}, false
}

// invalidSecret is a 400 for a secret whose shape isn't the provider's. It names the secret, not
// the value.
func invalidSecret(name string) *apierr.Error {
	return apierr.New(http.StatusBadRequest, apierr.CodeIntegrationSecretInvalid,
		fmt.Sprintf("%s isn't a valid credential for this provider", name), apierr.Params{"secret": name})
}

// checkSettings validates the settings against the provider's specs: no setting it doesn't
// declare, every required one present, a choice among its options, and an identifier otherwise.
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
			// Not echoed: the name came from the request.
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

// stripe is Stripe, for payments through its hosted checkout. Its keys say whether they are
// for testing: sk_test_ and rk_test_ (a restricted key) against sk_live_ and rk_live_.
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

// StripeAPIVersion is the version of Stripe's API the adapter is written against, sent in the
// Stripe-Version header: the one Stripe's documentation lists as current when the adapter was
// written. Upgrading it is a change of code, tested against the fake, never of configuration.
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

// square is Square (square.go, square_webhook.go). Its access tokens don't say whether they are for
// the sandbox, so the environment is a setting, and so is the location payments are made at.
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

// Square's hosts: an integration reaches the one of its environment, never the other.
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
