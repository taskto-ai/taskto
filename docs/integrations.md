# Integrations

Design note: let a workspace connect an account at an outside service, so a personal app can
one day ask for a payment link and be told the payment happened. An **integration** is the
admin's connection to one provider account: its credentials, its limits and the spaces that
may use it. The first kind of integration is **payments**; Stripe and Square are the first
providers.

This page describes what the server does today, the rules every later part keeps, and, marked
as such, what is still to come. The routes are in [api.md](api.md#integrations).

## What exists today

- The **closed registry** of providers, in the server's code ([Providers](#providers)).
- **Integrations**: an admin connects an account, changes it, switches it off and on, and
  removes it, in Settings → Integrations or through the API.
- **Sealed, write-only credentials** ([Secrets](#secrets)).
- **Limits** the admin sets, never an app: the largest amount of one payment and the
  currencies a payment may ask for.
- **Allowed spaces**: which spaces' apps may use an integration. A new integration has none.
- The **address of the webhook**, shown when the server has a public URL.
- The **egress policy** for calls to providers and the **request builder** that pins each
  adapter's hosts ([Egress](#egress)).
- **Audit events** for every change ([audit.md](audit.md)).

- **Slots in an app's definition**, the **contract** the server works out from them and the
  **approval** by an admin of that contract's hash, which a publish and a rollback need
  ([Apps and approval](#apps-and-approval)).

Nothing calls a provider yet: no payment link is made, no event is received, and a
`create_payment_link` effect that an app declares does nothing when an action runs. Those arrive
as separate changes, in this order: the payment link; the webhook and its verification; applying a
payment to a record; polling the provider for payments the webhook missed. The rules below are the
ones those changes have to keep.

## Concepts

| Term | Meaning |
|---|---|
| **Provider** | An entry of the registry in Go (`stripe`, `square`): the secrets and settings it needs, its fixed API hosts, the kinds it implements. |
| **Kind** | A closed class of operations an app may use. Today `payments`: create a payment link, and four normalized events (`payment.succeeded`, `payment.expired`, `payment.failed`, `payment.refunded`). |
| **Integration** | A workspace admin's connection to one provider account: sealed secrets, settings, limits, the spaces that may use it, and a random `public_id` for its webhook address. |
| **Slot** | A name in an app's definition (`payments`) of a kind. It holds no id and no secret; the admin binds it to an integration when approving the app. |
| **Contract** | What the server derives from an app's draft for one slot: what it sends out and which fields the provider's events may write. An admin approves one by its hash. |
| **Approval** | An admin's decision, for one app, slot and contract hash, naming the integration the slot uses. |

An app never names a provider or an integration, and nothing in a definition, an action or
an effect holds a URL, a header, a method, a body, a secret or a limit. The providers are a
closed registry, not data: an operator, an admin or an app can't add one.

## Providers

The registry is the list in `internal/integrations/registry.go`. Each provider says:

- its **kinds**: `payments` for both;
- its **secrets**: Stripe takes an `api_key` (required) and a `webhook_secret`; Square takes
  an `api_key` (required) and a `webhook_secret`. The webhook secret is optional because an
  install without a public URL never receives a webhook (reconciling payments without webhooks is a later change, not built yet);
- its **settings**, which are short identifiers or choices, never a URL or free text: Stripe
  has none; Square has `environment` (`sandbox` or `production`) and `location_id`;
- how to **recognise its credentials**: a Stripe secret key starts with `sk_` or `rk_` (a
  restricted key) and `test_` or `live_`, a webhook secret with `whsec_`. A publishable key
  (`pk_`) is refused where a secret one is needed. The check is of the shape only, it never
  asks Stripe, and an error never quotes the value;
- its **mode**, `test` or `live`: read from the Stripe key's prefix and from Square's
  `environment`: Stripe's label can't disagree with its key, and Square's reflects the configured
  environment, not a proof that the stored token belongs to it (nothing asks Square until its
  first call), so replace the token when the environment changes;
- its **API hosts**, constants of the adapter (`api.stripe.com`; Square's production host or
  its sandbox host, by `environment`), and the **API version** it is written against, which
  Stripe's adapter pins (`Stripe-Version`) and Square's adapter pins when it makes its first call.

A new provider is a change of code and tests, never a migration: the `provider` column is
validated in Go.

## Secrets

- Credentials are sealed with the server's key (`internal/serverkey`, AES-256-GCM) with
  associated data that names the workspace, the integration (its row id, which is never
  reused, and its public id) and the secret, so a sealed value copied to another row,
  workspace or name doesn't open.
- They are **write-only**. The API answers `"set"` or `"not set"` for each. A secret is
  replaced by sending a new one and can't be read or removed; to stop using an account,
  switch the integration off or remove it.
- The clear value exists between the request and sealing, and between opening a sealed value
  and a call to the provider. In code it is a `Secret`, which prints as `[redacted]` through
  `fmt`, `slog`, JSON and `%#v`; the one way to the value is `Reveal()`. The audit log records
  that a secret was set or changed, never what it is; errors carry codes, never a value or a
  provider's text; a test greps the logs and every reply for the credentials it used.
- A replaced webhook secret keeps verifying events for a day, so events the provider signed
  before the change took effect there aren't refused.
- **Backups.** A backup holds `server.key` next to the sealed values, so a backup is as
  sensitive as the credentials. Restic encrypts it ([install.md](install.md#backups)). After
  restoring to another machine, rotate each credential at its provider and here. If the key
  is not the one that sealed a secret, a [check](#checking-a-connection) says so and the
  admin sets it again.
- Credentials never reach the agent, MCP, a record, a template, a log or an agent's
  environment. No tool or route reads one.

## Who may do what

- Every route that manages integrations is **admin only** and **session only**. An access
  token, and so MCP, is refused. The routes are all under the workspace of the request; an
  integration of another workspace is `404`. Settings are the workspace's, not the server's,
  so an admin of any workspace manages that workspace's integrations: there are no operator
  routes.
- An admin may allow their own private space and the workspace's shared spaces, whether or not
  they are a member of them. Another person's private space, or another workspace's, is `404`
  and nothing changes: the admin never needs to read it, and no reply shows it: an integration
  lists only the allowed spaces the caller manages, and counts the others. A change of the
  list changes only the caller's own subset, so one admin can't take away another's private
  space. The database refuses an allowance across workspaces, on insert and on update.
- Anyone who can read a space sees which integrations are allowed there, with a name, kind and
  status. No secret, no webhook address, no limit, no provider.
- The name, provider and workspace of an integration, and its `public_id`, never change (the
  database refuses it).

## Limits

The admin sets, on the integration, the largest amount of one payment (a whole number of the
currency's smallest unit, up to a trillion) and 1 to 20 currencies from the server's closed list.
The list has each currency's number of decimals in the provider's API, which is how a payment's
amount is converted exactly, never rounded. That is Stripe's unit, which isn't always ISO 4217's:
COP, HUF and IDR have two decimals, and ISK is written with two that are always `00` (5 ISK is
500), so an ISK amount must be whole (`WholeUnitsOnly`). These limits are not in a definition and no app can raise them.

## Gates

An integration is **usable** in a space when it exists in the workspace, isn't switched off and
the space is allowed. `Service.Usable` is that question, and the code that calls a provider for
an app asks it at every call, so switching an integration off or removing a space stops the
app at once. Removing an integration is refused while an approval of an app names it.

## Apps and approval

An app declares what it uses of an outside service in the `integrations` block of its definition
([api.md](api.md#integrations-of-an-app)), strictly checked when it is saved: a slot of a kind, the
effect that asks for a payment link, and for each event of the kind the fields it may set, with a
literal or one of a closed list of values the server takes from the verified event. The definition
has no place for a URL, a redirect, a header, a secret, a provider, an integration id or a limit;
those are the admin's, on the integration.

**The contract** is computed by the server from the draft, never sent by a client: the kind, the
operations the app asks for, the record fields it sends out (names only), the currencies, the events
it handles, for each field the integration may write the literals and sources it may write, and the
rules that gate them. It has no wording, so a changed description needs no new approval; sending
another field, writing another field or value, or loosening a rule does. Its SHA-256 is the
contract's **hash**.

**Approval is by hash, through a queue.**

1. The person who may publish the app reads the slot's contract and **requests** its approval,
   naming the hash they read; a draft that changed since is refused.
2. A workspace admin sees the queue: the app's and its space's names, who asked and the
   contract. Nothing of the app or its records is in it, which is why an app in another person's
   private space, which an admin can't open, is approved the same way.
3. The admin **approves**, choosing an integration of the workspace of the right kind that is switched
   on, or **rejects**. Approving may also allow the app's space for the integration when the admin
   says so: the space is the one the request is in, which an admin can't otherwise allow when it is
   another person's private space. Approving a request is how they consent to that app using the
   integration there.
4. **Publish and rollback refuse** (`app_integration_unapproved`) a release with a slot whose contract
   has no approval whose integration is still switched on and allowed in the space. The check runs
   under the write lock, next to the check of the effects' targets. A rollback is checked for the
   release it returns to.
5. The code that uses an integration for an app asks again each time (`Service.Binding`): the app is
   online, its live release declares the slot, that release's contract is approved, the integration is
   on and the space allowed. An admin who **revokes** an approval, switches the integration off or
   removes the space stops the app at once.

An approval is for the next draft as much as the live release: the live release's stays approved
while the draft's is decided. In a one-person install the publisher is the admin: requesting,
approving and publishing are the same person's three calls, which the web app will offer as one
"approve and publish" step.

**Copies carry nothing.** Approvals belong to an app, not to a definition. A duplicate and an
installed template have the slots and no approval, and can't be published until their own contract
is approved, so a template works with whichever integration the admin binds. Moving an app to
another space deletes its approvals, as it drops its roles: they were given for the old space.
Records, and in later changes payments and events, are never copied.

## Checking a connection

`POST …/check` looks at what the server holds: the provider's required secrets are set and
open with the server's key, and the settings are the provider's. It does **not** call the
provider yet (`remote` is `false` in the answer). It answers `200` with `ok`, or with a code,
and never with a provider's reply. The one harmless call that proves the credentials work
comes with the first change that calls a provider.

## Egress

Calls to providers go through their own egress policy, `-integration-egress` /
`TASKTO_INTEGRATION_EGRESS`, **`public` by default**: the address of every connection is
checked after name resolution, so loopback, private, link-local and metadata addresses are
refused, and a redirect is an answer, never followed. An empty flag or file entry is `public` too (an empty
environment variable is not set, so the file's value stays), and a value that can't be parsed
stops the server at start: nothing falls back to a wider policy.

Besides, every adapter has constant API hosts, and the **request builder** (`Client`) refuses
any URL whose host the adapter doesn't own (a look-alike, a sibling, a port, a plain `http`
URL, an address, credentials in the URL, an empty port, a host with any non-ASCII character
such as a look-alike whose Unicode case mapping equals an adapter's host) before the dialer is reached. Only the Go server
calls providers; the agent runtime never does. Tests reach a fake provider through a
constructor option that no setting can set.

The policy never lists provider names: a listed name is trusted wherever it resolves, which
would switch the address check off. `any` is there for a trusted network, and a server that
reaches the internet only through a mandatory proxy needs it, since a restricted policy
ignores the environment's proxy.

## Webhooks and the public URL

Each integration has a random 128-bit `public_id`, and its webhook address is
`<public URL>/hooks/<public_id>`. The settings page shows it when the server has
`-public-url`; without one it says that webhooks are unavailable and that reconciling payments
without them isn't built yet. The id is the only thing in the path, and it holds no secret: the
signature will be the authentication. The reverse proxy must forward `POST /hooks/*` (and
nothing more has to be exposed for payments). Receiving, verifying and applying events is to come.

## Trying an integration by hand

CI never calls a real provider, and nothing here does yet: **check** only looks at what the
server holds (the secrets are set and open, the settings are valid) and says nothing about
whether the provider accepts the credentials. Real-provider testing arrives with the payment
link. Meanwhile, use the provider's **test mode**, with test keys of your own that you create at
the provider: connect them in Settings → Integrations, allow a space, and run the local check.
Never use live keys to try the software, and never paste a key into an issue, a chat or a
commit.

## Not here, on purpose

- **No model-authored HTTP call.** An app, an effect or an action never holds a URL.
- **No card data in TaskTo.** Hosted checkout only; TaskTo will keep a link, a provider's
  object id and the amount it asked for.
- **No raw webhook bodies** will be stored, only normalized fields and a hash.
- No refunds, payouts, subscriptions or customer records.
