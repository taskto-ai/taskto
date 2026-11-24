package apierr

// The API's error codes: one list, which docs/api.md describes and the web app translates
// (errors.<code> in web/src/i18n/locales/en.json; CI checks every code here has one). A
// code is lower_snake_case, never reused for another meaning, and never renamed once
// released. Add one when people can cause the error and should be told what to do about
// it; errors only a broken client causes share CodeBadRequest.
const (
	// Any route.

	CodeInternal     Code = "internal"      // 500: details are in the server's log
	CodeBadRequest   Code = "bad_request"   // 400: a request the web app never sends
	CodeInvalidJSON  Code = "invalid_json"  // 400: the body isn't one JSON object of known fields
	CodeNotFound     Code = "not_found"     // 404
	CodeCrossOrigin  Code = "cross_origin"  // 403: refused by the CSRF guard
	CodeContentType  Code = "content_type"  // 415: the body's Content-Type isn't accepted
	CodeNotLoggedIn  Code = "not_logged_in" // 401
	CodeAdminOnly    Code = "admin_only"    // 403: only a workspace admin may
	CodeTokenRefused Code = "token_refused" // 403: needs a browser login, not an access token
	CodeTokenInvalid Code = "token_invalid" // 401: the access token is unknown, revoked or expired

	// Workspaces, accounts and invites (internal/auth).

	CodeWorkspaceNotFound   Code = "workspace_not_found"
	CodeSetupDone           Code = "setup_done"
	CodeSetupToken          Code = "setup_token"
	CodeEmailInvalid        Code = "email_invalid"
	CodeNameLength          Code = "name_length"           // params: max
	CodeWorkspaceNameLength Code = "workspace_name_length" // params: max
	CodePasswordTooShort    Code = "password_too_short"    // params: min
	CodePasswordTooLong     Code = "password_too_long"     // params: max (bytes)
	CodeCredentialsRequired Code = "credentials_required"
	CodeWrongCredentials    Code = "wrong_credentials" //nolint:gosec // G101: an error code, not a credential
	CodeTooManyLogins       Code = "too_many_logins"
	CodeLastAdmin           Code = "last_admin"
	CodeAlreadyMember       Code = "already_member"
	CodeMemberNotFound      Code = "member_not_found"
	CodeInviteNotFound      Code = "invite_not_found"
	CodeInviteInvalid       Code = "invite_invalid"
	CodeInviteOtherEmail    Code = "invite_other_email"
	CodeInviteAccountExists Code = "invite_account_exists"
	CodeLocaleInvalid       Code = "locale_invalid"
	CodeTokenNotFound       Code = "token_not_found"
	CodeTokenExpiryInvalid  Code = "token_expiry_invalid" //nolint:gosec // G101: an error code, not a credential
	CodeTooManyTokens       Code = "too_many_tokens"      // params: max

	// Passwords and reset links (internal/auth/passwords.go).

	CodeCurrentPasswordRequired Code = "current_password_required"
	CodeWrongPassword           Code = "wrong_password"     //nolint:gosec // G101: an error code, not a credential
	CodeTooManyPasswords        Code = "too_many_passwords" //nolint:gosec // G101: an error code, not a credential
	CodePasswordChanged         Code = "password_changed"   //nolint:gosec // G101: an error code, not a credential
	CodeResetInvalid            Code = "reset_invalid"
	CodeResetOwn                Code = "reset_own"
	CodeResetElsewhere          Code = "reset_elsewhere"

	// Single sign-on (internal/auth/sso.go). The callback redirects the browser to
	// /?sso_error=<code>; the same codes name the API's own answers.

	CodePasswordLoginDisabled     Code = "password_login_disabled" //nolint:gosec // G101: an error code, not a credential
	CodeSSOUnknownProvider        Code = "sso_unknown_provider"
	CodeSSOFailed                 Code = "sso_failed"
	CodeSSOExpired                Code = "sso_expired"
	CodeSSOEmailUnverified        Code = "sso_email_unverified"
	CodeSSOGoogleEmailUntrusted   Code = "sso_google_email_untrusted"
	CodeSSONoAccount              Code = "sso_no_account"
	CodeSSOAccountLinkedElsewhere Code = "sso_account_linked_elsewhere"

	// Spaces (internal/access).

	CodeSpaceNotFound       Code = "space_not_found"
	CodeSpaceReadOnly       Code = "space_read_only"
	CodeSpaceArchived       Code = "space_archived"
	CodeSpaceNameLength     Code = "space_name_length" // params: max
	CodeSpaceNotMember      Code = "space_not_workspace_member"
	CodeSpaceAlreadyMember  Code = "space_already_member"
	CodeSpaceMemberNotFound Code = "space_member_not_found"

	// Boards and cards (internal/boards).

	CodeBoardArchived       Code = "board_archived"
	CodeBoardChanged        Code = "board_changed"         // 409, with the board as it is now in "current"
	CodeCardChanged         Code = "card_changed"          // 409, with the card as it is now in "current"
	CodeBoardNameLength     Code = "board_name_length"     // params: max
	CodeCardTitleInvalid    Code = "card_title_invalid"    // params: max
	CodeCardBodyTooLong     Code = "card_body_too_long"    // params: max (KiB)
	CodeCommentLength       Code = "comment_length"        // params: max (KiB)
	CodeBlockedReasonLength Code = "blocked_reason_length" // params: max
	CodeTooManyLinks        Code = "too_many_links"        // params: max
	CodeDocPathInvalid      Code = "doc_path_invalid"      // params: path
	CodeAssigneeNoAccess    Code = "assignee_no_access"
	CodeCardBlockerNotFound Code = "card_blocker_not_found" // params: id
	CodeCardDueInvalid      Code = "card_due_invalid"
	CodeCardLabelInvalid    Code = "card_label_invalid" // params: max
	CodeTooManyLabels       Code = "too_many_labels"    // params: max

	// Personal apps (internal/apps).

	CodeAppOffline     Code = "app_offline"
	CodeAppChanged     Code = "app_changed"     // 409, with the app as it is now in "current"
	CodeAppNameLength  Code = "app_name_length" // params: max
	CodeTooManyApps    Code = "too_many_apps"   // params: max
	CodeAppFull        Code = "app_full"        // params: max_rows, max_mib
	CodeAppRowNotFound Code = "app_row_not_found"
	// CodeAppPreviewFull is a preview at its own row limit (params: max_rows); CodeAppPreviewTooLarge
	// a preview's request over its size limit (413; params: max_mib).
	CodeAppPreviewFull     Code = "app_preview_full"
	CodeAppPreviewTooLarge Code = "app_preview_too_large"
	CodeAppRecordConflict  Code = "app_record_conflict"  // 409: the record changed since the client read it; params: id, revision
	CodeAppFieldRequired   Code = "app_field_required"   // params: field
	CodeAppFieldTooLong    Code = "app_field_too_long"   // params: field, max
	CodeAppFieldNotNumber  Code = "app_field_not_number" // params: field
	CodeAppFieldTooSmall   Code = "app_field_too_small"  // params: field, min
	CodeAppFieldTooLarge   Code = "app_field_too_large"  // params: field, max
	CodeAppFieldNotDate    Code = "app_field_not_date"   // params: field
	CodeAppFieldNotTime    Code = "app_field_not_time"   // params: field
	CodeAppFieldNotOption  Code = "app_field_not_option" // params: field
	// CodeAppFieldRequiredWhen is a field the app requires when another field has a value
	// (params: field).
	CodeAppFieldRequiredWhen Code = "app_field_required_when"
	CodeAppForbidden         Code = "app_forbidden"        // 403: the caller's app roles don't allow it; params: capability
	CodeAppRolesInvalid      Code = "app_roles_invalid"    // 400
	CodeAppRolesNotMember    Code = "app_roles_not_member" // 400: a role for someone who can't read the space
	CodeAppRolesLockout      Code = "app_roles_lockout"    // 400: the change would take the caller's own right to manage roles

	// Effects of apps on boards and documents (internal/apps/effects.go, outbox.go).

	CodeAppEffectTarget      Code = "app_effect_target"       // 400: a target the person saving the app can't write; params: space_id
	CodeAppEffectTargetUnset Code = "app_effect_target_unset" // 400: an effect has no board chosen yet, which publishing needs; params: action
	CodeAppTemplateNotFound  Code = "app_template_not_found"  // 404: no such app template
	CodeAppTemplateBoard     Code = "app_template_board"      // 400: the template needs a board and none was given, or takes none and one was
	CodeAppOperationInvalid  Code = "app_operation_invalid"   // 422: the operation (idempotency key) isn't 8 to 64 letters, digits, - or _
	CodeAppOperationReused   Code = "app_operation_reused"    // 409: the operation was used for another request
	CodeAppEffectsBacklog    Code = "app_effects_backlog"     // 409: too many effects wait; params: max
	CodeAppEffectNotFailed   Code = "app_effect_not_failed"   // 409: only a failed effect is retried
	// The reasons an effect failed, kept with it and shown by its listing, not answers to a request.
	CodeAppEffectAppAccess    Code = "app_effect_app_access"    // the user no longer has the access to the app the action needed
	CodeAppEffectTargetAccess Code = "app_effect_target_access" // the user no longer may write what the effect targets
	CodeAppEffectTargetGone   Code = "app_effect_target_gone"   // the target board or card is no longer there
	CodeAppEffectBoardOffline Code = "app_effect_board_offline" // the target board is archived
	CodeAppEffectInvalid      Code = "app_effect_invalid"       // what the effect makes isn't accepted (a title, a path)
	CodeAppEffectDocExists    Code = "app_effect_doc_exists"    // a different document is at the path; params: path
	CodeAppEffectNoCard       Code = "app_effect_no_card"       // the card to change was never made for the record
	CodeAppEffectCardArchived Code = "app_effect_card_archived" // the card to change is archived
	CodeAppEffectAppOffline   Code = "app_effect_app_offline"   // the app is offline or its space archived
	CodeAppEffectRecordGone   Code = "app_effect_record_gone"   // the record it came from was deleted: restore it and retry
	CodeAppEffectGaveUp       Code = "app_effect_gave_up"       // it kept failing for a reason that may pass: retry it

	// Querying an app's records (internal/apps/query.go).

	CodeAppQueryCursor    Code = "app_query_cursor"    // 400: the cursor isn't one this query gave; start again from the first page
	CodeAppQueryField     Code = "app_query_field"     // 400: a field the live release doesn't declare, or can't be used so; params: field
	CodeAppQueryCondition Code = "app_query_condition" // 400: an operator or value that doesn't fit the field; params: field, op

	// Drafts and releases of apps (internal/apps/release.go).

	CodeAppDefinitionVersion   Code = "app_definition_version"   // 400: params: version, supported
	CodeAppNotPublished        Code = "app_not_published"        // 409: no release yet, so nothing to launch
	CodeAppReleaseNotFound     Code = "app_release_not_found"    // 404: the app has no such release
	CodeAppReleaseOutdated     Code = "app_release_outdated"     // 409: an action of an older release the live one can't take; params: release, live
	CodeAppReleaseIncompatible Code = "app_release_incompatible" // 409: the rows don't fit the release; params: fields
	CodeAppReleaseLive         Code = "app_release_already_live" // 409
	CodeAppNothingToPublish    Code = "app_nothing_to_publish"   // 409: the draft is the live release
	CodeAppMigrationRequired   Code = "app_migration_required"   // 409: schema changes to acknowledge; params: changes
	CodeAppMigrationUnneeded   Code = "app_migration_unneeded"   // 409: an acknowledgement of a change that isn't happening; params: changes
	CodeAppMigrationInvalid    Code = "app_migration_invalid"    // 400

	// Photo attachments of apps and their uploads (internal/apps/attachments.go).

	CodeAppFieldFiles       Code = "app_field_files"       // 400: an attachment field's value isn't a list of at most max attachment ids; params: field, max
	CodeAppAttachmentBad    Code = "app_attachment_bad"    // 400: an attachment can't be used here (unknown, not yours, not finished, another app's or field's, or already attached); params: field
	CodeAppAttachmentType   Code = "app_attachment_type"   // 400: the bytes aren't a JPEG, PNG or GIF picture the server can read, or it has too many pixels; params: max_megapixels
	CodeAppUploadSize       Code = "app_upload_size"       // 400: the declared size is 0 or over the field's limit; params: field, max (bytes)
	CodeAppUploadOffset     Code = "app_upload_offset"     // 409: a chunk wasn't at the upload's offset; params: offset (where the server is)
	CodeAppUploadState      Code = "app_upload_state"      // 409: the upload isn't in the state that call needs (finished already, or not complete yet)
	CodeAppUploadChunk      Code = "app_upload_chunk"      // 413: a chunk is over the limit, or runs past the declared size; params: max (bytes)
	CodeAppUploadsPending   Code = "app_uploads_pending"   // 409: too many unfinished uploads of the caller's; params: max
	CodeAppFilesFull        Code = "app_files_full"        // 409: the app's attachments are over their total; params: max_mib
	CodeAppFilesUnavailable Code = "app_files_unavailable" // 503: this server keeps no files for apps

	// Location, signature and barcode fields of apps (internal/apps/capture.go).

	CodeAppFieldNotLocation    Code = "app_field_not_location"    // 400: not a {lat, lon, accuracy_m, captured_at, source} within bounds; params: field
	CodeAppFieldLocationFuture Code = "app_field_location_future" // 400: a location captured later than the server's clock allows; params: field
	CodeAppFieldNotSignature   Code = "app_field_not_signature"   // 400: a signature field holds one attachment id; params: field
	CodeAppFieldNotBarcode     Code = "app_field_not_barcode"     // 400: not a barcode's text, or not valid for the field's format; params: field, format
	CodeAppFieldLocked         Code = "app_field_locked"          // 400: a captured location or a signature can't be changed once stored; params: field
	CodeAppSignatureType       Code = "app_signature_type"        // 400: a signature is a PNG picture

	// Offline use of apps (internal/apps/offline.go).

	CodeOfflineDisabled Code = "offline_downloads_disabled" // 403: an admin turned off downloading this space's apps for offline use
	CodeAccountChanged  Code = "session_account_changed"    // 409: the request was made for another account than the session's (the browser signed in as someone else)

	// Home widgets and navigation pins (internal/apps/home.go).

	CodeHomeWidgetNotFound Code = "home_widget_not_found" // 404: the app's live release has no such widget
	CodeHomeLimit          Code = "home_limit"            // 409: params: max
	CodeHomeOrderInvalid   Code = "home_order_invalid"    // 400: the ids aren't the caller's of that kind
	CodeHomeDefaultsShared Code = "home_defaults_shared"  // 409: defaults are for the apps of shared spaces

	// The knowledge base (internal/knowledge).

	CodeRequestTooLarge   Code = "request_too_large"    // 413; params: max (bytes)
	CodeDocNotFound       Code = "doc_not_found"        // 404
	CodeDocNameInvalid    Code = "doc_name_invalid"     // params: name
	CodeDocPathTooLong    Code = "doc_path_too_long"    // params: max (bytes)
	CodeDocPathTooDeep    Code = "doc_path_too_deep"    // params: max
	CodeDocNameTooLong    Code = "doc_name_too_long"    // params: max (bytes)
	CodeDocMessageInvalid Code = "doc_message_invalid"  // params: max
	CodeDocNotUTF8        Code = "doc_not_utf8"         // params: path
	CodeDocConflict       Code = "doc_conflict"         // 409
	CodeDocChanged        Code = "doc_changed"          // 409; params: path
	CodeDocDeleted        Code = "doc_deleted"          // 409; params: path
	CodeDocExists         Code = "doc_exists"           // 409; params: path
	CodeDocCaseConflict   Code = "doc_case_conflict"    // 409; params: name, other
	CodeDocPathTaken      Code = "doc_path_taken"       // 409; params: path
	CodeDocMoveIntoItself Code = "doc_move_into_itself" // params: path
	CodeDocMoveKind       Code = "doc_move_kind"        // params: path
	CodeDocDeleteFolder   Code = "doc_delete_folder"    // params: path
	CodeFileTooLarge      Code = "file_too_large"       // 413; params: path, max (bytes)
	CodeKnowledgeFull     Code = "knowledge_full"       // 507
	CodeDocNotKept        Code = "doc_not_kept"         // 410

	// Storage quotas (internal/quota): the operator's default for a workspace, and for each
	// member's private space.

	CodeQuotaWorkspace Code = "quota_workspace" // 507: the workspace's storage quota is used up; params: max_mib
	CodeQuotaUser      Code = "quota_user"      // 507: the private space's storage quota is used up; params: max_mib

	// Search (internal/search): searching, and a space's consent to embeddings.

	CodeSearchQueryRequired Code = "search_query_required"
	CodeSearchQueryTooLong  Code = "search_query_too_long" // params: max (bytes)
	CodeSearchQueryNoWords  Code = "search_query_no_words"

	// Conversations (internal/conversations).

	CodeConversationNotFound     Code = "conversation_not_found"
	CodeConversationArchived     Code = "conversation_archived"
	CodeConversationTitleInvalid Code = "conversation_title_invalid" // params: max
	CodeConversationLost         Code = "conversation_lost"          // 410: the agent runtime no longer has its content
	CodeConversationNoModel      Code = "conversation_no_model"
	CodeAgentOff                 Code = "agent_off"        // 503: the server runs without -agent
	CodeAgentNotReady            Code = "agent_not_ready"  // 503: starting or restarting; Retry-After
	CodeModelUnknown             Code = "model_unknown"    // the caller may not use the model, or it isn't configured
	CodeTooManyStreams           Code = "too_many_streams" // params: max
	CodeCLIPermissionNotFound    Code = "cli_permission_not_found"

	// The agent browser (internal/agentbrowser). The CLI workers' rule applies to it too.

	CodeBrowserOff           Code = "browser_off"             // the server runs without -browser
	CodeBrowserCLIWorkersOff Code = "browser_cli_workers_off" // the CLI workers it runs with are off
	CodeBrowserSingleUser    Code = "browser_single_user"     // local mode, and the server has more than one user
	CodeBrowserUnusable      Code = "browser_unusable"        // another reason it can't be used now
	CodeBrowserNotInstalled  Code = "browser_not_installed"
	CodeBrowserNotRunning    Code = "browser_not_running"
	CodeBrowserTooManyLive   Code = "browser_too_many_live"
	CodeBrowserBusy          Code = "browser_busy" // too many browsers run on the server
	CodeBrowserShuttingDown  Code = "browser_shutting_down"

	// Model provider accounts (internal/providers). The endpoint's codes are also those of
	// the other settings that keep an endpoint of their own: shared CLI accounts and
	// dictation; and of the tests of an endpoint, in their answer's code.

	CodeProviderNotFound           Code = "provider_not_found"
	CodeProviderNameInvalid        Code = "provider_name_invalid" // params: max
	CodeProviderNameTaken          Code = "provider_name_taken"
	CodeProviderDisplayNameInvalid Code = "provider_display_name_invalid" // params: max
	CodeProviderModelsCount        Code = "provider_models_count"         // params: max
	CodeProviderModelInvalid       Code = "provider_model_invalid"        // params: max
	CodeTooManyProviders           Code = "too_many_providers"            // params: max
	CodeBaseURLInvalid             Code = "base_url_invalid"              // params: max
	CodeBaseURLCredentials         Code = "base_url_credentials"          //nolint:gosec // G101: an error code, not a credential
	CodeBaseURLInsecure            Code = "base_url_insecure"
	CodeAPIKeyInvalid              Code = "api_key_invalid" //nolint:gosec // G101: an error code, not a credential; params: min, max
	CodeEndpointUnreachable        Code = "endpoint_unreachable"
	CodeEndpointKeyRefused         Code = "endpoint_key_refused" // params: status
	CodeEndpointStatus             Code = "endpoint_status"      // params: status
	CodeEndpointNoModels           Code = "endpoint_no_models"

	// Egress policies (internal/egress).

	CodeEgressRefused Code = "egress_refused" // params: host

	// Outgoing email (internal/mail, internal/server/mail.go).

	CodeMailNotConfigured Code = "mail_not_configured"
	CodeMailConnectFailed Code = "mail_connect_failed" // params: host
	CodeMailTLSFailed     Code = "mail_tls_failed"
	CodeMailAuthFailed    Code = "mail_auth_failed"
	CodeMailRejected      Code = "mail_rejected" // params: smtp_code
	CodeMailFailed        Code = "mail_failed"
	// CodeMailTestRateLimited is a 429: the person sent a test email a moment ago, or the server's
	// test sends are all busy. Retry-After says how long.
	CodeMailTestRateLimited Code = "mail_test_rate_limited"

	// CLI accounts, shared CLI accounts and automatic switching (internal/cliaccounts).

	CodeCLIWorkersOff          Code = "cli_workers_off"
	CodeCLIWorkersNoBoundary   Code = "cli_workers_no_boundary"
	CodeCLINotInstalled        Code = "cli_not_installed" // params: cli
	CodeCLIWorkerClosing       Code = "cli_worker_closing"
	CodeCLINoLogin             Code = "cli_no_login"
	CodeCLILoginCodeInvalid    Code = "cli_login_code_invalid" // params: max
	CodeCLILoginNoCode         Code = "cli_login_no_code"
	CodeCLILabelInvalid        Code = "cli_label_invalid" // params: max
	CodeCLIAccountNotConnected Code = "cli_account_not_connected"
	CodeSharedAccountsOff      Code = "shared_accounts_off"
	CodeSharedAccountsNoProxy  Code = "shared_accounts_no_proxy"
	CodeSharedAccountNotFound  Code = "shared_account_not_found"
	CodeSharedLabelInvalid     Code = "shared_label_invalid" // params: max
	CodeSharedModelInvalid     Code = "shared_model_invalid" // params: max
	CodeSharedKeyUnreadable    Code = "shared_key_unreadable"
	CodeTooManySharedAccounts  Code = "too_many_shared_accounts" // params: max
	CodeQueueTooLong           Code = "queue_too_long"           // params: max
	CodeQueueAccountNotFound   Code = "queue_account_not_found"

	// Dictation (internal/dictation): its settings, the engine's state, and dictating.

	CodeDictationLanguageInvalid    Code = "dictation_language_invalid"
	CodeDictationModelInvalid       Code = "dictation_model_invalid" // params: max
	CodeDictationKeyRequired        Code = "dictation_key_required"
	CodeDictationNoticeRequired     Code = "dictation_notice_required"
	CodeDictationEngineUnavailable  Code = "dictation_engine_unavailable"
	CodeDictationAddonMissing       Code = "dictation_addon_missing" // params: name, vendor
	CodeDictationLocalOff           Code = "dictation_local_off"
	CodeDictationLocalNotDownloaded Code = "dictation_local_not_downloaded"
	CodeDictationLocalDownloading   Code = "dictation_local_downloading" // params: percent
	CodeDictationLocalNoBuild       Code = "dictation_local_no_build"    // params: model, platform
	CodeDictationLocalFailed        Code = "dictation_local_failed"
	CodeDictationLocalUnavailable   Code = "dictation_local_unavailable"
	CodeDictationBusy               Code = "dictation_busy"
	CodeDictationServerBusy         Code = "dictation_server_busy"
	CodeDictationFailed             Code = "dictation_failed"
	CodeDictationTimeout            Code = "dictation_timeout"
	CodeAudioInvalid                Code = "audio_invalid"
	CodeAudioTooLong                Code = "audio_too_long" // params: minutes
	CodeAudioEmpty                  Code = "audio_empty"

	// Search's settings and spaces' consent (internal/search).

	CodeEmbeddingsModelInvalid   Code = "embeddings_model_invalid" // params: max
	CodeEmbeddingsAccountInvalid Code = "embeddings_account_invalid"
	CodeEmbeddingsFailed         Code = "embeddings_failed"
	CodeConsentForbidden         Code = "consent_forbidden"
	CodeConsentNoModel           Code = "consent_no_model"
	CodeConsentEndpointChanged   Code = "consent_endpoint_changed"

	// Notifications and Web Push (internal/notifications).

	CodeNotificationNotFound     Code = "notification_not_found"      // 404: not the caller's, or gone
	CodePushEndpointInvalid      Code = "push_endpoint_invalid"       // 400: a subscription's endpoint isn't an https address of at most 2048 characters
	CodePushKeysInvalid          Code = "push_keys_invalid"           // 400: a subscription's keys aren't a P-256 key and a 16-byte secret
	CodePushTooManySubscriptions Code = "push_too_many_subscriptions" // 409: the person has the most devices subscribed; params: max
	CodePushNoWorkspace          Code = "push_no_workspace"           // 403: push is for people in a workspace, and the caller is in none
)
