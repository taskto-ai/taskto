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
- The **webhook route**, `POST /hooks/<public id>`, which verifies a provider's signature and
  keeps the event ([Webhooks and the public URL](#webhooks-and-the-public-url)), and the worker
  that will apply what it keeps.
- The **egress policy** for calls to providers and the **request builder** that pins each
  adapter's hosts ([Egress](#egress)).
- **Audit events** for every change ([audit.md](audit.md)).

- **Slots in an app's definition**, the **contract** the server works out from them and the
  **approval** by an admin of that contract's hash, which a publish and a rollback need
  ([Apps and approval](#apps-and-approval)).

- **Payment links**: the `create_payment_link` effect makes a hosted payment page at Stripe and
  writes its address to the record ([Payment links](#payment-links)).

The webhook is received and its events are stored, but no stored event is applied to a record
yet. Those arrive as separate changes, in this order: applying a payment to a record; polling the
provider for payments the webhook missed. The rules below are the ones those changes have to keep.

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
  before the change took effect there aren't refused. Two integrations never share a webhook
  secret (current or still-valid previous), whatever their workspaces, so a signature opens one
  integration only.
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
500), so an ISK amount must be whole (`currency.WholeUnitsOnly`, which `currency.Minor` enforces: a preview refuses what the run would). These limits are not in a definition and no app can raise them.

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
approving and publishing are the same person's three calls, which the web app offers as one
"approve and publish" step.

**In the web app.**

- **The app builder's publish panel** has an "Outside services" part when the draft declares a slot:
  each slot with what the app asks of it in plain words (the contract: the fields it sends out, the
  payment links it asks for, what action triggers each and when, the text they send out, what each
  event the service reports may write to a record, and what each field involved may hold: its type,
  whether it may be left empty, its limits and options, never the app's own wording), where its
  approval stands (not asked for yet, waiting for an admin, approved,
  turned down, revoked, or approved but not usable now because the integration was switched off or the
  space taken away) and, for someone who may publish the app, a button to ask for the approval. The
  slots are the draft's, which the agent writes: to change what an app asks, ask the agent in the
  conversation. Publishing is not offered while a slot has no usable approval; the server refuses it
  all the same (`app_integration_unapproved`).
- **An admin who may publish the app** has no separate request: the panel offers "Approve and publish",
  a dialog with the same words, a choice of the workspace's integrations of the kind that are switched
  on, a line that says that continuing approves exactly what is listed and publishes it (and, when the
  integration isn't allowed in the space yet, that approving allows it there while an approval needs
  it), and then the request, the approval and the publish as three calls in that order. A request that
  already waits is approved as it is, never asked for twice; a contract that was turned down or revoked
  is asked for anew, and it is the new request's id that is approved. A step the server refuses (a
  request decided meanwhile) reads the slots again, so a retry starts from what the server holds.
- **Settings → Integration approvals** (admins only: the tab isn't listed for anyone else, and the server
  refuses its routes) is the queue: Waiting, Approved, Turned down and Revoked, each request with the
  app's and its space's names (a private space is only called private), who asked and when, and, in
  a dialog, the contract in the same words. A waiting request is approved with an integration or turned
  down; an approval is revoked after a confirmation that says whether it is the live release's, which
  stops using the integration at once, and whether the space's grant it made goes with it. A decision on
  a request someone decided first closes the dialog, says so and reads the list again. Keeping an
  allowed space after its approval ends is the integration's own spaces dialog (Settings →
  Integrations, "Keep … after the approval ends"), which the queue points to and doesn't repeat. The
  admin reads no app and no record: the screen has only what the requester submitted.
- **The builder's preview** says what a payment link would ask for from the sample record ("Create a
  payment link", simulated, the sum in the currency) and calls no service, opens no secret and needs
  no approval.

**What the agent can do.** The read-only tool `list_integrations`
([agent-tools.md](agent-tools.md#the-tools)) lists the integrations allowed in a space (name, kind and
status: never an id, a provider, a key, a limit or another space) and, for an app the user may design or
publish, the slots its draft declares with the status of their approval. The agent writes the
declaration into a draft like any other JSON; it can't request, approve, bind or read anything about an
account.

**Copies carry nothing.** Approvals belong to an app, not to a definition. A duplicate and an
installed template have the slots and no approval, and can't be published until their own contract
is approved, so a template works with whichever integration the admin binds. Moving an app to
another space deletes its approvals, as it drops its roles: they were given for the old space.
Records, and in later changes payments and events, are never copied.

## Checking a connection

`POST …/check` looks at what the server holds: the provider's required secrets are set and
open with the server's key, and the settings are the provider's. It also says when another
integration's webhook secret was found shared with another (`integration_webhook_secret_shared`,
which names neither the integration nor its workspace; the other holder may have changed theirs
since): such a secret verifies no delivery and must be replaced. It does
**not** call the provider yet (`remote` is `false` in the answer). It answers `200` with `ok`, or
with a code, and never with a provider's reply. The one harmless call that proves the credentials work
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
re-checks that the live release still asks for exactly this link and has an approved contract for it
(the action and its kind, the amount field and its bounds, the currency and where it is read from, the
rule the record must meet, where the link goes), that the integration is switched on and allowed in
the app's space, and the admin's limits. These are read in one snapshot before the provider is asked
(the app's place, the approval, and the user's access through the same read transaction, so no check
mixes two instants), and again in the transaction that writes the link, with the user's access: what
changes while the provider answers stops the write. An entry runs only in the space it was queued in:
moving an app clears its approvals, and what an admin approves again in the new space is not what the
entry asked of the old one, so the entry fails with `app_effect_space_changed` and the payment is
started again. A record edited so that it no longer meets the action's condition (`when`) fails the
entry with `integration_condition_changed`, which says so (nothing was created; no admin is needed). It makes
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
  with none noted, asks again under the same key, which Stripe answers with the object it made, while
  the key is trusted: Stripe keeps idempotency keys for at least 24 hours, so a reference a day old
  with a call made and no object noted looks for the object first (below).
- **Nothing is saved as the link until its state is read.** What a call answers, and above all what
  Stripe replays for a key it kept, is the object as it was made: a Checkout Session replayed a day
  later still says it is open. So before an object is saved as the link, whether a call made it, a
  replay returned it or the lookup adopted it, it is retrieved (`GET /v1/checkout/sessions/{id}`,
  `GET /v1/payment_links/{id}`) and saved only if a payer can use it now: an `open` session, an
  `active` Payment Link. An expired session is never given: the call is made again under a key of its
  own, once. A completed session and an inactive link stop the entry with
  `integration_link_already_paid`. If the state can't be read, the object is not saved either and the
  outcome is marked unknown (next item).
- **An unknown outcome is never answered with a second link.** Stripe documents a `5xx` as
  indeterminate: "the operation might have produced side effects even though you received an error",
  and says not to "retry with a new idempotency key until you've resolved the original outcome"
  (<https://docs.stripe.com/error-low-level#server-errors>). A call whose outcome is unknown (a `5xx`,
  which Stripe keeps under the key; a request that may have reached Stripe and got no answer, such as
  a timeout or a dropped connection; an answer that can't be read; an object whose state can't be
  read) marks the reference (`outcome_unknown_at`), and from then on the reference **never gets a new
  key by itself**, and no call that makes anything is made for it. A request that certainly never left
  (the connection was not made) and one Stripe says it didn't execute (`429`, `409`, `408`) leave the
  outcome known: they are tried again under the same key.
  - *The entry only looks.* Each try lists what the account made (the Checkout Sessions of any status
    created since the reference, matched on `client_reference_id`, or the Payment Links, active or
    not, newest first, matched on `metadata[taskto_ref]`; neither has a filter for them and the search
    API doesn't cover them). An object of the token that is open (an active link) and asks the amount
    and the currency of the payment is adopted as if a call had answered it, after its state is read.
    An object of the token that doesn't ask them, a completed session and an inactive link
    (deactivated, or at its limit of payments) are never given to a payer and stop the entry
    (`integration_link_already_paid` or the provider's refusal): nothing else is made, and a person
    reconciles the payment. An expired session can't be paid and was never given: it accounts for the
    call, whose outcome is then known, and the entry goes on under a key of its own.
  - *The wait is bounded.* Between lookups the entry waits half the time the outcome has been unknown,
    at least 30 seconds and at most 30 minutes (the entry shows `integration_link_lookup_pending`; these
    waits don't use up its tries), for at most **6 hours** from when the outcome became unknown
    (`outcomeWait`). When the listing can't be read (a failed read, an answer that isn't a whole list:
    no `object: list`, no `data`, no `has_more`, or `has_more` with nothing to read on from), nothing is
    made, and the entry tries again as for any failed read.
  - *Then a person decides.* If the 6 hours end with nothing found, the entry fails with
    `integration_outcome_unknown`, which tells an admin to check the Stripe account for an object of
    the reference (a Payment Link's `metadata.taskto_ref`, a Checkout Session's `client_reference_id`)
    and then either
    - *adopt one*: if the link is there, the person who acted retries the entry; its first step is the
      lookup, which adopts the link (it fails again at once if it still sees none); or
    - *confirm that none exists*: an admin of the workspace confirms "no link exists, create a new
      one" (`POST /api/workspaces/{workspace}/integrations/{integration}/payment-links/{effect}/confirm-no-link`,
      [api.md](api.md#confirm-that-no-payment-link-exists)), which allows **exactly one** new key for
      the reference, queues the entry again as it was (the person who acted, with the access they have
      then), and is recorded in the audit log (`integration_new_link_allowed`). The next try still looks
      before it calls, so a link that turned up since is adopted, not doubled. If that call too has an
      unknown outcome, the cycle starts again and needs another confirmation. The admins' list of what
      waits for a decision is `GET …/integrations/{integration}/payment-links/unresolved`. Both routes
      cover only the spaces the admin manages (a private space's payments are not another admin's to
      decide).
- **A lookup is kept on the reference until it has been made.** A refusal (a `4xx`, which Stripe may
  keep under the key too) fails the reference and sets `lookup_required`; a retry of the entry clears
  "failed" but not that need, which only a lookup that read the account in full ends. So a try whose
  lookup could not be read is followed by one that looks, never by one that calls under the old key
  and is answered with what Stripe kept of it.
- **A lookup that can't see everything fails the entry.** A lookup reads at most 20 pages of 100,
  newest first. If every read worked and the account still holds more (for example more than 2,000
  active Payment Links, none of them the reference's), the lookup can't say whether an earlier try made
  the object, and asking again reads the same pages, so the entry doesn't wait for ever: it fails with
  `integration_lookup_incomplete`, nothing is made and the reference stays. An admin settles it by
  reconciling the original payment: look in the account for an object of the reference (a Payment
  Link's `metadata.taskto_ref`, a Checkout Session's `client_reference_id`); if one was paid, nothing
  is recreated; if one is unpaid, it is made unable to take payment first; only then is the action
  run again, which is a new entry with a new reference. Deactivating other links doesn't shrink what
  the lookup reads, since it lists inactive links too.
- **A reference belongs to its account and its request.** When the reference is made, the integration
  reads the account its key belongs to (`GET /v1/account`, which with no id answers the account of the
  key; <https://docs.stripe.com/api/accounts/retrieve>, so the key must be allowed to read it) and
  keeps the account's id with the mode of the key (test or live: one Stripe account answers both keys
  with the same id, and a test key doesn't see what a live one made), the integration, and the two
  pages of the request (or none) as the first try asked them.
  A retry sends the same parameters, which Stripe requires of a retry under a key, and looks among
  the same kind of objects (Checkout Sessions or Payment Links), whatever the server's public URL is
  by then. If the integration's key has been replaced by another account's, or by the same account's
  key of the other mode, while a reference has no object, nothing is looked for or made in the new
  account or mode, where an object the old one made could
  not be found: the entry fails with `integration_account_changed` and the reference is left as it is.
  Its old link is neither cancelled nor known to be unpaid, so an admin reconciles the original
  payment in the old account first (paid: nothing is recreated; not paid: the old link is made unable
  to take payment), and only then runs the action again (a new entry, a new reference). A
  reference is only ever used under the integration that made it, with its link or without
  (`integration_changed` otherwise, wherever the reference is found), so a record is never written
  with a link of an integration that is no longer the slot's; the same reconciliation applies.
- **A key that may hold a dead answer is not used again, once its outcome is known.** Stripe answers
  a call's saved result to the same key, a refusal (a 4xx included) and an object that has expired
  since. So a retry of a failed reference (a refusal is a known outcome), and a try that looks for the
  reference's object and finds only an expired Checkout Session, moves on to a key of its own (once a
  call was made under the current one) before it makes one. An unknown outcome never moves the key
  (above). A lookup that finds an object of the reference that was paid, or that can't take payment for
  a reason that doesn't say it wasn't (a completed session, a Payment Link deactivated or at its
  limit), fails the entry with `integration_link_already_paid`: nothing is made, and an admin
  reconciles it in the provider's account.
- **The newest entry wins, and what is sent is what was approved.** Entries are done in the order they
  were queued, but one tried again later could write its link after a newer entry for the same record
  and fields wrote its own. An entry for which a newer one exists (same record, either of the same two fields, any
  state, kept apart from the entries so that cleaning them up doesn't lose it) asks the provider for nothing and writes nothing, and ends done with a result that says it
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
signature is the authentication. The reverse proxy must forward `POST /hooks/*` (and nothing
more has to be exposed for payments; [install](install.md#webhooks-from-payment-providers)).
The route is in [api.md](api.md#post-hookspublic_id).

What a delivery goes through, in this order, each step cheaper than the next:

1. **Limits and size.** A limit per client address, counted before anything is looked up (so a
   limit says nothing of which ids exist), and a body of at most 256 KiB. The limit per id is
   counted only once the signature has verified, so unsigned requests, from anywhere, can't use
   up the quota of an integration's real deliveries.
2. **The integration**, by `public_id`: enabled, of a provider that receives webhooks, with a
   webhook secret. An id that is none of these is not turned away here: step 3 runs for it
   anyway.
3. **The signature, over the raw bytes, before any parsing**: constant-time comparison against
   the current webhook secret and, for a day after an admin replaces it, the previous one; the
   provider's timestamp tolerance applies. A delivery for an integration is only ever checked
   against that integration's own secrets, so a valid signature of one integration opens no
   other, in this workspace or another. For this to hold, **no two integrations share a webhook
   secret**: setting or replacing one that another integration (in any workspace) uses as its
   current secret, or as a previous one that hasn't expired, is refused with
   `integration_webhook_secret_reused`, which says nothing of whose it is. The test is a lookup
   of a **fingerprint** stored beside each webhook secret: an HMAC of it under a key derived
   from the server key (`internal/serverkey`), so the database holds neither the secret nor a
   plain hash that could be tried against guesses, and nothing is opened under the write lock.
   A database from before the check can hold the same secret twice; the server fingerprints
   those secrets when it starts, and a secret that **more than one integration holds is used for
   none of them**: deliveries signed with it are the plain `404` for each, nothing is stored,
   and each integration's [check](#checking-a-connection) says so (without naming the other
   one) until **each** has a fresh secret from its provider. The server records the secret's
   fingerprint as found shared (`integration_shared_secrets`, written at start and by every
   change or removal that could take a copy away), so a secret that was ever shared is never
   used again for any integration: when one holder rotates, or is removed, the other keeps
   refusing, because the secret is known to the first's workspace, and no integration may be
   given it again (`integration_webhook_secret_reused`). A rotation doesn't keep such a secret
   as the previous one either.

   Every well-formed signature costs the same work: the body is MAC'd against exactly two keys,
   the current and the previous secret, and a slot with none holds a random key of the server's
   own that nobody has, whose match is never counted even so. An id with no usable secret (unknown, disabled, shared or with none) is
   verified against a random secret the server made, so the work over the body is the same and
   the result is thrown away. A header that isn't well formed is refused before any MAC, for
   every id alike.

   What this does **not** hide: finding an id's integration is a database read that an id that
   doesn't exist skips, so how long a refusal takes can still tell an existing id from one that
   doesn't, to a client that measures it finely enough and repeats it (the address limit bounds
   how often). That an id exists is not a secret worth much on its own, since a delivery is
   authenticated by its signature, never by the id; what matters is that the answer, the log and
   the MACs over the body say nothing of it, which tests pin.
4. **One answer for every refusal**: an unknown id, a disabled integration, no webhook secret, a
   bad or old signature and a valid signature of another integration are all the same `404`,
   after the same work. The route logs nothing of a refusal, no id and no reason: the
   integration's [check](#checking-a-connection) is where an admin sees a secret that doesn't
   open.
5. **Normalizing.** The provider's adapter reads the body into closed, typed fields
   (`Event`: the provider's event id, one of four normalized kinds, the server-issued token the
   provider echoed back, the provider's object and payment ids, the amount in minor units, the
   currency, when it happened) and none of the free text around them: a payer's name, email
   and notes are never decoded, so there is nowhere for them to go. A type the server doesn't
   act on is answered `200` and not stored, since a provider disables an endpoint that
   answers errors.
6. **Storing once.** `integration_events` has `UNIQUE (integration_id, provider_event_id)` and the
   insert is `ON CONFLICT DO NOTHING`: a replay inside the tolerance, or a provider's retry, is
   answered `200` like the first delivery and changes nothing. The body is never stored, only
   its SHA-256 for support. Older than the tolerance, a replay is refused at step 3. The write
   happens in one transaction that first checks again that the integration is still enabled, of
   the same mode and with the webhook secrets the delivery was verified against: a delivery
   that was in flight when an admin disabled the integration or rotated its secret stores
   nothing (not even `last_event_at`) and is refused like any other.

   Events are purged after a time (below), and the purge forgets their ids: a provider's retry
   of an event that was purged, signed afresh, is stored again. **Whatever applies events must
   be idempotent**: the same provider event may reach it twice, and the second must change
   nothing.

A stored event is applied after the answer, by the **event worker**: leased to one worker at a
time (a lease that runs out, because the server died, gives the event to the next try), at most
five tries with growing waits, and finished events are purged after 30 days (failed ones after
90). The apply step is the next card's: until one is set, events wait in `pending`, untouched.

### What the adapter was written against (Stripe)

Pinned from Stripe's public documentation on the day the adapter was written (the code comments
in `internal/integrations/stripe_webhook.go` say the same). A change of any of it is a change of
code and of this section; the real-provider check is manual and never in CI.

- **Signature.** The `Stripe-Signature` header is comma-separated `name=value` entries: `t`, the
  Unix seconds at which Stripe signed this delivery, and one `v1` per active signing secret
  (two while a rolled secret is still valid). `v0` is a fake signature of sandbox events and the
  `secret`, `destination` and `payload` entries are debug metadata that "aren't covered by the
  HMAC": all are ignored, as is any entry the adapter doesn't know. Only `v1` authenticates:
  the hex HMAC-SHA256 of `<t>.<exact body>`, keyed with the signing secret's text as it is,
  `whsec_` prefix included. A header with no `t`, two of them or no `v1` is refused, as is more
  than one `Stripe-Signature` header.
- **Tolerance.** Five minutes between `t` and the server's clock, which Stripe's own libraries
  use by default (and never 0, which turns the check off). The server also refuses a timestamp
  that far *ahead*. A retried event is signed afresh by Stripe, with a new `t`.
- **Rotation.** Stripe can keep a rolled secret valid for up to seven days. The previous secret
  verifies for one day after an admin replaces it in TaskTo.AI, which is enough for the provider
  to have switched; after it only the new one does.
- **Delivery.** A non-2xx answer or a redirect is a failed delivery, retried for up to three
  days in live mode. Events repeat and "aren't in order", and Stripe says to find duplicates by
  event id and to answer 2xx before doing any work, which is why an event is stored and
  answered, and applied later.
- **Events** (snapshot events, the format of an endpoint registered with
  `event_payload=snapshot`). One Stripe type is authoritative for each normalized event; the
  rest of what Stripe sends for one payment is ignored.

  | Normalized | Stripe's event | Notes |
  |---|---|---|
  | `payment.succeeded` | `checkout.session.completed` with `payment_status` `paid`, and `checkout.session.async_payment_succeeded` | Of one session exactly one of them says `paid`: a delayed payment method's first event says `unpaid` and is ignored. |
  | `payment.expired` | `checkout.session.expired` | |
  | `payment.failed` | `checkout.session.async_payment_failed` | A declined card is not a failure: the payer may try another on the same page and the session stays open, so `payment_intent.payment_failed` and `charge.failed` are ignored. |
  | `payment.refunded` | `charge.refunded` | Partial refunds too: the amount is what has been refunded so far (`amount_refunded`). |

  A session event must be a one-time payment (`mode` `payment`) that carries a
  `client_reference_id` of the shape of a token the server issues (32 lowercase hex digits):
  that is how a payment is mapped to what the server made, and a session without one (another
  use of the same Stripe account) is not ours. A charge has no such reference: its event
  carries the payment intent's id, which the same verified session events carry, so the apply
  step can map a refund through it; a charge with no payment intent is not stored. Ids are checked against Stripe's shapes (`evt_`, `cs_test_`
  or `cs_live_`, `ch_`, `pi_`), the currency is three lower-case letters and the amount a
  whole number above zero; an event that isn't of the shape is answered `200` and not stored.
  An event of live money for an integration in test mode, or the other way round, is ignored
  the same way.

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
- **No card data in taskto.** Hosted checkout only; taskto will keep a link, a provider's
  object id and the amount it asked for.
- **No raw webhook bodies** are stored, only normalized fields and a hash.
- No refunds, payouts, subscriptions or customer records.
