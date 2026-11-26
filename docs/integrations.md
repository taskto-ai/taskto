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

- **Payment links**: the `create_payment_link` effect makes a hosted payment page at Stripe and
  writes its address to the record ([Payment links](#payment-links)).

No event is received yet. Those arrive as separate changes, in this order: the webhook and its
verification; applying a payment to a record; polling the provider for payments the webhook missed.
The rules below are the ones those changes have to keep.

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
- A space an app's approval allowed (the admin ticked "allow the space" when approving) is the
  approval's: it is marked in the integration's settings and goes with the approval. Saving the
  settings' list never makes it the admin's; only naming it in `keep`, which the settings offer
  as "Keep after the approval ends", does, and that is an audit event of its own.
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
rules that gate them, for each payment link the action that asks and what it does (insert, update
or delete: it decides who may trigger it), its amount, its currency, its `when` and whether its
description names the record's id, and the constraints of every field the integration reads, writes
or is gated by (type, whether it is required, length, bounds, options, a barcode's format, whether a
captured value may be replaced, and an attachment's limits), and the fields those depend on through a
`required_when`, followed all the way. It has no wording, so a
changed description that names the same fields needs no new approval; sending another field or the
record's id, writing another field or value, writing a value in another event, asking in another
currency or from another action, changing what the action does, loosening a field's constraints
(a higher maximum, another option, no barcode format), dropping a `when`, or loosening a rule does. Its
SHA-256 is the contract's **hash**.

**Approval is by hash, through a queue.**

1. The person who may publish the app reads the slot's contract and **requests** its approval,
   naming the hash they read; the request is refused if the slot's contract hash has changed since.
2. A workspace admin sees the queue: the app's and its space's names, who asked and the
   contract. Nothing of the app or its records is in it, which is why an app in another person's
   private space, which an admin can't open, is approved the same way.
3. The admin **approves**, choosing an integration of the workspace of the right kind that is switched
   on, or **rejects**. Approving may also allow the app's space for the integration when the admin
   says so: the space is the one the request is in, which an admin can't otherwise allow when it is
   another person's private space. Approving a request is how they consent to that app using the
   integration there. The space stays allowed while an approval needs it: revoking the last approval
   that allowed it takes it back (as does the app's moving or deletion), while a space an admin
   allowed in the integration's settings stays.
   A contract asked for again after a rejection or a revocation is a new request, with its own id and
   its own place in the queue; the old one keeps its decision.
4. **Publish and rollback refuse** (`app_integration_unapproved`) a release with a slot whose contract
   has no approval whose integration is still switched on and allowed in the space. The check runs
   under the write lock, next to the check of the effects' targets. A rollback is checked for the
   release it returns to.
5. The code that uses an integration for an app asks again each time (`Service.Binding`): the app is
   online, its live release declares the slot, that release's contract is approved, the integration is
   on and the space allowed. An admin who **revokes** an approval, switches the integration off or
   removes the space stops the app's use of the integration at once; the app stays online.

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

## Payment links

An app's action may declare a `create_payment_link` effect that names a slot of kind `payments`
([Apps and approval](#apps-and-approval)). When the action runs, the effect is queued in the app's
outbox in the same transaction as the record's change, and the outbox worker does it as the user who
acted, with that user's access then (`docs/api.md`, [Payment links](api.md#payment-links)). It
re-checks that the live release still asks for exactly this link and has an approved contract for it,
that the integration is switched on and allowed in the app's space, and the admin's limits; it makes
the link at the provider with the integration's key, which is opened only for the call; and it writes
the link and its id to the two fields the effect names through the system writer, in no way a user's
write.

- **Two shapes, by whether the server has a public URL.** Both are one line item with the exact
  amount and the description as the product's name, made once under the reference's token.
  - *With `-public-url`: a Checkout Session in payment mode* (`POST /v1/checkout/sessions`):
    `client_reference_id` set to the reference's token, and the server's own `success_url` and
    `cancel_url`. A session expires by itself after Stripe's default of 24 hours (its limit), so a
    payer who comes later finds an expired page, and sending the invoice again makes a new link.
  - *Without it: a Payment Link* (`POST /v1/payment_links`) with `after_completion[type]` set to
    `hosted_confirmation`, so the payer stays on a confirmation page Stripe hosts and no URL of ours
    is sent; `restrictions[completed_sessions][limit]` set to `1`, so an invoice's link can't be
    paid twice; and the token in the link's `metadata` (`taskto_ref`), which Stripe copies to the
    session of the payment. A Payment Link takes no `client_reference_id` in the API, so the token is
    also in the link's address (`?client_reference_id=<token>`), the way Stripe documents for it,
    which reaches `checkout.session.completed`. A link doesn't expire by itself, and the payments of
    one are found at Stripe by listing Checkout Sessions with `payment_link=<plink id>`. The record's
    `id_field` gets the link's `plink_…` id instead of a session's `cs_…`.

  Stripe's documentation relied on (checked against the pinned version): Create a Checkout Session
  (<https://docs.stripe.com/api/checkout/sessions/create>: `success_url` is "required conditionally",
  and every hosted example sets one, so a session without one is not relied on), Create a payment link
  (<https://docs.stripe.com/api/payment-link/create>: `after_completion`, `restrictions`,
  `line_items[].price_data`, `metadata` copied to the link's sessions), the Payment Links API guide
  (<https://docs.stripe.com/payment-links/api>), Track a payment link
  (<https://docs.stripe.com/payment-links/url-parameters>: the `client_reference_id` URL parameter) and
  List Checkout Sessions (<https://docs.stripe.com/api/checkout/sessions/list>: the `payment_link`
  filter).

  **Paid status without a public URL** can't come from a webhook, which Stripe can't deliver to such a
  server. It is to come from reconciliation polling (a later change, not built yet): until then a
  record made without a public URL is not marked paid by itself.

  The API version is pinned in code (`integrations.StripeAPIVersion`, sent in the
  `Stripe-Version` header; the version Stripe's documentation lists as current when the adapter was
  written) and is raised by a change of code with its tests, never by configuration.
- **Exact money.** Minor units from a closed currency table, never a rounded number: more decimals
  than the currency has, zero or less, above the integration's ceiling, or a currency the integration
  doesn't list are each refused, with a code that says which, before anything is made.
- **Once, through crashes.** The entry's reference (`integration_refs`) has a random token that is
  Stripe's `Idempotency-Key` and the `client_reference_id` its events echo back, and is unique by the
  outbox entry. A retry finds the reference and either the object Stripe made (not asked again) or,
  with none noted, asks again under the same key, which Stripe answers with the object it made. Stripe
  keeps idempotency keys for at least 24 hours, so a crash and then a wait of more than a day with
  no object noted is the one case that can leave two payable links for one reference (it is looked
  for first, below). A failure that
  Stripe keeps under a key (a `5xx`) would be answered to the same key for ever, so the next try has
  a key of its own; but Stripe documents a `5xx` as an indeterminate result, the object may have been
  made, so before that call the integration lists what the account made (the open Checkout Sessions
  created since the reference, matched on `client_reference_id`, or the active Payment Links, newest
  first, matched on `metadata[taskto_ref]`; neither has a filter for them and the search API doesn't
  cover them) and adopts an object of the token as if a call had answered it, if it asks the amount
  and the currency of the payment (an object of the token that doesn't is never given to a payer, and
  the entry fails with the provider's refusal for a person to settle). The same look comes before a
  call under the same key once the reference is a day old, when Stripe may have forgotten the key.
  When the listing can't be read (a failed read, an answer that isn't a whole list: no `object: list`,
  no `data`, no `has_more`, or `has_more` with nothing to read on from), nothing is made: the entry
  waits and tries again.
- **A lookup that can't see everything fails the entry.** A lookup reads at most 20 pages of 100,
  newest first. If every read worked and the account still holds more (for example more than 2,000
  active Payment Links, none of them the reference's), the lookup can't say whether an earlier try made
  the object, and asking again reads the same pages, so the entry doesn't wait for ever: it fails with
  `integration_lookup_incomplete`, nothing is made and the reference stays. An admin settles it by
  deactivating the links the account no longer uses (the lookup reads only active links), then the
  person who acted retries the entry.
- **A reference belongs to its account and its request.** When the reference is made, the integration
  reads the account its key belongs to (`GET /v1/account`, which with no id answers the account of the
  key; <https://docs.stripe.com/api/accounts/retrieve>, so the key must be allowed to read it) and
  keeps the account's id, and the two pages of the request (or none) as the first try asked them.
  A retry sends the same parameters, which Stripe requires of a retry under a key, and looks among
  the same kind of objects (Checkout Sessions or Payment Links), whatever the server's public URL is
  by then. If the integration's key has been replaced by another account's while a reference has no
  object, nothing is looked for or made in the new account, where an object the old one made could
  not be found: the entry fails with `integration_account_changed` and the reference is left as it is,
  for an admin to check the old account for a link and to recreate it from the record. A link a
  reference already made is only ever given under the integration that made it
  (`integration_changed` otherwise), so a record is never written with a link of an integration that
  is no longer the slot's.
- **The newest entry wins, and what is sent is what was approved.** Entries are done in the order they
  were queued, but one tried again later could write its link after a newer entry for the same record
  and fields wrote its own. An entry for which a newer one exists (same record, same two fields, any
  state) asks the provider for nothing and writes nothing, and ends done with a result that says it
  was superseded; the check is made again in the transaction that writes the link. And an entry's
  description names the fields it was filled from: the live contract must still name each of them (and
  the record's id, if it was in the text), or the entry fails as no longer approved
  (`integration_unavailable`); wording alone is not in the contract.
- **The server's own pages, or none.** With `-public-url` the payer comes back to
  `<public URL>/payment/done` or `/payment/cancelled`, web app pages that show a short message and
  call nothing; without it the payer stays on Stripe's confirmation page and no URL of ours is sent.
  A new reference takes the server's address at that moment and keeps it for its tries.
  A definition can't name a URL, so no app can send a payer elsewhere.
- **A preview never calls the network**: it says what the link would ask and refuses an amount the
  currency can't hold, with no integration, key or reference.
- **The key stays in the call.** It is not in a reply, a record, an entry's result, the reference,
  the audit log or a log line; a failure is a code, never the provider's words.

## Webhooks and the public URL

Each integration has a random 128-bit `public_id`, and its webhook address is
`<public URL>/hooks/<public_id>`. The settings page shows it when the server has
`-public-url`; without one it says that webhooks are unavailable and that reconciling payments
without them isn't built yet. The id is the only thing in the path, and it holds no secret: the
signature will be the authentication. The reverse proxy must forward `POST /hooks/*` (and
nothing more has to be exposed for payments). Receiving, verifying and applying events is to come.

## The fakes tests use

`internal/integrations/testprovider` is what the tests of the cards that call a provider run
against, so none of them needs a network:

- **A fake Stripe**, an in-process HTTP server for the endpoints the integration uses: Checkout
  Sessions (create, retrieve, list, expire), Payment Links, the events list and a harmless
  balance call. Its lists work as Stripe's do (newest first, with `limit` and the cursors, and `created`
  and `status` on sessions, `active` on links). It answers with the shapes of Stripe's API at the version the adapter pins
  (`integrations.StripeAPIVersion`; a test checks that the two agree), refuses unknown
  parameters, wrong keys, other versions and bodies that aren't form-encoded, and keeps
  idempotency keys as Stripe documents for API v1: the same key and parameters get the saved
  answer (`Idempotent-Replayed: true`), the same key with other parameters is an
  `idempotency_error`, and keys are forgotten after 24 hours. A test makes it fail on demand (a
  server error, rate limiting with `Retry-After`, a refusal, a revoked key, a timeout, a dropped
  connection), before the work is done or after it, the case where a retry must find the object
  that was already made. A test plays the payer (pay, delayed payment, declined attempt,
  expiry, refund), and the fake makes the events Stripe sends for each, several for one payment.
- **A sender of signed webhook events**, to a URL or straight to an `http.Handler`: the
  `Stripe-Signature` header in Stripe's format (`t=…,v1=…`, HMAC-SHA256 over `t.body`), signed
  with the right secret, a wrong one, an old or future timestamp, two secrets (a rotation), the
  v0 test scheme only, no signature or a malformed one, or a body changed after signing; and
  duplicate, replayed (the same bytes and the same signature), delayed, reversed and shuffled
  deliveries.
- The clock is a constructor option of both (`ManualClock` moves time by hand), and the fake's
  host reaches the adapter only through the request builder's test option
  (`integrations.WithTestEndpoint`), never through configuration. The keys and secrets in it are
  obvious fakes. Nothing in the server imports the package, and a test in `cmd/taskto` fails if
  its binary links it. The fake Square comes with the Square adapter.

## Trying an integration by hand

CI never calls a real provider: tests run against the fake. **Check** only looks at what the
server holds (the secrets are set and open, the settings are valid) and says nothing about
whether the provider accepts the credentials. To try a payment link against the real Stripe, by
hand and never in CI, use its **test mode** with test keys of your own that you create at Stripe:
connect them in Settings → Integrations, set a public URL, allow a space, approve an invoicing app
and send an invoice; the link opens a hosted page that takes Stripe's test cards. Never use live
keys to try the software, and never paste a key into an issue, a chat or a commit.

## Not here, on purpose

- **No model-authored HTTP call.** An app, an effect or an action never holds a URL.
- **No card data in TaskTo.** Hosted checkout only; TaskTo will keep a link, a provider's
  object id and the amount it asked for.
- **No raw webhook bodies** will be stored, only normalized fields and a hash.
- No refunds, payouts, subscriptions or customer records.
