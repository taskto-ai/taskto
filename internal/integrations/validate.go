package integrations

import (
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"taskto.ai/taskto/internal/apierr"
)

const (
	maxNameLen        = 40
	maxDisplayNameLen = 100
)

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Validation failures are 400s with a code, and never quote a value: a key pasted in the wrong
// field mustn't come back in an error or a log line.
var (
	errUnknownProvider = apierr.New(http.StatusBadRequest, apierr.CodeIntegrationProviderUnknown, "this provider isn't supported")
	errUnknownSecret   = apierr.New(http.StatusBadRequest, apierr.CodeBadRequest, "secrets has a name the provider doesn't use")
	errBadCurrencies   = apierr.New(http.StatusBadRequest, apierr.CodeIntegrationCurrencyInvalid,
		"currencies must list 1 to 20 different currency codes the server knows", apierr.Params{"max": maxCurrencies})
	// errWebhookSecretReused is a webhook secret that another integration already verifies with. It
	// says no more than that: not whose, nor in which workspace.
	errWebhookSecretReused = apierr.New(http.StatusConflict, apierr.CodeIntegrationWebhookSecretReused,
		"this webhook secret is already used by another integration")
	errBadCeiling = apierr.New(http.StatusBadRequest, apierr.CodeIntegrationLimitInvalid,
		"max_amount_minor must be a whole number from 1 to the largest the server allows", apierr.Params{"max": MaxAmountMinor})
)

func checkName(name string) error {
	if len(name) > maxNameLen || !nameRe.MatchString(name) {
		return apierr.New(http.StatusBadRequest, apierr.CodeIntegrationNameInvalid,
			"name must be 1 to 40 characters of a-z, 0-9, '.', '_' or '-', starting with a letter or digit", apierr.Params{"max": maxNameLen})
	}
	return nil
}

// checkDisplayName trims a display name and returns it, or fallback when it is empty.
func checkDisplayName(display, fallback string) (string, error) {
	display = strings.TrimSpace(display)
	if display == "" {
		return fallback, nil
	}
	if utf8.RuneCountInString(display) > maxDisplayNameLen || !printable(display) {
		return "", apierr.New(http.StatusBadRequest, apierr.CodeIntegrationDisplayNameBad,
			"display_name must be up to 100 printable characters", apierr.Params{"max": maxDisplayNameLen})
	}
	return display, nil
}

func printable(s string) bool {
	for _, r := range s {
		if r < ' ' || r == 0x7f {
			return false
		}
	}
	return true
}

func checkCeiling(n int64) error {
	if n < 1 || n > MaxAmountMinor {
		return errBadCeiling
	}
	return nil
}

// newDraft validates the body of a create against the registry.
func newDraft(req createRequest) (draft, error) {
	p := Lookup(req.Provider)
	if p == nil {
		return draft{}, errUnknownProvider
	}
	if err := checkName(req.Name); err != nil {
		return draft{}, err
	}
	display, err := checkDisplayName(req.DisplayName, req.Name)
	if err != nil {
		return draft{}, err
	}
	settings := req.Settings
	if settings == nil {
		settings = map[string]string{}
	}
	if err := checkSettings(p, settings); err != nil {
		return draft{}, err
	}
	for name := range req.Secrets {
		if _, ok := secretSpec(p, name); !ok {
			return draft{}, errUnknownSecret
		}
	}
	for _, spec := range p.Secrets() {
		value, given := req.Secrets[spec.Name]
		switch {
		case !given && spec.Required:
			return draft{}, apierr.New(http.StatusBadRequest, apierr.CodeIntegrationSecretRequired,
				spec.Name+" is required", apierr.Params{"secret": spec.Name})
		case given:
			if err := p.CheckSecret(spec.Name, value); err != nil {
				return draft{}, err
			}
		}
	}
	if err := checkCeiling(req.MaxAmountMinor); err != nil {
		return draft{}, err
	}
	currencies, ok := normalizeCurrencies(req.Currencies)
	if !ok {
		return draft{}, errBadCurrencies
	}
	mode := p.Mode(settings, req.Secrets[SecretAPIKey])
	if mode == "" {
		return draft{}, invalidSecret(SecretAPIKey)
	}
	return draft{
		provider: p, name: req.Name, displayName: display, settings: settings, secrets: req.Secrets,
		maxAmount: req.MaxAmountMinor, currencies: currencies, mode: mode,
	}, nil
}
