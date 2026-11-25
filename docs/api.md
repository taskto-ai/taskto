# HTTP API

The API is JSON under `/api`. This page covers accounts, workspaces, members, spaces,
access tokens, boards, personal apps, model providers, dictation, the backups' status, outgoing email, the knowledge base and its search;
later features add their routes here. People's own agents use the knowledge base, boards
and apps over MCP, at `/mcp`, with an access token: see [mcp.md](mcp.md).

## Conventions

- **Bodies** are JSON objects. Unknown fields, trailing data and bodies over 64 KiB are
  rejected with `400`.
- **Errors** are `{"error": "what went wrong", "code": "name_length", "params": {"max": 100}}`
  with a 4xx or 5xx status. `error` is English, for people reading the API; `code` is
  stable, for clients to branch on and for the web app to show the error in the user's
  language; `params`, when present, are the values the message names. A 5xx never carries
  details; they go to the server log. See [Error codes](#error-codes). Routes not moved to
  codes yet send `error` alone.
- **Times** are UTC strings like `2026-01-02T03:04:05.000Z`.
- **IDs** are integers.
- Replies are `Cache-Control: no-store`.

### API types

The web app's types for the replies (`web/src/api.ts`) are checked against the Go structs that
make them, so a field renamed or retyped in Go breaks the build instead of the page. This holds
for each registered reply type (below); a type with no line in the contract is not checked:

1. `internal/apitypes` lists the reply structs (`Roots`) and writes their shapes, read by
   reflection as `encoding/json` writes them, to `web/src/api.gen.ts`
   (`go generate ./internal/apitypes`). The file is committed, and `go test` fails when it is
   not what the structs say.
2. `web/src/api.contract.ts` checks each type `api.ts` declares against its generated shape
   (`Check<Gen.Card, Api.Card>`): every field the web app reads must exist in the reply, be
   present and non-null there unless the web app's type allows it, and have a type the
   reply's is assignable to. `pnpm typecheck` (part of `pnpm build`) fails, naming the field,
   when one does not. The web app's types may be wider than the generated ones and may leave
   out fields it does not read. They may be narrower in one way: a union of string literals
   for a Go string (`'admin' | 'member'`). `any` is refused on either side, and a field of
   free-form JSON (`unknown`) is read as `unknown` or not at all. A Go map is a
   `Record<string, T>`: the web app can't read a fixed key from it unless the key is optional.

What the generator tells, as `encoding/json` writes it:

- A key that may be **left out** (`?`: `omitempty` on a value that can be empty, `omitzero`, or
  a field of an embedded pointer that may be nil) is told apart from a value that may be
  **null** (`| null`: a nil pointer, slice or map, a nil `[]byte`). A nil value that is left
  out is not null, but a pointer that is not nil can hold one: an `omitempty` `*[]string` is
  `p?: string[] | null`, since a pointer to a nil slice is written as `null`.
- A type with a `MarshalJSON` or `MarshalText` of its own writes what its struct doesn't say, so
  the generator refuses it (a root, or any field, slice or map element of one) unless its wire
  shape is registered (`wireShapes` in `internal/apitypes/generate.go`: `time.Time` is a
  string; `apierr.Params` is a map of texts and numbers). A `MarshalText` alone is a string.
- An `any`, `interface{}` or `json.RawMessage` is refused unless its field is tagged
  `apitypes:"json"`, which tells it as `unknown`.
- A nil slice or map is told `T[] | null`. A field the web app reads as a plain list is tagged
  `apitypes:"nonnull"` and then told `T[]`: a promise that the server sends `[]` or `{}`, so
  initialize it in Go. `TestNonNullFieldsAreKept` fails for a tagged field whose package has no
  test calling `nonnull.Require` (`internal/apitypes/nonnull`) on what its routes reply.
- A map's key is a string, an integer (`uintptr` too) or itself an `encoding.TextMarshaler`.

To cover another reply, add its struct to `Roots`, run `go generate ./internal/apitypes`, and
add a line to `api.contract.ts`. A reply struct that is not exported, or that a handler builds
as a `map` or an anonymous struct, needs a named, exported struct first (the card route's
reply is `boards.Thread`, a card with its comments). What this does not check: that a call's
type argument (`api<Card[]>(…)`) is the reply of its path, or the values of a Go string
type's constants.

### Error codes

Every code is listed here and in `internal/apierr/codes.go`, the one list; a code keeps its
meaning once released. The web app has a message for each (`errors.<code>` in
`web/src/i18n/locales/`), which CI checks.

| Code | Status | When |
|---|---|---|
| `internal` | 500 | Something failed on the server; the log has the details. |
| `bad_request` | 400 | A request the web app never sends (a wrong enum, a missing field); `error` says what. |
| `invalid_json` | 400 | The body isn't one JSON object of the route's fields, or is over its size. |
| `not_found` | 404 | What the path names doesn't exist, or isn't the caller's to see. |
| `cross_origin` | 403 | The CSRF guard refused a cross-origin request. |
| `content_type` | 415 | The body's `Content-Type` isn't `application/json` (or `application/octet-stream` where a route takes bytes). |
| `not_logged_in` | 401 | No valid session. |
| `admin_only` | 403 | Only a workspace admin may do this. |
| `token_refused` | 403 | The route needs a login in the browser, not an access token. |
| `token_invalid` | 401 | The access token is unknown, revoked or expired. |
| `workspace_not_found` | 404 | No such workspace, or the caller isn't a member. |
| `setup_done` | 409 | The server is already set up. |
| `setup_token` | 403 | The setup token is missing or wrong. |
| `email_invalid` | 400 | Not a valid email address. |
| `name_length` | 400 | A name must have 1 to `max` characters. |
| `workspace_creation_refused` | 403 | This server doesn't let the caller create a workspace (`-workspace-creation`). |
| `workspace_limit` | 403 | The caller has already created `max` workspaces, the most one account may. |
| `too_many_workspaces` | 429 | The caller created 5 workspaces within the last hour; `Retry-After` says when to try again. |
| `operator_only` | 403 | Only the server's operator (an admin of the workspace setup made) may do this. |
| `workspace_name_length` | 400 | A workspace's name must have 1 to `max` characters. |
| `password_too_short` | 400 | The password has fewer than `min` characters. |
| `password_too_long` | 400 | The password has more than `max` bytes. |
| `credentials_required` | 400 | Email and password are required. |
| `wrong_credentials` | 401 | Wrong email or password. |
| `too_many_logins` | 429 | Too many login attempts; `Retry-After` says when to try again. |
| `last_admin` | 409 | A workspace needs at least one admin. |
| `already_member` | 409 | Already a member of this workspace. |
| `member_not_found` | 404 | No such member of the workspace. |
| `invite_not_found` | 404 | No such pending invite. |
| `invite_invalid` | 404 | The invite is unknown, used or expired. |
| `invite_other_email` | 403 | The invite is for another email address than the logged-in user's. |
| `invite_account_exists` | 409 | An account with the invite's email exists: log in, then accept it. |
| `locale_invalid` | 400 | Not a BCP 47 tag such as `pt-BR`, or `""`. |
| `token_not_found` | 404 | No such access token of the caller's. |
| `token_expiry_invalid` | 400 | `expires_at` isn't a future time in RFC 3339. |
| `too_many_tokens` | 409 | The caller has `max` access tokens in the workspace: revoke one first. |
| `current_password_required` | 400 | Changing a password needs the current one. |
| `wrong_password` | 403 | The current password is wrong. |
| `too_many_passwords` | 429 | Too many wrong current passwords; `Retry-After` says when to try again. |
| `password_changed` | 409 | The password changed meanwhile: try again. |
| `reset_invalid` | 404 | The reset link is unknown, used or expired. |
| `reset_own` | 400 | An admin changes their own password in Settings, not with a reset link. |
| `reset_elsewhere` | 403 | The member also belongs to a workspace the caller doesn't administer. |
| `wrong_two_factor_code` | 401 / 403 | The code from the authenticator app (or the recovery code) is wrong, was used already, or is for another time. `401` at the second step of a login, `403` for a logged-in user's own settings. |
| `too_many_two_factor_codes` | 429 | Too many wrong codes; `Retry-After` says when to try again. |
| `two_factor_challenge_invalid` | 401 | The half-done login is unknown, used, expired (5 minutes) or had too many wrong codes: log in with the password again. |
| `two_factor_already_on` | 409 | Two-factor login is on already. |
| `two_factor_not_on` | 409 | Two-factor login isn't on, for this user. |
| `two_factor_not_started` | 409 | No secret waits to be confirmed: start with `enrol`. |
| `two_factor_no_password` | 403 | The account has no password (it signs in with a provider), so there is no password login to protect. |
| `two_factor_clear_own` | 400 | An admin turns off their own two-factor login in Settings, not by clearing it. |
| `two_factor_clear_elsewhere` | 403 | The member also belongs to a workspace the caller doesn't administer. |
| `password_login_disabled` | 403 | Passwords are turned off on this server ([sso.md](sso.md#turning-passwords-off)): logging in, a new account with a password, making a reset link and using one answer this. |
| `sso_unknown_provider` | 404 | No such single sign-on provider. |
| `sso_failed` | 502 | The provider didn't answer. As `?sso_error=`: the provider or the exchange failed, or the person declined (details are in the server's log). |
| `sso_expired` | | Only as `?sso_error=`: the sign-in's state is missing, doesn't match the browser's cookie, expired or was used already. Start again. |
| `sso_email_unverified` | | Only as `?sso_error=`: the provider gave no email, or didn't verify it. |
| `sso_google_email_untrusted` | | Only as `?sso_error=`: with Google, the email can't link an account or make one from an invite unless it ends in `@gmail.com` or the ID token has an `hd` claim (a Google Workspace account). |
| `sso_no_account` | | Only as `?sso_error=`: no account has the email, and no invite is pending for it. |
| `sso_account_linked_elsewhere` | | Only as `?sso_error=`: the email's account is linked to another identity of this provider. |
| `space_not_found` | 404 | No such space, or the caller can't read it. |
| `space_read_only` | 403 | The caller may only read this space. |
| `space_archived` | 403 | The space is archived, so read-only. |
| `space_name_length` | 400 | A space's name must have 1 to `max` characters. |
| `space_not_workspace_member` | 404 | The user isn't a member of the space's workspace. |
| `space_already_member` | 409 | The user is already a member of the space. |
| `space_member_not_found` | 404 | The user isn't a member of the space. |
| `board_archived` | 403 | The board is archived, so it and its cards are read-only. |
| `board_changed` | 409 | The board changed since the client read it; `current` is the board as it is now. |
| `card_changed` | 409 | The card changed since the client read it; `current` is the card as it is now. |
| `board_name_length` | 400 | A board's name must have 1 to `max` characters. |
| `card_title_invalid` | 400 | A card's title must be one line of 1 to `max` characters. |
| `card_body_too_long` | 400 | A card's body is over `max` KiB. |
| `comment_length` | 400 | A comment is blank, or over `max` KiB. |
| `blocked_reason_length` | 400 | A blocked reason must have 1 to `max` characters. |
| `too_many_links` | 400 | A card links to more than `max` documents, conversations or cards it waits for. |
| `doc_path_invalid` | 400 | `path` isn't a document path within the space, like `notes/plan.md`. |
| `assignee_no_access` | 400 | The assignee isn't a member of the workspace who can read the space. |
| `card_blocker_not_found` | 400 | Card `id`, which the card would wait for, isn't on the board (any more). |
| `card_due_invalid` | 400 | A due date (or `due_before`, `due_after`, `today`) isn't a calendar date like `2026-03-31`. |
| `card_label_invalid` | 400 | A label must be 1 to `max` characters, on one line. |
| `too_many_labels` | 400 | A card has more than `max` labels, or a list filters by more than `max`. |
| `app_offline` | 409 | The app is offline: restore it to launch it or run its actions. |
| `app_changed` | 409 | The app changed since the client read it; `current` is the app as it is now. |
| `app_name_length` | 400 | An app's name must have 1 to `max` characters. |
| `too_many_apps` | 409 | The space has `max` apps. |
| `app_full` | 409 | The app holds `max_rows` rows or `max_mib` MiB of data. |
| `app_preview_full` | 409 | A [preview](#post-appsapppreview) holds `max_rows` sample rows already. |
| `app_preview_too_large` | 413 | A preview's request, with all its sample rows, is over `max_mib` MiB. |
| `app_forbidden` | 403 | The space allows it, but the caller's roles in the app don't: `capability` is `design`, `publish`, `submit`, `modify_records` or `manage_roles`. |
| `app_definition_version` | 400 | The definition's `definition_version` is `version`, which this server doesn't read (it reads up to `supported`). |
| `app_not_published` | 409 | The app has no release yet: it is a draft, so it can't be launched or have its actions run. |
| `app_release_not_found` | 404 | The app has no such release. |
| `app_release_outdated` | 409 | An action from release `release` that the live release `live` can't take; reload the app. |
| `app_release_incompatible` | 409 | A rollback to a release the app's data doesn't fit; `fields` names the fields that don't. |
| `app_release_already_live` | 409 | The release to roll back to is the live one. |
| `app_nothing_to_publish` | 409 | The draft is the live release's definition. |
| `home_widget_not_found` | 404 | The app's live release offers no widget with that id. |
| `home_limit` | 409 | A person has at most `max` widgets (or pins) in a workspace, and a space at most `max` default widgets (or pins). |
| `home_order_invalid` | 400 | The ids to put in order aren't all the caller's own placements of that kind, or one is listed twice. |
| `home_defaults_shared` | 409 | Defaults are only for the apps of shared spaces. |
| `audit_time_invalid` | 400 | The audit log's `from` or `to` is outside the years 1000 to 9999. |
| `app_migration_required` | 409 | The publish changes the collection in ways stored rows are affected by; `changes` lists them, to acknowledge. |
| `app_migration_unneeded` | 409 | The publish acknowledges changes it doesn't make; `changes` lists them. |
| `app_migration_invalid` | 400 | An acknowledgement that is malformed: an unknown `change`, a field that is no name, or one twice. |
| `app_effect_target` | 400 | An effect of the definition targets space `space_id` (or a board of it) that the person saving, publishing or rolling back the app can't use (write, for an effect that makes or moves something; read, for a route), or that is in another workspace than the app's, whatever they may do there. |
| `app_effect_target_unset` | 400 | The draft has an effect with no board chosen yet (a template's, or a copy's whose boards its copier can't write): choose one (`POST …/effect-board`) before publishing; `action` names the first. |
| `app_template_not_found` | 404 | No app template has that id. |
| `app_template_board` | 400 | The template needs a board and none was given, or takes none and one was. |
| `app_operation_invalid` | 422 | An action's `operation` isn't 8 to 64 letters, digits, `-` or `_`. |
| `app_operation_reused` | 409 | The `operation` was used before for a different request. |
| `app_token_rate_limited` | 429 | The caller's access tokens ran their budget of actions in this workspace (60 a minute); `Retry-After` says when to try again (params: `max`, `seconds`). |
| `app_effects_backlog` | 409 | The app has `max` effects waiting: the action changed nothing. |
| `app_effect_not_failed` | 409 | Only a failed effect is retried. |
| `app_roles_invalid` | 400 | The role assignments name an unknown role, give a user no roles or twice, or are too many. |
| `app_roles_not_member` | 400 | A role is for someone who can't read the space. |
| `app_roles_lockout` | 400 | The assignments would take the caller's own right to manage the app's roles. |
| `app_row_not_found` | 422 | The action names a row the app doesn't have (any more), or one the caller may not change. |
| `app_record_conflict` | 409 | The record changed since the client read it (or isn't deleted, for a restore); `params.id` is its ID, and `current` is the record as an item as it is now, only when the caller may read it. |
| `app_query_cursor` | 400 | The `cursor` of a records query isn't one this query gave (it is another query's, another person's or damaged): start again from the first page. |
| `app_query_field` | 400 | A records query names a `field` the live release doesn't declare, or can't use that way (a sum of text, a deletion time of live records). |
| `app_query_condition` | 400 | A filter's operator or value doesn't fit its `field` (`op` says which operator), or the field holds photos, a signature or a location, which can't be filtered by. |
| `app_field_required` | 422 | The action leaves out `field`, which the app requires. |
| `app_field_too_long` | 422 | The value of `field` has more than `max` characters. |
| `app_field_not_number` | 422 | The value of `field` isn't a number. |
| `app_field_too_small` | 422 | The value of `field` is under `min`. |
| `app_field_too_large` | 422 | The value of `field` is over `max`. |
| `app_field_not_date` | 422 | The value of `field` isn't a date, `YYYY-MM-DD`. |
| `app_field_not_time` | 422 | The value of `field` isn't a time, `HH:MM` on a 24-hour clock. |
| `app_field_not_option` | 422 | The value of `field`, a choice, isn't one of its options (or is several of them). |
| `app_field_required_when` | 422 | `field` is required when the row meets the condition its `required_when` rule sets, and the action leaves it out. |
| `app_field_files` | 400 / 422 | The value of `field`, an attachment field, isn't a list of at most `max` different attachment ids. |
| `app_attachment_bad` | 422 | An attachment named in a record can't be used there: it isn't the caller's own upload for that `field` of this app, isn't finished, or is in a record already. The reason is never told, so someone else's id isn't told from one that doesn't exist, and the whole change is refused. |
| `app_attachment_type` | 400 | The file isn't a JPEG, PNG or GIF picture the server can read, or has more pixels than it takes (`max_megapixels`); the upload is removed. |
| `app_upload_size` | 400 | The declared size of an upload is 0 or over the `max` bytes `field` takes. |
| `app_upload_offset` | 409 | A chunk wasn't at the offset the server is at, or the upload isn't complete: `params.offset` is the bytes the server has, where to continue from. |
| `app_upload_state` | 409 | The upload can't take that now: it is finalized already (a chunk), or a record holds it (abandoning it). |
| `app_upload_chunk` | 413 | A chunk is over 2 MiB, or runs past the declared size; `params.max` is what it may be. |
| `app_uploads_pending` | 409 | The caller has `max` photos in this app that are in no record yet. |
| `app_files_full` | 409 | The app's attachments would take it over `max_mib` MiB in all. |
| `app_files_unavailable` | 503 | This server keeps no files for apps. |
| `app_field_not_location` | 422 | The value of `field`, a location field, isn't `{lat, lon, accuracy_m, captured_at, source}` within bounds ([Location, signature and barcode fields](#location-signature-and-barcode-fields)). |
| `app_field_location_future` | 422 | A location's `captured_at` is more than five minutes ahead of the server's clock. |
| `app_field_not_signature` | 422 | The value of `field`, a signature field, isn't the id of one attachment. |
| `app_field_not_barcode` | 422 | The value of `field`, a barcode field, isn't the text of a code (no control characters, within `max_length`), or isn't valid for the `format` the field reads. |
| `app_field_locked` | 422 | `field` holds a captured location or a signature, which no later change may alter or clear unless the field says `editable`. |
| `app_signature_type` | 400 | The picture uploaded for a signature field isn't a PNG; the upload is removed. |
| `offline_downloads_disabled` | 403 | An admin turned off downloading this space's apps for offline use. |
| `session_account_changed` | 409 | The request names (`X-Taskto-Expected-User`) another account than the session's: a queue made by one account is being sent with another's session. Nothing was done. |
| `request_too_large` | 413 | The request's body is over `max` bytes. |
| `doc_not_found` | 404 | No such document, folder or version in the space. |
| `doc_name_invalid` | 400 | `name` can't be a path in a space: empty parts, `..`, `.git`, `.taskto` (the server's own storage), a leading `/`, backslashes or control characters. |
| `doc_path_too_long` | 400 | A path has at most `max` bytes. |
| `doc_path_too_deep` | 400 | A path has at most `max` levels of folders. |
| `doc_name_too_long` | 400 | A file or folder name has at most `max` bytes. |
| `doc_message_invalid` | 400 | A change's message is one line of at most `max` characters. |
| `doc_not_utf8` | 400 | The Markdown document `path` isn't UTF-8 text. |
| `doc_conflict` | 409 | The change conflicts with the space's documents as they are now. |
| `doc_changed` | 409 | `path` has changed since the version the change was based on. |
| `doc_deleted` | 409 | `path` was deleted since the version the change was based on. |
| `doc_exists` | 409 | `path` already exists: to replace it, send the version you edited as `base_version`. |
| `doc_case_conflict` | 409 | `name` differs only by case, or by how an accent is written, from `other`, which exists. |
| `doc_path_taken` | 409 | A folder is at `path`, or a file is where one of its folders would be. |
| `doc_move_into_itself` | 400 | A folder can't be moved into itself (`path`). |
| `doc_move_kind` | 400 | The unversioned file `path` can't become a Markdown document, or the other way round. |
| `doc_delete_folder` | 400 | `path` is a folder: delete its documents. |
| `file_too_large` | 413 | `path` is over the limit of `max` bytes. |
| `knowledge_full` | 507 | The space's knowledge base is full. |
| `quota_workspace` | 507 | The write would take the workspace over its storage quota of `max_mib` MiB. |
| `quota_user` | 507 | The write would take the private space over its storage quota of `max_mib` MiB. |
| `quota_unavailable` | 503 | The storage in use couldn't be measured; the write was refused, and trying again may work. |
| `doc_not_kept` | 410 | Only the current version of an unversioned file is kept. |
| `search_query_required` | 400 | A search needs something to search for. |
| `search_query_too_long` | 400 | A search is at most `max` bytes of text. |
| `search_query_no_words` | 400 | The search has no words to search for. |
| `conversation_not_found` | 404 | No such conversation of the caller's in the workspace. |
| `conversation_archived` | 409 | The conversation is archived: unarchive it first. |
| `conversation_title_invalid` | 400 | A title must have at most `max` printable characters. |
| `conversation_lost` | 410 | The agent runtime no longer has the conversation's content. |
| `conversation_no_model` | 409 | The conversation has no model: choose one. |
| `agent_off` | 503 | The agent runtime is off: the server runs without `-agent`. |
| `agent_not_ready` | 503 | The agent runtime is starting or restarting; `Retry-After` says when to try again. |
| `model_unknown` | 400 | The caller may not use that model, or it isn't configured. |
| `too_many_streams` | 429 | The caller has `max` event streams open: close one first. |
| `cli_permission_not_found` | 404 | The CLI's question isn't waiting: answered already, timed out, or its turn is over. |
| `browser_off` | 409 | The agent browser is off: the server runs without `-browser`. |
| `browser_cli_workers_off` | 409 | The CLI workers the browser runs with are off. |
| `browser_single_user` | 409 | The CLI workers run in local mode, allowed only while the server has a single user. |
| `browser_unusable` | 409 | The browser can't be used now for another reason; `error` says why. |
| `browser_not_installed` | 503 | The browser isn't installed on the CLI worker. |
| `browser_not_running` | 409 | The browser isn't running: start it first. |
| `browser_too_many_live` | 429 | Too many live views of the browser are open: close one first. |
| `browser_busy` | 503 | Too many browsers are running on the server: try again in a few minutes. |
| `browser_shutting_down` | 503 | The browser service is shutting down. |
| `provider_not_found` | 404 | No such provider account of the workspace's, or of the caller's own. |
| `provider_name_invalid` | 400 | A provider account's name must be 1 to `max` characters of `a-z`, `0-9`, `.`, `_` or `-`. |
| `provider_name_taken` | 409 | An account with this name exists. |
| `provider_display_name_invalid` | 400 | A provider account's display name may have up to `max` printable characters. |
| `provider_models_count` | 400 | A provider account lists 1 to `max` models. |
| `provider_model_invalid` | 400 | A model's ID must be 1 to `max` printable characters. |
| `too_many_providers` | 409 | The workspace, or the user, has `max` provider accounts. |
| `base_url_invalid` | 400 | Not an http or https URL of up to `max` characters, without a query or fragment (provider accounts, shared CLI accounts, dictation). |
| `base_url_credentials` | 400 | The base URL has credentials in it: the key goes in `api_key`. |
| `base_url_insecure` | 400 | A key would be sent over plain http to another machine: use https. |
| `api_key_invalid` | 400 | An API key must be `min` to `max` printable ASCII characters, without spaces. |
| `endpoint_unreachable` | 200 | In a test's answer (`code`): the endpoint didn't answer. |
| `endpoint_key_refused` | 200 | In a test's answer: the endpoint answered `status` (401 or 403) to the key. |
| `endpoint_status` | 200 | In a test's answer: the endpoint answered `status`. |
| `endpoint_no_models` | 200 | In a test's answer: the endpoint's `GET /models` isn't a list of models. |
| `egress_refused` | 400 | The server's egress policy doesn't allow the endpoint's `host` (also in a test's answer, and 502 for a dictation or a test email). |
| `mail_not_configured` | 409 | The server's operator hasn't set an SMTP server ([install.md](install.md#email)). |
| `mail_connect_failed` | 502 | The SMTP server at `host` couldn't be reached. |
| `mail_tls_failed` | 502 | The connection to the SMTP server couldn't be made secure: its certificate isn't trusted, or it doesn't offer STARTTLS. |
| `mail_auth_failed` | 502 | The SMTP server refused the sign-in. |
| `mail_rejected` | 502 | The SMTP server refused the message, with its reply code `smtp_code`. |
| `mail_failed` | 502 | The message couldn't be sent. |
| `mail_test_rate_limited` | 429 | A test email was sent a moment ago by the caller, or the server's test sends are all busy; `Retry-After` says how long. |
| `export_running` | 409 | The caller has an export of their data running; `Retry-After` says when to look again. |
| `export_rate_limited` | 429 | The caller started their share of exports this hour, or the server's exports are all busy; `Retry-After` says how long. |
| `cli_workers_off` | 409 | CLI workers are off on this server (also a list's `reason_code`). |
| `cli_workers_no_boundary` | 409 | CLI workers run in local mode, which allows a single user only (also a list's `reason_code`). |
| `cli_not_installed` | 503 | The CLI, `cli`, isn't installed on this server. |
| `cli_worker_closing` | 503 | The CLI worker is shutting down. |
| `cli_no_login` | 404 | No sign-in is in progress for this account. |
| `cli_login_code_invalid` | 400 | A sign-in's code must be 1 to `max` printable characters. |
| `cli_login_no_code` | 409 | This sign-in doesn't take a code. |
| `cli_label_invalid` | 400 | A CLI account's name may have up to `max` characters, on one line. |
| `cli_account_not_connected` | 404 | The CLI account isn't connected: connect it first. |
| `shared_accounts_off` | 409 | Shared CLI accounts are off on this server (also a list's `reason_code`). |
| `shared_accounts_no_proxy` | 200 | A list's `reason_code`: the server has no proxy for the CLIs, which shared accounts need. |
| `shared_account_not_found` | 404 | No such shared CLI account in the workspace. |
| `shared_label_invalid` | 400 | A shared account's name must have 1 to `max` characters, on one line. |
| `shared_model_invalid` | 400 | A shared account's model is up to `max` of `A-Z a-z 0-9 . _ : / [ ] -`. |
| `shared_key_unreadable` | 200 | In a test's answer: the account's key can't be read (was the server key replaced?). |
| `too_many_shared_accounts` | 409 | The workspace has `max` shared CLI accounts. |
| `queue_too_long` | 400 | The switching queue may have up to `max` accounts. |
| `queue_account_not_found` | 404 | An account in the queue isn't the caller's, or one of their workspaces'. |
| `dictation_language_invalid` | 400 | The language must be an ISO 639-1 code such as `pt`, or empty to detect it. |
| `dictation_model_invalid` | 400 | The hosted API's model must be 1 to `max` printable characters. |
| `dictation_key_required` | 400 | The API key must be sent again to change the base URL's host. |
| `dictation_notice_required` | 400 | An add-on's notice must be confirmed before choosing it. |
| `dictation_engine_unavailable` | 400, 503 | The workspace's engine isn't available on this server (also a status's `reason_code`). |
| `dictation_addon_missing` | 503 | The add-on `name` by `vendor` isn't installed any more (also a status's `reason_code`). |
| `dictation_local_off` | 400, 503 | The server's local speech model is off (also a status's `reason_code`). |
| `dictation_local_not_downloaded` | 503 | The local model isn't downloaded yet: it downloads on first use (also a `reason_code`). |
| `dictation_local_downloading` | 503 | The local model is downloading, `percent` done (also a `reason_code`). |
| `dictation_local_no_build` | 503 | The local model `model` has no build for `platform` (also a `reason_code`, and the local state's `error_code`). |
| `dictation_local_failed` | 503 | Downloading the local model failed; the next dictation tries again (also a `reason_code` and `error_code`). |
| `dictation_local_unavailable` | 503 | The local model can't run on this server (also a `reason_code`). |
| `dictation_busy` | 429 | The caller is already dictating. |
| `dictation_server_busy` | 429 | The server is transcribing too many dictations at once. |
| `dictation_failed` | 502 | The speech-to-text engine failed; the log has the details. |
| `dictation_timeout` | 504 | The transcription took too long. |
| `audio_invalid` | 400 | The recording isn't a 16-bit PCM, mono, 16 kHz WAV file. |
| `audio_too_long` | 400 | The recording is longer than `minutes` minutes. |
| `audio_empty` | 400 | The recording is empty. |
| `embeddings_model_invalid` | 400 | The embeddings model must be 1 to `max` printable characters. |
| `embeddings_account_invalid` | 400 | The embeddings account must be one of the workspace's provider accounts. |
| `embeddings_failed` | 400 | The account's endpoint didn't return embeddings for the model. |
| `consent_forbidden` | 403 | Only the space's owner or one of its editors can change its consent. |
| `consent_no_model` | 409 | The workspace has no embeddings model to consent to. |
| `consent_endpoint_changed` | 409 | The workspace's embeddings endpoint changed: review it, then consent again. |
| `notification_not_found` | 404 | The notification isn't the caller's, or is gone. |
| `push_endpoint_invalid` | 400 | A push subscription's `endpoint` isn't an `https` address of at most 2048 characters without credentials or a fragment. |
| `push_keys_invalid` | 400 | A push subscription's `keys` aren't a P-256 public key (`p256dh`) and a 16-byte `auth` secret, base64url. |
| `push_too_many_subscriptions` | 409 | The caller already has push turned on for `max` devices (a device that changes hands counts for its new person). |
| `push_no_workspace` | 403 | The caller is in no workspace: push is for people who are in one. |
| `notification_email_invalid` | 400 | An email choice that isn't one: another kind or mode, an hour outside 0 to 23, or a time zone that isn't an IANA name. |
| `notification_email_verification_invalid` | 400 | The link that confirms the caller's email address is unknown, used, expired, replaced by a newer one, or for an address the caller no longer has. |
| `notification_email_verification_too_soon` | 429 | A confirmation link was sent to the caller a moment ago (`Retry-After`; `seconds` says how long to wait). |
| `notification_email_verification_unavailable` | 502 | The confirmation email couldn't be sent; another may be asked for at once. |

### Sessions

Logging in (setup, login, [signing in with a provider](#single-sign-on), accepting an invite as a
new person, or setting a password, by changing it or with a reset link) sets the session cookie: `__Host-taskto_session`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, lasting
30 days. The token is 256 random bits; the database only keeps its SHA-256. Logging out
deletes the session at once. With `insecure_cookies` (local plain-http development only)
the cookie is called `taskto_session` and isn't `Secure`, since browsers refuse a
`__Host-` cookie without it.

A route that needs a login answers `401` without a valid session.

### Access tokens

Agents and scripts authenticate with a personal access token instead of a session:
`Authorization: Bearer ttp_…` ([make one](#tokens)). A token is one user's in one
workspace, and acts with exactly that user's access there, checked again on every request:

- it must exist, not have expired, and its user must still be a member of its workspace;
  otherwise `401`. Revoking it, its expiry and its user leaving take effect at once;
- it reaches its own workspace only: any other is `404`, as for an outsider;
- only these routes take one, and all of them do the same checks as with a session:

  | Routes | |
  |---|---|
  | `GET /api/workspaces/{workspace}/members` | who can be assigned a card |
  | `GET …/spaces`, `GET …/spaces/{space}`, `GET …/spaces/{space}/members` | reading spaces |
  | `…/spaces/{space}/docs…`, `GET …/history…`, `…/restore/…` | the [knowledge base](#knowledge-base), all of it but removing old versions from history |
  | `GET …/search`, `GET …/spaces/{space}/search` | [searching](#search) |
  | `…/spaces/{space}/boards…` | the [boards](#boards), all of it |
  | `GET …/apps`, `GET …/apps/{app}`, `GET …/apps/{app}/surface`, `GET …/apps/{app}/releases[/{release}]`, `GET …/apps/{app}/records…` (the list, `deleted`, `query` and a record's `history` and `effects`), `GET …/apps/{app}/effects`, `POST …/apps/{app}/actions` | [personal apps](#personal-apps): reading an app and its rows, and running its actions, as the token's user: the space's access and the app's roles are checked as for a session, and rows a token adds or changes are marked with its name |

  The rest of the apps' routes take a session only (`403` with a token): creating, changing,
  moving, copying, previewing, publishing and rolling back an app, its roles, installing a
  template, restoring a record, retrying an effect, a device's [offline
  download](#get-appsappoffline) and the photo uploads and attachments. They are the web
  app's own (the builder, a device's queue, bytes that aren't JSON), and a token builds and
  changes apps through the [MCP tools](mcp.md) (`create_app`, `update_app`), which check the
  same rules. A request to an apps route that names an account (`X-Taskto-Expected-User`, the
  offline outbox's) is a session's: with a token it is `403` too.

  Every other route answers `403` to a request carrying a token, whatever else it carries
  (a session cookie too): logging in and out, setup, `GET /api/me`, invites, managing
  members and spaces, removing a file's old versions from history, provider accounts,
  search settings and consent, conversations, and the tokens API itself. So a token never changes the account and never makes a token.

A request carrying a token is authenticated by the token alone: its session cookie, if it
has one, is ignored. Only `Bearer` values starting with `ttp_` are tokens; other
`Authorization` headers (a reverse proxy's Basic auth, say) are left alone. Changes made
with a token are marked with its name: a document's commit has the token as committer
(`agent` in the history is `<name> (access token)`), and a card or comment's author has
`"agent": true, "via": "<name>"`.

### CSRF

Every `/api` request with a method other than `GET`, `HEAD` or `OPTIONS` must:

- not be cross-origin: a browser's `Sec-Fetch-Site` must be `same-origin` or `none`, or,
  without that header, an `Origin` must match the `Host`. Otherwise `403`.
- have `Content-Type: application/json` when they carry a body. Otherwise `415`. A
  request without a body (`DELETE`, `POST /api/logout`) needs no content type; the
  cross-origin rule above still applies to it. The two routes that take bytes,
  [`PUT …/docs/{path}`](#put-apiworkspacesworkspacespacesspacedocspath) and
  [`PUT …/apps/{app}/uploads/{upload}`](#photo-attachments), also take
  `application/octet-stream`; no other route does.

A cross-site form can't meet the second rule (it can't send either type), and a script on another site can't meet
either without a CORS preflight, which the server never grants. Clients that aren't
browsers send neither header and pass the first rule. This is middleware around the whole
server, so new routes get it without doing anything. `GET` routes must never change
state.

Requests with an [access token](#access-tokens) go through the same checks: an agent or
script sends no `Sec-Fetch-Site` or `Origin` and a JSON body, so they pass, and a web page
can't attach a token it doesn't have. `/mcp`, outside `/api`, refuses cross-origin
requests the same way.

### Workspaces and access

A user belongs to one or more workspaces, each with a role: `admin` or `member`. Admins
manage members and invites. A solo install is a workspace with one admin; a team is the
same workspace with more members. "Switching workspace" is using another workspace ID
from `GET /api/me`. The first workspace is made by [setup](#post-apisetup); later ones by
[`POST /api/workspaces`](#post-apiworkspaces), if the operator's `-workspace-creation`
setting lets the caller ([install.md](install.md#who-may-create-a-workspace)). Workspaces
share nothing: members, spaces, boards, apps, documents, tokens and settings belong to one.

Workspace routes are `/api/workspaces/{workspace}/…`. A caller who isn't a member gets
`404`, the same as for a workspace that doesn't exist, so outsiders can't tell which IDs
exist. A member calling an admin-only route gets `403`.

In Go, a route opts in with `auth.Service.RequireMember` or `RequireAdmin`, and reads the
caller with `auth.UserFrom` and `auth.MembershipFrom`.

### Spaces and access

Everything people store (files, conversations, boards, apps) lives in a **space** of a
workspace. Every member has a **private space**, and admins create **shared spaces** with
a member list. Who may do what:

| Space | Reads | Writes |
|---|---|---|
| Private | its owner | its owner |
| Shared | its editors and viewers | its editors, until the space is archived |

- Only members of the space's workspace have any access. Removing someone from the
  workspace removes them from all its shared spaces at once.
- **Admins get no access to other people's private spaces**, and don't read shared spaces
  they aren't members of. They can list every shared space (`?all=true`), see its details
  and member list, and manage its members, themselves included: an admin who wants to read
  a shared space adds themselves, and every member sees that in the member list.
- A space the caller can't read answers `404`, the same as one that doesn't exist, also
  when it belongs to another workspace. `403` means the caller can read the space but not
  do this: write as a viewer, write to an archived space, or call an admin-only route.

Routes on a space's content are `/api/workspaces/{workspace}/spaces/{space}/…` and wrap
their handler in `access.Checker.RequireSpace(access.Read or access.Write, …)`, which puts
the space, with the caller's role and access, on the context (`access.SpaceFrom`). Code
without a request, like the agent acting for a user, asks `Check`, `CanRead`, `CanWrite`
or `Readable` with that user's ID. A space's files are under
`<data>/workspaces/<workspace>/spaces/<space>/`; open them with `access.OpenRoot`, or
build paths with `access.SafeJoin`, never `filepath.Join` on names from people or models.

**Leaving a workspace.** The member's private space and its content are kept, and nobody
can open them: not admins, not the former member. If they rejoin, they get the same
private space back; their shared-space memberships are not restored. An account with
private spaces can't be deleted yet: what becomes of them is still open.

## First-run setup

### `GET /api/setup`

`200 {"needed": true}` while no user exists, `{"needed": false}` after.

### `POST /api/setup`

Creates the owner, as admin of the first workspace, and logs them in. Only works while no
user exists; otherwise `409`. Two setups at the same time can't both succeed.

```json
{"email": "owner@example.com", "name": "Owner", "password": "…", "workspace_name": "Home", "setup_token": "…"}
```

`setup_token` is the token the server prints to its log when it starts with no user
(`setup_token=…`), so only someone who can read the log can set it up. It is 256 random
bits, compared in constant time, made anew at each start, and refused once setup is done.
Missing or wrong: `403`.

`201` with the same body as `GET /api/me`, and the session cookie. `400` for an invalid
email, an empty name or workspace name (1–100 characters), or a password of fewer than 12
characters or more than 1024 bytes.

## Logging in and out

### `POST /api/login`

```json
{"email": "owner@example.com", "password": "…"}
```

`200` with the `GET /api/me` body and the session cookie; an existing session cookie on
the request is revoked. `401 wrong email or password` for an unknown address or a wrong
password, which take the same time. Email addresses are matched without regard to case.

Failed logins are limited in memory, in 15-minute windows: 30 per client address (an IPv6
client counts per /64), 10 per email from one address, and 100 per email from all
addresses. Over any of them, `429` with `Retry-After` in seconds, even with the right
password. A successful login doesn't count. The limits treat an email the same whether it
has an account or not, so the answers never reveal which addresses exist, and someone
failing from their own address can't lock the account's owner out. Behind a reverse proxy,
the client address is taken from `X-Forwarded-For` only when the direct peer is one of the
`trusted_proxies`: the right-most address in it that isn't a trusted proxy. Otherwise the
header is ignored ([install.md](install.md#the-clients-address-behind-a-proxy)).

Passwords are hashed with argon2id (64 MiB, 3 passes, 4 lanes; RFC 9106's second
recommended setting), at most two at a time to bound memory.

`403 password_login_disabled` when the operator turned passwords off. An account made by
signing in with a provider has no password: it gets the same `401` as a wrong one, in the
same time.

A user with [two-factor login](#two-factor-login) on gets no session from the right
password: `200` with a challenge instead, no cookie, and the existing session cookie left
alone. The login finishes at [`POST /api/login/two-factor`](#post-apilogintwo-factor).

```json
{"two_factor_required": true, "challenge": "…", "expires_at": "…"}
```

### `GET /api/login/methods`

No login needed. How people can sign in here, for the login screen:

```json
{"password": true, "sso": [{"id": "google", "name": "Google"}, {"id": "authentik", "name": "Authentik"}]}
```

`password` is `false` when the operator turned passwords off; `sso` is `[]` without providers.

## Single sign-on

Signing in with an OpenID Connect provider the operator configured ([sso.md](sso.md)). Both
routes refuse access tokens.

### `POST /api/sso/{provider}/start`

No login needed. `{"next": "/path#fragment"}`, where `next` is optional: the page to come back
to. It must be a path on this server (one leading `/`, not `//` or `/\`, no backslash or
control character, at most 2048 bytes); otherwise `/` is used.

```json
{"url": "https://accounts.example.com/o/oauth2/v2/auth?…"}
```

`200` with the provider's authorization URL, which the web app opens, and a cookie that holds
the login's state: `__Host-taskto_sso`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, for 10
minutes (`taskto_sso` and not `Secure` with `insecure_cookies`). The state, a nonce and a PKCE
verifier are kept in the server's memory only. `404 sso_unknown_provider`; `502 sso_failed`
if the provider can't be reached; `429` over 30 starts per client address in 15 minutes, or
when too many logins wait (`Retry-After`). It is a `POST`, so the CSRF guard applies.

### `GET /api/sso/{provider}/callback`

The provider redirects the browser here with `code` and `state`. It is the one `GET` that
changes state, since a redirect can't be a `POST`. It acts only when `state` equals the
`__Host-taskto_sso` cookie and a pending login of this provider that it uses up: a link
forged for another browser, and a replay, find none, and a callback to another provider than
the login was started for finds none and leaves the login for the right one. The code is
exchanged with the PKCE verifier, without following a redirect. Every call to the provider is
`https`; `http` is accepted only for a provider whose issuer is itself `http` on a loopback
host (development), and only to loopback hosts. Discovery, keys and user info follow a redirect
only within the origin they started from: the same scheme, port and host name, compared in
ASCII letters only (a host name that isn't ASCII never matches, so Unicode look-alikes can't
pass as the same origin). So nothing is downgraded to `http` or sent to another origin, user
info's bearer token included. The ID token's signature, issuer, expiry and nonce
are checked, its `aud` must be this client alone (a token that names another audience too is
refused), and its `azp`, when present at all, must be the string of this client's ID (an empty,
`null` or non-string `azp` is refused).

It always answers `303`. It never clears the `__Host-taskto_sso` cookie, which expires with the
state after 10 minutes and works once, so a slow callback can't take away a newer sign-in's
cookie:

- on success, to `next`, with a new session cookie (a session the browser already had is
  revoked). An identity already linked logs in its user; otherwise a verified email (or a
  `trust_email` provider's) that belongs to a user links the identity to them; otherwise a
  pending invite for the email creates an account with no password and accepts every pending
  invite for that email; otherwise it is refused. All of it in one transaction. With Google,
  linking and creating need an address Google vouches for: a domain that is `gmail.com` in
  ASCII letters, in either case, as the `email` claim has it, or an `hd` claim that isn't blank.
  A `next` that is the invite screen (`/invite…`) goes to `/w/<id>/conversations` instead when
  the sign-in made the account, since its invites are used up: the workspace is the one of the
  invite whose token is in `next`'s fragment (`/invite#<token>`) if this sign-in accepted that
  very invite, else of the first invite accepted (the oldest); another invite's token (someone
  else's, or an expired one) never picks it. For an existing account the invite stays pending and `next` is kept;
- on failure, to `/?sso_error=<code>`, one of the `sso_*` [error codes](#error-codes) above.
  An error from the provider, such as `access_denied`, is `sso_failed`.

### `POST /api/logout`

Revokes the session and clears the cookie. `204`, also without a session.

### `GET /api/me`

```json
{
  "user": {"id": 1, "email": "owner@example.com", "name": "Owner", "locale": "pt-BR"},
  "workspaces": [{"id": 1, "name": "Home", "role": "admin", "locale": ""}],
  "can_create_workspace": true,
  "is_operator": true
}
```

Workspaces are listed oldest first. Requires a login. `can_create_workspace` is whether
[`POST /api/workspaces`](#post-apiworkspaces) would let this user in now, which the web
app uses to offer "New workspace". `is_operator` is whether the user is an
[operator](#operators), which the web app uses to show Settings' server-wide panels (the backups'
status, outgoing email).

#### Operators

Some routes show or use what belongs to the whole server, not to one workspace: the backups'
status (which can name the backup host) and the outgoing email (the SMTP server, and test
messages sent through the operator's relay). Their callers must be admins of the workspace in the
URL **and operators**: admins of the workspace the server's [setup](#post-apisetup) made. That
workspace carries a stored marker (`workspaces.is_setup`, set by setup, or by the migration on a
server that already ran it, for its oldest workspace); it is not "the lowest id", and no route
can set it on another. Admins of workspaces created later with
[`POST /api/workspaces`](#post-apiworkspaces) are not operators, so they get `403`
`operator_only` there, and `is_operator` is `false` for them. The [setup checklist](#setup-checklist)
lists its two server-wide steps (`email`, `backups`) to operators only. Other admin routes act only on
their own workspace (its members, providers, search settings, dictation settings, shared CLI
accounts), so any of its admins may use them.

`user.locale` is the language the user picked, a BCP 47 tag, or `""` for none (the web app
then follows the browser). A workspace's `locale` is its default language, or `""` for
English. Only `/api/me` shows a user's own locale; member lists show nobody's. See
[i18n.md](i18n.md).

### `PATCH /api/me`

`{"locale": "pt-BR"}` → the new `GET /api/me`. Changes the caller's own settings; a field
left out stays as it is. `locale` is a BCP 47 tag of a language, optionally with a script
and a region, in canonical case (`pt-BR`, `zh-Hant-TW`, `sr-Latn`, `es-419`), or `""` for
none; anything else, variants and extensions included (`de-CH-1901`), is a `400`. Such a
tag is stored also when the web app has no translation for it. Requires a login (a session, not an access token).

### `POST /api/workspaces`

`{"name": "Side project"}` → `201` with `GET /api/me`'s reply for the caller, now with the
new workspace in it, and the new one's membership as `workspace`:
`{"workspace": {"id", "name", "role": "admin", "locale": ""}, "user", "workspaces", "can_create_workspace"}`.

Creates a workspace with the caller as its first admin, and nothing else in it but their
own private space (with its personal board), as setup does for the first one, in one
transaction. Who may is the operator's `-workspace-creation` setting: `admins` (the
default: someone who is an admin of at least one workspace), `anyone` (every logged-in
person) or `off` (nobody, so the server keeps the workspace setup made). Otherwise `403`
`workspace_creation_refused`, which `can_create_workspace` told in advance. The name is 1
to 100 characters (`400` `workspace_name_length`), after trimming, with no control
character (NUL, a newline), line or paragraph separator, or bidirectional override or isolate
(setup's names follow the same rule); names needn't be unique. Whatever the setting, one account
can create at most 10 workspaces, ever (`403` `workspace_limit`), and 5 within an hour (`429`
`too_many_workspaces`, with `Retry-After`; the check is in the database, so it holds across
restarts and concurrent requests). Requires a login (a session, not an access token: `403` `token_refused`).
Recorded as `workspace_created` in the [audit log](audit.md), on the new workspace (so
it is in that workspace's log, which the person who made it admins, and not in the first's).

### `PATCH /api/workspaces/{workspace}`

Admins. `{"locale": "pt-BR"}` → the workspace as in `GET /api/me`
(`{"id", "name", "role", "locale"}`). Sets the workspace's default language, which members
get when their browser asks for none the web app has; `""` is English. Same tags as
`PATCH /api/me`.

## Passwords

A user changes their own password with the current one. An admin can make a reset link for
a member who lost theirs. Either way, every session of the account ends, and so do its
reset links; the browser that set the new password gets a new session (with a reset link, only for an
account without two-factor login: the others get a code to give first). Access tokens keep
working: they are listed, and revoked, under [Tokens](#tokens). These routes take a
session, never an access token (`403`). New passwords are 12 to 1024 characters, as at
setup.

### `POST /api/me/password`

The caller's own password; the route names no user, and the body takes no other field.

```json
{"current_password": "…", "new_password": "…"}
```

`204` with a new session cookie. `400` without a current password, or with a new one that
is too short or too long. `403 the current password is wrong`: `403` rather than `401`,
since the session is fine, which is also the answer for an account that has no password (made by
signing in with a provider). Wrong current passwords count against the
[login limits](#post-apilogin), for the caller's email and address: past them, `429` with
`Retry-After`, even with the right one. `409` if the password changed meanwhile.

### `POST /api/workspaces/{workspace}/members/{user}/password-reset`

Admin only, with no body. Makes a reset link's token for a member, in place of any earlier
one. It works once, for 24 hours, and is shown only now; the database keeps its SHA-256.
There is no email: the admin sends the link (`/reset-password#<token>`, the token in the
fragment, like an invite's).

```json
{"token": "…", "expires_at": "…"}
```

`201`. `404` if the user isn't a member of the workspace. `400` for the admin's own
account: they change it with their current password. `403` if the member also belongs to a
workspace the admin doesn't administer: whoever has the link can take over the account, so
only someone who administers every one of its workspaces can make one. `403 password_login_disabled`
when the operator turned passwords off: a link nobody could use isn't made.

### `POST /api/password-reset`

No login needed. `{"token": "…", "password": "…"}` sets the password of the account the
link is for, ends its sessions, half-done logins and reset links, and logs it in here: `200` with the
`GET /api/me` body and the session cookie (a session the browser already had, anyone's,
is revoked). **For an account with [two-factor login](#two-factor-login) on, the link is no
second factor**: the password is set and everything ends as above, but the answer is the login's
challenge (`200`, `{"two_factor_required": true, "challenge": "…", "expires_at": "…"}`), no
cookie and no session, and the browser finishes at [`POST /api/login/two-factor`](#post-apilogintwo-factor)
with a code. A reset also drops a two-factor enrolment that wasn't confirmed. `400` for a password that is too short or too long, which leaves the link
usable. `404` if the token is unknown, used, replaced or expired. `403 password_login_disabled`
when the operator turned passwords off.

## Two-factor login

A user can ask for a code from an authenticator app (TOTP, RFC 6238: HMAC-SHA1, six digits,
30-second steps) after the right password, on top of it. The routes under `/api/me/two-factor`
take the caller's session, never an access token (`403`), and act on the caller only: none names a
user, and none takes one in the body. `POST /api/login/two-factor` is the exception that needs
no session (the password step gave a challenge, not a session), and an admin's clearing a member's
is the other, both below.

- **Where it applies.** The password login (`POST /api/login`). **Signing in with a provider
  has no second step**: the identity provider's own sign-in, and its own two-factor login if
  the operator asked it for one, is what vouches for the person
  ([sso.md](sso.md#two-factor-login)). Access tokens and the CLI turns never had one and are
  unaffected, and so is an invite's acceptance by a new person (they have none yet). A
  password reset link's use is **not**: for an account with it on, the link sets the password and then
  asks for a code, as a login does. An existing account that takes an invite logs in first, and
  so is asked for its code too.
- **The secret** is 160 random bits, kept sealed with the server's key
  (`<data>/server.key`, AES-GCM, bound to the user) in `user_totp`, never in clear and never
  returned again after the enrolment; losing the key makes it unreadable, as for the other
  sealed secrets, and an admin then clears it. **Recovery codes** are ten of 60 random bits
  (`xxxx-xxxx-xxxx`), shown once and kept as a keyed hash (HMAC under a key derived from the
  server's); each works once.
- **Codes.** A TOTP code is accepted for the current 30-second step and the one either side,
  but only for a step later than the last one accepted for the user, so a code can't be
  replayed (not in its own step, and not an earlier step's). A code is six digits; a recovery
  code is twelve characters, with case, spaces and dashes not mattering.
- **Limits.** The second step has limits of its own, in 15-minute windows, of wrong codes:
  30 per client address, 10 per user from one address, and 30 per user from all addresses.
  Over any of them, `429 too_many_two_factor_codes` with `Retry-After`, even with the right
  code. One challenge also takes at most 5 wrong codes: they are counted, and the code
  checked, in the transaction that uses the challenge up, so requests sent at once can't try
  more. A wrong password in the routes below
  counts against the [login limits](#post-apilogin), as at `POST /api/me/password`.

### `POST /api/login/two-factor`

The second step of a login that answered with a challenge. No session needed.

```json
{"challenge": "…", "code": "123456"}
```

`code` is the app's code or a recovery code. `200` with the `GET /api/me` body and the
session cookie, as `POST /api/login` answers, and an existing session cookie on the request is
revoked. The challenge is used up. `401 wrong_two_factor_code` for a wrong code, which leaves
the challenge for another try (up to 5); `401 two_factor_challenge_invalid` for a challenge
that is unknown, used, expired or used up. `400` without a code. Until this succeeds there is
no session: the password step (a login, a reset link) makes the challenge in the transaction
that checks the password hash is still the user's and reads whether two-factor login is on, so
neither a concurrent enrolment nor a concurrent change of the password gets a session or a challenge it
shouldn't; a change of the password ends every challenge. The challenge is a token only this route reads, stored as its SHA-256, and no
other route accepts it, as a cookie or as an access token.

### `GET /api/me/two-factor`

```json
{"enabled": true, "recovery_codes_left": 8}
```

### `POST /api/me/two-factor/enrol`

`{"password": "…"}`: starts turning it on. `200`:

```json
{"enrolment": "…", "secret": "GEZDGNBVGY3TQOJQ…", "otpauth_uri": "otpauth://totp/TaskTo:me@example.com?secret=…&issuer=TaskTo&…"}
```

The web app draws `otpauth_uri` as a QR code in the browser (the secret goes nowhere else)
and shows `secret` as text for typing in. Nothing is on yet: login asks for no code until
[`confirm`](#post-apimetwo-factorconfirm), which needs `enrolment`, a random id for this
enrolment that expires after 15 minutes. Asking again before that makes a new secret and a new
id, and the earlier one can no longer be confirmed (another tab's code can't turn on a secret it
didn't show). A change of the password, or a reset, drops an enrolment that wasn't confirmed.
`403 wrong_password` (counted against the login limits); `409 two_factor_already_on`;
`403 two_factor_no_password`; `403 password_login_disabled` when passwords are off, since
two-factor login protects nothing then.

### `POST /api/me/two-factor/confirm`

`{"enrolment": "…", "code": "123456"}`: a code from the app for the secret `enrol` made,
with the `enrolment` it answered, turns two-factor login on, and answers `200` with the recovery codes, this once:

```json
{"recovery_codes": ["k3m9-p0qz-7xaf", "…"]}
```

`403 wrong_two_factor_code`; `409 two_factor_not_started` without an `enrol` first, or with
an `enrolment` that is missing, unknown, replaced by a later one or expired; `409 two_factor_already_on`.

### `POST /api/me/two-factor/disable`

`{"password": "…", "code": "…"}`: the password and a code (the app's, or a recovery code
for someone who lost the phone). `204`; the secret, the recovery codes and any half-done
logins are deleted. `403 wrong_password`, `403 wrong_two_factor_code`, `409 two_factor_not_on`.

### `POST /api/me/two-factor/recovery-codes`

Same body and errors as `disable`: makes ten new recovery codes, in place of all the old ones,
and answers `200` with `{"recovery_codes": […]}` this once.

### `DELETE /api/workspaces/{workspace}/members/{user}/two-factor`

Admin only, no body. Clears a member's two-factor login: their secret, recovery codes and
half-done logins are deleted and **their sessions end**, so a login of someone else's that
got through is out too, and they log in with the password alone, to turn it on again.
The rule is the password reset link's: the admin must administer every workspace the member
belongs to (`403 two_factor_clear_elsewhere`), and can't use it on their own account
(`400 two_factor_clear_own`). `404` if the user isn't a member of the workspace;
`409 two_factor_not_on` if the member has none. `204`.

## Members

### `GET /api/workspaces/{workspace}/members`

Any member. Members can see each other (boards need it to assign cards).

```json
{"members": [{"id": 2, "email": "member@example.com", "name": "Member", "role": "member", "joined_at": "…"}]}
```

An admin's list also has `"two_factor": true` or `false` for each member, which says whether
they have [two-factor login](#two-factor-login) on, for the clear above; nobody else sees it.

### `PATCH /api/workspaces/{workspace}/members/{user}`

Admin only. `{"role": "admin"}` or `{"role": "member"}`. `204`; `404` if the user isn't a
member; `409` if it would leave the workspace without an admin.

### `DELETE /api/workspaces/{workspace}/members/{user}`

Admin only. Removes the member; their access to the workspace, and to all its shared
spaces, ends with their next request. Their private space is kept, closed (see
[Spaces and access](#spaces-and-access)). `204`; `404` if the user isn't a member; `409`
for the last admin.

## Invites

An invite is for one email address, with a role. It works once and expires after 7 days.
Its token is shown only when the invite is created; the database keeps its SHA-256. A
link carrying the token should put it in the URL fragment (`/invite#<token>`), which
browsers don't send to servers or in `Referer`.

### `POST /api/workspaces/{workspace}/invites`

Admin only. `{"email": "new@example.com", "role": "member"}`; `role` defaults to
`member`.

```json
{"id": 3, "email": "new@example.com", "role": "member", "created_at": "…", "expires_at": "…", "token": "…"}
```

`201`; `409` if the address already belongs to a member.

### `GET /api/workspaces/{workspace}/invites`

Admin only. The invites not yet accepted or expired, without tokens:
`{"invites": [{"id": 3, "email": "…", "role": "member", "created_at": "…", "expires_at": "…"}]}`.

### `DELETE /api/workspaces/{workspace}/invites/{invite}`

Admin only. Withdraws a pending invite. `204`, or `404`.

### `POST /api/invites/accept`

No login needed. Two cases:

- **A new person:** `{"token": "…", "name": "New Person", "password": "…"}` creates the
  account for the invite's address, adds it to the workspace with the invite's role, and
  logs it in: `201` with the `GET /api/me` body and the session cookie. If an account with
  that address exists, `409`: log in, then accept.
- **An existing user**, logged in as the invited address: `{"token": "…"}` adds them to
  the workspace. `200` with the `GET /api/me` body. Logged in as another address, `403`.
  Already a member, `409` (the invite stays unused).

`404` if the token is unknown, used, withdrawn or expired. `403 password_login_disabled` for a
new person when the operator turned passwords off: they sign in with a provider instead,
which accepts every pending invite for their address. Joining as a logged-in user still works.

## Tokens

A member's own [access tokens](#access-tokens) in a workspace, for their agents and
scripts. These routes take a session only (`403` with a token).

### `GET /api/workspaces/{workspace}/tokens`

The caller's tokens in the workspace, oldest first, expired ones included, without the
tokens themselves:

```json
{"tokens": [{"id": 3, "workspace_id": 1, "name": "laptop: Claude Code", "created_at": "…", "expires_at": null, "last_used_at": "…"}]}
```

`last_used_at` is updated at most once a minute; `null` if never used.

### `POST /api/workspaces/{workspace}/tokens`

`{"name": "laptop: Claude Code", "expires_at": "2027-06-30T00:00:00Z"}`: `name` is 1–100
characters, `expires_at` (RFC 3339, in the future) is optional; without it the token
doesn't expire. `201` with the token as above plus `"token": "ttp_…"`, shown this once:
the database keeps only its SHA-256. `400` for a missing name or a bad `expires_at`; `409`
past 50 tokens per member of a workspace.

### `DELETE /api/workspaces/{workspace}/tokens/{token}`

Revokes one of the caller's tokens: requests with it fail from now on. `204`, or `404`
(also for someone else's token).

## Export my data

A person's own data in a workspace, as a zip they keep. It is the person's export, not the
operator's: `taskto backup` takes the whole server's data directory, and this takes one person's
share of one workspace, through the same access checks as everything else.

### `POST /api/workspaces/{workspace}/export`

Starts an export: it takes one of the caller's exports of the hour, records a
`data_export_started` event on the [audit log](audit.md), and answers `200` with the address to
download the zip from:

```json
{"download": "/api/workspaces/1/export/<token>", "expires_in": 60}
```

It is a `POST`, with a JSON body (`{}`), so the [CSRF guard](#csrf) covers it: a link, a
cross-site navigation or a prefetch is a `GET`, and starts nothing, uses up none of the hour's
exports and writes nothing to the audit log. It takes a session only (`403` with an access token,
so a leaked token can't take a person's data out), and any member of the workspace may call it,
for themselves.

### `GET /api/workspaces/{workspace}/export/{token}`

Streams the zip of an export that was started (`200`, `Content-Type: application/zip`,
`Content-Disposition: attachment; filename="taskto-export-YYYYMMDD.zip"`, `Cache-Control:
no-store`). It changes no data on the server (the token it takes is used up, in memory). The token is the one the `POST` answered with: random,
good once, for `expires_in` seconds, and only for the same person, workspace and login that started
it; anything else is `404`. A `HEAD` is `405`, so nothing but the download uses the token. The web
app's button does both: it posts, then goes to the address.

**What is in it: only what the caller owns in this workspace.**

| Path in the zip | What |
|---|---|
| `documents/…` | The current version of every file in the caller's **private space**, under the names it has there, with a file's bytes as they are: Markdown and other versioned files from git, and large files, audio, video and archives (the [unversioned files](#unversioned-files)) from the space's own storage, never as a Git LFS pointer. Older versions aren't included. |
| `boards/<id>-<name>.json`, `boards/<id>-<name>.md` | Each board of the private space, archived ones too (the personal board): the board and its cards (all columns, archived cards too, in the order of their IDs), each with its thread of comments. The JSON is `{"board": {…}, "cards": [{…card as the API gives it…, "comments": […]}]}`, exactly what `GET …/cards/{card}` answers. The Markdown is the same, for reading: a heading per card, with its column, details, body and comments. The two files are read one after the other, so a card edited between them can differ in them. |
| `conversations/<id>-<title>.json` | Each conversation the caller owns **in this workspace**, archived ones too: `{"conversation": {…as the conversations API gives it…}, "entries": […], "transcript": "complete"}`. The entries are the agent runtime's, oldest first, in the shape the web app draws, as `GET …/conversations/{conversation}/entries` pages them, as the history was when its export began. If the agent runtime is off, no longer has the conversation or stops answering (each read waits at most 30 seconds), `transcript` is `"unavailable"` (`entries` empty) or `"incomplete"` (the entries read before it stopped), and the manifest says so. |
| `apps/<id>-<name>.json` | Each app of the private space, offline ones too: `{"app": {id, name, archived_at, created_at, updated_at}, "records": […]}`. The records are the live ones (not deleted), as the agent's `list_app_rows` tool gives them (each field by name, with `id` and `revision`). An attachment field holds each photo's metadata, not its bytes; the app's definition isn't included (copy the app, or see [templates](templates.md)). |
| `manifest.json` | `{"format": 1, "exported_at", "workspace": {id, name}, "user": {id, name, email}, "counts": {documents, boards, cards, comments, conversations, apps, records}, "skipped": […]}`. `skipped` lists what couldn't be included, as `{"kind", "reason", "ref"}`: `document` with `unavailable` and its path (a file whose bytes the server no longer has), `documents` with `unavailable` (the space's history was rewritten while the export read it) or `off` (the server has no knowledge base), `app` with `unavailable` and its ID (the app was moved out of the private space while the export read it), `conversations` with `off` (the server has none), or `transcript` with `unavailable` or `incomplete` and a conversation's ID; it is capped at 1000 (`"skipped_truncated": true` then). |

**The scope is where a thing is now, and who owns a conversation.** The documents, boards and
apps are those in the caller's private space at the moment each is read, so an app or a document
that was moved into it from a shared space is exported, and an app that is moved out meanwhile is
left out and listed in the manifest. An app's records are all its live records, including those
teammates added while it was shared. The caller's conversations are exported whole: a transcript
can hold text from shared documents the agent read for the caller. The Settings text says so.

**What is never in it:** the content of a shared space (where it still is when it is read), even
where the caller is a member or an editor; other people's private spaces, cards, conversations and
records; the caller's data in other workspaces (export each workspace on its own); search indexes,
tokens, sessions and every secret. Boards, apps and documents of a shared space belong to the
space, not to a person, so ownership isn't the question there: someone who wants them copies them
into their private space first.

**Names inside the zip** come from people, so none is used as it is. Each element of a path is
cleaned (a name's `/`, `\`, `:`, `*`, `?`, quotes, `<`, `>`, `|`, control and bidirectional
characters become `_` or go; `.` and `..` become `_`; a trailing dot or space goes; names Windows
reserves, such as `NUL` or `COM1`, get a `_` in front), cut to 120 bytes at a character
boundary with the extension kept (a name that is nothing after that becomes `_`), and made unique
even on a file system that ignores case or writes an accent the other way (names are compared
normalized to NFC and case-folded), with ` (2)`, ` (3)`… before the extension. A folder keeps the
name it was given for all its files, and two different folders never share one. No entry is absolute or has a `..`, so unpacking can't
write outside its folder, and no name is ever joined to a file system path on the server: the
zip is a stream.

**Streamed.** The zip is written to the response as it is made, one entry at a time, with a data
descriptor per entry and Zip64 where needed, and has no `Content-Length`. A file's bytes are
copied through a fixed buffer, so a large file costs a buffer, not its size; audio, video and other
files git doesn't version are stored as they are, not compressed again. What is held at once is
a page of a listing, one card's thread, a page of a conversation's history or one app's records,
whatever the size of the space, besides what a zip always keeps until its end (a header per entry, so the cost grows with how many files there are, not how large they are) and the list of the person's conversations. The documents are listed and read at one commit of the knowledge
base, taken when the export reaches them, so a file renamed or replaced meanwhile appears once, as
it was; a versioned file is read from git into memory (up to 1 MiB) or a temporary file that is removed when it has been sent, so a slow download never holds up changes to the space. The server never writes:
the knowledge base's repositories are only read, through `internal/knowledge`.

**If it stops.** The status goes out with the first bytes, so an export that fails or whose
reader goes away is ended by cutting the connection: the zip has no end of central directory, so
no unzipper takes it for whole. The manifest is the last entry and the zip's end follows it, so a zip that opens in an unzipper, with its manifest, is whole.
Each write has two minutes to be taken by the connection, so a download that stalls is ended and
frees its slot; a download address nobody uses frees it when it expires.

**Limits.** One export at a time per person (`409 export_running`), at most 3 per hour per
person and 4 at once on the whole server (`429 export_rate_limited`), both with `Retry-After`, on
the `POST`; a refused attempt isn't counted. A started export holds its slot from the `POST` until
its download ends or its address expires. They are in memory: a restart forgets them. A person who
closes the download frees their slot at once. `401` without a session, `404` for a workspace
the caller isn't a member of.

Each export that is started is a `data_export_started` event on the [audit log](audit.md), with
the exporter as actor and target; one turned away by its limits isn't.

**The web app** has a "Your data" tab in Settings, for every member: a button that posts, shows
the 409 and 429 reasons in the person's language, and goes to the address it is given.

## Spaces

A space as the caller sees it:

```json
{
  "id": 4, "workspace_id": 1, "kind": "shared", "name": "Team",
  "archived_at": null, "created_at": "…",
  "offline_downloads": true,
  "role": "editor", "access": "write"
}
```

- `kind`: `private` or `shared`. A private space is called `Private` and can't be renamed
  yet.
- `role`: the caller's: `owner` (their private space), `editor` or `viewer`. Absent for a
  shared space an admin sees without being a member.
- `access`: what the caller may do with the content: `write`, `read` or `none` (only
  admins see spaces with `none`).
- `archived_at`: when an admin archived the space, or `null`. Archived spaces are
  read-only, for editors too.
- `offline_downloads`: whether the space's apps may be [downloaded for offline
  use](#get-appsappoffline). `true` for a new space and for every private space; an admin
  turns it off for a shared space whose data shouldn't be kept on devices.

### `GET /api/workspaces/{workspace}/spaces`

Any member. The spaces the caller can read: their private space, then the shared spaces
they are a member of, by name. `{"spaces": [ … ]}`.

With `?all=true` (admin only, otherwise `403`), also the workspace's shared spaces the
caller isn't a member of, with `access: "none"`, for managing them. Other people's
private spaces are never listed.

### `POST /api/workspaces/{workspace}/spaces`

Admin only. Creates a shared space: `{"name": "Team"}` (1–100 characters). The creator
becomes its first editor and can remove themselves. `201` with the space.

### `GET /api/workspaces/{workspace}/spaces/{space}`

The space, for those who can read it, and for admins if it is shared. `404` otherwise.

### `PATCH /api/workspaces/{workspace}/spaces/{space}`

Admin only, shared spaces only (`404` for private ones). Renames, archives or unarchives, and
allows or refuses offline downloads:
`{"name": "New name"}`, `{"archived": true}`, `{"archived": false}`,
`{"offline_downloads": false}`, or several at once.
Archiving an archived space keeps its first `archived_at`. `200` with the space; `400` for
an empty body or name.

### `GET /api/workspaces/{workspace}/spaces/{space}/members`

The members of a shared space, for its members and for admins. `404` for a private space.

```json
{"members": [{"id": 2, "email": "member@example.com", "name": "Member", "role": "viewer", "added_at": "…"}]}
```

### `POST /api/workspaces/{workspace}/spaces/{space}/members`

Admin only. `{"user_id": 2, "role": "editor"}` (`editor` or `viewer`). `204`; `404` if the
user isn't a member of the workspace, or for a private space; `409` if they are already a
member of the space (change their role instead).

### `PATCH /api/workspaces/{workspace}/spaces/{space}/members/{user}`

Admin only. `{"role": "viewer"}`. `204`; `404` if the user isn't a member of the space.

### `DELETE /api/workspaces/{workspace}/spaces/{space}/members/{user}`

Admin only. Removes the member from the space; their access ends with their next request.
`204`; `404` if the user isn't a member of the space.

## Audit log

### `GET /api/workspaces/{workspace}/audit`

Admins only (`403 admin_only` to a member, `404 workspace_not_found` to anyone who isn't in the
workspace). A browser session only: an access token is refused (`403 token_refused`). The
workspace's own security and admin events, newest first, as [the audit log](audit.md) records
them. The workspace is the one in the path that the caller administers; no parameter names
another, and events that belong to no workspace (logins, password changes, a person's own
provider accounts) are never in the reply.

Query, all optional:

| Parameter | What |
|---|---|
| `action` | one action code, such as `member_removed` ([the list](audit.md#what-is-recorded)) |
| `actor` | the id of the user who did it |
| `target_type` | `user`, `workspace`, `invite`, `space`, `access_token`, `provider_account` or `shared_cli_account` |
| `from`, `to` | RFC 3339 times in the years 1000 to 9999; `from` is included, `to` is not |
| `limit` | the page size, 1 to 200; 50 if omitted |
| `cursor` | the `next` of the previous page |

An unknown action or target type, or a malformed value, is `400 bad_request`; a `from` or `to` outside the years 1000 to 9999 is `400 audit_time_invalid`.

```json
{
  "events": [{
    "id": 41, "at": "2026-03-05T14:30:00.000Z", "workspace_id": 1,
    "action": "member_removed",
    "actor_id": 1, "actor": {"name": "Owner", "email": "owner@example.com"},
    "target_type": "user", "target_id": 9, "target_created_at": "2026-02-01T10:00:00.000Z",
    "target_user": null, "target_name": null,
    "details": {"role": "member"}, "client_addr": "203.0.113.9"
  }],
  "next": "…"
}
```

`next` is empty on the last page; pages follow each other without gaps or repeats while events
are added. The names are read when the log is, not when the event was recorded, and only for
what belongs to the workspace: `actor` and `target_user` are the user while they are a member
of it (otherwise `null`, and `actor_id` and `target_id` still say who), and `target_name` is the
name of the workspace, a shared space, a provider account or a shared CLI account the event is
about, if it still exists (matched by its id **and** creation time, since ids are reused;
`null` otherwise). An access token's or an invite's name is never resolved. `details` are the
facts the action declares, never free text.

## Knowledge base

Each space's documents are Markdown files (`.md`) and other uploaded files in a git
repository, in the space's directory: the files are its working tree, and every change is
a commit on `main`. Large files, audio and video, and heavy binaries such as archives,
disk images and design files are [unversioned](#unversioned-files): git records them, but keeps only a pointer to their
bytes. The repository is created by the first write. Reading needs read
access to the space, every change write access (see
[Spaces and access](#spaces-and-access)); a viewer gets `403` for a change, and a space
the caller can't read is `404` on every route.

- **Names** are paths relative to the space, with `/` between folders:
  `notes/2026/plan.md`. In URLs, the path follows `/docs/`, `/history/` or `/restore/`,
  percent-encoded where needed. A name is refused with `400` if it is empty, absolute, not
  in its shortest form (`a//b`, `a/./b`, a trailing `/`), has a `..` element, a
  backslash or a control character, or has a `.git` element in any spelling (`.GIT`,
  `.git.`, `git~1`, with ignorable Unicode): nothing in `.git` can be read or written
  through the API. A path has at most 16 levels and 1024 bytes, each name at most 255
  bytes. Two names in a folder can't differ only by case, or by how an accent is written (é as one character or as e and a combining accent: the same file on macOS and Windows) (`409`).
- **Versions.** A document's `version` is the git blob ID of its content (40 hex
  digits), or of its pointer for an unversioned file. A change to an existing document says which version it was based on, in
  `base_version`; if the document has changed since, the answer is `409` and nothing is
  overwritten.
- **Commits.** A change's `commit` is the git commit ID. Its author is the logged-in user
  (name and email). When an agent makes a change for a user, the user stays the author
  and the agent is the committer, reported as `agent` in histories. A change may carry a
  `message` (one line, at most 200 characters); without one, it says what changed
  (`Update notes/plan.md`). A change that changes nothing makes no commit and returns
  the current one.
- **Limits.** A file is at most 2 GiB (`kb_max_file_size`), and a Markdown document at
  most 50 MiB (`kb_large_file_size`): over it, `413`. A space's repository, history
  included, is at most 1 GiB (`kb_max_repo_size`); unversioned files' bytes don't count
  there. Every change counts: its content plus about 1 KiB for the commit and trees,
  moves and deletes included. A change that wouldn't fit is `507`, so a full repository
  is read-only until the limit is raised; deleting doesn't free space, since history
  keeps every version. A space's unversioned files are at most 20 GiB together
  (`kb_max_unversioned_size`, `507` over it); deleting or replacing one frees its bytes.
  A `.md` file whose bytes aren't UTF-8 (an upload renamed to `.md`) reads back in
  `content_base64`, like an upload.
- **Storage quotas.** Besides these limits of a space, the operator may set a storage quota
  for each workspace and for each member's private space ([Storage usage](#storage-usage),
  [quotas.md](quotas.md)). A write that adds content and wouldn't fit is `507` with
  `quota_workspace` or `quota_user`; deleting, moving and restoring add none, so they are
  never refused for it.

A document as listed:

```json
{"path": "notes/plan.md", "version": "3b18e512dba79e4c8300dd08aeb37f8e728b8dad", "size": 7, "versioned": true}
```

`versioned` is `false` for an [unversioned file](#unversioned-files). The result of a
change:

```json
{"commit": "9fceb02d0ae598e95dc970b74767f19372d61af8", "doc": {"path": "notes/plan.md", "version": "…", "size": 7, "versioned": true}}
```

`doc` is absent after a delete or a folder's move.

### Unversioned files

A file other than Markdown is unversioned when it is over 50 MiB (`kb_large_file_size`),
or is one of the kinds below, whatever its size: by its extension, in any case (`.ZIP` is
`.zip`), and by its last one (`.tar.gz` is `.gz`). Markdown documents are always
versioned. These are files that are large as a rule, and that git can't store any smaller,
since they are compressed already or change all over at every save; kinds that are often
small (images such as PNG and JPEG, PDFs, office documents, fonts, executables) stay
versioned under the size limit. `internal/knowledge/large.go` has the list, and a test
keeps this one the same.

<!-- unversioned-types -->
- **Video:** `.mp4`, `.m4v`, `.mov`, `.webm`, `.mkv`, `.avi`, `.wmv`, `.flv`, `.mpg`,
  `.mpeg`, `.ogv`, `.3gp`.
- **Audio:** `.mp3`, `.m4a`, `.aac`, `.wav`, `.flac`, `.ogg`, `.oga`, `.opus`, `.weba`,
  `.wma`, `.aif`, `.aiff`, `.amr`.
- **Archives and compressed files:** `.zip`, `.7z`, `.rar`, `.tar`, `.gz`, `.tgz`, `.xz`,
  `.txz`, `.bz2`, `.tbz2`, `.zst`, `.lz4`.
- **Disk and virtual-machine images:** `.iso`, `.dmg`, `.img`, `.vmdk`, `.vdi`, `.vhd`,
  `.vhdx`, `.qcow2`.
- **Installer packages:** `.msi`, `.pkg`, `.deb`, `.rpm`, `.apk`, `.ipa`.
- **Raw camera images:** `.raw`, `.dng`, `.cr2`, `.cr3`, `.nef`, `.arw`, `.orf`, `.raf`,
  `.rw2`.
- **Layered images, design and 3D files:** `.psd`, `.psb`, `.tif`, `.tiff`, `.ai`, `.indd`,
  `.sketch`, `.fig`, `.xcf`, `.kra`, `.blend`.
- **Model weights:** `.gguf`, `.safetensors`.
<!-- /unversioned-types -->

An MPEG transport stream's `.ts` isn't on it: that is TypeScript far more often.

It is decided at each write. A file that was versioned and becomes unversioned, because
its new content is over the limit or a rename gives it one of the kinds above, leaves git
history completely: its older versions are
[taken out of history](#taking-a-files-old-versions-out-of-history) too, as unversioned
files' older versions are, and can no longer be read or restored (`410`).

Git records an unversioned file like any other, as a small pointer in
[Git LFS's format](https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md) (its
SHA-256 and size): it is listed, moved, deleted, found by name in search, and changed
with `base_version`, and its history lists every change to it, with who made it. Only
its current bytes are kept:

- replacing or deleting it frees the old bytes as soon as the change is committed;
- reading an older version's bytes (`?raw=true&at=…`), or restoring one, is `410` once
  they are gone; an older version's entry (`?at=…`, without `raw`) still reads;
- a deleted unversioned file can't be restored.

Its bytes are never in JSON: read it with `?raw=true`, and send a large one as
`application/octet-stream` ([`PUT`](#put-apiworkspacesworkspacespacesspacedocspath)).
See [Agents and the files](#agents-and-the-files) for where it is on disk.

### Taking a file's old versions out of history

When a versioned file becomes unversioned, the server rewrites the space's history, as
`git lfs migrate import` would: in every commit of `main`, each earlier version of the
file, at its path (at its old path, for a rename), becomes a Git LFS pointer to those
bytes, and the bytes themselves are deleted. Nothing else in any commit changes:

- **Authors, committers, dates (with their time zones) and messages are kept.** The
  history lists the same changes, by the same people, at the same times.
- **Commit IDs change** from the file's first version on, since a commit's ID covers its
  files and its parent's ID; commits from before the file existed keep theirs. A
  history link, a `restore` or `?at=` with an old commit ID is `404`, so a page showing
  the old history needs reloading. Anything that kept a commit ID (a conversation that
  mentions one, say) is left with an ID that no longer exists, never one that means
  something else.
- **Versions are blob IDs**, which change only for that file's older versions: every
  other document keeps its `version`, and so does the file's current one, so a
  `base_version` held by an open editor still matches, or is still a `409`, exactly as
  before. A `base_version` from before the change that made the file unversioned is a
  `409`, never an overwrite.
- **Search** reflects a commit of `main`; once it is gone, the index lists `main`'s files
  again and catches up, reading again only what changed.

It runs as part of the change that calls for it, under the space's lock: other changes
to the space, and reads of it, wait for it, and see the history before it or after it,
never half of it. Its answer has the new commit. Other spaces aren't affected.

**Crash safety.** The paths go first into a journal (`.git/taskto-rewrite`), written
atomically; the new commits and trees are only added; `main` moves last, by renaming a
new ref file over the old one; then the objects `main` no longer reaches (the old
commits, trees and versions, packed ones included) are deleted, and the journal goes. A
server that stopped half-way finds the journal when it starts, before it serves anyone
(so its start takes as long as the rewrite), and does the rewrite again from the start:
it is idempotent, so it ends with the history rewritten, whichever step it stopped at. A
journal for a change that never landed is dropped. If a rewrite fails, the change itself still stands, and the rewrite is tried
again at the space's next change.

**Cost.** It reads every commit of `main`, writes a commit and the trees down to the
file for each one from the file's first version on, and then reads every object `main`
reaches to find what to delete: it grows with the length of the history, not with the
files' sizes. About a second for 1,000 commits on an SSD, during which the space waits.

An admin can do the same to any file, to remove what shouldn't have been saved
([`DELETE …/history/{path}`](#delete-apiworkspacesworkspacespacesspacehistorypath)).

**Limits.** A copy of the same bytes at another path keeps its own history: only the
path that changed is rewritten. Only `main` is rewritten: a branch or tag made with git
outside the server (which the server never does) keeps what it reaches, and is logged. [Backups](install.md#backups) taken before the rewrite
still hold the old versions until they expire.

### `GET /api/workspaces/{workspace}/spaces/{space}/docs`

Lists documents, by path, recursively: `{"docs": [ … ], "next": "…"}`. Query:

- `prefix`: only the documents in this folder (`404` if it isn't one).
- `limit`: 1 to 1000, default 200.
- `after`: start after this path. `next`, present when there are more, is the `after` of
  the next page.

A space nothing was ever written to lists no documents.

### `GET /api/workspaces/{workspace}/spaces/{space}/docs/{path}`

A document with its content: `content` (text) for Markdown, `content_base64` for other
files, neither for an unversioned file.

```json
{"path": "notes/plan.md", "version": "…", "size": 7, "versioned": true, "content": "# Plan\n"}
```

- `?at=<commit>`: the document as it was at that commit (`404` if it didn't exist then, or if the commit isn't in the space's history: one a failed change left behind is never read).
- `?raw=true`: the bytes themselves, as a download: `application/octet-stream` (audio
  and video: their media type, such as `video/mp4`, so the web app can play them),
  `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff` and
  `Content-Security-Policy: sandbox`, so an uploaded file never runs as a page of this
  site. `Range` requests are served (`206`), so players can seek.

`404` for a document or commit that doesn't exist, or a folder; `410` for the bytes of an
unversioned file's older version.

### `PUT /api/workspaces/{workspace}/spaces/{space}/docs/{path}`

Creates or replaces a document. Markdown takes its text in `content` (UTF-8), any other
file its bytes in `content_base64`:

```json
{"content": "# Plan\n", "base_version": "…", "message": "Add the plan"}
```

- Without `base_version`, it creates the document: `201`, or `409` if it exists.
- With `base_version`, it replaces that version: `200`, or `409` if the document has
  changed or been deleted since.

`400` for content in the wrong field for the file, invalid base64 or UTF-8, or Markdown
that is exactly a Git LFS pointer (it would read as a version taken out of history); `409` also if
a folder has the name, or a file is in the way of its folders; `413` over the size limit
(a JSON body may be up to twice `kb_large_file_size` plus 64 KiB, for base64 and JSON
escapes); `507` over a space's limits.

**A file's bytes.** With `Content-Type: application/octet-stream`, the body is the file
itself, of any size up to `kb_max_file_size`, streamed to disk rather than held in
memory: how the web app uploads every file other than Markdown, and the way to send a
large one. `base_version`
and `message` go in the query (`?base_version=…&message=…`), and any other parameter is
`400`. Markdown sent this way must be UTF-8, as in JSON. The answers are the same.

### `PATCH /api/workspaces/{workspace}/spaces/{space}/docs/{path}`

Moves or renames a document or a folder, with everything in it:

```json
{"to": "archive/plan.md", "base_version": "…", "message": "…"}
```

`base_version` is optional, for documents only: if given, the document must still be at
it (`409`). `409` if `to` exists; `400` for a move into itself. A versioned file renamed
to a kind that is always [unversioned](#unversioned-files) (`photo.dat` to `photo.psd`)
becomes unversioned, and its older versions, at its old path, are
[taken out of history](#taking-a-files-old-versions-out-of-history); an unversioned file
renamed stays unversioned.

### `DELETE /api/workspaces/{workspace}/spaces/{space}/docs/{path}`

Deletes a document; its history stays, and a folder disappears with its last document.
The body is optional: `{"base_version": "…", "message": "…"}`, with `base_version` checked
as above. `200` with `{"commit": "…"}`; `404` if there is no such document, `400` for a
folder.

### `GET /api/workspaces/{workspace}/spaces/{space}/history/{path}`

The commits that changed a document, newest first. It doesn't follow moves: a move is a
`delete` in the old path's history and a `create` in the new one's.

```json
{
  "commits": [
    {
      "commit": "…", "author": {"name": "Alice", "email": "alice@example.com"},
      "agent": "TaskTo.AI agent", "time": "2026-01-02T03:04:05.000Z",
      "message": "Update notes/plan.md", "change": "update", "version": "…", "versioned": true
    }
  ],
  "next": "…"
}
```

`change` is `create`, `update` or `delete`; `version` is the document's version after the
commit, absent after a delete; `versioned`, with `version`, is `false` for a version whose
bytes git doesn't keep: an unversioned file's (only the current one's bytes can still be
read), or one [taken out of history](#taking-a-files-old-versions-out-of-history), which
can't be viewed or restored; `agent` is absent when the user made the change, and
`<name> (access token)` for a change made with an [access token](#access-tokens). `limit`
is 1 to 200, default 50; `next`, when there are more, is the `before` of the next page
(`?before=<commit>`: `404` if the commit isn't in the space's history, as for `?at=`).

### `GET /api/workspaces/{workspace}/spaces/{space}/history`

Every commit of the space, newest first, paged the same way, without `change` and
`version`.

### `POST /api/workspaces/{workspace}/spaces/{space}/restore/{path}`

Makes a document what it was at a past commit, as a new commit; it also brings back a
deleted document.

```json
{"commit": "…", "base_version": "…", "message": "…"}
```

`base_version` is optional and checked as above (`409`). `200` with the change's result;
`404` if the document didn't exist at that commit, or if `commit` isn't in the space's history
(a commit a failed change left behind is never a source); `410` for an unversioned file whose
bytes at that commit are gone.

### `DELETE /api/workspaces/{workspace}/spaces/{space}/history/{path}`

Removes a file's older versions from the space's history, for good: for personal data
saved by mistake, say. Its current version stays as it is; every other version is
[taken out of history](#taking-a-files-old-versions-out-of-history), as when a file
becomes unversioned: each becomes a Git LFS pointer whose bytes are deleted, so it can't
be read or restored again (`410`). A file that was deleted loses every version. It works
on any file, Markdown included, and on an unversioned one whose versions from before it
became unversioned are still in git.

Workspace admins only, with a session, in a space they may write: `403` (`admin_only`) for
the space's other editors, and `403` for an access token. The body is optional:
`{"base_version": "…"}`, the current version the admin saw (`409` if it has changed).

```json
{"commit": "…", "doc": {"path": "contacts.md", "version": "…", "size": 41, "versioned": true}, "removed": 2}
```

`removed` is how many versions were taken out; `doc` is absent for a deleted file. The
history records it as a commit by the admin that changes no file ("Remove the old versions
of contacts.md from history"); with nothing to remove, nothing happens and `commit` is the
current one. The server logs who did it. `404` if the path never had a version, `400` for a
folder. It works on a full space, since it frees space.

The rewrite gives commits from the file's first version new IDs, so pages showing the
space's history need reloading. A copy of the same content at another path keeps its own
versions. Backups made before still hold the removed versions until they expire
([install.md](install.md#backups)).

### Agents and the files

The repository is an ordinary git repository with a clean working tree, so agents and
tools can read the files directly (`git log` works too). An unversioned file's entry in
the working tree is its Git LFS pointer; its bytes are in the space's directory, in Git
LFS's local layout, at `.git/lfs/objects/<first 2 hex digits>/<next 2>/<sha256>`
(uploads are written to `.git/lfs/tmp/` first and moved there by the commit). They are
inside the data directory, so `taskto backup` includes them. Changes go through the API, or,
for the agent, through `knowledge.Store` with the user it acts for (after
`access.Checker` allows it) and the agent's name in `knowledge.Author.Agent`, as the
agent's tools do ([agent-tools.md](agent-tools.md)). Files
written into the directory any other way aren't committed and aren't seen by the API.

## Search

Search finds documents in the knowledge base by their words and, in a workspace with an
**embeddings model**, by their meaning. It searches one space, or every space of the
workspace the caller can read, and never another one: not another member's private space,
and not a shared space the caller isn't a member of, admins included.

**Full text** covers each Markdown document's path, title and text, and each upload's
path only (an upload's content isn't searched). The title is the document's front-matter
`title:`, else its first `# ` heading, else its file name. Every word of the query must
match, as a prefix (`plan` finds `planning`); words in double quotes must match as a
phrase. Case and accents don't matter. Other characters are separators: there are no
operators, so no query is a syntax error. A title match counts most, then the path, then
the text; uploads, matched by name only, count half. Words are found by the spaces and
punctuation between them, except in scripts written without spaces (Chinese, Japanese,
Thai, Lao, Khmer, Burmese): there every character counts as a word, with its vowel and
tone marks, and a query's run of such characters must match as a phrase, so `京都` finds
`東京都庁` and `กินข้าว` finds it in the middle of a Thai sentence. A query of one or two
such characters finds every document they appear in, next to each other. Korean is written
with spaces, and searched by its words. Of a long query, the first 32 words and phrases,
and 64 words in all (each character of those scripts counting as one), are searched.

**Semantic search** compares the query's embedding with those of the documents' passages
(a document is split at its headings into passages of up to about 1500 characters; past
128 passages, the rest of a long document is found by full text only). With an embeddings
model, every search is **hybrid**: the full-text and semantic rankings are combined by
reciprocal rank fusion (each document scores `1/(60 + rank)` in each ranking that finds
it), so a document both find comes first. Without one, search is full text only, and
says so. The server keeps the vectors it compares in memory, up to `-search-vector-cache`
(see the README); what it finds is the same either way. A space is embedded only with **consent**, given by its owner (a private space)
or one of its editors (a shared one) to the workspace's endpoint as it is
([below](#put-apiworkspacesworkspacespacesspacesearchembedding)); until then it is
searched by full text only, which never leaves the server.

**Git is the truth.** The index records which commit of `main` it reflects in each space.
It catches up after every change, in the background, and before every search, so a search
always reflects `main`, also after a crash or a change made outside the server. The one
exception: if a long catch-up of a space is already running (after a large change made
outside the server, say), a search waits for it at most 2 seconds, then answers from what
is indexed and says so in `notice`. An upgrade that changes how text is indexed indexes
every space again this way, from `main`, in the background (embeddings are kept); until a
space is done, its searches say so in `notice` too. Embeddings
are made in the background, and a change never waits for them: a new or changed document
is found by full text at once and by meaning once its passages are embedded (failures are
retried, waiting from 30 seconds up to an hour).

### `GET /api/workspaces/{workspace}/spaces/{space}/search`

Searches one space: read access (`404` for a space the caller can't read). Query:

- `q`: what to search for, required, at most 500 bytes; `400` without a word in it.
- `limit`: results per page, 1–100, default 20.
- `offset`: results to skip, 0–1000, for the next page. Pages of one search follow one
  order: as long as nothing changes in between, they neither repeat nor skip a result.

```json
{
  "results": [
    {
      "space_id": 3,
      "space_name": "Team",
      "path": "notes/plan.md",
      "title": "Quarterly plan",
      "snippet": [{"text": "…we grow the "}, {"text": "orchard", "match": true}, {"text": " next year…"}],
      "score": 0.0325,
      "match": "both",
      "version": "3b18e512dba79e4c8300dd08aeb37f8e728b8dad",
      "commit": "9fceb02d0ae598e95dc970b74767f19372d61af8"
    }
  ],
  "mode": "hybrid",
  "notice": "2 documents are still being embedded; until they are, only their text is searched.",
  "notices": [{"code": "pending", "count": 2}],
  "next_offset": 20
}
```

- `snippet`: the passage that matched, as plain text in pieces, those that matched marked
  `"match": true`. It is never markup: show it as text. For an upload, it is from its
  path or title, whichever matched. For a document found by meaning only, it is the start
  of its passage most like the query.
- `score`: higher first; it compares the results of one search only (bm25 in full-text
  mode, the fused score in hybrid mode).
- `match`: `"text"`, `"semantic"` or `"both"`.
- `version` and `commit`: the document's version and the commit of `main` the index had;
  read the document `?at=` that commit to see exactly what was found.
- `mode`: `"hybrid"` or `"fulltext"`. `notice` says why a search is full text only (no
  embeddings model, it didn't answer, the server's egress policy doesn't allow its
  endpoint, or no space searched has consent for it: then the query isn't sent to the
  endpoint either), what it can't find by meaning yet, or that the index is still
  catching up.
- `notices`: what `notice` says, as codes, for a client to say in its own language:
  `embeddings_off`, `embeddings_failed`, `egress_refused`, `no_consent`, `pending` (with
  `count`, the documents still being embedded) and `behind` (the index is catching up),
  in the order `notice` says them; present when `notice` is.
- `next_offset`: present when there are more results.

### `GET /api/workspaces/{workspace}/search`

Any member. The same, across every space of the workspace the caller can read (their
private space, and the shared spaces they are a member of, archived ones included).

### Searching cards

The cards of boards are searched apart from the documents, by full text only: a card's
title, Markdown body and comments (a card's newest comments, up to 256 KiB of them), with
the same words, phrases, scripts and limits as a document search. They are never sent to an
embeddings endpoint, whatever a space has consented to. Only the cards of boards in
spaces the caller can read are found: a personal board only by its owner, a team board by
the members of its space, archived cards and boards, and boards of an archived space, too,
since those stay readable; never another member's personal board or the board of a shared
space the caller isn't in, admins included. Results come best first: a card with every word
in its title, then one with every word in its title and body, then the rest, and the card
changed last first within each. The order depends only on the cards the caller can read,
never on others', and no score is returned.

The index follows the boards: every change to a card's title or body, and every comment
added, edited or deleted, queues the card in the same transaction, and a search indexes the
queued cards of the spaces it looks in before it answers, so it never quotes text older than
the card. A card that changes while the search runs is looked for once more; if the index
still can't catch up, the reply has `"incomplete": true` and leaves those cards out, and a
later search finds them. An incomplete reply has no `next_offset`: an offset counts the cards
indexed at that moment, so it could repeat or skip a card once the others are indexed; search again
instead. A card goes with its board or space when that is deleted, and every
card is indexed when the server starts. Who may read a space is checked again after the
indexing, so a member removed meanwhile gets none of its cards.

### `GET /api/workspaces/{workspace}/spaces/{space}/search/cards`

Read access to the space (`404` for one the caller can't read). Query: `q`, `limit` and
`offset`, as for a document search, with the same errors.

```json
{
  "results": [
    {
      "space_id": 3,
      "space_name": "Team",
      "board_id": 7,
      "board_name": "Roadmap",
      "card_id": 42,
      "title": "Orchard rollout",
      "column": "doing",
      "snippet": [{"text": "ship the "}, {"text": "orchard", "match": true}, {"text": " release"}]
    }
  ],
  "next_offset": 20
}
```

- `column`: where the card is on its board, `archived` if it is off it. Open the card
  with `GET .../boards/{board_id}/cards/{card_id}`; the web app links to
  `/w/{workspace}/boards/{space_id}:{board_id}:{card_id}`.
- `snippet`: the passage of the title, body or comments that matched, as plain text in
  pieces, as for a document: never markup.
- `incomplete`: `true` when cards that changed during the search were left out; then there is no `next_offset`.
- `next_offset`: present when there are more results, and the reply is not incomplete.

### `GET /api/workspaces/{workspace}/search/cards`

Any member. The same, across the boards of every space of the workspace the caller can read.
Both routes also take an access token.

### `GET /api/workspaces/{workspace}/search/settings`

Any member. The workspace's embeddings model, or `null`, and how far the index is:

```json
{"embeddings": {"account": 4, "provider": "ws1-ollama", "model": "nomic-embed-text"}, "documents": 120, "pending": 3}
```

`documents` counts the workspace's indexed files; `pending`, the Markdown documents still
waiting to be embedded (in spaces with consent).

### `PUT /api/workspaces/{workspace}/search/settings`

Admin only. Sets the embeddings model: one of the **workspace's** provider accounts
([Model providers](#model-providers)) and a model its endpoint serves at
`{base_url}/embeddings`, OpenAI's embeddings API (Ollama, vLLM, LiteLLM and hosted APIs
serve it). The model needn't be in the account's `models`, which lists chat models.
`null` turns semantic search off.

```json
{"embeddings": {"account": 4, "model": "nomic-embed-text"}}
```

The server asks the endpoint for one embedding first, and refuses the setting (`400`) if
it doesn't get one, if the egress policy of workspaces' accounts doesn't allow the
endpoint (the message says so), or if the account isn't the workspace's: a personal
account never serves a workspace. `200` with the settings. A different model drops every vector of the
workspace and embeds its documents again; deleting the account turns semantic search off.

It is set per workspace, not by the operator, because the index serves the workspace:
its admins already choose its provider accounts, the same model must embed every space
for a workspace-wide search to compare them, and a workspace on a laptop and one on a GPU
server want different models.

**Privacy.** Admins choose the endpoint, but they don't decide what goes to it: a space's
Markdown documents, and the searches of it, are sent there (from the server) only with
the consent of the space's owner or one of its editors, and only to the very endpoint
they consented to: the account, the model and the account's base URL at the time. An
admin who changes any of them, by mistake or to read spaces they aren't a member of,
stops embedding until a member of each space consents again; nothing is sent to the new
endpoint meanwhile. Choose an endpoint the workspace trusts, such as a local Ollama.

### `GET /api/workspaces/{workspace}/spaces/{space}/search/embedding`

Whoever can read the space (`404` otherwise). Its consent:

```json
{
  "enabled": true,
  "endpoint": {"account": 4, "provider": "ws1-ollama", "model": "nomic-embed-text", "base_url": "http://ollama:11434/v1"},
  "given_by": 7,
  "given_at": "2026-11-18T14:03:11.204Z"
}
```

- `enabled`: the space is embedded: consent was given for `endpoint`, the workspace's
  embeddings endpoint now.
- `endpoint`: what consent is, or would be, for; `null` without an embeddings model.
- `given_by` and `given_at`: who consented, and when, `null` if nobody has. With
  `enabled: false`, consent was for an earlier endpoint.

### `PUT /api/workspaces/{workspace}/spaces/{space}/search/embedding`

The space's owner or one of its editors, archived or not (`403` for a viewer; `404` for
anyone who can't read it, admins included). `{"enabled": true}` consents to the
workspace's endpoint as it is now (`409` without one); consent to another endpoint than
before drops the space's vectors, which are made again by the new one. A client that
showed the endpoint sends it back, `{"enabled": true, "endpoint": {…}}` as `GET` answered
it: consent is then given only if the account, model and base URL are still those, else
`409`, so a change an admin makes in between is never consented to unseen. `{"enabled":
false}` withdraws consent and deletes the space's vectors at once, including any being
made then. `200` with the consent.

## Boards

Every private space has one **personal board**, created with the space; a shared space
has any number of **team boards**. A board's access is its space's (see
[Spaces and access](#spaces-and-access)): whoever may read the space reads its boards and
cards, and its **writers** (the owner of a private space, the editors of a shared one)
change them. Every route below is under
`/api/workspaces/{workspace}/spaces/{space}/boards` and answers `404` for a space, board
or card the caller can't read, or that isn't where the path says (a board of another
space, a card of another board), and `403` for a write the caller may only read: a
viewer, an archived space, or an archived board.

**Versions.** Boards and cards carry a `version`, bumped by every change. Every update
(`PATCH`, move, archive, restore) sends the `version` the client last read; if someone
changed the board or card since, the update changes nothing and answers `409`, with the
current state to redo it from:

```json
{"error": "it changed since you read it: reload and try again", "code": "card_changed", "current": { … }}
```

The code is `board_changed` for a board, `card_changed` for a card.

A missing `version` is a `400`. Comments don't change a card's version.

A board:

```json
{
  "id": 3, "space_id": 4, "kind": "team", "name": "Launch",
  "archived_at": null, "version": 1,
  "created_at": "…", "created_by": {"id": 2, "name": "Member", "agent": false},
  "updated_at": "…", "updated_by": {"id": 2, "name": "Member", "agent": false}
}
```

`kind` is `personal` or `team`. An author is `null` if their account no longer exists;
`agent` is `true` when the agent made the change for that user.

### `GET …/boards`

The space's boards, its personal board or its team boards by name:
`{"boards": [ … ]}`. Archived boards are left out unless `?archived=true`.

### `POST …/boards`

Writers of a shared space. `{"name": "Launch"}` (1–100 characters). `201` with the
board. `400` in a private space, which has only its personal board.

### `GET …/boards/{board}`

The board.

### `PATCH …/boards/{board}`

Writers. Renames, archives or unarchives: `{"version": 1, "name": "New name"}`,
`{"version": 1, "archived": true}`, or both. An archived board stays readable and
listed with `?archived=true`, and refuses changes to its cards (`403`) until unarchived.
A personal board can be renamed, not archived (`400`). `200` with the board.

### Cards

A card:

```json
{
  "id": 12, "board_id": 3,
  "title": "Write the launch post", "body": "Markdown…",
  "column": "todo", "rank": "V", "archived_from": null,
  "assignee": {"kind": "user", "id": 5, "name": "Member"},
  "blocked": true, "blocked_reason": "waiting for legal", "blocked_by": [9],
  "docs": ["launch/post.md"], "conversations": ["7"],
  "due": "2026-03-31", "labels": ["launch", "writing"],
  "version": 4,
  "created_at": "…", "created_by": {"id": 2, "name": "Member", "agent": false},
  "updated_at": "…", "updated_by": {"id": 2, "name": "Member", "agent": true, "via": "laptop"}
}
```

- `created_by`, `updated_by`, and a comment's `author`: the user, `agent` when the agent
  made the change for them, and `via`, the [access token](#access-tokens)'s name, when it
  was made with one (an agent of their own); `via` is absent otherwise.

- `column`: `backlog`, `todo`, `doing`, `review`, `done`, or `archived`. An archived card
  is off the board; `archived_from` is the column it left, where restoring puts it back.
- `rank` orders the cards of a column (compare as byte strings). It is opaque: clients
  place cards by position (below), and the server gives the card a rank between its new
  neighbours', so a move changes only that card.
- `assignee`: `null`, `{"kind": "agent"}`, or `{"kind": "user", "id": …}`, a member of
  the workspace who can read the space (a viewer is fine), checked when assigned. In
  requests `name` is ignored. Someone who later loses access stays assigned until the
  card is changed.
- `blocked` is true when the card has a `blocked_reason` or cards in `blocked_by`, which
  are other cards of the same board.
- `docs` are knowledge-base paths within the board's space, like `notes/plan.md`
  (relative, no `..`, no `.git`), not checked to exist. `conversations` are
  [conversation](#conversations) IDs, the API's `id` in decimal (`"7"`). Conversations are
  private, so a conversation can only be added to a card by its owner (or the agent
  working for them), and only one of the board's workspace: anything else is `400`, the
  same for someone else's conversation as for one that doesn't exist. Whoever may edit the
  card may keep or remove a link, and everyone who can read the card sees it, but only the
  owner can open the conversation (`404` for anyone else). Up to 50 of each, and of
  `blocked_by`; repeats are dropped.
- `title`: one line, 1–200 characters. `body`: Markdown, up to 32 KiB.
- `due`: a calendar date, `YYYY-MM-DD`, or `null`. It has no time and no time zone: a card
  due on the 31st is due on the 31st wherever it is read, and clients decide what "today"
  is for their person. A card is *overdue* when its `due` is before today and it isn't in
  `done` or `archived`; the card doesn't say so, since that changes with the day. Its assignee
  is reminded as described under [due-date reminders](#due-date-reminders).
- `labels`: up to 10 free-text labels per card, each 1–30 characters on one line. The server
  normalizes them (Unicode NFC, inner whitespace collapsed, case folded), drops repeats and
  returns them sorted, so `"Bug "` and `"bug"` are one label, as are a composed and a
  decomposed `é`, and `ΟΣ` and `ος`. Clients send what people typed and show what comes back. Labels belong to the card, not to a board:
  there is no list of labels to manage, and a label is in use as long as a card has it.

### `GET …/boards/{board}/cards`

The board's cards in board order: by column, then rank. `{"cards": [ … ]}`, without
threads. Archived cards are left out unless asked for with `column=archived`. Filters,
combined with AND:

| Parameter | Keeps |
|---|---|
| `column` | the cards of one column |
| `assignee` | `me`, `agent`, `none` (unassigned), or a user ID |
| `blocked` | `true` or `false` |
| `q` | cards whose title or body contains the text, ignoring ASCII case (up to 200 characters) |
| `label` | cards that have this label (normalized like a card's labels); repeat it, up to 10 times, to require several (`too_many_labels` past that, for the agent's `list_cards` too) |
| `due_before` | cards due on or before this date (`YYYY-MM-DD`); cards without a due date are never kept |
| `due_after` | cards due on or after this date |
| `overdue` | `true`: cards due before today that aren't `done` or `archived` |
| `today` | the date `overdue` counts from, `YYYY-MM-DD`; default today in UTC. Send your own, since the server doesn't know the reader's time zone |
| `sort` | `board` (default: by column, then rank) or `due` (soonest due date first, cards without one last, each group in board order) |

### `POST …/boards/{board}/cards`

Writers. `title` is required; everything else is optional:

```json
{
  "title": "Write the launch post", "body": "…", "column": "todo", "position": 0,
  "assignee": {"kind": "user", "id": 5}, "blocked_reason": "…", "blocked_by": [9],
  "docs": ["launch/post.md"], "conversations": ["7"],
  "due": "2026-03-31", "labels": ["launch", "writing"]
}
```

`column` defaults to `backlog` and can't be `archived`. `position` is the card's 0-based
place in its column; left out or past the end, the card goes last. `201` with the card.

### `GET …/boards/{board}/cards/{card}`

The card with its thread, oldest first:

```json
{"id": 12, …, "comments": [{"id": 1, "card_id": 12, "author": {"id": 2, "name": "Member", "agent": false}, "body": "…", "created_at": "…", "mentions": []}]}
```

### `PATCH …/boards/{board}/cards/{card}`

Writers. Edits the fields it names, with the `version`: `title`, `body`, `assignee`,
`blocked_reason`, `blocked_by`, `docs`, `conversations`, `due`, `labels`. `null` clears
`assignee`, `blocked_reason` and `due`; the lists, `labels` included, replace the whole list
(`[]` clears it). Setting the blocked
state is this route: `{"version": 4, "blocked_reason": "…", "blocked_by": [9]}`, and
`{"version": 5, "blocked_reason": null, "blocked_by": []}` to unblock. `200` with the
card; `400` with nothing to change.

### `POST …/boards/{board}/cards/{card}/move`

Writers. `{"version": 4, "column": "doing", "position": 0}`: puts the card at `position`
in `column`, counting the column's cards without this one; left out or past the end,
last. Any column but `archived` (use archive). Moving an archived card brings it back.
`200` with the card.

### `POST …/boards/{board}/cards/{card}/archive`

Writers. `{"version": 4}`. Moves the card to `archived`, remembering its column. `200`
with the card; `400` if it is already archived.

### `POST …/boards/{board}/cards/{card}/restore`

Writers. `{"version": 5}`, or with `"column": "todo"` to restore elsewhere. Puts the card
last in the column it was archived from. `200` with the card; `400` if it isn't archived.

### `POST …/boards/{board}/cards/{card}/comments`

Writers. `{"body": "Markdown…"}` (up to 32 KiB, not blank). Adds to the thread as the
caller. `201` with the comment. A comment tells people in their notifications
([below](#notifications)): the card's assignee, and each person it mentions.

#### Mentions

A comment mentions a person with the token `@[Name](user:ID)`, where `ID` is their user ID (the
`id` of the space's members, the agent's `list_members`). The web app's comment box writes it when
you pick someone. The ID is what counts: a different or stale `Name` tells the person of that ID
all the same, and `@name` typed as plain text is only text. A token counts only where Markdown
shows it as a link in the text of a paragraph, heading or table cell: not in a fenced code block
(also in a quote or a list item), not in a code span, not after a backslash (`\@[…](user:…)`), not
in a link's address or an image's label, and its `Name` has no `[`, `]` or `\`. A comment mentions
at most 20 people: the first 20 different IDs its tokens name, whether or not they exist or may
read the space; each is told once.

Every comment in an answer has `mentions`: the people its body mentions, as `[{"id": 5, "name":
"Bo"}]`, with the name the server has for that ID, for those the reader may see in the space (its
members). A token for anyone else, or an ID that doesn't exist, has no entry, so a comment can't be
used to look up names. The web app shows a mention by this name, never by the `Name` in the text,
and as a neutral placeholder when it has no entry.

Who is told is decided in the transaction that writes the comment, with the card's assignee and
everyone's access as they are then.

A comment tells nobody who may not read the space at that moment, whether they are mentioned or
assigned, and answers the same whether or not a mentioned ID exists, so it can't be used to find
who is in a workspace. It never tells its author.

### The agent

The agent works boards through tools that call the same code, for the user it works for
and with exactly that user's access. See [boards-agent-tools.md](boards-agent-tools.md)
and [agent-tools.md](agent-tools.md).

## Notifications

The notification inbox and, for people who opt in, Web Push ([pwa.md](pwa.md#notifications),
[install.md](install.md#web-push-notifications)). A notification is about one person in one
workspace: its kind and the IDs of what it is about, never a copy of what the record says. What an
item says is worked out each time it is read, or pushed, from what the recipient **may read at that
moment**: once they can't read what it was about (the space's membership ended, the card or app is
gone, the app's roles changed), the item is `{"kind": "changed"}` with no `params`, no IDs and the
workspace's home as its target, and a push for it says the same. Notifications are made for:

- `card_assigned`: a card was assigned to you by someone else (`params.title`; target: the card);
- `card_due`: a card assigned to you is due today (`params.title`, `params.due`; target: the
  card), and `card_overdue`: it was due yesterday and isn't done
  ([due-date reminders](#due-date-reminders));
- `card_comment`: someone else commented on a card assigned to you (`params.title`; target: the
  card). One per comment;
- `card_mention`: someone else mentioned you in a comment on a card ([mentions](#mentions);
  `params.title`; target: the card). One per comment: a person who is both the assignee and
  mentioned in a comment is told of the mention only. No notification holds the comment's text;
- `app_effect_failed`: an effect of an app you acted in failed for good and waits for you to retry
  it (`params.app`; target: the app). One per failure, so a retry that fails again is a new one.

Leaving a workspace deletes the person's notifications of it. Old ones are removed after 90 days
(read) or 180 days, and past 500 per person and workspace. These routes take a session only (`403`
with an access token) and the CSRF guard checks the ones that change something (send `{}` for a
body that has no fields).

An item:

```json
{"id": 12, "workspace_id": 1, "kind": "card_assigned", "params": {"title": "Fix the gate"},
 "target": {"section": "boards", "item": "3:4:9"}, "read": false, "created_at": "…"}
```

`target` is an address of the web app under `/w/{workspace_id}/`: its section and item, as the
app's router reads them. The page it opens checks access again.

### Due-date reminders

A card with a `due` date that is assigned to a person (not the agent, not nobody) is reminded to
that person by two notifications: `card_due` on the morning of the due day and `card_overdue` on
the morning after. They are made by jobs on the general scheduler ([architecture.md](architecture.md)),
kept in step with the card by the change that touches it:

- **When.** The morning is 08:00 UTC. A due date is a calendar date and TaskTo doesn't know a
  person's time zone, so one instant is used for everyone (the morning of the day in the Americas
  and Europe, the afternoon in east Asia). A job whose time has already passed when the date or
  assignee is set is not made: setting a card due today after 08:00 gets no reminder but still the
  overdue notice, and a date in the past gets none. A reminder that is delivered after its day (a
  server that was off) is dropped, since the overdue notice says what is true then.
- **Who.** Only the assignee. A card assigned to the agent, or to nobody, has none. Changing the
  assignee cancels the previous person's and makes the new person's; changing or clearing `due`
  moves or cancels them; moving the card to `done` or archiving it cancels them, and moving it
  back (or restoring it) makes them again if their time is still ahead.
- **Once.** A person is told once per card, kind and due date, however many times a job runs (a
  retry, a clock that stepped back, a card given to someone else and back). Edits that change
  neither `due`, the assignee nor the card's column leave the jobs alone, a reminder that is ready
  but hasn't run yet included. A new date is a new reminder. An old notice whose card has a
  different date now (or none) is shown as a plain "something changed" mention of the card.
- **Where.** Every way of changing a card keeps the jobs: the web app's routes, the agent's tools,
  the MCP tools and the apps' effects. Archiving a board or a shared space cancels its cards' jobs
  and restoring it makes them again; a person who leaves a space or the workspace loses theirs, and
  gets them back, for the cards still assigned to them and dated, when they are added to the space
  again (a workspace's invite gives back those of their private space; a shared space's come with
  adding them to it); a server that starts with dated cards that have none (made before reminders existed) schedules them.
  A date the scheduler can't hold (the overdue notice of `8999-12-31` would be in the year 9000)
  has no notice for that part, and the card is saved all the same.
- **Access.** When a job runs, in the transaction that makes the notification, it checks that it is
  still the job that was claimed, that the assignee may read the card's space (a person who left it,
  or the workspace, gets nothing), that the space and board aren't archived and that the card still
  has the date, assignee and an open column the job was made for.

### `GET /api/workspaces/{workspace}/notifications`

The caller's notifications in the workspace, newest first: `{"notifications": […], "more": true,
"unread": 3}`. `limit` (1–50, default 30), `before` (a notification's ID: the page of older ones),
`unread=true` (unread ones only). `unread` counts all the caller's unread ones, not the page's.
`400` for a bad `limit` or `before`.

### `GET /api/workspaces/{workspace}/notifications/count`

`{"unread": 3}`: what the badge shows.

### `POST /api/workspaces/{workspace}/notifications/{notification}/read`

Marks one of the caller's notifications read. `204`; `404` for one that is someone else's, in
another workspace, or gone.

### `POST /api/workspaces/{workspace}/notifications/read`

Marks all the caller's notifications of the workspace read. `204`.

### `GET /api/me/notifications/{notification}`

One notification of the caller's, whatever its workspace, as above: the address a push opens,
`/notification/{id}`, which holds nothing but this ID. `404` (`notification_not_found`) for
someone else's.

### Notification email

Email is optional, per person and workspace, and off until chosen ([pwa.md](pwa.md#email)). A mail
carries no record data, only the kind of thing that happened and a link back; the daily summary only
a count. Nothing is mailed to an address until its owner has confirmed it (below). These routes are
for the workspace's members, about the caller only.

#### `GET /api/workspaces/{workspace}/notification-email`

`{"available": true, "address": {"email": "ana@example.com", "state": "verified"}, "kinds":
{"card_assigned": "off", "app_effect_failed": "digest"}, "digest": {"hour": 8, "timezone": "UTC"}}`.
`available` is false when the server has no SMTP server or no public URL (nothing can be turned on
then, and everything can still be turned off). `address.state` is `unverified` (no link waits),
`pending` (a link was sent and may still be opened) or `verified`; nothing is sent unless it is
`verified`. Each kind is `off`, `instant` (a mail soon after, one for each notification) or `digest`
(counted in the daily summary, sent at `hour` o'clock, 0 to 23, of the IANA `timezone`; the default
is 8 o'clock UTC; `chosen` is false until the person has set the time themselves, so a client may
offer its own zone then and never replaces one they chose).

#### `PUT /api/workspaces/{workspace}/notification-email`

`{"kinds": {"card_assigned": "instant"}, "digest": {"hour": 18, "timezone": "Europe/Lisbon"}}`; both
fields are optional and what is left out stays. Answers with the new choices, as the GET does. `400
notification_email_invalid` for another kind or mode, an hour outside 0 to 23 or a time zone that
isn't an IANA name; `409 mail_not_configured` for turning anything on where `available` is false
(turning things off always works). Turning something on for an address whose state is `unverified`
mails it a confirmation link (the choice is kept even if the mail can't be sent: the state says so).
Choosing `digest` for a kind schedules the person's summary on the
[general scheduler](../internal/schedule/schedule.go) (job kind `notifications.digest`, one per
person and workspace). It has no fixed interval: each run schedules the next at the same hour of the
person's own date, so it keeps its hour through daylight saving time. The summary counts the kind's
notifications that come after the kind was turned to `digest`, are still unread, are less than 48
hours old, weren't counted in an earlier summary and that the person may still read: so its number
can be lower than the inbox's. Having no kind on `digest` ends it,
and so does leaving the workspace.

#### `POST /api/workspaces/{workspace}/notification-email/verification`

No body. Mails the caller's own address a link to confirm it (`204`), unless it is confirmed
already (`204`, nothing sent). The link holds a random single-use token that the server keeps only
as a SHA-256 hash and never logs; it expires after 24 hours, and a newer link replaces it. At most
one link is sent every 5 minutes (`429 notification_email_verification_too_soon`, with
`Retry-After`); `409 mail_not_configured` where the server can't send mail; `502
notification_email_verification_unavailable` when the mail couldn't be sent (the wait isn't used).

#### `POST /api/me/notification-email/confirm`

`{"token": "..."}`, the part of the link after `#`. Confirms the signed-in caller's address, once
(`204`); a token of someone else's, unknown, used, expired or for an address the caller no longer
has is `400 notification_email_verification_invalid`. It takes a browser session, not an access
token. The link opens `/confirm-notification-email` in the web app, which sends it.

### Web Push

Push is optional, per device, and never needed: everything above works without it. A push is
a prompt to look at the inbox, not a message and not a way to sync: the browser's push service
carries it with a lifetime of an hour and may deliver it late, twice or not at all. What is sent is
`{"id": 12, "kind": "card_assigned"}` (the kind is `changed` if the recipient may no longer read what
it was about), encrypted to the browser (RFC 8291, `aes128gcm`), signed with the server's VAPID key
(RFC 8292). These routes take a session only.

### `GET /api/me/push/key`

`{"public_key": "B…"}`: the server's VAPID public key, base64url, which a browser subscribes with
(`applicationServerKey`). It is made once, when the server first starts, and its private half is
sealed with the server key and never returned or logged.

### `PUT /api/me/push/subscription`

Registers the calling browser: `{"endpoint": "https://…", "keys": {"p256dh": "…", "auth": "…"}}`,
what a browser's `PushSubscription` says (no other fields). `204`. The endpoint, the device's
identity, is an `https` address of at most 2048 characters (`400 push_endpoint_invalid`), the keys
a P-256 point and a 16-byte secret (`400 push_keys_invalid`), at most 20 devices per person
(`409 push_too_many_subscriptions`; registering a device again is not another, but a browser that
takes over another person's endpoint is one more of its new person's). The endpoint must be an
address the server may connect to under its [egress policy](install.md#web-push-notifications): a
loopback, private, link-local or metadata address is `400 egress_refused` here, as it is when a
push is sent. A person who is in no workspace can't register one (`403 push_no_workspace`). It is
bound to the session that made it: signing out, a session that expires or ends elsewhere, signing
out everywhere and a password change remove it, and no push is sent to a subscription whose session
has expired, even before it is removed (the server sweeps them hourly and checks at each push). A
browser that is signed in as someone else takes the endpoint over, and what was queued for the
first person is dropped. Only notifications made after this are pushed to the device.

### `DELETE /api/me/push/subscription`

`{"endpoint": "https://…"}`: removes the caller's subscription for that endpoint. `204`, also for one
that isn't there or isn't theirs.

Subscriptions also end when the push service answers a push with `404` or `410`, and when the person
leaves the last workspace they were in. A push that fails with `429` or a server error is tried again
(at most five tries in all, which holds across a restart, over less than an hour); one refused with another status (`400`, `401`, `403`,
`413`), or to an address the [egress policy](install.md#web-push-notifications) refuses, is dropped.

## Personal apps

A personal app is a screen built from a fixed catalog of [A2UI](https://a2ui.org/) v0.9
components, a **collection** of rows with a schema, and **actions** the screen's buttons
run on the server: a fixed set, add, change or delete a row. Nothing an app defines runs
as code on the server. Apps are built by asking the agent (its app tools, see
[agent-tools.md](agent-tools.md)); these routes launch them, run their actions and manage
them.

An app lives in a space, and its access, and its rows', is the space's (see
[Spaces and access](#spaces-and-access)): a private space's apps are its owner's, a
shared space's are its members'. Whoever may read the space launches its apps and sees
their rows; its **writers** create and change apps and run their actions (which change
rows). A viewer's action is `403`. **Sharing** an app is moving it to a shared space,
with its rows (`…/move`); a whole workspace shares an app through a shared space everyone
is a member of. Every route below is under
`/api/workspaces/{workspace}/spaces/{space}/apps`, needs a session (`401`), or an
[access token](#access-tokens) on the routes that read an app and run its actions (the others
answer a token `403`), goes through the [CSRF guard](#csrf) unless it is a `GET`, and answers `404` for a space or app the
caller can't read, or that isn't where the path says (an app of another space), and `403`
for a write the caller may only read: a viewer, or an archived space. Besides, `403` with
the code `app_forbidden` is for something the space allows but the caller's **roles in the
app** don't.

### App roles

The space is the outer boundary; roles in an app only **narrow** what the caller's role in
the space allows, never widen it (a viewer of the space holds no right to write, whatever
roles an app gives them; nobody writes in an archived space). Roles are a set of
capabilities, and a user may hold several:

| Role | Design | Publish | Submit | Read records | Modify records |
|---|---|---|---|---|---|
| `builder` | yes | | | | |
| `publisher` | | yes | | | |
| `user` | | | yes | own | own |
| `data_manager` | | | yes | all | all |
| `reader` | | | | all | |

Every role may view the app (launch it and read its live definition); a `builder` also manages
the app's roles. Roles are removed with the member who held them when they leave the space, so
the last builder leaving would strand an app whose roles are assigned. **Only while no assignment
gives anyone the `builder` role, a workspace admin who may write the shared space (the space's
creator is one, as its editor) can view the app and manage its roles**, to give someone the
builder or data manager role again; once someone is a builder, the admin has only the roles
assigned to them, like anyone. It is a way back, not standing access: it adds nothing else, and
while the app has a builder an admin can't use it to give themselves its data. **Design** is changing the app's draft, renaming it and moving it;
**publish** is publishing the draft, rolling back to an earlier release (see
[Drafts and releases](#drafts-and-releases)), and taking it offline and restoring it;
**submit** is adding a record (an `insert` action). **Own** records are those the caller
created, which includes the ones the agent created for them; records whose creator's
account was deleted are in nobody's own, and only readers of all see them. Every way of
reading records answers to the read scope: the screen's rows, its summaries (`count`,
`sum`), `rows` in an app's list, the records query (pages, totals and aggregates) and the
agent's `list_app_rows` and `query_app_rows`. A change to a record the
caller may not change is answered as `app_row_not_found`.

### Records: revisions, history and deleting

A record is a row of the app's collection. Each has a **revision**, 1 when it is created
and up by one with every change (an update that changes nothing is not a change). Besides
its data it keeps who made it, who last changed it and, once deleted, who deleted it,
each with the time, whether it was the agent and which access token it used if it was, and
the app's action that did it. Every revision is also kept whole, as a **version** in the
record's history: its operation (`insert`, `update`, `delete` or `restore`), its data, who,
when and by which action. A record that existed before revisions were kept has revision 1
and one `synthetic` version holding its data as it was then, dated at its creation:
earlier states were never kept.

An update or delete says which revision it was made against (`expectedRevision` on the
action, see below), and if the record has moved on it is refused with `409` and the code
`app_record_conflict`, changing nothing. The answer carries `current`, the record as an
item as it is now, if the caller may read it, and nothing else about it. **Omitting the
revision is allowed:** the change then applies to the record as it is (last writer wins),
which is what the agent's and scripts' callers do. The web app always sends it, from the
`_revision` of the item the person acted on.

**Deleting is a tombstone**: the record leaves every ordinary read, the screen, its
summaries, the counts in lists, the agent's reads and the limits, but is kept (with its
history) and can be restored. Tombstones stay, so a client that syncs later can be told
what was deleted. They go only with the app.

Reading a record's history, or the deleted ones, takes reading the record: the same read
scope as for the record itself (every record, or only those the caller created; a role
that reads no record, such as a builder alone, is `403 app_forbidden` with `read_records`).
Restoring takes the right to change the record, as deleting did (`modify_records`, of all
records or only the caller's own). A record the caller may not read or change is answered
as one that doesn't exist (`404`).

An app **without role assignments is unrestricted**, as every app was before roles: for a
user who may write the space it is every role, for one who may only read it, `reader`. So
nothing changes for an app until someone sets roles. An assignment is for one user, or with
`"user_id": null` the default, for every member of the space who has no assignment of their
own. Once an app has **any** assignment it is restricted for everyone: a member holds their
own roles, else the default's, else **none**, and then the app answers them `404` and is left
out of their list, like an app of a space they can't open. So restricting an app never leaves
someone it doesn't name (a member who joins the space later included) with every role. The agent acts as the user who started it, with
exactly their roles; it has no tool to set roles, and a definition grants none. Roles
belong to the space they were set in: someone who leaves the space (or the workspace) loses
their roles in its apps, so rejoining gives back nothing.

An app as one caller sees it has `access`, what they may do:

```json
"access": {"roles": ["user"], "restricted": true, "view": true, "design": false,
  "publish": false, "submit": true, "manage_roles": false,
  "read_records": "own", "modify_records": "own"}
```

### `GET …/apps/{app}/records`

The live records the caller may read, most recently changed first, at most 1,000
(`truncated`): `{"records": [ … ], "truncated": false}`. Each is
`{"id", "revision", "data", "created_at", "created_by", "created_action", "updated_at",
"updated_by", "updated_action", "deleted_at", "deleted_by", "deleted_action"}`, data
holding the fields the **live release's** collection declares (the draft's don't count until
published); `created_by` and the others are
`{"id", "name", "agent", "via"}` or `null`. Read scope and `403`/`404` as for the history.

### `GET …/apps/{app}/records/query`

The data explorer's route: **a page** of the records the caller may read, filtered, searched and
sorted by the server, with **totals over every record that matches**, never over the page.
`{"records": [ … ], "next_cursor": "…" | null, "total": 412, "aggregates": [ … ]}`; each record is
as in `GET …/records`. Parameters, all optional:

| Parameter | Meaning |
|---|---|
| `state` | `live` (default) or `deleted`. |
| `filter` | `field:op:value`, repeated, at most 8, **all** of which must hold (the value is what follows the second colon, so a time `12:30` is fine). `op` is `eq` or `ne` for every field, `lt`, `lte`, `gt`, `gte` for number, date, time and string fields, and `contains` for a string field (a case-insensitive substring, by Unicode case folding). The value is of the field's type; a number or a boolean may be text. A record with no value of the field equals nothing: `ne` matches it, the other operators don't (as in [rules](#personal-apps)). |
| `q` | Words (at most 5, each at most 100 characters) that each have to be part of some string or choice field of the record, in any case. |
| `sort` | A declared field, `_updated` (default), `_created`, `_deleted` (deleted records only) or `id`. |
| `dir` | `asc` or `desc`: `desc` by default for the three times, `asc` for the rest. |
| `limit` | Records in the page: 50 by default, at most 200. |
| `cursor` | The `next_cursor` of the previous page **of the same query**. |
| `aggregate` | `kind` or `kind:field`, repeated, at most 8: `count` (of the matching records, or with a field those that have a value of it), `sum` and `avg` of a number field, `min` and `max` of a number, date or time field. |

**What a query reads.** The fields it names are the **live release's**, and a value counts only
if it is of its field's type, as JSON holds it: a field the live release removed is in no filter,
sort, search or total (it is a `400 app_query_field`), nor is a value stored before the field was
retyped (a number kept as text, `"12"`, is shown as it is stored but is in no comparison, sort or
sum, as in no summary). Deleted records are in none of the live ones, and the read scope of the
caller's role (all records, or only the ones they created) applies to the page, to `total` and to
every aggregate: they are computed by SQL over exactly the records the caller reads. A role that
reads no record is `403 app_forbidden` (`read_records`), an app the caller can't see `404`.

**The order is total.** Records are ordered by the sort key, those with no value of it last
whichever the direction, and then by `id` in the same direction as the key. A page is read **by
keyset**: it asks for the records after the position the cursor names in that order, so paging
neither skips nor repeats a record that does not change meanwhile, whatever is inserted or deleted
between the pages. A record whose own sort key changes meanwhile (one edited while paging by
`_updated`) is read where it is when each page is asked: again if it moved into the pages still to
come, not at all if it moved into the ones already read; sort by `_id` or `_created` for a walk
that edits can't disturb. A text field orders by its first 200 characters, then by `id`, so a
cursor stays short. `total` and `aggregates` are asked
again with every page, so they are the whole matching set as of that request.

**Cursors** are opaque. A cursor is only a position and a fingerprint of the query it ends a
page of (the app, the caller, their read scope, the state, the filters, the search and the order,
not the page size or the aggregates); a cursor of another query, or of another person, or damaged
or too long, is `400 app_query_cursor`. The scope, the filters and everything else are decided
again at every request, so a cursor, even one forged with a valid fingerprint, can put a caller
anywhere in the order of the records they read and nowhere else.

**Aggregates** come back in the order asked: `{"kind", "field", "value", "count"}`, `count` being
the records the value was computed from (those with a value of the field). A `sum` or `count` over
none is 0; `avg`, `min` and `max` over none are `null`, as is a result too large to be a number.
Dates and times are given as the text they are. Numbers are summed as floating point in SQL, in
no particular order, so a sum of many fractions may differ from one added in another order in its
last digits.

### `GET …/apps/{app}/records/{record}/history`

A record's versions, newest first, at most 200 (`truncated` says if older ones were left
out): `{"record": {…}, "history": [{"revision": 3, "operation": "update", "data": {…},
"at": "…", "by": {"id", "name", "agent", "via"}, "action": "edit_expense"}, …],
"truncated": false}`. `record` is the record as it is now, with its `revision`, `data`,
who created, changed and deleted it, and `deleted_at` if it is deleted (its history can
still be read). Data holds the fields the live release's collection declares. Needs reading the record:
`404` for one the caller may not read or that doesn't exist, `403` with `read_records`
for a role that reads no record.

### `GET …/apps/{app}/records/deleted`

The app's deleted records the caller may read, most recently deleted first, at most 1,000
(`truncated`), in the same shape as `GET …/records`.

### `POST …/apps/{app}/records/{record}/restore`

Writers who may change the record (`modify_records`). `{"expected_revision": 3}`, the
revision of the deleted record the caller saw; it may be left out (`null`). Brings the
record back, with its history, as a new revision, and answers with it. `409` with
`app_record_conflict` if it isn't deleted, or has another revision; `409` with `app_full` if
the app has no room for it; `400` if it no longer fits the live release's collection, which
changed since (the field's code, such as `app_field_required`; a draft that isn't published
doesn't count); `409` with `app_offline` while the app is offline, `409 app_not_published`
before the first release. The history and deleted routes work for an offline app, whose data is kept.

### `GET …/apps/{app}/roles`

Those who manage the app's roles (`403 app_forbidden`, `manage_roles`, otherwise):
`{"grants": [{"user_id": 3, "roles": ["user"]}, {"user_id": null, "roles": ["reader"]}], "roles": [every role]}`.

### `PUT …/apps/{app}/roles`

Those who manage the app's roles, in a space they may write. `{"grants": […]}` replaces all
the assignments; an empty list makes the app unrestricted again. Every `user_id` must be able
to read the space (`400 app_roles_not_member`), and the caller must keep the right to manage
the roles (`400 app_roles_lockout`). `200` with the same reply as `GET`.

**Offline.** Taking an app offline archives it: it can't be launched (`409` from
`…/surface`) and its actions don't run (`409`), and its rows are kept, until it is
restored.

**Versions.** An app carries a `version`, bumped by every change to it (not by its rows
changing). Every update sends the `version` the client last read; if the app changed
since, it changes nothing and answers `409` with the code `app_changed` and the current
app, as for [boards](#boards).

An app:

```json
{
  "id": 9, "space_id": 4, "name": "Expense tracker",
  "archived_at": null, "version": 1, "rows": 12,
  "created_at": "…", "created_by": {"id": 2, "name": "Member", "agent": true},
  "updated_at": "…", "updated_by": {"id": 2, "name": "Member", "agent": true},
  "access": { … },
  "live": {"number": 2, "definition_version": 1, "requires": ["component.Stat", …],
    "published_at": "…", "published_by": {"id": 2, "name": "Member", "agent": false},
    "live": true, "live_since": "…", "definition": { … }},
  "unpublished_changes": true,
  "draft": { … },
  "effect_targets": [{"space_id": 7, "space": "Maintenance", "space_kind": "shared", "board_id": 12, "board": "Repairs"}]
}
```

`rows` counts the collection's rows the caller may read (see [App roles](#app-roles)).
`access` is what the caller may do in the app. `agent` is `true` when the agent made the change
for that user. `live` is the release the app serves (`null` until its first publish) and
`unpublished_changes` says the draft differs from it (always so before the first publish); the
**definitions** (`live.definition`, and `draft`) are only in a single app's reply. `draft` and
`unpublished_changes` are only for callers who may design or publish the app: anyone else
sees the live release alone. See [Drafts and releases](#drafts-and-releases).

`effect_targets` comes with `draft`, for the same callers, when the draft's [effects](#effects-of-actions)
name a space: one entry for each space (and board) they act on or open, so a page can say where
they go. `space`, `space_kind` and `board` are told only when the caller may read the space (in the
app's own workspace) and the board is one of its (not archived or gone); otherwise the entry has
the IDs alone, which the draft holds anyway, and nothing is told of what they are, the space's name
and kind included.

### The definition

```json
{
  "definition_version": 2,
  "requires": ["component.Button", "component.Column", …],
  "components": [{"id": "root", "component": "Column", "children": ["title", "items", "form"]}, …],
  "collection": {"fields": [
    {"name": "description", "type": "string", "required": true, "max_length": 200},
    {"name": "amount", "type": "number", "required": true, "min": 0}
  ]},
  "actions": {"add_expense": {"kind": "insert"}, "delete_expense": {"kind": "delete"}},
  "summaries": {"total": {"kind": "sum", "field": "amount"}}
}
```

The server checks every definition when it is saved, strictly, and refuses it (`400`,
saying what is wrong) unless:

- **definition_version** is the version of the definition's own format, TaskTo.AI's,
  independent of the A2UI protocol's version: `2` today (left out, it is the server's
  current). A higher one is refused (`app_definition_version`), and a client that finds a
  release in a version it doesn't read asks to be updated instead of drawing it. **Version 1**
  (one screen; `string`, `number`, `boolean` and `date` fields; `insert`, `update` and
  `delete`) is still read, checked and drawn exactly as it was; a definition that says `1` can't
  use anything of version 2 (`400`), which is everything in [Screens](#screens-and-navigation),
  [time and choice fields](#fields-and-rules) and [rules](#fields-and-rules).
- **requires** lists the catalog **capabilities** the screen needs: `component.<Name>` for
  each component it draws and `function.<name>` for each function it calls. The server works
  it out from the screen and replaces whatever was sent, so a definition can't claim less than
  it uses; the list is in each release, and a client whose catalog lacks one shows that it
  must update. A new component or function is a new capability, and a collection with an
  [attachment field](#photo-attachments) always requires `field.attachment`, and one with a
  [location, signature or barcode field](#location-signature-and-barcode-fields) `field.location`,
  `field.signature` or `field.barcode`, whatever its screens draw.

- **components** is the screen of an app with one, 1–300 A2UI components, one with the ID `root`
  (an app with several has `screens` instead, never both), each
  `{"id", "component", …props}` with only the catalog's components and props
  (`internal/apps/catalog.go`): A2UI's basic catalog without `Image`, `Video`,
  `AudioPlayer` and the `openUrl` function (an app is shared, and a picture from
  elsewhere would tell that server who opens it, and could carry the app's data in its
  address), plus `Stat` (a labelled value), `Show` (draws its child while a rule holds,
  [below](#fields-and-rules)), and `PhotoInput` and `Photos` (a photo input and the thumbnails of
  attachments, which upload to and draw from this server's own authenticated routes, never an
  outside address: [Photo attachments](#photo-attachments)), and `LocationInput`, `SignatureInput` and
  `BarcodeInput` ([below](#location-signature-and-barcode-fields)). `Screens`, the host of an app's screens, is the server's own and
  is refused in a definition. Props must have the catalog's types and enum
  values; dynamic values are a literal, a data binding `{"path": …}`, or a call of a
  catalog function returning the right type. Every component referred to must exist, no
  component may contain itself, and every button's event must be one of the app's
  actions. The web app's renderer runs with A2UI's strict validation too, against the
  same catalog.
- **collection.fields**, at most 50: `name` (a lowercase letter, then up to 39 lowercase
  letters, digits or `_`; not `id`, `items` or `draft`), `type` (`string`, `number`,
  `boolean`, `date`: `YYYY-MM-DD`, `time`: `HH:MM` on a 24-hour clock, `choice`, `attachment`: a
  list of photos, or `location`, `signature` and `barcode`, [below](#location-signature-and-barcode-fields)), `required`,
  `required_when` (a rule), `max_length` (strings: 1–10000, 1000 when left out), `min` and `max`
  (numbers), `options` (a choice's, 1–50 different texts of at most 100 characters), `option_labels` (a choice's:
  `{"<option>": "<label>"}`, each key one of the options and each label 1–100 characters, how a stored
  value reads to people; the data model below says how a screen shows it), `max_files`
  (an attachment field's: 1–10, 1 when left out) and `max_bytes` (each file's size: at most 16 MiB,
  8 MiB when left out; a signature field's: 1 MiB when left out), and `capture`, `format` and `editable`
  (a location's, a barcode's, and a location's or signature's). An attachment field is version 2, and no
  rule can read it; neither can a rule read a location or a signature.
- **actions**, 1–50, map an event name to its `kind`: `insert` adds a row from the
  event's context; `update` changes the fields the context gives of the row whose `id` it
  gives (blank text clears a field); `delete` removes the row whose `id` it gives; `navigate`
  opens a screen (`"screen"`). An insert, update or delete may say `"then"`, a screen to open
  once it has run.
- An insert or an update may have **effects**, at most 5 (see [Effects](#effects-of-actions)).
- **summaries**, at most 20: `{"kind": "count"}` or `{"kind": "sum", "field": …}` of a
  number field, shown as text; `"where"`, a rule, narrows either to the rows it holds of, and
  `"month_of"`, the name of a date field, to the rows dated in the current calendar month (UTC,
  by the server's clock; the device's own clock offline). Only a value that is a real date of the
  field counts (`2026-11-31`, or text stored before the field was retyped, is in no month). A
  definition that uses `month_of` requires the capability `summary.month_of`, so a client that
  predates it says it must be updated, instead of showing every month's total as this month's.
- **contributions** (optional, version 2): what the app offers the platform's [Home page and
  navigation](#home-widgets-and-pins), in versioned slots:
  `{"version": 1, "widgets": […], "pin": {…}}`. `version` is `1` (left out, it is). A widget is
  `{"id", "title", "kind", "size"?, "summaries"?, "screen"?}`: an `id` (like a field's name,
  unique among the app's widgets), a `title` of plain text (1–60 characters, one line, no space
  at either end), a `kind` of `summaries` (1–4 `{"summary": a summary's name, "label": plain
  text of 1–40 characters}`, each summary once), `screen` (`"screen"` names one of the app's
  screens that doesn't show a record, so the app needs `screens`) or `shortcut` (only a title
  that opens the app), and a `size` of `small` (the default), `medium` or `large`. At most 6
  widgets. The `pin` is `{"label": plain text of 1–30 characters, "screen"?: a screen that shows
  no record}`; the app is pinned under its name if it has none. Anything else is refused: an
  unknown slot or key (`400 invalid_json`), a version this server doesn't read, text with control
  characters or a line break, a reference to a summary or screen the app doesn't have, nothing at
  all (`contributions` that offers neither widgets nor a pin). `requires` gains the capability of
  each slot the definition fills, `slot.home_widgets.v1` and `slot.nav_pin.v1` (and, for a
  summaries widget, `component.Column` and `component.Stat`, which draw it), so a client that
  doesn't know a slot asks to be updated instead of drawing it wrongly. A contribution is only
  an offer, plain data: it names no page, address or component of its own, and grants no access.
- The whole definition is at most 48 KiB of JSON.

The data model an app's screen binds to: `/items` is the live rows, each with its `id`, its
revision as `_revision` and the collection's fields; `/draft` is an empty form; each summary is at `/<name>`.
Each item (and `/record`) also has `_labels`, `{"<field>": "<label>"}` for each choice field the row has a value
for: the field's `option_labels` entry for the stored option, or the stored text where it has none. A screen
that shows a choice binds to `_labels/<field>` (`/record/_labels/<field>` on a detail screen), because the field
itself holds the stored option (`"food"`, not `"Food"`); the label changes nothing that is stored, filtered,
sorted or compared by a rule. A definition whose screens bind to a label requires the capability `data.labels`, so
a client that predates labels says it must be updated; a release that only changes labels is not a change of the
collection (rolling back across it is not judged by the rows). The records explorer and a record's history show
labels the same way.
An app with attachment fields also has `/_files/<field>`, `{"max_files", "max_bytes"}`: the limits its
photo input and the signature pad show (a signature field has `max_files` 1).
An app with location, signature or barcode fields also has `/_fields/<field>`: `{"capture"}` (a location's: `"button"`
or `"submit"`), `{"format"}` (a barcode's, `""` for any), and for each `{"required", "editable"}`, which the inputs
follow.
An app with screens also has `/screen`, the name of the screen shown, and `/record`, the record a
detail screen shows.

#### Screens and navigation

An app has one screen (`components`) or several (`screens` and `entry`):

```json
"screens": [
  {"name": "tasks", "root": "tasks-root", "components": [ … ]},
  {"name": "detail", "root": "detail-root", "record": true, "components": [ … ]}
],
"entry": "tasks",
"actions": {"open_task": {"kind": "navigate", "screen": "detail"},
            "add_task": {"kind": "insert", "then": "tasks"}}
```

At most 12 screens, named like fields, and at most 300 components in all. Each screen has the ID of
its root component (`root` is the server's, for the host that draws the screen `/screen` names).
Every component's ID is unique across the screens, and a component refers only to those of its
own screen. `entry` is the screen the app opens on and can't be a `record` screen: one that shows a
record at `/record`, and is opened with the record's ID. A Button navigates by sending an event
that names a `navigate` action: the event's `context` is `{"id": …}` for a `record` screen and is
left out for any other (the server checks both when the definition is saved, and again on every
action). The answer to the action is two `updateDataModel` messages, `/screen` and `/record`
(the record as an item, read as the caller may read it; `422 app_row_not_found` for one they may
not, or that isn't there): **nothing else in the data model is touched**, so what was typed
into `/draft` is still there when the person comes back. Navigating changes no row, so it is
open to everyone who may open the app, a viewer included; a screen drawn from an older release
is navigated as that release defines it (a release the app doesn't have is answered as for any
action: `404 app_release_not_found` to those who may run actions, a refusal to the others).
Anything else an action answers, in an app with screens, replaces `/items` and the summaries
only (and `/draft`, for an insert); an update also sends `/record` again, a delete empties it,
and `then` opens its screen.

#### Fields and rules

A `time` is `HH:MM` (draw it with a `DateTimeInput` with `enableTime` and not `enableDate`); a
`choice` holds one of its `options` (draw it with a `ChoicePicker`, whose value is a list: one
choice is taken as the choice, none as no value, several are refused). A value outside the options
is `422 app_field_not_option`; a bad time `422 app_field_not_time`.

A **rule** is small, declarative data, never code:

```json
{"field": "category", "op": "eq", "value": "other"}
{"all": [rule, …]}      {"any": [rule, …]}
```

`op` is `eq`, `ne`, `lt`, `lte`, `gt` or `gte`, the last four for number, date and time fields;
the literal is of the field's type (a choice's is one of its options). A field with no value
equals nothing: only `ne` holds for it. Rules nest `all` and `any` at most 3 deep, hold at most
8 rules in a list and 16 parts in all, and are refused (`400`) if they name a field the collection
doesn't declare or a literal that doesn't fit. A field's `required_when` makes it required when
its rule holds of the row, and **the server enforces it on every insert and update**, whichever
release's screen sent it and whether it came from a person or the agent: a missing field is `422
app_field_required_when`. The `Show` component, `{"when": rule, "source": "draft" | "record",
"child": id}`, draws its child while the rule holds of the form (`/draft`, the default) or of
the record a screen shows; it hides an input and is never what limits a row.

An app holds at most 5,000 live rows and 4 MiB of their data (`409` beyond; deleted
records don't count, and restoring one needs room for it); a space at most 500 apps.

What is capped, as it stays: a launched app's **screen** is drawn from all of its live rows at
once (its items and summaries are computed in memory, so the 5,000 rows and 4 MiB are the
limit of what one screen holds), `GET …/records` and `…/records/deleted` answer the 1,000 most
recent, and `list_app_rows` reads every row a page at a time. The data explorer
(`GET …/records/query`) and `query_app_rows` are not bound by those: pages and totals are
computed in the database.

### `GET …/apps`

The space's apps, by name, without definitions: `{"apps": [ … ]}`. Offline apps are left
out unless `?archived=true`, after the others.

### `POST …/apps`

Writers. `{"name": "Expense tracker", "definition": { … }}` (a name of 1–100
characters). `201` with the app.

### `GET …/apps/{app}`

The app, with its definition.

### `PATCH …/apps/{app}`

Writers. With the `version`: `name` renames it and `definition` replaces the app's **draft**:
its screen, collection, actions and summaries (these take the design capability), `archived`
takes it offline (`true`) or restores it (`false`; the publish capability). A new definition is
checked against the catalog, not against the rows, and changes nothing people use: the app
keeps serving its live release until the draft is published. Fields no longer declared stay in
the rows, unseen, and come back if declared again. `200` with the app.

### `POST …/apps/{app}/move`

Writers of both spaces, with the design capability in the app. Its role assignments stay
behind: they were set for the old space's members, so the moved app is unrestricted until
roles are set in the new space. `{"version": 1, "space_id": 7}`: moves the app, with its rows,
to another space of the workspace the caller may write (`404` for one they can't read or
of another workspace, `403` for one they may only read). `200` with the app. The app's
[attachments](#photo-attachments) go with it: their files are copied to the new space's directory
after the move, and are read from wherever they are meanwhile.

### Templates and copies

Starting an app from a template, and copying an app, make a **new, independent app** in a space
the caller may write, whose **draft** is a copy of a definition. Nothing else is copied: not a
record, an attachment, a record's history, an effect's queued or finished work, a release, a Home
placement or default, a role assignment of the source, a membership or a secret (a definition
holds none). The new app is a draft until someone with the publish capability publishes it, the
ordinary way (so the audience panel is shown first), and installing the same template twice makes
two apps that share nothing. [docs/templates.md](templates.md) describes the three that come
with the server.

An effect whose board is not chosen yet (`space_id` and `board_id` both absent on a `create_card`,
a `create_document` or a board or document `open_route`) is allowed in a draft, shown by the
app's `needs_board: true` (told to callers who may design or publish), and refused when
publishing (`400 app_effect_target_unset`). Half a target (a space with no board) is refused as
always.

#### `GET /api/workspaces/{workspace}/app-templates`

Members. `{"templates": [{"id", "version", "name", "description", "recommended_space":
"private"|"shared", "needs_board", "captures": ["attachment"|"location"|"signature"|"barcode"],
"roles"?: {"installer": […], "members"?: […]}, "widgets": ["…"]}]}`. The templates are the
server's own and the same for everyone; `captures` are the kinds of field that use the phone's
camera, position, a signature pad or a scanner.

#### `POST /api/workspaces/{workspace}/spaces/{space}/app-templates/{template}/install`

Writers of the space. `{"name"?, "board"?: {"space_id", "board_id"}}`: installs the template as a
new app in the space (`201` with the app, a draft). `board` is required when the template
`needs_board` and refused (`400 app_template_board`) when it doesn't: it must be a board the
caller may write, in a space of the same workspace (`400 app_effect_target` otherwise, and
nothing is made). `404` for an unknown template, for a space the caller can't read or of another
workspace; `403` for one they may only read.

If the template has `roles` and the space is shared, the app is made with its first role
assignments in the same transaction: the installer holds `roles.installer`, and every other member
without roles of their own `roles.members`, so the app is restricted from its first moment (for
the equipment checkout: members read and change their own records, the installer reads all). In a
private space there is nothing to restrict and no roles are made.

#### `POST …/apps/{app}/duplicate`

Anyone who may view the app (a space reader included), copying into a space they write.
`{"name"?, "space_id"?, "source"?: "live"|"draft"}`: `space_id` is the copy's space (the app's own
if left out; `404` for one they can't read or of another workspace, `403` for one they may only
read); `source` is the definition copied: the live release (`409 app_not_published` if there is
none) or the draft, which only callers who may design or publish can read (`403 app_forbidden` for
the others). Left out, it is the draft for those and the live release for everyone else. `201` with
the new app, with no records and unrestricted: the copy can't carry the source's data or its grants
(the space's own rules decide), and its creator gets the roles the creator of any new app gets. An
effect aimed at a space of another workspace is always cleared, whatever the copier may write. An effect whose target the copier can't
write (or read, for a route) has its target cleared and must be chosen again before publishing
(`needs_board`); the ones they can use are kept. An app the caller holds no role in is `404`, as
everywhere.

#### `POST …/apps/{app}/effect-board`

Callers who may design the app. `{"version", "space_id", "board_id"}`: gives every effect of the
draft that has no board chosen the board, which the caller must be able to write (`400
app_effect_target`; `400` if no effect waits for one). `200` with the app, as a change of the
draft is (`409 app_changed` with the current app if it moved).

### `GET …/apps/{app}/surface`

Launches the app: the A2UI messages that draw it with its rows, `createSurface`,
`updateComponents` and `updateDataModel`, for the surface `app-<id>` and the catalog
`https://taskto.ai/catalogs/apps/v1.json`. `409` while it is offline, and `409
app_not_published` before its first release. It draws the **live release**; `?release=draft`
draws the draft, and `?release=<number>` that release (`404 app_release_not_found`), both
only for those who may design or publish the app (`403 app_forbidden`, `design`) unless the
number is the live one. The draft is drawn with the app's real rows, to look at: it is for
previews, and its actions are not run (to try a draft use [`…/preview`](#post-appsapppreview), which runs on sample rows instead). The data model has the rows the caller may read, and the
summaries over those rows alone. A client names the release it drew in its actions: it knows
it from `live.number` of the app it read, which it passes as `?release=`.

`?screen=<name>` opens an app with screens on that screen instead of its entry (`400` unless it
is one of its screens that doesn't show a record): what a pin that names a screen does.
`?widget=<id>` draws one of the **live release's** [Home widgets](#home-widgets-and-pins)
instead: a summaries widget is a column of `Stat` components over its summaries (the data model
has only those, counted over the rows the caller reads); a screen widget is the app's own screens
opened on the widget's, with at most 25 rows (the latest) in `/items`, its summaries counting
every row the caller reads; a shortcut draws nothing. It is the app's surface, so it is refused
as it is (`404` for a caller who may not view the app, `409` while it is offline or unpublished),
and `404 home_widget_not_found` for a widget the release doesn't offer. `widget` can't be
combined with `release` or `screen` (`400`).

### `GET …/apps/{app}/offline`

Readers of the app. The app's **live release and the records the caller may read**, for the web
app to keep on the device for [offline use](offline.md). Nothing is stored by the server: it is a
snapshot, and the writes made against it come back as ordinary
[actions](#post-appsappactions) under their `operation` keys.

```json
{
  "app_id": 9, "space_id": 4, "name": "Expenses", "release": 3,
  "definition": {
    "definition_version": 2, "requires": ["component.Column", …],
    "collection": {"fields": [ … ]}, "entry": "…", "screens": [{"name": "…", "record": true}],
    "actions": {"add_expense": {"kind": "insert", "then": "…"}},
    "summaries": {"total": {"kind": "sum", "field": "amount"}}
  },
  "messages": [ … ],
  "scope": "all", "records": 120, "total": 120, "truncated": false,
  "can_insert": true, "captured_at": "…",
  "operations": {"off-6b0c…": 118},
  "modifiable": [101, 118]
}
```

- `messages` are the A2UI messages [`…/surface`](#get-appsappsurface) sends for the live release:
  they draw the app with the downloaded records, and its summaries count **those records only**.
- `definition` is what a client needs to use the app without the server: what it requires of a
  client (a client that lacks a capability shows an update-required state instead of the app),
  the collection, and each action's kind, screens and `then`. It holds none of an action's
  `effects` (their targets and templates are the server's), and no component.
- `scope`: `all` records, or only the caller's `own` (from their roles in the app). The records
  are those of every screen's own filter: none deleted, and each as the screen shows it, with no
  field the collection doesn't declare and none of another type.
- At most 2000 records, the latest: `records` is how many were sent, `total` how many the caller
  reads, `truncated` whether they differ.
- `can_insert`: the caller may add records now (the submit capability, in a writable space).
- `modifiable`: which of the downloaded records the caller may **change or delete** now: every
  one when their role's modify scope is all, the ones they created when it is their own, none
  (`[]`) when it is none or the space can't be written. A client offers an offline edit or
  deletion only for these; it decides nothing, since the send is checked again (below).
- `operations`: which of the downloaded records **the caller's own operations** made: the
  operation key (see [Replaying an offline insert](#get-appsappoffline)) to the record's id. It
  names only the caller's operations and only records the download holds. A device that sent a
  record and downloads again knows by it that its entry for the record and the downloaded record
  are one.

`403 offline_downloads_disabled` while an admin has [turned downloads
off](#patch-apiworkspacesworkspacespacesspace) for the space; `404` for a caller who may not view
the app; `409 app_offline` while it is offline and `409 app_not_published` before its first
release. A client that holds a download and is refused with `403 offline_downloads_disabled` or
`404` removes its copy, and one whose copy holds more records than the caller's `read_records`
now says (`GET …/apps/{app}`) downloads it again.

**Replaying an offline insert.** The client sends the action it queued to
[`…/actions`](#post-appsappactions) with `release` set to the release its screen was drawn from
and a stable `operation`. The server records the operation's receipt in the same transaction as
the record, so sending it again (the answer was lost, the page was reopened) changes nothing and
is answered `200` with the rows the caller reads **as they are now**, not with the first answer's
body (the receipt remembers the request and the record it made, not what was answered; a record
deleted since is simply not among the rows); another request under the same key is `409
app_operation_reused`. A request that **committed** is answered with its receipt even when the
caller's access has changed since (a role narrowed or removed, a space archived or made read-only),
or the app has been archived since: where the access checks would now refuse it (`403`, `404`), or
the app is offline (`409 app_offline`), a request that names the `release` and
repeats the same `operation`, action and context gets `200` with an empty list of messages, never the
rows, so a device that lost the answer learns that the change is in. The operation is the caller's own
(it is looked up by user, app and the space of the route: a receipt answers only on the route of the
space and app it was made in, never through another space the caller can still reach that names the
same app id), and a request that was not made is refused as it ever was (an archived app still
refuses a new operation with `409 app_offline`).

**Replaying an offline edit or deletion.** The client sends the `update` or `delete` action it
queued to [`…/actions`](#post-appsappactions) like an insert (the `release` its screen was drawn
from, a stable `operation`), and with `expectedRevision` always set: **the revision of the record
in the download** (the item's `_revision`), which is what makes a change made on a stale copy a
conflict rather than an overwrite. What the answers mean to the client:

| Answer | Meaning |
|---|---|
| `200` | made, or made before under the same `operation` (then the rows as they are now; the revision check is not repeated for a request already made) |
| `409 app_record_conflict` | the record has another revision. Nothing changed, no receipt was kept for the `operation`, and the body carries `current`: the record as it is now **only if the caller may read it** (as the screen's item, with its `_revision`) |
| `422 app_row_not_found` | the record isn't there to change: it was deleted (a change never brings a record back), or it isn't the caller's to change. The answer shows nothing of it |
| `422` for a field (`app_field_required`, `app_field_not_option`, `app_field_not_number`, `app_field_files`, …) or `400 bad_request` for a field the app no longer declares | the change doesn't fit the live release |
| `409 app_release_outdated` | the release it was drawn from can't be taken by the live one (see [actions](#post-appsappactions)) |
| `403` / `404` | the right to change records, or the app, is gone |

A record deleted meanwhile is brought back, if the person chooses, by
[`POST …/records/{record}/restore`](#records-revisions-history-and-deleting) with the deletion's
`expected_revision` (listed by `GET …/records/deleted`): the change is then sent on the revision
the restore returns. A restore sent again after it was made is a `409 app_record_conflict` whose
`current` is the live record, which the client takes to be its own restore when the revision is
the very next one.

**For one account.** A request that is sent later than it was made, from a browser whose session
may have changed hands, says whose it is: the header `X-Taskto-Expected-User` holds the id of the
account. On any route (an app's replay, uploads and checks, the space's metadata), if the session is
another account's the answer is `409 session_account_changed` before anything else is looked at (a
space the session can't open is not a `404` then): nothing is read or written as the wrong
account, and the client pauses the queue rather than count a failure. The check is made for
**every `/api` route** that carries the header, not only an app's (the offline client names the
account on its downloads, on the space and app checks that revalidate them, and on the queue): the
answer is the same `409` before the route's own checks. A request without the header
is as it always was; a value that is no account's id is `400`. An insert that names a photo (an attachment id) is accepted only when the
photo's upload has been finalized (`422 app_attachment_bad` otherwise), which is why the client
uploads a queued insert's photos, with the resumable [upload routes](#photo-attachments), before
it sends the insert.

### `POST …/apps/{app}/actions`

Writers, and by the action's kind, the submit capability (`insert`) or the right to modify
the row (`update` and `delete`, of all records or only the caller's own; else
`403 app_forbidden`); a `navigate` action is for anyone who may open the app. The A2UI client action a button sent:
`{"name": "add_expense", "surfaceId": "app-9", "sourceComponentId": "add", "timestamp": "…", "context": {…}}`,
with, for an update or a delete, the optional `"expectedRevision"` (see
[Records](#records-revisions-history-and-deleting); `422` if it isn't a revision, or the
action inserts, `409` `app_record_conflict` if the record has another one). A delete marks
the record deleted: it can be restored.

With an [access token](#access-tokens) the action runs as the token's user, the changes are marked
with the token's name, and the user's tokens together (with `run_app_action` over MCP) have a
**budget of 60 actions a minute** in a workspace, with a burst of 60: over it the answer is
`429 app_token_rate_limited` with `Retry-After`, in seconds, and nothing is done. A session is never
limited. A token's `operation` keys are in the same scope as the browser's: **the key belongs to the
user and the app, not to the credential**, so a token and a session (or two tokens) of one user who
send the same key meet. A client should make a random key for each change (8 to 64 letters, digits,
`-`, `_`) and never reuse one for another: the same key with the same request replays it, and the
same key with a different request is `409 app_operation_reused`. The effects such an action sets going
(a card, a document) are done by the server's worker, as the user and not through the token: a
document's commit says it was made by "TaskTo app" for the user, whichever credential asked (the
commit is found again by that name after a crash, so it doesn't carry the token's name), while a
card made by an effect is marked with the token's name, like the record.

The server checks it against the app (`422` for an event the app doesn't declare,
another surface, a context field the collection doesn't have, a value that doesn't fit
its field, or a row the app doesn't have), applies it, and answers with the
`updateDataModel` message to apply: the rows the caller may read, an empty `/draft`, and the
summaries over them (for an app with screens, see [Screens and navigation](#screens-and-navigation):
the data model isn't replaced, and a navigate action is open to readers).
Form inputs arrive as text, so number and boolean fields accept their text too. `409`
while the app is offline, and `409 app_not_published` before its first release.

**The release an action comes from.** The action may carry `"release": <number>`, the release
its screen was drawn from (`…/surface?release=`); left out, it is the live one. From the live
release the action runs as the live definition says. From another release (the app was
published or rolled back since the screen was drawn) it runs as *that release* defines it,
and is **applied only if the live release can take it as it is**: the live release declares
the same action (same name, kind and screens it opens; what it sets going besides the change, its
`effects`, is the live release's whatever the older one said, so a release that only changed those
refuses nothing), every field the action sets is a field the live collection
declares with the same type, and an insert gives every field the live collection requires. Then
the live collection checks **the row the action would store** as always, an insert's and an
update's alike, whichever release's screen it came from: a required field missing, or a value
outside the live limits, is the usual error of that field (`app_field_required`,
`app_field_too_long`, `app_field_too_small`, `app_field_too_large`, `app_field_required_when`,
`app_field_not_option`…) and nothing is stored.
Otherwise nothing changes and the answer is `409 app_release_outdated` (`release`, `live`), which
tells the client to reload the app. A release the app doesn't have is `404 app_release_not_found`,
but only for a caller who may run actions at all (the right to add or to change records); the
others get `403 app_forbidden` whether the release exists or not, so naming one tells them
nothing. The data model that answers is drawn by the release the action came from, **limited to
the fields the live release declares** (as it declares them), and its summaries to the ones that
read such fields: nothing the live app leaves unseen, such as a field it removed, is returned. A
value stored with another type than its field's current one is left out of the data model (and
stays stored).

An action may be sent with `?widget=<id>` (a widget's actions): it is the same action with the
same checks, answered with at most 25 rows in `/items`.

### Effects of actions

An insert or an update may declare **effects**: what the change sets going on boards and documents.
The kinds are a fixed registry, strictly checked when the definition is saved; a key that isn't one
of a kind's (a URL, a tool name, a command), an unknown `kind` or a hole in a template that isn't a
field refuses the definition (`400`). A definition with effects needs `definition_version` 2, and
its release needs the capability `effect.<kind>` of each kind it uses (`requires`).

```json
"actions": {
  "submit_inspection": {"kind": "insert", "effects": [
    {"kind": "create_card", "name": "maintenance", "space_id": 7, "board_id": 12, "column": "todo",
     "when": {"field": "result", "op": "eq", "value": "fail"},
     "title": "Fix {{site}} (inspection {{id}})", "body": "Notes: {{notes}}"},
    {"kind": "create_document", "space_id": 7, "path": "inspections/{{id}}.md", "content": "Site: {{site}}\n"},
    {"kind": "open_route", "route": "card", "card": "maintenance"}]},
  "resolve": {"kind": "update", "effects": [{"kind": "update_card", "card": "maintenance", "column": "done"}]}
}
```

| `kind` | Takes | Does |
|---|---|---|
| `create_card` | `space_id`, `board_id`, `title`, `column`?, `body`?, `docs`?, `name`? | a card on the board; `name` lets other effects refer to the card made for the record |
| `update_card` | `card`, `column` | moves the card the `create_card` effect called `card` made for the record |
| `create_document` | `space_id`, `path`, `content`? | a Markdown document, never over another |
| `open_route` | `route`: `board` (`space_id`, `board_id`), `card` (`card`) or `document` (`space_id`, `path`) | a place to open in the web app, as a kind and IDs; never an address |

Every kind may have `when`, a rule ([Fields and rules](#fields-and-rules)) over the record as the change leaves it: the
effect applies only then. `title`, `body`, `content`, `path` and `docs` are **templates**: text
with `{{field}}` holes (a field of the collection, or `id`, the record's number) and nothing else:
no expressions, no functions, and what fills a hole is never read as a template again. A title is
put on one line (200 characters), and what fills a hole in a path is made a safe file name, so a
value can't climb out of the folder.

**What a definition may not do.** It gives no authority. Saving, publishing or rolling back to an app
whose effects target a space or board the caller can't use is refused (`400 app_effect_target`; the
same answer for a space they can't read). Each target is checked for each permission an effect needs
of it (a route that only reads a board doesn't stand in for a card made on it, which writes), as of
the moment of the write, with the write lock held. Every effect runs as the user who acted, with
their access **when it runs**.

**The outbox.** When the action changes a record, the change and one entry per effect that applies
are committed **in the same transaction**. The request answers then; the server's worker does the
entries afterwards, so they go on when nobody has the app open, and a crash loses none. An action
may carry an `operation`, its **idempotency key** (8 to 64 letters, digits, `-`, `_`), which a
client makes once for a press of a button and sends again with every retry of it (the web app keeps
one key for each submission whose outcome it doesn't know, whatever time the renderer stamped on each
press, across a refresh or a reopened screen, and makes a new one only after a definite answer: a
success, or a refusal with a code of its own (`4xx`); a dropped connection, a timeout, a `5xx` or a
gateway's reply leaves the key to be sent again): a request made again under the same operation (by the
same user, for the same app) changes nothing, queues nothing and is answered as the first was; one
made for a different request is `409 app_operation_reused`. A request already made is answered as it
was even if the live release has changed since (a release that no longer takes the older screen's
action doesn't turn the retry of a committed request into `409 app_release_outdated`: that is asked
only of a request not yet made). What makes a request "the same" is the
release it runs from (the `release` it names, or the live one), the action, its context and its
`expectedRevision`: the same key and body run by another release is a different request. An action with effects and no `operation` is given one by the server
(an update made against an `expectedRevision` can't be applied twice anyway). Entries are queued once and
done at most once to effect: a card carries its entry's key, and an entry's number is never given
again (even after the cleanup below), so doing the entry again after a crash between making the card
and noting it finds the card, and no later entry can find an earlier one's; a document's commit names its entry in its message, so an entry found again after a crash between the
commit and the note that it was done is done at once, **whatever became of the document since** (edited,
moved, deleted: nothing is written again); a document that is at the path with the very content the
entry writes is the entry's own too, a different one is never overwritten (`app_effect_doc_exists`); moving a card to the column it is in is nothing to do. An update that left
the record as it was queues nothing. An action from an older release runs the **live** release's effects.

**The record's card.** The card a `create_card` effect (by its `name`) made for a record is kept in a
link of its own, made in the same transaction as the card, which the cleanup of entries never
removes: `update_card` and a route to the card find it there, for as long as the record is. A card
made later for the same name replaces the link; an entry done again late never takes it back.

**Order.** The `update_card` entries of a card are ordered by their number (the order they were
queued in). A move is made in one transaction with a note on the link of which entry made it, and
only if no later entry's move was noted before: an entry that is done again late (its worker died
before noting it done, and its lease ran out) never undoes what a newer one did; it ends as done and
moves nothing.

**What happens meanwhile.** Queued entries keep what they were queued with: publishing a new release
or rolling back neither cancels nor changes them (what they make was filled in from the record when the
action ran), but what people are shown of them follows the live release (see the listing). Deleting a
record doesn't remove its entries: the ones that haven't run **fail** with `app_effect_record_gone`
(nothing is made for a deleted record), and the ones that already ran stay as they are. Restoring a
record queues nothing and retries nothing; whoever acted retries the failed entries on purpose, as for
any failure. Entries are cleaned up by their own last activity (queued, tried or retried): finished
ones 30 days after it, any 90 days after it, so a retry made today is not erased because its request
is old; an operation goes when none of its entries is left and 30 days passed since the request.

Before an entry runs the worker checks, as the user who acted and as of now, that the app is there and
online and they may still open it, that they hold the right the action needed (submit for an insert;
for an update, to change **and** to read the record the entry came from, in the scope their roles give
them now: their own records or all), that the record is still there, and that they may write the
target; if not, the entry fails at once with `app_effect_app_access`, `app_effect_app_offline`,
`app_effect_record_gone` or `app_effect_target_access` and does nothing. The checks are repeated as
close to the write as the data allows: inside the transaction that makes or moves a card (which holds
the write lock), and for a document **while the space's write lock is held**, right before the commit,
so a right withdrawn while the entry waited for the lock stops it. Other
failures with a code of their own (`app_effect_target_gone`, `app_effect_board_offline`,
`app_effect_invalid`, `app_effect_quota`, `app_effect_doc_exists`, `app_effect_no_card`, `app_effect_card_archived`,
`app_effect_app_offline`) fail at once too; a failure that may pass (the database was busy, a card
being changed) is tried again after 5 s, 10 s, 20 s…, at most 5 tries in all (a worker that died on
the last try doesn't give an entry another: its lease running out fails it), and then is failed
as `app_effect_gave_up`. A failed entry stays until its user retries it.

An **effect** as the routes list it:

```json
{"id": 31, "operation": "k3j9x0vq2m", "position": 0, "kind": "create_card", "name": "maintenance",
 "action": "submit_inspection", "record": 14, "record_revision": 1,
 "status": "done", "attempts": 1, "next_attempt_at": null, "error": null,
 "detail": {"title": "Fix East gate (inspection 14)", "column": "todo"},
 "result": {"workspace_id": 1, "space_id": 7, "board_id": 12, "card_id": 88, "title": "Fix East gate (inspection 14)"},
 "created_at": "…", "updated_at": "…", "done_at": "…", "can_retry": false}
```

`status` is `pending`, `running`, `done`, `failed` or, only in a preview, `simulated`. `error` is
`{"code", "params"}` for a failed entry (and for a pending one that failed before and waits for
its next try; `next_attempt_at` is then when). `result` is what a done entry made: a card's IDs, a
document's `space_id` and `path`, a route as `{"kind", "workspace_id", "space_id", "board_id"?, "card_id"?, "path"?}`
for the web app to turn into its own address, in the workspace the space is in (which may not be the
app's); it is present only if the caller may read the space it points into.

What an entry says of its target is for those who may read the target **now**, exactly as `result` is:
for a caller who can't read the space an effect targets, `detail` keeps only the kind of route and
`error.params` is left out (the status and `error.code` stay). And what the templates made of the
record's fields (`detail.title`, `detail.path`, a result's `title` and `path`, and the `path` of an
error's `params`) is shown only while the live release still declares every field the entry was filled
from, with the type it had: after a release removes or retypes one, those are left out.

#### `GET …/apps/{app}/effects`

Readers of the app: the effects of the records the caller may read (all, or only their own: the
same filter as the records), newest first, `?status=` (`pending`, `running`, `done`, `failed`, or
`active` for the two that wait), `?limit=` (1–200, 100), `?before=` (an effect's ID: only older ones,
to read on past a page): `{"effects": […], "more": false, "active": 2, "failed": 1}`, where `more`
says older effects match too, and `active` and `failed` count everything the caller may see that waits
or failed, whatever the page. A record the caller may not read has no effects for them.

#### `GET …/apps/{app}/records/{record}/effects`

The same for one record.

#### `POST …/apps/{app}/effects/{effect}/retry`

Writers; body `{}`. Puts a failed effect back to be done, with its tries counted afresh, and answers
it. Each time the worker takes an entry it is given a new claim, which only the worker holding it can
complete or fail, so one slow after its lease ran out can't end a retry made since. Only the **user who acted** may retry it, and only while they hold the right the action needed
(`403 app_forbidden`, `capability` `retry_effect`, `submit` or `modify_records`; an update needs the
right to change that record, in the scope their roles give now); `409 app_effect_not_failed` for one
that didn't fail; `409 app_effects_backlog` when the app already has 500 effects waiting (a retry is
waiting work like any other); `404` for an effect of a record the caller can't read. The worker checks access again when it runs, so a retry never widens what anyone may do.
A session only: an effect read with an [access token](#access-tokens) says `"can_retry": false`,
since the route is not one a token reaches.

#### `POST …/apps/{app}/actions/preview`

Those who may design or publish the app (`403 app_forbidden` otherwise). The same body as an action;
it is worked out against the **draft** and **does nothing**: no record changes, no operation is
kept, no entry is queued, no card or document is made. It answers `{"effects": […]}`, the effects
that would apply, with `status: "simulated"`, what they would make in `detail` (a new record's `id`
is `new`), and an `error` with the code an effect would fail with if the caller couldn't write its
target.

Like the preview below, it never reads the app's records: the action is tried on the `rows` of the
body (the preview's own, as below) or, without them, on sample rows made from the draft's
collection. A field that holds a photo or a signature is never required there.

### `POST …/apps/{app}/preview`

Tries the draft on **sample rows, not the app's data**. For those who may design or publish the app
(`403 app_forbidden`, `design`, for anyone else; `404` for an app the caller can't read, or of another
space or workspace), in a space the caller may read; `409` while the app is offline. The draft is
drawn or used with rows that the caller keeps and sends back, and the server **reads and writes none
of the app's rows**: no row is stored, changed or deleted, no revision or history is made, and
nothing an action could do outside the rows happens, whatever the action is. It changes no state at
all, which is why it is a `POST`: the sample rows travel in the body.

`{}` starts a preview: the answer's `rows` are a few sample rows made from the draft's collection
(plausible values of every field, the same each time). `{"rows": [{"id": 1, "revision": 1, "data": {…}}, …],
"action": {…}}` goes on from those rows: `action` is the A2UI client action a button sent, in the
form of [`…/actions`](#post-appsappactions), and runs as the **draft** defines it, on the rows
sent. Without `action` the rows are only drawn. A preview holds at most 100 rows (`409
app_preview_full`, which names the preview's limit, not a live app's), and its request at most 8 MiB, since it carries every row
(`413 app_preview_too_large`, unlike the 64 KiB of a definition's body); rows must each have an `id` and a `revision` from 1, an `id` of their own, and fit the
draft's collection (`422`; a client starts the preview again when the draft changed). A preview holds no photos: the values
of an `attachment` field, in the rows sent or in an action, are dropped, so a preview never reads or
returns an attachment of the app, and sample rows have none. An action the
draft doesn't declare, a value that doesn't fit its field, or a row the sample rows don't have is
`422`, as in a live app; a stale `expectedRevision` is `409 app_record_conflict`.

`200` with `{"messages": […], "rows": […], "effects"?: […]}`: the A2UI messages that draw the screen (`createSurface`,
`updateComponents`, `updateDataModel` for a start, as `…/surface` answers) or update it (as `…/actions`
does), and the rows as the action left them, to send with the next step. For an insert or an update
with effects, `effects` are the ones that would apply to the record, simulated on those rows (as
[`…/actions/preview`](#post-appsappactionspreview) answers; nothing is made). In a preview a
**signature** or a **photo** is never required, since none is held, so an action whose record would
carry one is not refused for the lack of it. The answer is the same
for the draft of an app that was never published, which is how it is tried first. The web
app's builder and its Apps page use this route for every draft they show, and label it a **Preview**
with sample data, apart from the **Live** release.

### Photo attachments

A record never holds a file: an `attachment` field holds a **list of attachment ids** (32 hexadecimal
characters, from the system's random source), and the files are separate resources the server keeps
in the managed storage of the space that holds the app, under its data directory
(`.taskto/app-files/<app>/<id>`, with `<id>.thumb` beside it; the `.taskto` directory is the server's own, which no document name can lead into), never in the database, which keeps only their
metadata. A photo is never an address: the catalog still has no `Image`, and nothing in a record can
make a browser or the server fetch from outside. The `PhotoInput` component uploads and the `Photos`
component draws thumbnails, both through the routes below with the viewer's own session. A browser
that takes a photo with its camera sends it to the app; it may not also keep it in the device's
camera roll, and nothing here promises that.

An attachment field takes at most `max_files` pictures of at most `max_bytes` each. The accepted types
are **JPEG, PNG and GIF**, found in the bytes themselves (never from the client's `Content-Type` or the
file's name, and `svg`, `html` and the rest are refused), with a bound on the pixels (50 megapixels for a
JPEG, 24 for the others), so a file can't take more memory to read than that. A photo is served to every
reader of the record that holds it, so **the metadata inside it is removed when it is finalized**: a phone's
location, the camera, the time and the rest (Exif and XMP, text and time chunks, comments), losslessly, without
decoding or re-encoding, so the pixels are exactly the ones sent. A JPEG keeps its JFIF header, its colour
profile and Adobe's colour transform, and, if the photo isn't upright, a minimal Exif segment holding its
orientation alone, so a browser still shows it right. A PNG keeps every chunk but `eXIf`, `tEXt`, `zTXt`,
`iTXt` and `tIME`; a GIF every block but comments and application extensions other than its loop control.
A file whose structure can't be walked with certainty is refused (`400 app_attachment_type`), not guessed at.
What is kept is therefore a little smaller than what was sent, and its `size` is the stored one.

An attachment is made in steps, by a writer of the space whose roles in the app let them submit records
or change some (otherwise `403 app_forbidden`; a viewer of the space is `403` as for any write). All the routes are under `…/apps/{app}`, need a session, and go through the
[CSRF guard](#csrf). Every one of an upload's routes belongs to its **uploader**: anyone else, in the
space or not, gets `404`, as for an upload that doesn't exist.

#### `POST …/apps/{app}/uploads`

`{"field": "photos", "size": 2345678}`: starts an upload for an attachment field of the live release
(`409 app_not_published` before one), declaring the file's size in bytes (`400 app_upload_size` if it is 0
or over the field's `max_bytes`). `201` with `{"id", "field", "status": "uploading", "size", "received": 0,
"attached": false, "created_at", "chunk_size": 1048576, "max_chunk": 2097152}`. A person may have 20
photos waiting to be saved in a record per app, and an app's attachments take at most 1 GiB in all
(`409 app_uploads_pending`, `app_files_full`).

#### `GET …/apps/{app}/uploads/{upload}`

Where the server is: the same object, with `received`, the offset to continue from. For the uploader only.

#### `PUT …/apps/{app}/uploads/{upload}?offset=N`

One chunk: the body is the bytes, up to 2 MiB, as `application/octet-stream`. `offset` must be the
server's `received`: the chunk is written at that offset and the answer is the object with the new
`received`. At any other offset it is refused, `409 app_upload_offset` with `params.offset` the bytes the
server has, and nothing is written. So a chunk that was applied but whose answer was lost, sent again, is
never applied twice: the client asks where the server is (or reads it from the `409`) and continues from
there. An upload interrupted by anything, the server's restart included, resumes the same way, and the
finished file is exactly the bytes sent, once. The body is read whole before anything is written, so a
connection that drops half way changes nothing; `413 app_upload_chunk` for a chunk that is too large or runs
past the declared size; `409 app_upload_state` once the upload is finalized.

#### `POST …/apps/{app}/uploads/{upload}/finalize`

`{}`. Checks that all the bytes are there (`409 app_upload_offset` with where the server is, if not), that
they are a picture of an accepted type within the pixel bound, and makes a thumbnail (a JPEG of at most
320 pixels on a side, turned the way the picture's orientation says) with the standard library's image
packages, and keeps the picture without its metadata (see above). A file that isn't an accepted picture is
removed and answers `400 app_attachment_type`. `200` with the attachment: `"status": "ready"`,
`content_type`, `width` and `height` as shown, and `size` and `received` now the **stored** size, the length the
picture is served with (while it is uploading they are the declared size and what the server has). The
received bytes are removed only after the attachment is saved as ready, so a finalize that was interrupted
starts again from them, and the timer removes any that were left. Finalizing a ready attachment answers it
again, so a retry is safe.

#### `DELETE …/apps/{app}/uploads/{upload}`

Removes an upload, or a ready photo, that no record holds yet. `204`; `409 app_upload_state` if a record holds it.

#### Putting photos in records

An insert or update names attachments in the field's value, a list of their ids (`{"photos": ["…", "…"]}` in
the action's context, as a `PhotoInput` bound to `/draft/photos` sends). The server attaches them in the same
transaction that writes the record, and **the whole change is refused, writing nothing, with `422
app_attachment_bad`** unless each id that isn't already in that record's field is an attachment of this app
and this field, uploaded by the caller, finalized, and in no record yet. More than `max_files`, a repeat, or
anything that isn't an id is `app_field_files`. Every way that writes records goes through these checks,
actions of an older release (whose screens may send anything) and the live release alike. An attachment is
attached to one record, for good: replacing a photo in a record's list, deleting the record, and restoring it
all keep the files, since the record's history refers to them.

#### `GET …/apps/{app}/attachments/{attachment}` and `…/thumbnail`

The picture, or its thumbnail, for whoever may **read the record it is in**: with the read scope of their
role in the app, all records or only the ones they created ([App roles](#app-roles)), and **not for a
deleted record** (nobody reads its attachments until it is restored). Before it is in a record only its
uploader reads it, to see what they are about to submit. Whatever else, and for an attachment that doesn't
exist, is the same `404`, with the same body. The answer has the type found in the bytes (`image/jpeg`,
`image/png`, `image/gif`; the thumbnail is `image/jpeg`), `X-Content-Type-Options: nosniff`,
`Content-Disposition: inline` with a file name made from the id (never a name from a person),
`Content-Security-Policy: default-src 'none'; sandbox`, `Cross-Origin-Resource-Policy: same-origin`, and
`Cache-Control: private, no-cache` with an `ETag`, so a shared cache keeps nothing and a browser asks the
server (which checks access again) each time. `Range` and `If-None-Match` work.

#### What lasts, and what is cleaned up

- An attachment in no record **a day after its last change** (an abandoned upload, a photo taken and never
  submitted) is removed with its files. This happens as an app's uploads start and on a timer
  (`apps.Service.Maintain`, every ten minutes), which also removes a file no attachment of its space
  knows (what a failed removal left) and the files of an app that no longer exists. A record's attachments
  are never touched.
- **Moving an app** to another space moves its files: each attachment names the space whose directory
  holds its bytes, reads follow it, and after the move the server copies the files over and updates it,
  which the timer retries if it was interrupted. The photos stay readable at every step, to the readers of
  the app in its new space.
- No route deletes an app or a space. When one does, the rows go with it (`ON DELETE CASCADE`) and the
  sweep removes the files of an app whose row is gone, and a space's directory takes the rest with it.
- The agent sees an attachment field's ids and metadata (`content_type`, `size`, `width`, `height`) in
  `list_app_rows`, never the bytes; it has no tool to upload or attach a photo.

### Location, signature and barcode fields

Three field types, definition version 2, for what a phone is good for in the field: where it is, a
signature, a scanned code. Each needs a client that holds it (`field.location`, `field.signature`,
`field.barcode`, always in the release's `requires`), and each has an input component that takes a
`field` (the collection field it is for, which must be of its type), an optional `label` and a `value`
(a data binding, such as `/draft/<field>`): `LocationInput`, `SignatureInput` and `BarcodeInput`.
`LocationInput` and `SignatureInput` also take `readOnly`, which only shows what is stored (for a
detail screen, or a list's template, where `Photos` draws a signature too).

**Location** (`"type": "location"`). The value is an object, kept as
`{"lat", "lon", "accuracy_m", "captured_at", "source"}`:

- `lat` −90 to 90 and `lon` −180 to 180 (numbers, never text); `accuracy_m` 0 to 1,000,000 metres, which a
  position from the device (`source` `"device"`) must give and one typed by hand (`"manual"`) may leave out;
- `captured_at`, RFC 3339 from the year 2000 on, kept in UTC to the millisecond, and **not more than five
  minutes ahead of the server's clock** (`422 app_field_location_future`; the phone's clock isn't the server's,
  hence the margin). Reading a stored row never holds it to the clock again;
- no other key. Anything else is `422 app_field_not_location`. An empty object is no value.

The field's `capture` is `"button"` (the default) or `"submit"`. The web app reads the position **once, in the
foreground**: when the person presses *Capture location*, or, for `"submit"`, when they press the form's button,
which is why the button's event context must pass such a field under its **own name**
(`{"place": {"path": "/draft/place"}}`; the server refuses a screen that names it otherwise). It uses the
browser's one-shot position and never a subscription to changes, so nothing follows the device and nothing
runs in the background (a test searches the web app and the agent's source for it). Where the browser can't give
a position (denied, no support, an insecure page, a timeout) the person is told why and types the coordinates,
which are stored as `"manual"`. A form whose field is `required` and can't get a position isn't sent.

**Signature** (`"type": "signature"`, `max_bytes` 1 MiB when left out). The value is **one attachment id**
(a list of one is read as that id when a screen sends it; a record holds the id alone). The input draws on a canvas pad with pointer events (`touch-action: none` on the
pad only, so the page scrolls around it; undo and clear; the strokes are kept as fractions of the pad's size, so
turning the device redraws them) or takes a typed name confirmed with a checkbox, for the keyboard, a switch
device or a screen reader. The picture, always a PNG, is uploaded through the
[photo upload](#photo-attachments) (the same three steps and limits, `POST …/uploads` with the signature
field's name) and attached to the record the same way. A signature upload is checked to be a PNG by its bytes
(`400 app_signature_type`, the upload removed), and a record can only claim a PNG that was uploaded **for a
signature field** (each attachment says which kind of field it was uploaded for, so a photo is never a signature,
even a PNG one that was still waiting when a photo field was made a signature field), **within the field's
`max_bytes` as it is when the record is saved**, not as it was when the upload started (`422 app_attachment_bad`;
the same for a photo field's `max_bytes`). A list of photos that a field held before it became a signature field
is no signature: it is hidden like any value of another type, and never locked. It is read like any attachment, by
whoever reads the record; `Photos` draws it. Offline, the web app keeps the signature's picture on the device with
the record, like a photo, and sends it when it is back online ([offline.md](offline.md)).

**Barcode** (`"type": "barcode"`). The value is text, within `max_length` (1000 when left out), with no control
characters; blank is no value. `format` (one of `aztec`, `codabar`, `code_39`, `code_93`, `code_128`,
`data_matrix`, `ean_8`, `ean_13`, `itf`, `pdf417`, `qr_code`, `upc_a`, `upc_e`; any when left out) narrows what the
camera reads, and for `ean_8`, `ean_13` and `upc_a` the server checks digits, length and the check digit
(`upc_e`, digits only), however the code came in. A rule can compare a barcode with `eq` and `ne`; a filter takes
`eq`, `ne` and `contains`. The web app scans with the browser's own `BarcodeDetector` when it has one, and where it
hasn't (it isn't in every browser), with a bundled decoder (zxing-wasm through the `barcode-detector` package, both MIT, which wraps ZXing-C++, Apache-2.0; their notices are served with the app, [docs/pwa.md](pwa.md#device-capabilities-in-apps)),
a separate file that loads the first time it is needed and is served with the app, never from an outside
address. **The code can always be typed**; a blocked, missing or insecure camera, or a decoder that couldn't
load or that can't run (the browser says it isn't supported, or it fails every frame from the start), turns the
camera off, says so, and leaves the typed field as it was.

**Captured values are evidence.** Once a record holds a location or a signature, **no later change can alter or
clear it**: an insert or update (a screen's, an older release's, the records explorer's, which sends the app's
own update action) that would is refused whole with `422 app_field_locked`, and changes nothing, not even a new
revision. That is told before anything else about the change: clearing a captured location the field requires is
`app_field_locked`, not `app_field_required`. Giving it again unchanged is fine, a record that has none takes one once (that is the capture), and
a field that says `"editable": true` is not locked. The records explorer shows such values read-only. A location
or a signature can't be filtered or sorted by (`400 app_query_field` / `app_query_condition`); a barcode is a
text for both. A photo field can't be filtered or sorted by either (the server refuses both). The records
explorer offers to filter or sort by none of those three, and offers a filter only the operators its field's
type takes.

A barcode's text is taken as it was read: a line break, a tab or any other control character in it, even around
it, is `422 app_field_not_barcode` (only spaces are trimmed). A `captured_at` must have a real zone: `Z` or an
offset of at most `23:59` (`+24:60` is no time).

The agent sees these values in `list_app_rows` (a signature as the picture's metadata, never its bytes) and has no
tool to capture one.

**Dictation.** Free-text fields on an app's screen get a microphone beside them in the web app, which is the chat
box's recorder and the workspace's [dictation route](dictation.md) (`POST /api/workspaces/{workspace}/dictation/transcriptions`,
for a member of the workspace). It needs a connection, because the speech is sent to the server: offline, or where
the workspace has dictation off, the button is off and says so, and the keyboard's own dictation still works.

### Drafts and releases

An app has a **draft**, the definition builders (and the agent) edit, and **releases**:
numbered, immutable copies of drafts that someone with the publish capability published. The
app serves its **live release**; editing the draft (`PATCH`, `create_app`, the agent's
`update_app`) never changes what people use. A new app is a draft with no release: nobody
launches it until it is published. Publishing and rolling back take the **publish
capability** (`403 app_forbidden`, `publish`: a builder without it, a user, a reader, a viewer
of the space, and the agent, which has no tool for either) in a space the caller may write (`403`
otherwise; `404` for anything of another space or workspace), and neither changes a single row.
Releases are never edited or deleted; the history lasts as long as the app.

Every route here is under `…/apps/{app}`. Each change names the app's `version` and, like every
change of the app, answers `409 app_changed` with the app as it is now if it changed since: two
people editing the draft never overwrite each other, and a publish is of the draft its
publisher read.

#### `POST …/apps/{app}/publish`

`{"version": 4, "acknowledge": [{"change": "field_removed", "field": "notes"}]}`. Makes the
draft the next release (its number is one more than any before) and the live one, and answers
`200` with the app. First the server checks the draft against the catalog again (`400`), then
against **the app's rows**. Changes of the collection against the live release that stored rows
are affected by are **schema changes that must be acknowledged**, by naming each one in
`acknowledge` and nothing else:

| `change` | When |
|---|---|
| `field_removed` | a field is gone, and some row has a value for it (the value stays in the row, unseen) |
| `field_type_changed` | a field's type changed, and some row has a value for it; **a field that becomes a location or a signature field** is always one, whatever the rows hold, since what it takes from then on is locked once stored |
| `field_required` | a field became required (or was added as required), and some row has no value for it |
| `field_narrowed` | a field's `max_length`, `min` or `max`, or a choice's `options`, now refuses a value some row has |
| `field_required_when` | a field gained a `required_when` rule, or its rule changed, and some row lacks the field where the new rule requires it |

A change no stored row is affected by (a new optional field, a looser limit, a field nobody
filled in) needs no acknowledgement. Without the exact acknowledgement the answer is `409
app_migration_required` with `params.changes`, the list of `{change, field}` the publish makes
(never rows or values), and nothing is published; acknowledging a change the publish doesn't
make is `409 app_migration_unneeded`, and a malformed list `400 app_migration_invalid`. The
acknowledgement is the whole migration: **rows are never rewritten**. Rows that don't fit the
new collection stay as they are: a value of another type than its field's is left out of what
the app shows, and an update of such a row must give the fields that don't fit. A draft equal
to the live release is `409 app_nothing_to_publish`.

The rows are looked at only for a publisher who reads every record (read scope `all`). For
anyone else the publish is judged by the two collections alone, as if every row were affected:
every removed or retyped field, every field that became (or was added as) required, and every
tighter `max_length`, `min` or `max` is to be acknowledged, whether or not a row is affected
(the table above says when a change is one for a publisher who reads the rows). So the
answer is the same for a bound that some row breaks and one that none does, and repeated
publishes read nothing out of the data.

#### `POST …/apps/{app}/rollback`

`{"version": 4, "release": 2}`. Makes an earlier release the live one: the app serves it as
published, with the rows as they are now, and **no data is rolled back**. The draft is left
as it is (it then differs from the live release, and publishing it makes a new release, not a
return to the one before). A release whose collection **is the live one's** (a release that
changed only the screen, actions or summaries) is never judged by the rows, for anyone: nothing
about how the rows are read or written changes, and the answer is the same whatever they hold
(rows may not fit the live collection after a publish that acknowledged it, and a refusal would
tell which). A release with another collection is refused (`409 app_release_incompatible`,
`params.fields` the fields that don't fit, never rows) unless every row fits it: its required
fields present, every value of its field's type and within its limits; fields it doesn't
declare are ignored, and kept. Whether the rows fit is told only to a publisher who reads every
record: for anyone else a rollback to a release whose collection isn't the live one's is `403
app_forbidden` (`read_records`). `409 app_release_already_live` for the live release,
`404 app_release_not_found` for one the app doesn't have. `200` with the app.

#### `GET …/apps/{app}/releases`

For those who may design or publish the app (`403 app_forbidden`, `design`, otherwise): the
history, newest first, without definitions:
`{"releases": [{"number": 2, "definition_version": 1, "requires": […], "published_at": "…",
"published_by": {…}, "live": true, "live_since": "…"}, …]}`. `live_since` is when it became
live: its publish, or a rollback to it.

#### `GET …/apps/{app}/releases/{release}`

The release with its `definition`, for the same callers; `404 app_release_not_found`.

**Capabilities.** A release records `definition_version` and `requires`
([The definition](#the-definition)). The web app's catalog knows a set of capabilities and a
definition version (`web/src/apps/catalog.tsx`, held to the server's by
`internal/apps/testdata/catalog.json`); a release that needs one it doesn't know is not
drawn: the app shows that it must be updated, and the page reloaded, instead of a broken
screen. Every existing app became release 1, live, with its current definition also as its
draft, when this arrived; nothing about who can use it changed.

## Home widgets and pins

Each person has a **Home** page in a workspace: widgets from their apps, and **pins**, apps listed
in the navigation in a section of their own, after the platform's pages. An app *offers* them
in its definition ([`contributions`](#the-definition)); the person chooses which to use. A widget
is a bounded view, never markup of its own, and a contribution can't replace the platform's login,
settings, permission prompts or publishing controls: pins are one more link to launch an app, their
labels are plain text, and what a widget shows or does is the app's own surface and actions
(`GET …/apps/{app}/surface?widget=` and `POST …/apps/{app}/actions`), with the caller's access to
the app, so a read scope of `own` shows only their own records' summaries and an action needs
the capability it always does.

What is on a person's Home is a list of **placements**, each naming an app and, for a widget, one
of its widgets; they are stored per user and read back through the one place that answers app
access, so nothing about a placement is trusted. Reading Home gives each item as the caller
sees it **now**:

- an app the caller can no longer view (a lost role, a space they left, an app moved where they
  can't read it) or that was deleted is **left out** as if it were never placed, and comes back
  if they get access again;
- an app that is offline, was never published, or whose live release no longer has the widget is
  **unavailable**: `state` is `unavailable` with a `reason` (`offline`, `unpublished`, `removed`),
  and it has the app's name (the caller may view the app) and no widget, label or release;
- otherwise `state` is `ready`, with the `widget` (or, for a pin, its `label` and `screen`, else the
  app's name) and the live `release` (`number`, `definition_version`, `requires`) a client checks
  before drawing it.

Every route needs a session (`401`) and checks the workspace (`404` for another). Workspace routes
are under `/api/workspaces/{workspace}/home`; a person's placements are theirs alone (another's
id is `404`, never `403`).

### `GET /api/workspaces/{workspace}/home`

The caller's Home: `{"widgets": […], "pins": […], "hidden": […], "limits": {"widgets": 24,
"pins": 20}}`. An item is `{"id"?, "default_id"?, "source": "personal"|"default", "kind":
"widget"|"pin", "space_id", "app_id", "app_name", "widget_id"?, "widget"?, "label"?, "screen"?,
"state", "reason"?, "release"?, "can_manage"?}`. The defaults of the caller's shared spaces
come first, then their own in the order they chose; an item the caller placed themselves
replaces the same default. `hidden` are the defaults they hid.

### `GET /api/workspaces/{workspace}/home/available`

The apps the caller may put on Home or pin: those they may view, online and published, in the
spaces they read, `{"apps": [{"space_id", "space_name", "space_kind", "app_id", "name",
"widgets", "pin", "can_set_defaults", "defaults"}]}`. `can_set_defaults` and the `defaults` set
are only for those who may [set defaults](#shared-defaults) for the app.

### `POST /api/workspaces/{workspace}/home/items`

Places a widget or pins an app: `{"kind": "widget"|"pin", "space_id", "app_id", "widget"}`, with
`widget` for a widget only. `201` with the item, going last; placing what is already placed
returns it. `404` for a space of another workspace or an app the caller may not view,
`409 app_offline` or `app_not_published`, `404 home_widget_not_found`, `409 home_limit` (24
widgets, 20 pins per workspace), `400` for a request that isn't one of those shapes.

### `DELETE /api/workspaces/{workspace}/home/items/{item}`

Takes one of the caller's placements away. `204`; `404` for one that isn't theirs.

### `PUT /api/workspaces/{workspace}/home/order`

`{"kind": "widget"|"pin", "ids": […]}`: puts the caller's placements of that kind in the order
of `ids`, the ones not listed after them. All or nothing: an id that isn't theirs, of that kind,
or is listed twice is `400 home_order_invalid`. `200` with Home.

### `PUT` and `DELETE /api/workspaces/{workspace}/home/hidden/{default}`

Hides a default of a shared space the caller reads, from them alone, or shows it again. `204`,
idempotent; `404` for a default of an app the caller may not view.

### Shared defaults

The people who may **publish** an app of a shared space (the app's `publish` capability, which
every writer of the space has in an app without roles, and is the publisher role's in one with
them) may set its widgets and pin as defaults for the space's members, who see them unless they
hide them: a default changes what every member sees when they open TaskTo.AI, as publishing
changes what every member uses. Defaults are for the apps of **shared** spaces only, are bounded
(12 widgets and 8 pins per space), and go when the app moves to another space. Everyone who
reads the space may list them; they are seen only for the apps they may view.

- `GET …/spaces/{space}/home/defaults`: `{"defaults": […]}`, each item with `can_manage`, whether
  the caller may remove it.
- `POST …/spaces/{space}/home/defaults`: `{"kind", "app_id", "widget"}` as above. `201` with the
  item (`default_id`). A reader of the space `403`, someone who may write the space but not publish
  the app `403 app_forbidden` (`publish`), a private space `409 home_defaults_shared`.
- `DELETE …/spaces/{space}/home/defaults/{default}`: `204`, with the same rights; `404` for a
  default of another space.

## Model providers

A provider account is a model endpoint the agent can use: a base URL, the models it serves
and, if it needs one, an API key. It belongs to a **workspace** (its admins manage it, its
members use it) or to a **user** (personal: usable only in that user's own conversations,
in any of their workspaces). How someone reaches their models is their choice: any server
that speaks OpenAI's Chat Completions API works, such as Ollama, vLLM, LiteLLM or a hosted
API.

An account looks like this:

```json
{
  "id": 4,
  "scope": "workspace",
  "provider": "ws1-gateway",
  "name": "gateway",
  "display_name": "Team gateway",
  "kind": "openai-compatible",
  "base_url": "https://llm-gateway.example.com/v1",
  "api_key": "set",
  "compat": {"supportsStore": false},
  "models": [{"id": "some-model", "name": "Some model", "context_window": 131072, "max_tokens": 8192, "reasoning": true}],
  "created_at": "…",
  "updated_at": "…"
}
```

- **`name`**: 1–40 characters of `a-z 0-9 . _ -`, starting with a letter or digit, unique
  per workspace or user. It can't change, since it is part of `provider`.
- **`provider`**: the agent runtime's provider ID, `ws<workspace>-<name>` or
  `u<user>-<name>`. A conversation may only use a `ws<N>-` provider in workspace N, and a
  `u<N>-` provider only if it is user N's conversation; see
  [agent-providers.md](agent-providers.md#provider-accounts).
- **`display_name`**: up to 100 characters; defaults to `name`.
- **`kind`**: `openai-compatible`, the default and only kind so far.
- **`base_url`**: `http` or `https`, up to the version path (`…/v1`), without credentials,
  query or fragment. A trailing `/` is dropped. It must be one the server's egress policy
  for the account's scope allows (workspaces' and personal accounts each have one, `any`
  by default; see [install.md](install.md#where-provider-accounts-may-connect)): the
  server resolves the host when the account is saved, and refuses an address the policy
  forbids with `400` and `base_url: the server's egress policy doesn't allow this
  endpoint: <host> resolves to <address>, a private address` (or `is a loopback address`,
  `not on the allowlist`, …). A host that doesn't resolve then is accepted: every
  connection is checked anyway.
- **`api_key`**: **write-only**. Send the key to set it; it is answered as `"set"` or
  `"not set"`, never the key. 8–4096 printable ASCII characters without spaces. A key
  needs an `https` base URL, unless the host is this machine (`localhost`, `127.x.x.x`,
  `[::1]`), so it never crosses the network in cleartext.
- **`compat`**: optional pi-ai compatibility settings for a server's quirks, all flat:
  `supportsStore`, `supportsDeveloperRole`, `supportsReasoningEffort`,
  `supportsUsageInStreaming`, `supportsFinishReason`, `maxTokensField`,
  `requiresToolResultName`, `requiresAssistantAfterToolResult`,
  `requiresNonNullAssistantContent`, `requiresToolsWithToolHistory`,
  `requiresThinkingAsText`, `requiresReasoningContentOnAssistantMessages`,
  `thinkingFormat`, `zaiToolStream`, `thinkingTokenBudgetField`,
  `supportsThinkingTokenBudget`, `supportsOpenAIGrammarTools`,
  `supportsMidConvoSystemMessages`, `supportsMidConvoToolAdditions`, `supportsStrictMode`,
  `cacheControlFormat`, `sendSessionAffinityHeaders`, `sessionAffinityFormat`,
  `promptCacheKeyMode`, `supportsLongCacheRetention` and `vllmPriority` (see pi-ai's
  README for what each does). The nested routing and chat-template settings are only
  available in the operator's providers file.
- **`models`**: 1–100 models. `id` is what the server calls the model (1–256 characters);
  `name` defaults to `id`; `context_window` and `max_tokens`, if given, are positive
  integers (defaults 128000 and 8192); `reasoning` defaults to `false`.

Each workspace and each user can have up to 50 accounts. Errors name the field that's
wrong, never its value. Changes reach the running agent runtime at once, without
interrupting conversations (see [agent-providers.md](agent-providers.md#provider-accounts)).

**Keys at rest.** The database keeps each key encrypted with AES-256-GCM under the server
key, `server.key` in the data directory: 32 random bytes made on first start, mode 0600.
Back it up with the database but keep it apart from it: without it the stored keys can't
be read, and with it and the database anyone can read them. The server refuses to start
if the file is readable by others. Keys are never logged. To rotate a key, set a new one.

### `GET /api/workspaces/{workspace}/providers`

Any member. `{"providers": [account, …]}`, oldest first.

### `POST /api/workspaces/{workspace}/providers`

Admin only. The account's fields: `name`, `base_url` and `models` are required;
`display_name`, `kind`, `api_key` and `compat` are optional. `201` with the account;
`400` for an invalid field, or a `base_url` the egress policy forbids; `409` if the name is
taken or the workspace has 50 accounts.

### `PATCH /api/workspaces/{workspace}/providers/{account}`

Admin only. Any of `display_name`, `base_url`, `api_key`, `compat` and `models`; the rest
stay as they are. `api_key: null` removes the key and `compat: null` the settings. The
account is checked whole, so a base URL change that would send the stored key over plain
http to another host is refused, and so is a new `base_url` the egress policy forbids (an
account it came to forbid can still be changed otherwise). `200` with the account; `400`
for an invalid field or an attempt to change `name` or `kind`; `404` if the account isn't
the workspace's.

### `DELETE /api/workspaces/{workspace}/providers/{account}`

Admin only. `204`, or `404`. The key is deleted with the account, including the agent
runtime's copy.

### `POST /api/workspaces/{workspace}/providers/{account}/test`

Admin only, no body. Asks the account's endpoint for its models, `GET {base_url}/models`
with the account's key, as OpenAI's API serves them, within 10 seconds and without
following a redirect. `200` with the outcome, `404` if the account isn't the workspace's:

```json
{"ok": true, "missing": ["some-model"]}
```

- `ok`: the endpoint answered `200` with a list of models.
- `missing`: the account's models the endpoint didn't list, if any (a chat may still work:
  some servers list fewer models than they serve).
- `error`: with `ok: false`, why: no answer, the status it answered (`401` and `403` say to
  check the key), a reply that isn't a list of models, or that the egress policy refuses
  the endpoint (`the server's egress policy doesn't allow this endpoint: …`; nothing was
  sent). It never quotes the endpoint's reply, which could echo the key. `code` and
  `params` are its [error code](#error-codes), for a translation: `endpoint_unreachable`,
  `endpoint_key_refused`, `endpoint_status`, `endpoint_no_models` or `egress_refused`.

### Personal accounts: `/api/me/providers`

`GET /api/me/providers`, `POST /api/me/providers`, `PATCH /api/me/providers/{account}`,
`DELETE /api/me/providers/{account}` and `POST /api/me/providers/{account}/test` work as
the workspace routes, for the signed-in user's own accounts. Nobody else sees them, admins included; another user's account ID
is a `404`.

### `GET /api/workspaces/{workspace}/models`

Any member. What the caller's conversations in this workspace may use: the operator's
providers (scope `instance`, from `-agent-providers`, or the scripted offline model without
it, and only while the agent runtime is on), the CLI workers installed on the server (scope
`cli`, while `-cli-workers` is on, [cli-workers.md](cli-workers.md)), then the workspace's
accounts, then the caller's own.

```json
{"providers": [
  {"provider": "script", "name": "Scripted (offline)", "scope": "instance", "models": [{"id": "script-1", "name": "Scripted"}]},
  {"provider": "cli-claude", "name": "Claude Code", "scope": "cli", "models": [{"id": "default", "name": "Claude Code (its default model)"}, {"id": "opus", "name": "opus"}]},
  {"provider": "ws1-gateway", "name": "Team gateway", "scope": "workspace", "account": 4, "models": [{"id": "some-model", "name": "Some model"}]},
  {"provider": "u2-home", "name": "My Ollama", "scope": "user", "account": 9, "models": [{"id": "llama3.1:8b", "name": "llama3.1:8b"}]}
]}
```

## Dictation

Speech to text for the chat box ([dictation.md](dictation.md)). The web app records up to
10 minutes, and the server transcribes them with the workspace's **engine**:

- **`local`**, the default: the operator's local model, run by the server itself, so the
  audio never leaves it ([install.md](install.md#dictation)). It is downloaded on first use.
- **`api`**: a hosted API that speaks OpenAI's `POST {base_url}/audio/transcriptions`
  (OpenAI, Groq, a LiteLLM proxy, a self-hosted server), with the workspace's key. Used only
  when an admin picks it: setting a key doesn't switch to it.
- An add-on's ID (such as `desertant-voz`), when the operator installed it and an admin
  confirmed its notice.

**The glossary.** A space's `glossary.md`, at its root, lists terms the transcription
should get right, one per list item: the item's leading words, up to the first `:`, ` — `
or ` - ` (`- **TaskTo**: the product`). A dictation uses the glossaries of the spaces its
caller can read, up to 500 terms. Engines that run on the server can favour them; then,
for every engine, words that match a term ignoring case, accents and the spaces between
them, or nearly match a long term, are replaced by its spelling ("task to" becomes
"TaskTo"). Glossary terms are never sent to a hosted API.

### `GET /api/workspaces/{workspace}/dictation`

Any member. The engine a dictation uses now, the settings, and the local engine's state:

```json
{
  "engine": "local",
  "reason": "the local speech model is downloading (40%)",
  "settings": {
    "engine": "local",
    "language": "",
    "api": {"base_url": "https://api.openai.com/v1", "model": "whisper-1", "api_key": "not set"},
    "accepted_notice": "",
    "updated_at": "…"
  },
  "local": {"model": "parakeet-v3", "state": "downloading", "progress": 0.4},
  "credits": [{"name": "Parakeet TDT 0.6B v3", "by": "NVIDIA", "url": "…", "licence": "CC-BY-4.0", "licence_url": "…"}],
  "max_seconds": 600
}
```

- **`engine`**: `api`, `local`, an add-on's ID, or `""` when there is none (such as the
  default, `local`, on a server whose operator turned the local model off); **`reason`**
  then says why, or why the engine can't run yet, with its [code](#error-codes) in
  `reason_code` (and `reason_params`).
- **`local`**: `null` when the operator turned the local engine off. Its `state` is
  `unavailable` (not on this platform), `not_downloaded`, `downloading` (with `progress`
  from 0 to 1), `ready` or `failed` (with `error`, and its code in `error_code`; the next
  `warm` tries again).
- **`credits`**: the attributions the engine in use needs; the About page shows them.

### `PATCH /api/workspaces/{workspace}/dictation`

Admin only. Any of these; an absent field stays as it is:

```json
{"engine": "api", "language": "pt", "api": {"base_url": "https://api.groq.com/openai/v1", "model": "whisper-large-v3", "api_key": "…"}}
```

- **`engine`**: `local`, `api`, or an installed add-on's ID (with `accept_notice` naming it,
  in this request or an earlier one). `local` is refused as a change while the operator has
  the local engine off; sent back unchanged, it is accepted. Setting `api.api_key` never
  changes the engine.
- **`language`**: an ISO 639-1 code, or `""` for the engine to detect it.
- **`api.base_url`** and **`api.api_key`**: the rules of
  [provider accounts](#model-providers): an `http`/`https` base URL without credentials,
  one the egress policy of workspaces' accounts allows (checked now and on every
  connection), and `https` unless the host is this machine when there is a key. The key
  is **write-only**: answered as `"set"` or `"not set"`, sealed with the server key, never
  logged; `null` removes it. A stored key never moves to another host on its own: changing
  `base_url`'s host needs the key sent again.
- **`api.model`**: 1–100 printable characters.

`200` with the same reply as `GET`; `400` for an invalid field, naming it but never its value.

### `POST /api/workspaces/{workspace}/dictation/warm`

Any member, when the microphone is pressed. If the workspace's engine is `local` and its
model isn't here yet, starts downloading it, and answers at once with the same reply as
`GET`.

### `POST /api/workspaces/{workspace}/dictation/transcriptions`

Any member. The recording, as a WAV file of 16-bit PCM, mono, 16 kHz, base64-encoded; the
body may be up to 28 MiB (ten minutes is about 26 MiB). `language` is optional and wins
over the workspace's; `""` leaves it to the engine to detect. Without either, the engine
listens for the user's language ([i18n.md](i18n.md#the-agent-and-dictation)), from `browser_locale`
(optional, the browser's language, as in `PATCH /api/me`) when they picked none:

```json
{"audio_base64": "UklGR…", "language": "pt", "browser_locale": "pt-BR"}
```

`200` with `{"text": "…", "engine": "local"}`. `400` for audio that isn't such a WAV, is
empty or is over 10 minutes; `413` for a bigger body; `429` while the caller already has
a dictation in progress, or the server already transcribes 4 (try again shortly); `503`
with the reason when the
workspace has no engine or the local model isn't downloaded yet (call `warm`); `502` when
the engine fails (the server log has the details, never the reply); `504` when it takes
over 5 minutes.

## Outgoing email

The server's outgoing email ([install.md](install.md#email)) is one SMTP server for the whole
server, set by its operator, so only [operators](#operators) see it, as they see the backups: not
the admins of a workspace someone created later. Both routes are for operators who are admins of
the workspace in the URL, with a browser session (not an access token); other admins get `403`
`operator_only`.

### `GET /api/workspaces/{workspace}/mail`

Whether the server can send email, and where:

```json
{"configured": true, "host": "smtp.example.com", "port": 587, "security": "starttls",
 "from": "\"TaskTo\" <taskto@example.com>", "password": "set"}
```

`security` is `starttls`, `tls` or `none`. `password` is `"set"` or `"not set"`: whether the server
signs in to the SMTP server. The user name and the password are never returned. Without an SMTP
server the answer is `{"configured": false}`.

### `POST /api/workspaces/{workspace}/mail/test`

Sends a test message to **the caller's own address** (there is no way to name another recipient),
in the caller's language: the one they picked, else the workspace's default, else English. The
body is `{}`; anything else in it is ignored.

`200` with `{"sent_to": "ana@example.com"}` once the SMTP server accepted the message. That
means it took it, not that it was delivered. Errors: `409 mail_not_configured`; `502` with
`egress_refused` (the egress policy refuses the SMTP server's address), `mail_connect_failed`,
`mail_tls_failed`, `mail_auth_failed`, `mail_rejected` or `mail_failed`, which say at which step
it failed. The SMTP server's own reply isn't in the answer, since it can name internal hosts and
addresses, or repeat what was sent; the server's log has the step and the reply's number
(`smtp_code`), and never the server's words.

The route is limited, since each call sends a real message through the operator's relay: one
test per person every 30 seconds, whichever workspace it is in (a failed one counts too), and
three at once on the whole server. Past it, `429 mail_test_rate_limited` with a `Retry-After`
header in seconds, and nothing is sent.

## Backups

The server's scheduled backups (`taskto backup-schedule`, [install.md](install.md#backups))
write how they went to `<data>/backup-schedule/status.json`; these routes read it. The
backups are the whole server's, not a workspace's. `health` sums it up:

| `health` | When |
|---|---|
| `none` | No schedule has run on this server: backups aren't set up |
| `pending` | The schedule runs, and no backup has finished yet |
| `ok` | The last backup succeeded, and the next isn't late |
| `failing` | The last backup failed, and the night's retries still have time |
| `late` | No backup succeeded by `due_by`: a night was missed |

### `GET /api/workspaces/{workspace}/backup`

[Operators](#operators) who are admins of the workspace in the URL, with a browser session (not
an access token); other admins get `403` `operator_only`. The status, error included:

```json
{
  "health": "failing",
  "at": "03:15",
  "retries": 6,
  "retry_every_seconds": 3600,
  "last_success": "2026-03-09T03:16:40Z",
  "last_snapshot": "0123abcd…",
  "last_attempt": "2026-03-10T04:15:00Z",
  "last_error": "the repository didn't answer, so nothing was backed up: …",
  "last_error_at": "2026-03-10T04:17:00Z",
  "night": "2026-03-10T03:15:00Z",
  "failures": 2,
  "next_attempt": "2026-03-10T05:17:00Z",
  "due_by": "2026-03-10T10:15:00Z"
}
```

`at` is the nightly time in UTC. `failures` counts the failed backups for `night`, the
scheduled time they were for. A success doesn't clear `last_error`: it is current only if
`last_error_at` is after `last_success`. With `health` `none`, the other fields are absent.
A user or a URL's password in an error is replaced by `***`.

### `GET /healthz/backup`

No login, for outside monitoring: `200` while backups are on time (`ok`, `pending`,
`failing`), `503` when a night was missed (`late`), no schedule has run (`none`) or the
status can't be read (`unknown`). It never says why a backup failed, which can name the
backup host:

```json
{"status": "ok", "last_success": "2026-03-10T03:16:40Z", "due_by": "2026-03-11T10:15:00Z"}
```

## Storage usage

What each space stores, and the operator's storage quotas ([quotas.md](quotas.md)). Sizes
are in bytes; a quota of `0` is none, which is what a server has until its operator sets
one (`-storage-quota-workspace`, `-storage-quota-user`). Both routes take a browser session,
not an access token. The figures are the ones the server keeps as it writes: reading them
walks nothing in git.

### `GET /api/workspaces/{workspace}/usage`

Admins. Every space of the workspace, and only that workspace's: the shared spaces and each
member's private space, with its owner.

```json
{
  "quota": {"workspace": 107374182400, "user": 10737418240},
  "total": 123456789,
  "spaces": [
    {
      "id": 4, "kind": "private", "name": "Private",
      "owner": {"id": 7, "name": "Bob", "email": "bob@example.com"},
      "knowledge_versioned": 1048576,
      "knowledge_unversioned": 52428800,
      "attachments": 3145728,
      "total": 56623104,
      "limit": 10737418240
    },
    {"id": 2, "kind": "shared", "name": "Team", "knowledge_versioned": 2097152,
     "knowledge_unversioned": 0, "attachments": 0, "total": 2097152, "limit": 0}
  ]
}
```

`knowledge_versioned` is what git keeps of the space's knowledge base, every version
included; `knowledge_unversioned` the bytes of its [unversioned files](#unversioned-files);
`attachments` the apps' photos, thumbnails included, with the uploads on their way at the
size they declared. `limit` is the quota of the space itself: the private space's, and `0`
for a shared one, which only the workspace's quota bounds. `total` is the sum over all the
spaces, the figure the workspace's quota is checked against. A member gets `403 admin_only`;
someone who isn't in the workspace, `404 workspace_not_found`.

### `GET /api/workspaces/{workspace}/usage/me`

Any member. Their own private space in the workspace, in the same shape (`space` is `null`
if they have none), and the quotas. It says nothing of any other member's.

```json
{"quota": {"workspace": 0, "user": 10737418240}, "space": {"id": 4, "kind": "private", "…": "…"}}
```

## Setup checklist

The first-run checklist on an admin's Home (`internal/checklist`): the steps that make a new
workspace useful to a team. Whether a step is done is never stored. It is worked out from what
exists when the checklist is asked for, so a step done anywhere (the settings pages, the agent) is
done here, and a step undone is open again. What is stored, per workspace and shared by its admins,
is whether the checklist was dismissed and which steps were skipped as not applying.

| `id` | Done when |
|---|---|
| `invite` | the workspace has a member besides one, an invite that hasn't expired and wasn't used yet, or an accepted invite (an expired, unused invite doesn't count) |
| `provider` | the workspace has a model provider account |
| `space` | the workspace has a shared space that isn't archived |
| `app` | an app that isn't archived is in a shared space of the workspace that the caller is a member of, or in the caller's own private space (apps in spaces the caller can't read, and other people's private apps, are never counted) |
| `email` | the server can send email ([Outgoing email](#outgoing-email)); optional |
| `backups` | the server's scheduled backups have a status ([Backups](#backups)); optional |

The answer says whether things exist, never what they are. Every route is for the workspace's
admins, with a browser session (not an access token): `403 admin_only` for a member, `404
workspace_not_found` for someone outside the workspace.

### `GET /api/workspaces/{workspace}/checklist`

```json
{
  "dismissed": false,
  "complete": false,
  "items": [
    {"id": "invite", "state": "todo", "optional": false},
    {"id": "provider", "state": "done", "optional": false},
    {"id": "space", "state": "skipped", "optional": false},
    {"id": "app", "state": "todo", "optional": false},
    {"id": "email", "state": "todo", "optional": true},
    {"id": "backups", "state": "done", "optional": true}
  ]
}
```

`state` is `done`, `todo` or `skipped`; a step that is done is `done` even if it was skipped.
`complete` is true when every step that isn't `optional` is `done` or `skipped`. The web app shows
the checklist while it isn't `dismissed` and isn't `complete`.

`email` and `backups` belong to the whole server, so only [operators](#operators) get them in `items`:
for the admin of a workspace someone else created, the list is the first four.

### `PUT /api/workspaces/{workspace}/checklist/dismissed`, `DELETE …`

Puts the checklist away for the whole workspace, or brings it back. `204`, and the same again is
`204`.

### `PUT /api/workspaces/{workspace}/checklist/skipped/{item}`, `DELETE …`

Marks a step (an `id` above) as not applying, or takes the mark off. `204`; `404 not_found` for a
step that doesn't exist.

## CLI accounts

Each user's logins to the official coding-agent CLIs, Claude Code, Codex and Grok Build
([cli-workers.md](cli-workers.md)), signed in with the CLI's own login flow. Every route is
the signed-in user's own (a session, not an access token), acts on their logins only, and
answers `401` without a session. The CLI keeps its credentials in the user's own directory
on the worker; the server never stores or returns them.

Whether the CLIs can be used depends on the server's `-cli-workers` mode:

- `off` (the default): the list says so and every other route answers `409`.
- `local`: the CLIs run as the server's own OS user, with no OS boundary between users, so
  they work only while the server has a single user; with more, the list says why and the
  other routes answer `409`.
- `local-unsafe`: the same without that check, for development.

A user may connect up to nine accounts per CLI, each a login of their own in a config
directory of its own ([account-switching.md](account-switching.md)). `{account}` names one: the
CLI (`claude`, `codex` or `grok`) for the first, and the CLI followed by `-2` to `-9` for the
others (`claude-2`). Adding an account is starting a login on a handle that isn't connected
yet. Another name is a `404`. A CLI that isn't installed on the server answers `503`.

A conversation's turn on a CLI runs on the owner's first account for it, by number, that is
connected; with none connected, on the first, which asks for a new sign-in. A turn on another
account than the conversation's last starts a new CLI session there, with the conversation so
far before the prompt.

### `GET /api/me/cli-accounts`

```json
{"enabled": true, "mode": "local", "accounts": [
  {"id": "claude", "cli": "claude", "slot": 1, "label": "Personal", "name": "Claude Code", "available": true,
   "state": "connected", "auth_method": "claude.ai",
   "limits": [{"window": "five_hour", "used_percent": 42, "resets_at": "2026-11-19T18:00:00Z", "status": "allowed"}],
   "limits_at": "2026-11-19T15:02:11.120Z", "login": null},
  {"id": "claude-2", "cli": "claude", "slot": 2, "label": "Work", "name": "Claude Code", "available": true,
   "state": "connected", "auth_method": "claude.ai", "limits": [], "limits_at": null, "login": null},
  {"id": "codex", "cli": "codex", "slot": 1, "label": "", "name": "Codex", "available": true, "state": "disconnected",
   "auth_method": "", "limits": [], "limits_at": null,
   "login": {"cli": "codex", "state": "waiting", "url": "https://auth.openai.com/codex/device", "url_trusted": true,
             "code": "ABCD-EFGHI", "needs_code": false, "started_at": "2026-11-19T15:10:00Z"}},
  {"id": "grok", "cli": "grok", "slot": 1, "label": "", "name": "Grok Build", "available": false, "state": "disconnected",
   "auth_method": "", "limits": [], "limits_at": null, "login": null}
]}
```

The list has each CLI's first account, connected or not, then every other account that is
connected or needs a new sign-in, and one whose login started in the last 20 minutes, by CLI
and number. `enabled` is false with a `reason` when the CLIs can't be used now, and its
[code](#error-codes) in `reason_code`. `state` is `connected`,
`reconnect` (a turn or a check found the login expired or revoked: sign in again) or
`disconnected`. `limits` are the newest usage limits the CLI reported, merged by window, as
of `limits_at` ([cli-workers.md](cli-workers.md#usage-limits)).

### `POST /api/me/cli-accounts/{account}/login`

Starts the CLI's own login (`claude auth login`, `codex login --device-auth`, `grok login
--device-auth`), or returns the one in progress: `200` with the login. Show `url` as a link
only when `url_trusted` (it is on the CLI's known sign-in host); show `code` for the user to
enter there. With `needs_code` (Claude Code), the sign-in page shows a code the user pastes
back with the next route. `state` goes `starting` → `waiting` → `connected`, `failed`
(with a `message`), `expired` (after 15 minutes) or `cancelled`. A login that connects makes
the account `connected`.

### `GET /api/me/cli-accounts/{account}/login`

The login in progress or just finished, for polling; `404` if there is none.

### `POST /api/me/cli-accounts/{account}/login/code`

`{"code": "…"}`: the code Claude Code's sign-in page showed, passed to the login. `202` with
the login; `400` for a code that isn't 1–512 printable characters without spaces, `404`
without a login in progress, `409` for a CLI whose login takes no code.

### `DELETE /api/me/cli-accounts/{account}/login`

Cancels the login in progress. `204`.

### `POST /api/me/cli-accounts/{account}/check`

Asks the CLI whether its login works (`claude auth status`, `codex login status`, Grok's
credentials file) and records the answer: `200` with the account, `connected` or, for an
account that was connected, `reconnect`.

### `PATCH /api/me/cli-accounts/{account}`

`{"label": "Work"}`: the user's name for the account, up to 40 characters on one line (empty
for none). `200` with the account; `404` for an account that isn't connected.

### `DELETE /api/me/cli-accounts/{account}`

Signs the CLI out of the account with its own logout command and removes everything it kept
there (sessions, settings), and the account with its conversations' sessions. The user's
other accounts aren't touched. `204`.

## Automatic switching

The signed-in user's queue of CLI accounts ([account-switching.md](account-switching.md)): their
own and the shared accounts of their workspaces, in order. It is **off until the user turns it
on**. While it is on:

- a turn doesn't start on an account at a usage limit: it takes the next one in the queue for
  the same CLI, else for another;
- when the account a turn runs on stops (it reports a limit reached, or its login needs
  renewing), the turn goes on with the next account in the queue that isn't at a limit. That
  account starts a new session with the conversation so far and a note of what happened. The
  conversation shows a notice, and its model becomes the new CLI's.

A queue entry is used only where it is allowed: a shared account only in its own workspace,
and only where the member's mode allows it. The queue also orders the user's accounts while
switching is off: a turn runs on the first connected one in it.

### `GET /api/me/cli-switching`

```json
{"enabled": false,
 "queue": [{"kind": "own", "id": "claude", "cli": "claude", "title": "Claude Code (Personal)", "label": "Personal",
            "number": 1, "limited": false},
           {"kind": "shared", "id": "4", "cli": "codex", "title": "Codex (Company key, Home)", "label": "Company key",
            "workspace": 1, "workspace_name": "Home", "limited": true, "limited_until": "2026-11-19T18:00:00Z"}],
 "available": [ … every account that can go in the queue, in the same shape … ]}
```

`title` names the account in English; `label` (if any), `number` (an own account's, for its
CLI) and `workspace_name` are there for a client to name it in its user's language.

### `PUT /api/me/cli-switching`

`{"enabled"?: true, "queue"?: [{"account": "claude-2"}, {"shared": 4}]}`. A queue replaces the
one before. `200` with the state. An account that isn't the caller's, or a shared account of a
workspace they aren't a member of, is a `404`. An account twice is a `400`. An account that is
disconnected or removed leaves the queue.

## Shared CLI accounts

A workspace's shared accounts for the CLIs ([account-switching.md](account-switching.md)): an
API key per CLI, which members' turns use through the server's proxy. The key never reaches the
CLIs or comes back from the API (`"api_key": "set"`). It is never a subscription login, which
the providers' terms forbid sharing. Members read them; admins change them and the members'
modes. Another workspace's account is a `404`, like one that doesn't exist.

A member's **mode** in the workspace says which accounts their turns there use:

- `own_else_shared` (the default): their own account for the CLI if one is connected, else the
  workspace's first shared account for it;
- `own`: their own accounts only;
- `shared`: the workspace's shared accounts only. With none for the CLI, the turn is refused.

A turn on a shared account fails, rather than asking the member to sign in, when the provider
refuses the key. A `429` from the provider is kept as the account's limit (window `api`), with
when it resets.

### `GET /api/workspaces/{workspace}/cli-accounts`

Members.

```json
{"enabled": true, "mode": "own_else_shared", "default_mode": "own_else_shared", "accounts": [
  {"id": 4, "cli": "claude", "name": "Claude Code", "label": "Company key", "base_url": "https://api.anthropic.com",
   "model": "", "api_key": "set", "limits": [], "limits_at": null, "created_at": "2026-11-19T10:00:00.000Z"}
]}
```

`mode` is the caller's. `enabled` is false with a `reason` when turns can't use the accounts now:
CLI workers are off, or the server has no proxy (`-cli-proxy-listen`, [install.md](install.md#shared-accounts-and-their-proxy)),
with its [code](#error-codes) in `reason_code`.

### `POST /api/workspaces/{workspace}/cli-accounts`

Admins. `{"cli": "codex", "label": "Company key", "api_key": "…", "base_url"?: "…", "model"?: "…"}`.

- `base_url` is the provider's API by default (`https://api.anthropic.com`, `https://api.openai.com`,
  `https://api.x.ai`). Otherwise it is a gateway's: https, unless it is on this machine. A trailing
  `/v1` is dropped, since the CLIs add it. The workspace accounts' egress policy must allow it.
- `model`, if set, is the model every turn asks for, for a gateway that names its models
  differently.
- `label` is 1–60 characters. `api_key` is 8–4096 printable characters without spaces.

`201` with the account; `400` for an invalid field; `409` past 20 accounts.

### `PATCH /api/workspaces/{workspace}/cli-accounts/{account}`

Admins. Any of `label`, `base_url`, `model`, `api_key` (a new key replaces the stored one). The
CLI doesn't change. `200` with the account.

### `DELETE /api/workspaces/{workspace}/cli-accounts/{account}`

Admins. Removes the account, its key, and members' CLI sessions on it. `204`.

### `POST /api/workspaces/{workspace}/cli-accounts/{account}/test`

Admins. Asks the provider for its models with the key, through the egress policy:
`{"ok": true, "message": "the provider accepted the key"}`, or `ok` false with why, and its
[code](#error-codes) and `params` (`endpoint_unreachable`, `endpoint_key_refused`,
`endpoint_status`, `egress_refused`, `shared_key_unreadable`). The provider's answer is never
passed on, only its status.

### `GET` and `PATCH /api/workspaces/{workspace}/cli-accounts/modes`

Admins. `GET` → `{"default_mode": "own_else_shared", "members": [{"user_id": 2, "name": "Bea",
"email": "bea@example.com", "mode": null}]}`, where `null` is the default. `PATCH`
`{"default_mode"?: "own", "members"?: {"2": "shared", "3": null}}` sets the default and members'
own (`null` gives a member the default again), and answers like `GET`. A user who isn't a member
is a `404`.

### The proxy

Not part of this API: it has a listener of its own. It serves `/cli-proxy/v1/…` only, each request
with a turn's token (`Authorization: Bearer` or `x-api-key`), and forwards it to the account's
`base_url` with the key. A token works for its turn alone, until the turn ends. It stops at once
when its conversation is archived, its owner leaves the workspace, the account is removed, or the
owner's mode rules shared accounts out.

## Agent browser

Each user's own browsers, which the agent uses for them ([agent-browser.md](agent-browser.md)):

- `personal`, with their logins kept, and a live view;
- `testing`, clean and headless.

Every route is the signed-in user's own (a session, not an access token) and acts on their
browsers only: no path names a user. Without a session, a route answers `401`.

The browser is off unless the server runs with `-browser`. It runs where the CLI workers do,
so it follows their rule ([CLI accounts](#cli-accounts)): with it off, or in local mode on a
server with more than one user, the status says why, and the other routes answer `409`.

### `GET /api/me/browser`

```json
{"enabled": true, "browsers": {
  "personal": {"available": true, "running": true, "live": false, "stops_at": "2026-11-19T15:15:00Z",
               "tabs": [{"id": "6F2B…", "title": "Inbox", "url": "https://mail.example.com/", "agent": true}]},
  "testing": {"available": true, "running": false, "live": false}
}}
```

- `enabled` is false, with a `reason`, when the browser can't be used now, and its code in
  `reason_code` (one of the `browser_…` [error codes](#error-codes)), for the web app to show it
  translated.
- `available` says whether the browser's programs are installed.
- `live` says a live view is open.
- `stops_at` is when an unused browser stops; it is absent while a live view is open.
- `agent` marks the tabs the agent opened, which are the only ones it may close.

### `POST /api/me/browser/start`

`{"browser": "personal"}` (the default) or `"testing"`. Starts the browser if it isn't running,
and answers like `GET`. It answers `503` when the browser isn't installed, or when too many
browsers are running on the server.

### `POST /api/me/browser/stop`

`{"browser": "personal"}` or `"testing"`. Stops it, and answers `204`. The personal profile is
kept; the testing one is deleted.

### `GET /api/me/browser/live`

The personal browser's live view: a WebSocket (subprotocol `binary`, if asked) that carries
its display over RFB (VNC) both ways, for noVNC in the page.

- The upgrade is a `GET`, which the CSRF guard doesn't cover, and a browser sends cookies with
  a WebSocket to another site. So the request must come from this server's own pages:
  `Sec-Fetch-Site: same-origin`, or, without it, an `Origin` whose host is this server's.
  Anything else is refused with `403`.
- The browser must be running (start it first); if it isn't, the route answers `409`.
- At most 4 live views of one browser are open at once; another answers `429`.
- While the view is open, the browser doesn't stop for being idle.

## Conversations

A conversation with the agent belongs to one user in one workspace and is **private to
them**: nobody else sees it, admins included. Every route below answers `404` unless the
caller owns the conversation and is a member of its workspace now, the same as for one that
doesn't exist; a member who leaves the workspace loses access to their conversations there,
and gets them back on rejoining. (Conversations aren't in spaces yet; sharing one is a later
step.)

The agent runtime (`-agent`) keeps the conversations' content; the server keeps who owns
which. Routes that reach the agent runtime answer `503` while it is off (`the agent runtime
is off: start the server with -agent`) and, with `Retry-After`, when it isn't ready within a
few seconds (starting, or restarting after a crash). Listing, reading, renaming and archiving
need only the database.

A conversation whose content the agent runtime no longer has (its data was wiped, or
restored apart from the server's) answers `410` (`the agent runtime no longer has this
conversation's content`) on those routes. The server logs it, and an operator can remove it
([install.md](install.md#conversations-out-of-step-with-the-agent-runtime)).

A conversation as the API shows it:

```json
{"id": 7, "workspace_id": 1, "title": "Plans", "archived_at": null, "created_at": "…", "updated_at": "…", "cli_permissions": "ask"}
```

- `title`: up to 200 printable characters, trimmed; may be empty.
- `archived_at`: when the owner archived it, or `null`. An archived conversation can be read
  and streamed, but not sent to or switched to another model (`409`) until it is unarchived.
- `updated_at`: the last rename, archive or message.
- `cli_permissions`: on a CLI worker, `ask` (the default: the CLI asks the owner before it
  runs a command, edits a file or fetches a page) or `allow_all` (nothing asks). See
  [Permission questions](#permission-questions) and [agent-safety.md](agent-safety.md).
- `id` is the server's, not the agent runtime's (which the API never shows): a board card
  links a conversation by this `id`, as a string, and only its owner can add the link
  ([Cards](#cards)).

A **model** is `{"provider": "ws1-gateway", "model_id": "some-model"}`, one of those
`GET /api/workspaces/{workspace}/models` lists. A choice passes the rule in
[agent-providers.md](agent-providers.md#provider-ids-and-who-may-use-them): someone else's
personal provider, or another workspace's, is refused with `400 unknown model`, exactly like
a model that doesn't exist, so the answer doesn't reveal whose accounts exist.

A conversation on a **CLI worker** (`cli-claude`, `cli-codex`, `cli-grok`) runs each turn on that
CLI with the conversation owner's own login (`/api/me/cli-accounts`), never anyone else's: the
CLI's work streams in as the output of a `cli_turn` tool call, and its answer is the
assistant's message. The CLI session carries on from turn to turn; a conversation that moves
to a CLI from another model or CLI hands it the conversation so far. Without a connected
account the answer says so. When the login has expired, the tool's output says so (`⚠ … needs
you to sign in again`), the account turns `reconnect`, and the turn waits: once the owner signs
in again, it goes on by itself. A turn that a restart interrupted resumes with a `⟲` line.
When the CLI asks permission, the output shows `? Claude Code asks to use Bash: …`, then
`✓ allowed` or `✗ not allowed (…)` once the owner answers ([Permission
questions](#permission-questions)).

### `GET /api/workspaces/{workspace}/conversations`

The caller's conversations in the workspace, most recently updated first:
`{"conversations": [ … ]}`. Archived ones are left out; `?archived=true` lists only those.

### `POST /api/workspaces/{workspace}/conversations`

Creates one: `{}`, or with `"title"` and `"model"`. Without a model it runs on the agent
runtime's default (the operator's first, or the scripted offline model). `201` with the
conversation; `400` for a bad title or model.

### `GET /api/workspaces/{workspace}/conversations/{conversation}`

The conversation.

### `PATCH /api/workspaces/{workspace}/conversations/{conversation}`

Renames, archives or unarchives: `{"title": "New title"}`, `{"archived": true}`,
`{"archived": false}`, or several at once. `{"cli_permissions": "allow_all"}` lets a CLI act
without asking in this conversation, from its next turn, and `"ask"` turns the questions
back on. Archiving an archived conversation keeps its first `archived_at`. `200` with the
conversation; `400` for an empty body, a bad title or a bad `cli_permissions`.

### Permission questions

A CLI turn in an `ask` conversation waits while the CLI asks whether it may use a tool
([agent-safety.md](agent-safety.md#3-permission-prompts-unless-the-user-turns-them-off)).
Only the conversation's owner sees and answers its questions: the same `404` as for any
other route for anyone else. A question nobody answers is denied after 20 minutes, or when
the turn ends.

#### `GET /api/workspaces/{workspace}/conversations/{conversation}/cli-permissions`

The questions waiting, oldest first:

```json
{"requests": [{"id": "9f2c…", "turn": "c7-…", "cli": "claude", "tool": "Bash", "input": "npm test", "asked_at": "…"}]}
```

`input` is what the tool would do, cut to 2 KB: a command, or the tool's input as JSON. Show
it as text.

#### `POST /api/workspaces/{workspace}/conversations/{conversation}/cli-permissions/{id}`

Answers one: `{"allow": true}` or `{"allow": false}`. `204`; `404` for a question that isn't
waiting (answered already, timed out, its turn over) or isn't this conversation's; `400`
without `allow`.

### `POST /api/workspaces/{workspace}/conversations/{conversation}/messages`

Sends a message: `{"content": "…", "when_busy": "steer", "request_id": "…", "browser_locale": "pt-BR"}`.

- `content`: not empty.
- `when_busy`: what to do while the agent is working: `follow_up` (the default) waits for the
  current work to finish; `steer` joins the running work at its next step.
- `request_id`: optional, up to 128 printable ASCII characters. A retry with the same ID in the
  same conversation doesn't send the message again, and answers the first submission.
- `browser_locale`: optional, the browser's language, a tag of the same form as
  `PATCH /api/me`'s `locale`. The agent replies by default in the user's own pick of
  language, else this one, else the workspace's default ([i18n.md](i18n.md#the-agent-and-dictation));
  the message carries that language to the agent runtime.

`202 {"submission_id": 12}`; the answer arrives through the events stream. `409` for an
archived conversation.

Sending and switching the model check access and the archive again just before passing the
request to the agent runtime, after any wait for it to be ready. An archive, or a removal from
the workspace, that commits while that call is in flight doesn't stop it. That is accepted:
the message ends up as if it had been sent a moment earlier. Archiving doesn't stop a running
turn either, and someone removed can't read the conversation any more. Closing the window
would mean holding the database's write lock across the call, which would stall every write
on the server whenever the agent runtime is slow.

### `GET /api/workspaces/{workspace}/conversations/{conversation}/messages`

What the model sees next, simplified to role and text:
`{"messages": [{"role": "user", "text": "…"}, {"role": "assistant", "text": "…"}]}`.

### `GET` and `PUT /api/workspaces/{workspace}/conversations/{conversation}/model`

`GET` answers the conversation's model. `PUT` with a model switches to it from the
conversation's next model request on, also in the middle of a run: `200` with the model;
`400 unknown model` for one the caller may not use or that isn't configured; `409` for an
archived conversation.

### `POST /api/workspaces/{workspace}/conversations/{conversation}/stop`

Stops the agent: messages still waiting are withdrawn and the running work is aborted; the
conversation goes on with the next message. `204` once the agent is idle. An archived
conversation can be stopped too, since archiving doesn't stop a running turn. A stop carries
on if the client goes away before the answer, for up to 30 seconds.

### `GET /api/workspaces/{workspace}/conversations/{conversation}/entries`

The conversation's history before the events stream's window, a page at a time:
`?before=<entry ID>` (the oldest entry the client has; without it, the newest entries) and
`?limit=` (1–100, default 50). `{"entries": […], "before": 41}`: the entries just before
`before`, oldest first, in the same shape as the view's `entries`, and `before`, the ID of the
entry just before the page, absent when the page starts the conversation. For the page before
that, ask with the page's first entry's ID as `before`. `400` for a bad `before` or `limit`.

### `GET /api/workspaces/{workspace}/conversations/{conversation}/events`

The conversation as it changes, as [Server-Sent
Events](https://html.spec.whatwg.org/multipage/server-sent-events.html)
(`text/event-stream`, for `EventSource`). Each event's `data` is one JSON object, passed on
from the agent runtime unchanged:

1. `{"type": "value", "value": {…}}`: the conversation's view (its newest entries).
2. `{"type": "ops", "ops": […]}`: the changes of one or more commits, as Chord operations.
   Apply them with `applyImmutable(view, ops)` from `@earendil-works/chord/delta`; the
   server doesn't read them.
3. `{"type": "end", "kind": "…"}`: the stream is over. `stopped`, `aborted`, `ended`,
   `listener_error` and `failed` come from the agent runtime; `interrupted` is the server's,
   when the agent runtime restarted or broke the stream, the caller lost access, or the server
   is shutting down.

After an end, or any dropped connection, connect again: a new stream starts from the current
view, so nothing is missed. The stream starts with `retry: 2000`, so `EventSource`
reconnects after 2 seconds; it doesn't reconnect after a `404` or `503`, so a client retries
those itself. A comment line (`: heartbeat`) comes every 15 seconds, which keeps proxies from
closing a quiet stream; access is checked again each time (losing access ends the stream; a
database error only does after three checks in a row). A user holds at most 16 streams at
once, across their conversations: one more is `429`. A stream has no time limit, but a
client that leaves one event unread for a minute is disconnected. Events are passed on one at
a time, so a slow client slows the agent runtime's stream, which merges the changes it
holds back rather than queueing them.
