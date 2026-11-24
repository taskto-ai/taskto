package audit

import (
	"fmt"
	"regexp"
	"slices"
)

// Each action has a fixed set of details, each of a fixed type. A detail that isn't in its
// action's set, or whose value isn't of its type, makes Record fail, so free text a person typed
// (a token's name, a space's title, a model's name) can't get into the log, whatever it holds:
// the log carries the target's id, and the names of the fields that changed, instead.
//
// The text kinds are the ones whose values are checked against a pattern too narrow for a
// credential to pass as them, or come from the code or the operator, not from a person's typing.

// kind is the type of one detail: it checks a value and returns the form to store.
type kind struct {
	name  string
	check func(v any) (any, error)
	// boolean kinds may carry a secret's name ("api_key_changed"): they say that something
	// changed, never what it is.
	boolean bool
}

var (
	tagRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,34}$`)
	hostRe  = regexp.MustCompile(`^[A-Za-z0-9._:\[\]-]{1,255}$`)
	emailRe = regexp.MustCompile(`^[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]{1,64}@[A-Za-z0-9.-]{1,253}$`)
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

// oneOf is a string from a fixed list.
func oneOf(values ...string) kind {
	return typed("one of "+fmt.Sprint(values), func(s string) bool { return slices.Contains(values, s) })
}

// someOf is a list of strings from a fixed list: the names of fields.
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
	// tag is a short code: a provider's id from the operator's file, a reason code, a language.
	tag = matching("tag", tagRe, 35)
	// host is the host (and port) of an address, never its path or credentials.
	host = matching("host", hostRe, 255)
	// email is an address that has been through the same checks as an account's.
	email = matching("email address", emailRe, 254)

	roles       = oneOf("admin", "member")
	spaceRoles  = oneOf("editor", "viewer")
	modes       = oneOf("own_else_shared", "own", "shared")
	clis        = oneOf("claude", "codex", "grok")
	cascadeFlag = boolean
)

// schemas says which details each action may carry, and of what type. An action that isn't here
// can't be recorded.
var schemas = map[string]map[string]kind{
	SetupCompleted:       {},
	LoginSucceeded:       {"method": oneOf("password")},
	LoginFailed:          {"method": oneOf("password"), "reason": oneOf("unknown_account", "no_password", "wrong_password", "account_disabled")},
	Logout:               {},
	PasswordChanged:      {},
	PasswordChangeFailed: {},
	PasswordResetCreated: {},
	PasswordResetUsed:    {},

	SSOLoginSucceeded: {"provider": tag},
	SSOLoginFailed:    {"provider": tag, "reason": tag},
	SSOIdentityLinked: {"provider": tag, "created_account": boolean},

	WorkspaceUpdated:  {"fields": someOf("locale"), "locale": tag},
	InviteCreated:     {"email": email, "role": roles},
	InviteRevoked:     {"email": email, "role": roles},
	InviteAccepted:    {"role": roles, "already_member": boolean},
	MemberRoleChanged: {"from": roles, "to": roles},
	MemberRemoved:     {"role": roles},
	MemberDisabled:    {"logins_ended": number},
	MemberEnabled:     {},

	AccessTokenCreated: {"expires": boolean},
	AccessTokenRevoked: {"cascade": cascadeFlag},
	AccessTokenUsed:    {},

	SpaceCreated:           {},
	SpaceUpdated:           {"fields": someOf("name", "archived", "offline_downloads"), "archived": boolean, "offline_downloads": boolean},
	SpaceMemberAdded:       {"user_id": number, "role": spaceRoles},
	SpaceMemberRoleChanged: {"user_id": number, "from": spaceRoles, "to": spaceRoles},
	SpaceMemberRemoved:     {"user_id": number, "role": spaceRoles, "cascade": cascadeFlag},

	DataExportStarted: {},

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
}
