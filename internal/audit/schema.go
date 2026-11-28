package audit

import (
	"fmt"
	"regexp"
	"slices"
)

type kind struct {
	name    string
	check   func(v any) (any, error)
	boolean bool
}

var (
	tagRe       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,34}$`)
	hostRe      = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)
	skillNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,39}$`)
	emailRe     = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]{1,64}@[A-Za-z0-9.-]{1,253}$`)
)

func typed[T any](name string, ok func(T) bool) kind {
	return kind{name: name, check: func(v any) (any, error) {
		t, is := v.(T)
		if !is {
			return nil, fmt.Errorf("a %T isn't a %s", v, name)
		}
		if ok != nil && !ok(t) {
			return nil, fmt.Errorf("%v isn't a valid %s", v, name)
		}
		return t, nil
	}}
}

func matching(name string, re *regexp.Regexp, max int) kind {
	return typed(name, func(s string) bool { return len(s) <= max && re.MatchString(s) })
}

func oneOf(values ...string) kind {
	return typed("one of "+fmt.Sprint(values), func(s string) bool { return slices.Contains(values, s) })
}

func someOf(values ...string) kind {
	k := typed("list of "+fmt.Sprint(values), func(l []string) bool {
		if len(l) > maxDetailList {
			return false
		}
		for _, s := range l {
			if !slices.Contains(values, s) {
				return false
			}
		}
		return true
	})
	return k
}

var (
	boolean = func() kind { k := typed[bool]("boolean", nil); k.boolean = true; return k }()
	number  = kind{name: "number", check: func(v any) (any, error) {
		switch n := v.(type) {
		case int:
			return int64(n), nil
		case int64:
			return n, nil
		}
		return nil, fmt.Errorf("a %T isn't a whole number", v)
	}}
	skillName = matching("skill name", skillNameRe, 40)
	tag       = matching("tag", tagRe, 35)
	host      = matching("host", hostRe, 255)
	email     = matching("email address", emailRe, 254)

	roles                = oneOf("admin", "member")
	spaceRoles           = oneOf("editor", "viewer")
	modes                = oneOf("own_else_shared", "own", "shared")
	clis                 = oneOf("claude", "codex", "grok")
	integrationProviders = oneOf("stripe", "square")
	integrationKinds     = oneOf("payments")
	integrationModes     = oneOf("test", "live")
	cascadeFlag          = boolean
	skillScopes          = oneOf("space", "workspace")
)

var schemas = map[string]map[string]kind{
	SetupCompleted:       {},
	LoginSucceeded:       {"method": oneOf("password")},
	LoginFailed:          {"method": oneOf("password"), "reason": oneOf("unknown_account", "no_password", "wrong_password", "wrong_code", "account_disabled")},
	Logout:               {},
	SessionRevoked:       {},
	OtherSessionsRevoked: {"logins_ended": number},
	PasswordChanged:      {},
	PasswordChangeFailed: {},
	PasswordResetCreated: {},
	PasswordResetUsed:    {},

	TwoFactorEnabled:               {},
	TwoFactorDisabled:              {},
	TwoFactorRecoveryCodesReplaced: {},
	TwoFactorCleared:               {},

	PasswordResetRequested: {"emailed": boolean},

	SSOLoginSucceeded: {"provider": tag},
	SSOLoginFailed:    {"provider": tag, "reason": tag},
	SSOIdentityLinked: {"provider": tag, "created_account": boolean},

	WorkspaceCreated:  {"allowed_by": oneOf("admins", "anyone")},
	WorkspaceUpdated:  {"fields": someOf("locale"), "locale": tag},
	InviteCreated:     {"email": email, "role": roles},
	InviteRevoked:     {"email": email, "role": roles},
	InviteEmailed:     {},
	InviteAccepted:    {"role": roles, "already_member": boolean},
	MemberRoleChanged: {"from": roles, "to": roles},
	MemberRemoved:     {"role": roles},
	MemberDisabled:    {"logins_ended": number},
	MemberEnabled:     {},

	AccountDeletionRequested: {"purge_days": number, "logins_ended": number},
	AccountDeletionCancelled: {},
	AccountPurged: {
		"private_spaces": number, "conversations": number, "access_grants": number,
		"cli_accounts": number, "push_subscriptions": number, "documents_anonymized": number,
		"mentions_anonymized": number, "uploads_removed": number, "cleanups_pending": number,
	},

	AccessTokenCreated: {"expires": boolean},
	AccessTokenRevoked: {"cascade": cascadeFlag},
	AccessTokenUsed:    {},

	SpaceCreated:           {},
	SpaceUpdated:           {"fields": someOf("name", "archived", "offline_downloads"), "archived": boolean, "offline_downloads": boolean},
	SpaceMemberAdded:       {"user_id": number, "role": spaceRoles},
	SpaceMemberRoleChanged: {"user_id": number, "from": spaceRoles, "to": spaceRoles},
	SpaceMemberRemoved:     {"user_id": number, "role": spaceRoles, "cascade": cascadeFlag},

	ServerUpdateRequested: {"channel": oneOf("release", "main")},

	DataExportStarted:      {},
	WorkspaceExportStarted: {"spaces": number},

	ProviderAccountCreated: {"kind": oneOf("openai-compatible"), "scope": oneOf("workspace", "user"), "base_host": host, "api_key_set": boolean},
	ProviderAccountUpdated: {
		"kind": oneOf("openai-compatible"), "scope": oneOf("workspace", "user"), "base_host": host, "api_key_changed": boolean,
		"fields": someOf("display_name", "base_url", "api_key", "compat", "models"),
	},
	ProviderAccountRemoved:  {"kind": oneOf("openai-compatible"), "scope": oneOf("workspace", "user")},
	SharedCLIAccountCreated: {"cli": clis, "base_host": host, "api_key_set": boolean},
	SharedCLIAccountUpdated: {
		"cli": clis, "base_host": host, "api_key_changed": boolean, "fields": someOf("label", "base_url", "model", "api_key"),
	},
	SharedCLIAccountRemoved: {"cli": clis, "members_signed_out": number},
	CLIModesChanged:         {"default_mode": modes, "members_changed": number},
	SearchEmbeddingsChanged: {"enabled": boolean, "account": number, "cascade": cascadeFlag},
	SearchConsentChanged:    {"enabled": boolean, "cascade": cascadeFlag},
	DictationSettingsChanged: {
		"engine": tag, "language": tag, "base_host": host, "api_key_changed": boolean, "api_key_set": boolean,
		"notice_confirmed": boolean,
	},

	IntegrationCreated: {
		"provider": integrationProviders, "kind": integrationKinds, "mode": integrationModes,
		"api_key_set": boolean, "webhook_secret_set": boolean,
	},
	IntegrationUpdated: {
		"provider": integrationProviders, "mode": integrationModes,
		"fields":          someOf("display_name", "settings", "max_amount", "currencies", "api_key", "webhook_secret"),
		"api_key_changed": boolean, "webhook_secret_changed": boolean,
	},
	IntegrationRemoved:        {"provider": integrationProviders, "kind": integrationKinds},
	IntegrationDisabled:       {"provider": integrationProviders},
	IntegrationEnabled:        {"provider": integrationProviders},
	IntegrationSpacesChanged:  {"provider": integrationProviders, "added": number, "removed": number},
	IntegrationSpacesKept:     {"provider": integrationProviders, "kept": number},
	IntegrationNewLinkAllowed: {"provider": integrationProviders, "app_id": number, "effect_id": number},

	AppIntegrationRequested: {"kind": integrationKinds, "app_id": number},
	AppIntegrationApproved: {
		"kind": integrationKinds, "app_id": number, "integration_id": number, "provider": integrationProviders,
		"space_allowed": boolean,
	},
	AppIntegrationRejected: {"kind": integrationKinds, "app_id": number},
	AppIntegrationRevoked:  {"kind": integrationKinds, "app_id": number, "integration_id": number},

	SkillCreated: {"scope": skillScopes, "space_id": number, "name": skillName},
	SkillUpdated: {"scope": skillScopes, "space_id": number, "name": skillName, "fields": someOf("name", "title", "description", "body", "enabled")},
	SkillRemoved: {"scope": skillScopes, "space_id": number, "name": skillName},
}
