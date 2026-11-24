# The audit log

The server keeps a log of security and admin actions: who did what to which thing, when, and
from which address. It answers "who removed this member?", "who changed the model provider?"
and "did someone try to guess a password?" after the fact. Before it, such events only went to
the server's log, which nobody could search by workspace and which rotates away.

This page is about what is recorded and how. There is no screen or API for reading the log yet:
the table is written, and kept, so that the admin view has history to show from the day it
ships.

## What an event holds

Each event is one row of `audit_events` (`internal/db/migrations/0035_audit_events.sql`):

| Column | What |
|---|---|
| `at` | when, UTC, to the millisecond |
| `workspace_id` | the workspace the event belongs to; empty for one that belongs to none (a login, a password change, a personal provider account) |
| `actor_id` | the user who did it; empty for an anonymous attempt (a login to an account that doesn't exist, a refused single sign-on) or once that user is deleted |
| `action` | a stable snake_case code, from the table below |
| `target_type`, `target_id` | what it was done to: a `user`, `invite`, `space`, `access_token`, `provider_account`, `shared_cli_account` or `workspace` |
| `details` | a small JSON of non-secret facts: a role before and after, the names of the fields that changed, a provider's host |
| `client_addr` | the address the request came from, found as the login limits find it (the direct peer, or the client behind a [trusted proxy](install.md)); empty for an event with no request |

A workspace's events are deleted with it. An index serves the log of one workspace, newest
first.

## What is recorded

| Action | When | Workspace | Target | Details |
|---|---|---|---|---|
| `setup_completed` | first-run setup | the new one | workspace | |
| `login_succeeded` | a password login | | user | `method` |
| `login_failed` | a wrong password, or an account with no password or that doesn't exist | | user, if it exists | `method`, `reason` |
| `logout` | logging out of a live session | | user | |
| `password_changed` | a user changes their own password | | user | |
| `password_change_failed` | the current password was wrong | | user | |
| `password_reset_created` | an admin makes a reset link | the admin's | the member | |
| `password_reset_used` | a reset link sets a password | | user | |
| `sso_login_succeeded` | a single sign-on login | | user | `provider` |
| `sso_login_failed` | a refused single sign-on login | | | `provider`, `reason` |
| `sso_identity_linked` | an identity is linked to an account | | user | `provider`, `created_account` |
| `workspace_updated` | an admin changes the workspace | the workspace | workspace | `fields`, `locale` |
| `invite_created`, `invite_revoked` | an admin invites someone, or withdraws it | the invite's | invite | `email`, `role` |
| `invite_accepted` | an invite is used, by a new account or an existing one | the invite's | invite | `role`, `already_member` |
| `member_role_changed` | an admin changes a member's role | the workspace | user | `from`, `to` |
| `member_removed` | an admin removes a member | the workspace | user | `role` |
| `access_token_created`, `access_token_revoked` | a member makes or revokes their own token | the token's | access token | `name`, `expires` |
| `space_created` | an admin makes a shared space | the space's | space | `name` |
| `space_updated` | rename, archive, offline downloads | the space's | space | `fields`, and each new value |
| `space_member_added`, `space_member_role_changed`, `space_member_removed` | a shared space's members | the space's | space | `user_id`, `role`, or `from` and `to` |
| `provider_account_created`, `_updated`, `_removed` | a model provider account, a workspace's or a person's own | the account's, none for a personal one | account | `name`, `kind`, `scope`, `base_host`, `fields`, `api_key_set` or `api_key_changed` |
| `shared_cli_account_created`, `_updated`, `_removed` | a workspace's shared CLI account | the workspace | account | `cli`, `label`, `base_host`, `fields`, `api_key_set` or `api_key_changed` |
| `cli_account_modes_changed` | the default or members' modes for shared CLI accounts | the workspace | workspace | `default_mode`, `members_changed` |
| `search_embeddings_changed` | the workspace's embeddings model is set or turned off | the workspace | workspace | `enabled`, `account`, `provider`, `model` |
| `search_consent_changed` | a space is consented to, or withdrawn from, the embeddings endpoint | the space's | space | `enabled` |
| `dictation_settings_changed` | an admin saves the workspace's dictation settings | the workspace | workspace | `engine`, `language`, `api_model`, `base_host`, `api_key_changed`, `api_key_set` |

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
- Personal provider accounts (a member's own) have no workspace, so they are in no workspace's
  log. The person did them; they aren't the admins' to review.

## What is never recorded

No password (current, new or wrong), session cookie, setup token, invite token, reset link or
its token, access token, API key, provider key, or the body of any request or email. A reset link
event says that an admin made one, and for whom; it never holds the link. A key change says
that the key changed (`api_key_changed`), not what it is. A URL is kept as its host
(`base_host`), never its path, query or credentials.

The code enforces it, so a future change can't leak by accident:

- `audit.Record` refuses details whose names say they are a secret (`password`, `token`, `key`,
  `secret`, `hash`, `link`, `cookie`, `session`…) unless the value is a boolean, and refuses any
  value that isn't a short string, a number, a boolean or a short list of strings. A call that
  does either fails, and its change is rolled back, so the mistake shows in the tests.
- Strings are cut to 200 characters, and the details to 16 entries.
- Tests run each family's flow with known secret values and check that none of them is in any
  column of any row (`audittest.NoSecrets`). Add the new action's secrets to such a test when
  you add an action.

## Retention

Events older than the retention are deleted by a sweep that runs when the server starts and
then every hour, in small batches. The default is **365 days**. The operator sets it:

| Flag | Environment | Config file | Default |
|---|---|---|---|
| `-audit-retention` | `TASKTO_AUDIT_RETENTION` | `audit_retention` | `365` |

A number of days from 1 to 36500, or `off` to keep every event. Shortening it removes the
older events within the hour. Backups contain the log like the rest of the database.

## For code that adds an action

- Record the event inside the change's transaction with `audit.Record(ctx, tx, audit.Event{…})`.
  The actor and client address come from the request's context: the auth middleware puts the
  user there, and the server puts the address there for every request. Pass `Actor` when the
  actor isn't the request's user (accepting an invite as a new account).
- For an event with no transaction, use `audit.Log`, which logs a failure and returns nothing.
- Add the code to `internal/audit/actions.go`, and a test that does the action, checks the
  row (actor, workspace, target, details), checks that a refused request records nothing, and,
  if the action handles a secret, runs `audittest.NoSecrets`. Add it to the table above.
- A code is stable once released: a different meaning gets a new code.

`audit.List` reads events (by workspace and action, newest first) for the tests and for the
admin view; it checks no access, so whoever calls it decides who may see which workspace's
events.
