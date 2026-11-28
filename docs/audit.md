# The audit log

The server keeps a log of security and admin actions: who did what to which thing, when, and
from which address. It answers "who removed this member?", "who changed the model provider?"
and "did someone try to guess a password?" after the fact. Before it, such events only went to
the server's log, which nobody could search by workspace and which rotates away.

This page is about what is recorded and how, and who may read it. Admins read their workspace's
log in Settings, under Audit log, or through `GET /api/workspaces/{workspace}/audit`
([api.md](api.md#audit-log)); see [Reading the log](#reading-the-log).

## What an event holds

Each event is one row of `audit_events` (`internal/db/migrations/0036_audit_events.sql`):

| Column | What |
|---|---|
| `at` | when, UTC, to the millisecond |
| `workspace_id` | the workspace the event belongs to; empty for one that belongs to none (a login, a password change, a personal provider account) |
| `actor_id` | the user who did it; empty for an anonymous attempt (a login to an account that doesn't exist, a refused single sign-on) or once that user is deleted (`actor_deleted` then says which) |
| `actor_deleted`, `target_deleted` | 1 when the actor's account, or the account the event is about, was [deleted](#deleted-accounts) |
| `action` | a stable snake_case code, from the table below |
| `target_type`, `target_id`, `target_created_at` | what it was done to: a `user`, `invite`, `space`, `access_token`, `provider_account`, `shared_cli_account`, `integration`, `skill` or `workspace`, and when that object was created (see below) |
| `details` | a small JSON of facts the action declares: a role before and after, the names of the fields that changed, a provider's host. Never a name or other text a person typed (a skill's `name` is the one exception: the identifier that says which skill, recorded as the person gave it; its title, description and instructions are never recorded) |
| `client_addr` | the address the request came from, found as the login limits find it (the direct peer, or the client behind a [trusted proxy](install.md)); empty for an event with no request |

A workspace's events are deleted with it. An index serves the log of one workspace, newest
first.

### Which object an event is about

SQLite hands the id of a deleted row to the next one inserted when the row had the highest id,
and several of these tables work that way (a provider account, a shared CLI account, an access
token, an invite, a space…). So a retained event's `target_id` alone can later point at another
object. Each event carries the target's own `created_at` as `target_created_at`: the object an
event is about is the one with the same type, id **and** creation time. It is read from the
target when the event is recorded, or, when the action deletes the target, from the delete itself
(`RETURNING created_at`), so it is the same value in both cases. The creation time is a
timestamp to the millisecond and is not secret; it needs no change to the tables of the objects.
It is empty only when the target had no row to read. Two objects with one id made in the same
millisecond could not be told apart, which needs a delete and a create within a millisecond.

## What is recorded

| Action | When | Workspace | Target | Details |
|---|---|---|---|---|
| `setup_completed` | first-run setup | the new one | workspace | |
| `login_succeeded` | a password login: with two-factor login on, once its code is given (the password alone makes a challenge, which is not a login) | | user | `method` |
| `login_failed` | a wrong password, or an account with no password or that doesn't exist, or a right password for a disabled account, or a wrong code at the second step of a login | | user, if it exists | `method`, `reason` (`wrong_code` for the second step, `account_disabled` for a disabled account; the account is the target; no actor, since whoever typed isn't known to be its owner) |
| `session_revoked` | a user signs out one of their other sessions from Settings | | user | |
| `other_sessions_revoked` | a user signs out all their other sessions (none ended records nothing) | | user | `logins_ended` |
| `logout` | logging out of a live session: one that has expired is no logout, and records nothing | | user | |
| `password_changed` | a user changes their own password | | user | |
| `password_change_failed` | the current password was wrong | | user | |
| `password_reset_created` | an admin makes a reset link | the admin's | the member | |
| `password_reset_used` | a reset link sets a password | | user | |
| `two_factor_enabled` | a user confirms their [two-factor login](api.md#two-factor-login) with a code | | user | |
| `two_factor_disabled` | a user turns it off, with their password and a code | | user | |
| `two_factor_recovery_codes_replaced` | a user makes new recovery codes | | user | |
| `two_factor_cleared` | an admin clears a member's, who lost both their app and their recovery codes | the admin's | the member | |
| `password_reset_requested` | someone asked for a reset link by email from the login page, and an account with a password has the address (an address no account has records nothing, so the log can't be filled from outside) | | user | `emailed`: whether the SMTP server took the message (no actor, since whoever asked isn't known to be the account's owner) |
| `sso_login_succeeded` | a single sign-on login | | user | `provider` |
| `sso_login_failed` | a refused single sign-on login | | | `provider`, `reason` |
| `sso_identity_linked` | an identity is linked to an account | | user | `provider`, `created_account` |
| `workspace_created` | a person creates a workspace (`POST /api/workspaces`) | the new one (in its own log, not the first workspace's) | workspace | `allowed_by` (`admins` or `anyone`: the setting that let them) |
| `workspace_updated` | an admin changes the workspace | the workspace | workspace | `fields`, `locale` |
| `invite_created`, `invite_revoked` | an admin invites someone, or withdraws it | the invite's | invite | `email` (the full address), `role` |
| `invite_emailed` | an admin had an invite's link emailed to its address, and the SMTP server took the message (the invite itself is `invite_created`) | the invite's | invite | |
| `invite_accepted` | an invite is used, by a new account or an existing one | the invite's | invite | `role`, `already_member` |
| `member_role_changed` | an admin changes a member's role | the workspace | user | `from`, `to` |
| `account_deletion_requested` | a person deletes their own account ([Deleting an account](api.md#deleting-an-account)); one event in each workspace the account belongs to | each of them | user | `purge_days` (the grace period), `logins_ended` |
| `account_deletion_cancelled` | an operator cancels it before the purge | each of the account's workspaces | user | |
| `account_purged` | the grace period ended and the account was deleted for good | each of the account's workspaces | user (the account, deleted) | how many of each the purge deleted: `private_spaces`, `conversations`, `access_grants` (access tokens), `cli_accounts`, `push_subscriptions`, `documents_anonymized` (commits whose author was replaced), `mentions_anonymized` (comments changed), `uploads_removed` (unfinished uploads); a purge that took several tries counts what each did, once: the rewriting and the uploads are done before the account's transaction, and their counts are kept with the work (an upload's in the transaction that deletes it, a space's before its rewrite is journaled), and `cleanups_pending` (steps still to do when the account went: the providers file's rewriting, and those that wait for the agent runtime or the CLI workers to be on). No name and no address |
| `member_disabled`, `member_enabled` | an admin switches a member's account off or on (one already in that state records nothing) | the workspace the admin asked in | user | `logins_ended` on a disable: the sessions it signed out |
| `member_removed` | an admin removes a member | the workspace | user | `role`; the tokens and shared-space seats that go with it are events of their own (see below) |
| `access_token_created`, `access_token_revoked` | a member makes or revokes their own token | the token's | access token | `expires`; `cascade` on one revoked by a member's removal |
| `access_token_used` | a token authenticates a request, at most once a minute (the cadence of the token's `last_used_at`), through the REST API or MCP | the token's | access token | |
| `space_created` | an admin makes a shared space | the space's | space | |
| `space_updated` | rename, archive, offline downloads | the space's | space | `fields`, and the new `archived` and `offline_downloads` |
| `space_member_added`, `space_member_role_changed`, `space_member_removed` | a shared space's members | the space's | space | `user_id`, `role`, or `from` and `to`; `cascade` on one removed with the member |
| `provider_account_created`, `_updated`, `_removed` | a model provider account, a workspace's or a person's own | the account's, none for a personal one | account | `kind`, `scope`, `base_host`, `fields`, `api_key_set` or `api_key_changed` |
| `data_export_started` | a member starts the export of their own data (`GET …/export`); one event per export that began, none for one turned away by its limits | the workspace | user (the exporter) | |
| `workspace_export_started` | an admin starts the export of the workspace's shared spaces they are a member of and its member list; one event per export that began, none for one turned away by its limits | the workspace | workspace | `spaces`: how many shared spaces it holds (never their names) |
| `server_update_requested` | an operator presses "Update now" in Settings → About; one event per request that was written, none for one refused (a pending request, a running update, a channel that is off, a host that can't take one) | the workspace they asked in | workspace | `channel`: the updater's channel, `release` or `main` |
| `shared_cli_account_created`, `_updated`, `_removed` | a workspace's shared CLI account | the workspace | account | `cli`, `base_host`, `fields`, `api_key_set` or `api_key_changed`; `members_signed_out` on removal |
| `cli_account_modes_changed` | the default or members' modes for shared CLI accounts changed (a request that changes nothing records nothing) | the workspace | workspace | `default_mode` if it changed, `members_changed` |
| `search_embeddings_changed` | the workspace's embeddings model is set or turned off, or its account is removed | the workspace | workspace | `enabled`, `account`; `cascade` when the account's removal turned it off |
| `search_consent_changed` | a space is consented to, or withdrawn from, the embeddings endpoint, or its consent goes with the account | the space's | space | `enabled`; `cascade` |
| `dictation_settings_changed` | an admin saves the workspace's dictation settings | the workspace | workspace | `engine`, `language`, `base_host`, `api_key_changed`, `api_key_set`, `notice_confirmed` |
| `integration_created` | an admin connects an integration | the integration's | integration | `provider`, `kind`, `mode`, `api_key_set`, `webhook_secret_set` |
| `integration_updated` | an admin changes an integration's display name, settings, ceiling, currencies or secrets (a request that changes nothing records nothing; a secret that is sent counts as changed, even if it is the one already stored) | the integration's | integration | `provider`, `mode`, `fields`, `api_key_changed`, `webhook_secret_changed` |
| `integration_disabled`, `integration_enabled` | an admin switches an integration off or on | the integration's | integration | `provider` |
| `integration_spaces_changed` | an admin changes which spaces may use an integration | the integration's | integration | `provider`, `added`, `removed` (counts, never the spaces) |
| `integration_spaces_kept` | an admin keeps a space an app's approval allowed, so it stays allowed after the approval ends (saving the list never does; one event per request that kept any) | the integration's | integration | `provider`, `kept` (a count, never the spaces) |
| `integration_new_link_allowed` | an admin checked the provider, confirmed that no payment link exists for a payment whose outcome never became known, and allowed exactly one new try ([Payment links](integrations.md#payment-links)) | the integration's | integration | `provider`, `app_id`, `effect_id` (the entry's number; never the amount or the record) |
| `integration_removed` | an admin deletes an integration | the integration's | integration | `provider`, `kind` |
| `app_integration_requested` | someone who may publish an app asks for its contract to be approved (asking again for one that waits or is approved records nothing) | the workspace | app integration approval | `kind`, `app_id` (never the slot's name) |
| `app_integration_approved` | an admin approves a contract and binds an integration | the workspace | app integration approval | `kind`, `app_id`, `integration_id`, `provider`, `space_allowed` (whether approving also allowed the space) |
| `app_integration_rejected` | an admin turns a request down | the workspace | app integration approval | `kind`, `app_id` |
| `app_integration_revoked` | an admin withdraws an approval | the workspace | app integration approval | `kind`, `app_id`, `integration_id` |
| `skill_created`, `skill_updated`, `skill_removed` | a skill of a shared space or of the workspace is made, changed or removed ([skills](skills.md)): they change what the agent is told for other people. A person's own skills, which change nobody else's access, record nothing | the skill's | skill | `scope` (`space` or `workspace`), `space_id` (a space's), `name`; `fields` on an update (`name`, `title`, `description`, `body`, `enabled`: which changed, never the text) |

The names a person gives things (a token's, a space's, a shared account's label, an integration's name, a model's) are
not recorded: they are free text, and people paste secrets into such boxes. The one exception is a skill's `name`, the
identifier that says which skill an event is about. It is recorded as stored: as the person typed it, after leading and trailing spaces and tabs are trimmed. Its pattern (a lowercase letter,
then lowercase letters, digits and hyphens, 2 to 40 characters) limits its syntax and does not detect a secret: a value of
that shape, such as a token in hexadecimal, is accepted and kept in the log, so a skill's name must never hold a secret. A skill's title is never recorded or shown in the log, since it can be that of a space the reader may not open. The target's id
says which object it was, and `fields` says what about it changed.

Notes:

- **A change and its event commit together.** The event is written in the same database
  transaction as the change, so a change that fails leaves no event, and one that commits
  always has one. A refused request (not an admin, not found, invalid) changes nothing and
  records nothing.
- **Failures are the exception.** A failed login, a wrong current password and a refused
  single sign-on have no change to commit with: they are recorded on their own, and if that
  write fails the server logs it and still answers the request. Logins the rate limiter turns
  away are not recorded, so an attacker can't fill the log faster than the limit allows.
- **Invitees' addresses are recorded** with an invite, since the invite itself is deleted when
  it is withdrawn. The address typed into a failed login is **not**: it is often a password in
  the wrong box.
- **What a change takes with it is recorded.** Removing a member deletes their access tokens in
  the workspace and their seats in its shared spaces, by the schema's triggers; each is an event
  in the same transaction (`access_token_revoked` and `space_member_removed`, with
  `cascade: true` and the admin as the actor). Removing the model provider account that is the
  workspace's embeddings model deletes the search setting and the spaces' consents that name
  it; those are a `search_embeddings_changed` and a `search_consent_changed` with `enabled:
  false` and `cascade: true`. Removing a shared CLI account signs its users out of it, which its
  event counts (`members_signed_out`). Notifications and push subscriptions that go with a
  membership are not security state and are not recorded.
- **Concurrent changes record what happened.** A shared CLI account is read again inside the
  transaction that updates or deletes it, and the write is scoped to its workspace and checked
  for a row affected: an update or delete of an account that is already gone (or whose id now
  belongs to another workspace's) is a 404 with no event, so two deletes make one removal and an
  event never names the wrong workspace.
- Personal provider accounts (a member's own) have no workspace, so they are in no workspace's
  log. The person did them; they aren't the admins' to review.

## Reading the log

An admin of a workspace reads that workspace's events: the settings tab and the API route behind
the same check as the other admin routes (a member is refused, anyone outside the workspace
gets not-found, and an access token is refused). They can filter by action, by who did it, by
what kind of object it was done to and by time, and page back through the events with a cursor.

- **Only the workspace's own events.** The route asks for the events of the workspace the
  caller administers, taken from their membership, never from the request's parameters.
  `audit.ListWorkspace` refuses to run without a workspace, so a missing one is an error and
  not "every workspace"; the unrestricted `audit.List` is for tests and nothing that answers a
  request uses it.
- **Events of no workspace are not shown.** A login, a password change or reset use, a refused
  single sign-on and a person's own provider account belong to a person, not to a workspace.
  An admin does not see when members sign in, from where, or when they fail to: that would let
  an admin watch people across everything else they belong to. If the workspace's admins should
  see members' sign-ins, it is a decision to make on its own (it joins events to members by
  current membership, so it would show a person's sign-ins to every workspace they are in,
  which is more than their own workspace's).
- **Names are read now, and only for the workspace.** The log keeps ids, not names. The reply
  names an actor or a user an event is about while they are a member of the workspace; once they
  have left, only their id is shown. A space, a provider account or a shared CLI account is named
  only if it belongs to the workspace and is the object the event was about (its id and creation
  time match); an access token's or an invite's name is not shown (an invite's address is in the
  event's details). A name changed since the event shows as it is now.
- **Pages.** Newest first, 50 events to a page (at most 200), by a cursor that holds the time and
  id of the last event, so events recorded while someone pages don't shift the pages.
- **Reading is not recorded.** Looking at the log makes no event.

## Deleted accounts

When an account is purged ([Deleting an account](api.md#deleting-an-account)) its events stay, without
the person, in the same transaction that deletes the account:

- the events it did lose their `actor_id` (the schema sets it to null with the account), which would
  read like an anonymous attempt; they are marked `actor_deleted`, and the log shows "Deleted user";
- the events about the account (`target_type` `user`, matched by its id *and* its creation time,
  since an id is never given again, the creation time makes it certain) are marked `target_deleted`;
- the network address each of their events was made from (`client_addr`) is cleared;
- the invitations to the account's address, which hold the address in their details, lose it
  (`invite_created`, `invite_revoked`);
- nothing else in an event is about a person: details are typed and name no one but by the number
  of an account or a space. The purge's own event, `account_purged`, holds counts only.

So after a purge no event holds the person's name or address. Backups made before it still do
(see [install.md](install.md#privacy-and-account-deletion)).

## What is never recorded

No password (current, new or wrong), session cookie, setup token, invite token, reset link or
its token, access token, API key, provider key, or the body of any request or email. A reset link
event says that an admin made one, and for whom; it never holds the link. A key change says
that the key changed (`api_key_changed`), not what it is. A URL is kept as its host
(`base_host`), never its path, query or credentials.

The code makes the details narrow, so a future change can't leak by accident:

- Each action has a fixed set of details, each of a fixed type (`internal/audit/schema.go`):
  a boolean, a number, a value from a fixed list (a role, a mode, a CLI), the names of fields
  from a fixed list, or a short value checked against a pattern (a host, an invite's address, a
  tag such as an SSO provider's id or a reason code). `audit.Record` refuses an action without a
  schema, a detail the action doesn't declare, or a value of another type or that doesn't match.
  It fails, and its change is rolled back, so the mistake shows in the tests.
- No detail is free text. The values of the pattern-checked kinds are ones a person doesn't
  type freely (an address and a host pass their patterns; a pasted token doesn't), or come from the
  code or the operator. A name a person types is never a detail, except a skill's `name`, which the schema checks
  against the pattern of a skill's identifier. The pattern limits the name's syntax; it doesn't keep a secret out of it.
- A detail named like a secret (`password`, `token`, `key`, `secret`, `hash`, `link`, `cookie`,
  `session`…) must be a boolean, and a test checks every schema for it.
- Tests run each family's flow with known secret values and check that none of them is in any
  column of any row, as stored and as decoded from the details' JSON (`audittest.NoSecrets`).
  Add the new action's secrets to such a test when you add an action.

## Retention

Events older than the retention are deleted by a sweep that runs when the server starts and
then every hour, in small batches. The default is **365 days**. The operator sets it:

| Flag | Environment | Config file | Default |
|---|---|---|---|
| `-audit-retention` | `TASKTO_AUDIT_RETENTION` | `audit_retention` | `365` |

A number of days from 1 to 36500, or `off` to stop deleting events by age. (`off` doesn't keep
the events of a workspace that is deleted: they go with it.) Shortening it removes the older
events within the hour. Backups contain the log like the rest of the database.

## For code that adds an action

- Record the event inside the change's transaction with `audit.Record(ctx, tx, audit.Event{…})`.
  The actor and client address come from the request's context: the auth middleware puts the
  user there, and the server puts the address there for every request. Pass `Actor` when the
  actor isn't the request's user (accepting an invite as a new account).
- For an event with no transaction, use `audit.Log`, which logs a failure and returns nothing.
- Add the code to `internal/audit/actions.go`, its details to `internal/audit/schema.go` (fixed
  and typed; never a name or other text a person typed, but a skill's `name`, an identifier recorded as stored, trimmed of leading and trailing spaces and tabs), and a test that does the action, checks the
  row (actor, workspace, target, details), checks that a refused request records nothing, and,
  if the action handles a secret, runs `audittest.NoSecrets`. Add it to the table above.
- An action that deletes its target passes `TargetCreated` from the delete's `RETURNING
  created_at`; one that doesn't gets it from the target's row. An action whose change deletes
  other rows (a trigger, `ON DELETE CASCADE`) reads them first and records each one, in the same
  transaction.
- A code is stable once released: a different meaning gets a new code.

`audit.ListWorkspace` reads one workspace's events for the admin view, with names, filters and a
cursor; `audit.List` reads them (by workspace and action) for tests, and a zero workspace in it
means every workspace's. Neither checks access: whoever calls them decides who may see which
workspace's events, and a route must use `ListWorkspace` with the workspace its access check
found. A new action needs its label in the web app (`settings.audit.actions.<code>` in each
language, and the list in `web/src/settings/AuditTab.tsx`); a test checks it against
`actions.go`, and the same for a new detail name or target type.
