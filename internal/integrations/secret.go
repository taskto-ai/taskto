package integrations

import (
	"context"
	"fmt"
	"log/slog"
)

// Secret is a credential in clear. It exists only between opening a sealed value and using it
// (a request to the provider), and every way of printing, logging or encoding one gives
// "[redacted]": the only way to the value is Reveal, which a reader of a diff can see.
type Secret string

const redacted = "[redacted]"

// Reveal returns the credential.
func (s Secret) Reveal() string { return string(s) }

// String, GoString, LogValue and the encoders keep a credential out of fmt, %#v, slog and JSON.
func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }

// Credentials are an integration's opened secrets and its settings, for the code that calls the
// provider for it. It never leaves the server: no API route returns one.
type Credentials struct {
	Provider string
	Mode     string
	Settings map[string]string
	APIKey   Secret
	// WebhookSecret signs the provider's events; WebhookSecretPrevious is the one it replaced,
	// accepted until it expires so events signed before the change still verify. Either is empty
	// when there is none.
	WebhookSecret         Secret
	WebhookSecretPrevious Secret
}

// Usable says whether an integration may be used in a space right now: it exists in the
// workspace, isn't disabled and the space is allowed. It is what the gate of an app's release
// and the checks of every call made for an app ask (docs/integrations.md#gates).
func (s *Service) Usable(ctx context.Context, workspace, integration, space int64) (bool, error) {
	return s.store.usable(ctx, workspace, integration, space)
}

// Credentials opens the secrets of the workspace's integration. The sealed values are bound to
// their row and name, so one copied to another doesn't open. An integration that is disabled is
// returned too: whoever calls decides what a disabled one may do.
func (s *Service) Credentials(ctx context.Context, workspace, integration int64) (Credentials, error) {
	return s.store.credentials(ctx, workspace, integration)
}
