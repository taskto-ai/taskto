package audit

// The actions. A code is stable once released: the admin view and anyone who exports the log
// match on it, so a new meaning gets a new code. docs/audit.md lists what each one records.
const (
	// Accounts and sessions.
	SetupCompleted       = "setup_completed"
	LoginSucceeded       = "login_succeeded"
	LoginFailed          = "login_failed"
	Logout               = "logout"
	PasswordChanged      = "password_changed"
	PasswordChangeFailed = "password_change_failed"
	PasswordResetCreated = "password_reset_created"
	PasswordResetUsed    = "password_reset_used"

	// Single sign-on.
	SSOLoginSucceeded = "sso_login_succeeded"
	SSOLoginFailed    = "sso_login_failed"
	SSOIdentityLinked = "sso_identity_linked"

	// Workspaces, members and invites.
	WorkspaceCreated  = "workspace_created"
	WorkspaceUpdated  = "workspace_updated"
	InviteCreated     = "invite_created"
	InviteRevoked     = "invite_revoked"
	InviteAccepted    = "invite_accepted"
	MemberRoleChanged = "member_role_changed"
	MemberRemoved     = "member_removed"

	// Access tokens.
	AccessTokenCreated = "access_token_created"
	AccessTokenRevoked = "access_token_revoked"
	AccessTokenUsed    = "access_token_used"

	// Spaces.
	SpaceCreated           = "space_created"
	SpaceUpdated           = "space_updated"
	SpaceMemberAdded       = "space_member_added"
	SpaceMemberRoleChanged = "space_member_role_changed"
	SpaceMemberRemoved     = "space_member_removed"

	// A person takes their own data out (internal/export).
	DataExportStarted = "data_export_started"

	// Model provider accounts, shared CLI accounts, search and dictation.
	ProviderAccountCreated   = "provider_account_created"
	ProviderAccountUpdated   = "provider_account_updated"
	ProviderAccountRemoved   = "provider_account_removed"
	SharedCLIAccountCreated  = "shared_cli_account_created"
	SharedCLIAccountUpdated  = "shared_cli_account_updated"
	SharedCLIAccountRemoved  = "shared_cli_account_removed"
	CLIModesChanged          = "cli_account_modes_changed"
	SearchEmbeddingsChanged  = "search_embeddings_changed"
	SearchConsentChanged     = "search_consent_changed"
	DictationSettingsChanged = "dictation_settings_changed"
)

// Target types.
const (
	TargetUser          = "user"
	TargetWorkspace     = "workspace"
	TargetInvite        = "invite"
	TargetAccessToken   = "access_token"
	TargetSpace         = "space"
	TargetProvider      = "provider_account"
	TargetSharedAccount = "shared_cli_account"
)
