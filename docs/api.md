# HTTP API

The API is JSON under `/api`. This page covers accounts, workspaces, members, spaces,
access tokens, boards, personal apps, model providers, dictation, the backups' status, the knowledge base and its search;
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
| `app_offline` | 409 | The app is offline: restore it to launch it or run its actions. |
| `app_changed` | 409 | The app changed since the client read it; `current` is the app as it is now. |
| `app_name_length` | 400 | An app's name must have 1 to `max` characters. |
| `too_many_apps` | 409 | The space has `max` apps. |
| `app_full` | 409 | The app holds `max_rows` rows or `max_mib` MiB of data. |
| `app_forbidden` | 403 | The space allows it, but the caller's roles in the app don't: `capability` is `design`, `publish`, `submit`, `modify_records` or `manage_roles`. |
| `app_roles_invalid` | 400 | The role assignments name an unknown role, give a user no roles or twice, or are too many. |
| `app_roles_not_member` | 400 | A role is for someone who can't read the space. |
| `app_roles_lockout` | 400 | The assignments would take the caller's own right to manage the app's roles. |
| `app_row_not_found` | 422 | The action names a row the app doesn't have (any more), or one the caller may not change. |
| `app_field_required` | 422 | The action leaves out `field`, which the app requires. |
| `app_field_too_long` | 422 | The value of `field` has more than `max` characters. |
| `app_field_not_number` | 422 | The value of `field` isn't a number. |
| `app_field_too_small` | 422 | The value of `field` is under `min`. |
| `app_field_too_large` | 422 | The value of `field` is over `max`. |
| `app_field_not_date` | 422 | The value of `field` isn't a date, `YYYY-MM-DD`. |
| `request_too_large` | 413 | The request's body is over `max` bytes. |
| `doc_not_found` | 404 | No such document, folder or version in the space. |
| `doc_name_invalid` | 400 | `name` can't be a path in a space: empty parts, `..`, `.git`, a leading `/`, backslashes or control characters. |
| `doc_path_too_long` | 400 | A path has at most `max` bytes. |
| `doc_path_too_deep` | 400 | A path has at most `max` levels of folders. |
| `doc_name_too_long` | 400 | A file or folder name has at most `max` bytes. |
| `doc_message_invalid` | 400 | A change's message is one line of at most `max` characters. |
| `doc_not_utf8` | 400 | The Markdown document `path` isn't UTF-8 text. |
| `doc_conflict` | 409 | The change conflicts with the space's documents as they are now. |
| `doc_changed` | 409 | `path` has changed since the version the change was based on. |
| `doc_deleted` | 409 | `path` was deleted since the version the change was based on. |
| `doc_exists` | 409 | `path` already exists: to replace it, send the version you edited as `base_version`. |
| `doc_case_conflict` | 409 | `name` differs only by case from `other`, which exists. |
| `doc_path_taken` | 409 | A folder is at `path`, or a file is where one of its folders would be. |
| `doc_move_into_itself` | 400 | A folder can't be moved into itself (`path`). |
| `doc_move_kind` | 400 | The unversioned file `path` can't become a Markdown document, or the other way round. |
| `doc_delete_folder` | 400 | `path` is a folder: delete its documents. |
| `file_too_large` | 413 | `path` is over the limit of `max` bytes. |
| `knowledge_full` | 507 | The space's knowledge base is full. |
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
| `egress_refused` | 400 | The server's egress policy doesn't allow the endpoint's `host` (also in a test's answer, and 502 for a dictation). |
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

### Sessions

Logging in (setup, login, accepting an invite as a new person, or setting a password, by
changing it or with a reset link) sets the session cookie: `__Host-taskto_session`, `Secure`, `HttpOnly`, `SameSite=Lax`, `Path=/`, lasting
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
  cross-origin rule above still applies to it. The one route that takes a file's bytes,
  [`PUT …/docs/{path}`](#put-apiworkspacesworkspacespacesspacedocspath), also takes
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
from `GET /api/me`.

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

### `POST /api/logout`

Revokes the session and clears the cookie. `204`, also without a session.

### `GET /api/me`

```json
{
  "user": {"id": 1, "email": "owner@example.com", "name": "Owner", "locale": "pt-BR"},
  "workspaces": [{"id": 1, "name": "Home", "role": "admin", "locale": ""}]
}
```

Workspaces are listed oldest first. Requires a login.

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

### `PATCH /api/workspaces/{workspace}`

Admins. `{"locale": "pt-BR"}` → the workspace as in `GET /api/me`
(`{"id", "name", "role", "locale"}`). Sets the workspace's default language, which members
get when their browser asks for none the web app has; `""` is English. Same tags as
`PATCH /api/me`.

## Passwords

A user changes their own password with the current one. An admin can make a reset link for
a member who lost theirs. Either way, every session of the account ends, and so do its
reset links; the browser that set the new password gets a new session. Access tokens keep
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
since the session is fine. Wrong current passwords count against the
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
only someone who administers every one of its workspaces can make one.

### `POST /api/password-reset`

No login needed. `{"token": "…", "password": "…"}` sets the password of the account the
link is for, ends its sessions and reset links, and logs it in here: `200` with the
`GET /api/me` body and the session cookie (a session the browser already had, anyone's,
is revoked). `400` for a password that is too short or too long, which leaves the link
usable. `404` if the token is unknown, used, replaced or expired.

## Members

### `GET /api/workspaces/{workspace}/members`

Any member. Members can see each other (boards need it to assign cards).

```json
{"members": [{"id": 2, "email": "member@example.com", "name": "Member", "role": "member", "joined_at": "…"}]}
```

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

`404` if the token is unknown, used, withdrawn or expired.

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

## Spaces

A space as the caller sees it:

```json
{
  "id": 4, "workspace_id": 1, "kind": "shared", "name": "Team",
  "archived_at": null, "created_at": "…",
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

Admin only, shared spaces only (`404` for private ones). Renames, archives or unarchives:
`{"name": "New name"}`, `{"archived": true}`, `{"archived": false}`, or several at once.
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
  bytes. Two names in a folder can't differ only by case (`409`).
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

- `?at=<commit>`: the document as it was at that commit (`404` if it didn't exist then).
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
(`?before=<commit>`).

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
`404` if the document didn't exist at that commit; `410` for an unversioned file whose
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

### `POST …/boards/{board}/cards`

Writers. `title` is required; everything else is optional:

```json
{
  "title": "Write the launch post", "body": "…", "column": "todo", "position": 0,
  "assignee": {"kind": "user", "id": 5}, "blocked_reason": "…", "blocked_by": [9],
  "docs": ["launch/post.md"], "conversations": ["7"]
}
```

`column` defaults to `backlog` and can't be `archived`. `position` is the card's 0-based
place in its column; left out or past the end, the card goes last. `201` with the card.

### `GET …/boards/{board}/cards/{card}`

The card with its thread, oldest first:

```json
{"id": 12, …, "comments": [{"id": 1, "card_id": 12, "author": {"id": 2, "name": "Member", "agent": false}, "body": "…", "created_at": "…"}]}
```

### `PATCH …/boards/{board}/cards/{card}`

Writers. Edits the fields it names, with the `version`: `title`, `body`, `assignee`,
`blocked_reason`, `blocked_by`, `docs`, `conversations`. `null` clears `assignee` and
`blocked_reason`; the lists replace the whole list (`[]` clears it). Setting the blocked
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
caller. `201` with the comment.

### The agent

The agent works boards through tools that call the same code, for the user it works for
and with exactly that user's access. See [boards-agent-tools.md](boards-agent-tools.md)
and [agent-tools.md](agent-tools.md).

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
`/api/workspaces/{workspace}/spaces/{space}/apps`, needs a session (`401`), goes through
the [CSRF guard](#csrf) unless it is a `GET`, and answers `404` for a space or app the
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

Every role may view the app (launch it and read its definition); a `builder` also manages
the app's roles. **Design** is changing the app's definition, renaming it and moving it;
**publish** is taking it offline and restoring it (release steps will extend it);
**submit** is adding a record (an `insert` action). **Own** records are those the caller
created, which includes the ones the agent created for them; records whose creator's
account was deleted are in nobody's own, and only readers of all see them. Every way of
reading records answers to the read scope: the screen's rows, its summaries (`count`,
`sum`), `rows` in an app's list and the agent's `list_app_rows`. A change to a record the
caller may not change is answered as `app_row_not_found`.

An app **without role assignments is unrestricted**, as every app was before roles: for a
user who may write the space it is every role, for one who may only read it, `reader`. So
nothing changes for an app until someone sets roles, and then only for the people it names.
An assignment is for one user, or with `"user_id": null` the default, for every member of
the space who has no assignment of their own. The agent acts as the user who started it, with
exactly their roles; it has no tool to set roles, and a definition grants none.

An app as one caller sees it has `access`, what they may do:

```json
"access": {"roles": ["user"], "restricted": true, "view": true, "design": false,
  "publish": false, "submit": true, "manage_roles": false,
  "read_records": "own", "modify_records": "own"}
```

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
  "definition": { … }
}
```

`rows` counts the collection's rows the caller may read (see [App roles](#app-roles)).
`access` is what the caller may do in the app. `definition` is only in a single app's reply.
`agent` is `true` when the agent made the change for that user.

### The definition

```json
{
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

- **components** is the screen, 1–300 A2UI components, one with the ID `root`, each
  `{"id", "component", …props}` with only the catalog's components and props
  (`internal/apps/catalog.go`): A2UI's basic catalog without `Image`, `Video`,
  `AudioPlayer` and the `openUrl` function (an app is shared, and a picture from
  elsewhere would tell that server who opens it, and could carry the app's data in its
  address), plus `Stat` (a labelled value). Props must have the catalog's types and enum
  values; dynamic values are a literal, a data binding `{"path": …}`, or a call of a
  catalog function returning the right type. Every component referred to must exist, no
  component may contain itself, and every button's event must be one of the app's
  actions. The web app's renderer runs with A2UI's strict validation too, against the
  same catalog.
- **collection.fields**, at most 50: `name` (a lowercase letter, then up to 39 lowercase
  letters, digits or `_`; not `id`, `items` or `draft`), `type` (`string`, `number`,
  `boolean`, or `date`: `YYYY-MM-DD`), `required`, `max_length` (strings: 1–10000,
  1000 when left out), `min` and `max` (numbers).
- **actions**, 1–50, map an event name to its `kind`: `insert` adds a row from the
  event's context; `update` changes the fields the context gives of the row whose `id` it
  gives (blank text clears a field); `delete` removes the row whose `id` it gives.
- **summaries**, at most 20: `{"kind": "count"}` or `{"kind": "sum", "field": …}` of a
  number field, shown as text.
- The whole definition is at most 48 KiB of JSON.

The data model an app's screen binds to: `/items` is the rows, each with its `id` and the
collection's fields; `/draft` is an empty form; each summary is at `/<name>`.

An app holds at most 5,000 rows and 4 MiB of row data (`409` beyond); a space at most
500 apps.

### `GET …/apps`

The space's apps, by name, without definitions: `{"apps": [ … ]}`. Offline apps are left
out unless `?archived=true`, after the others.

### `POST …/apps`

Writers. `{"name": "Expense tracker", "definition": { … }}` (a name of 1–100
characters). `201` with the app.

### `GET …/apps/{app}`

The app, with its definition.

### `PATCH …/apps/{app}`

Writers. With the `version`: `name` renames it and `definition` replaces its screen,
collection, actions and summaries (these take the design capability), `archived` takes it
offline (`true`) or restores it (`false`; the publish capability). A new definition must fit the app's rows: every row must still satisfy the
collection (`400` otherwise, naming the rows), so new fields are added as optional.
Fields no longer declared stay in the rows, unseen, and come back if declared again.
`200` with the app.

### `POST …/apps/{app}/move`

Writers of both spaces, with the design capability in the app. Its role assignments move
with it. `{"version": 1, "space_id": 7}`: moves the app, with its rows,
to another space of the workspace the caller may write (`404` for one they can't read or
of another workspace, `403` for one they may only read). `200` with the app.

### `GET …/apps/{app}/surface`

Launches the app: the A2UI messages that draw it with its rows, `createSurface`,
`updateComponents` and `updateDataModel`, for the surface `app-<id>` and the catalog
`https://taskto.ai/catalogs/apps/v1.json`. `409` while it is offline. The data model has
the rows the caller may read, and the summaries over those rows alone.

### `POST …/apps/{app}/actions`

Writers, and by the action's kind, the submit capability (`insert`) or the right to modify
the row (`update` and `delete`, of all records or only the caller's own; else
`403 app_forbidden`). The A2UI client action a button sent:
`{"name": "add_expense", "surfaceId": "app-9", "sourceComponentId": "add", "timestamp": "…", "context": {…}}`.
The server checks it against the app (`422` for an event the app doesn't declare,
another surface, a context field the collection doesn't have, a value that doesn't fit
its field, or a row the app doesn't have), applies it, and answers with the
`updateDataModel` message to apply: the rows the caller may read, an empty `/draft`, and the
summaries over them.
Form inputs arrive as text, so number and boolean fields accept their text too. `409`
while the app is offline.

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

Admins, with a browser session (not an access token). The status, error included:

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
