package integrations

import (
	"context"
	"fmt"
	"log/slog"
)

type Secret string

const redacted = "[redacted]"

func (s Secret) Reveal() string { return string(s) }

func (Secret) String() string               { return redacted }
func (Secret) GoString() string             { return redacted }
func (Secret) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }
func (Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + redacted + `"`), nil }
func (Secret) Format(f fmt.State, _ rune)   { _, _ = f.Write([]byte(redacted)) }

type Credentials struct {
	Provider              string
	Mode                  string
	Settings              map[string]string
	APIKey                Secret
	WebhookSecret         Secret
	WebhookSecretPrevious Secret
}

func (s *Service) Usable(ctx context.Context, workspace, integration, space int64) (bool, error) {
	return s.store.usable(ctx, workspace, integration, space)
}

func (s *Service) Credentials(ctx context.Context, workspace, integration int64) (Credentials, error) {
	return s.store.credentials(ctx, workspace, integration)
}
