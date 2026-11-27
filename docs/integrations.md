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
  that applies what it keeps to the apps' records ([Applying events](#applying-events)).
- The **egress policy** for calls to providers and the **request builder** that pins each
  adapter's hosts ([Egress](#egress)).
- **Audit events** for every change ([audit.md](audit.md)).

- **Slots in an app's definition**, the **contract** the server works out from them and the
  **approval** by an admin of that contract's hash, which a publish and a rollback need
  ([Apps and approval](#apps-and-approval)).

- **Payment links**: the `create_payment_link` effect makes a hosted payment page at Stripe or
  Square and writes its address to the record ([Payment links](#payment-links); Square's adapter
  is described in [Square](#square)).

- **Applying events**: a verified payment event marks the record of its payment link paid (or
  refunded) as the integration, and only as the app's approved contract allows
  ([Applying events](#applying-events)).

- **Reconciliation polling**: a worker that is always on reads the open links at the provider and
  applies what it finds as a webhook's event is applied, which is what marks a record paid on a
  server with no public address ([Reconciliation polling](#reconciliation-polling)).

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
  install without a public URL never receives a webhook (payments are then reconciled by [polling](#reconciliation-polling));
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
  its sandbox host, by `environment`), and the **API version** it is written against and sends
  with every call (`Stripe-Version`, `Square-Version`);
- how long it **keeps an idempotency key** (`KeyKept`): Stripe documents at least 24 hours, which
  the server relies on for 23; Square publishes no period, so its keys are never relied on: a
  call whose outcome is unknown is never sent again under its key ([Square](#square)).

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
- **Settings → Integration events** (admins only, like the queue) is what became of an integration's
  provider events ([applying events](#applying-events)): for the integration chosen, its events newest
  first, filtered by state (failed, ignored, applied, waiting), each with its kind, amount, time, mode
  and state, and the reason an ignored or failed one wasn't applied, in the words of its code; a second
  payment and an amount that doesn't match are named so. A failed event has a Retry button, which calls
  the retry route and shows the server's refusal under the event (an event that is no longer failed
  reads the list again). An event holds no record, app or space, so none is shown. Above them, **Payment
  links to check** lists the unresolved payment links of the spaces the admin manages
  (`payment-links/unresolved`): each with its amount, record, app, space and the time its outcome became
  unknown, what to look for in the Stripe account (in the account and mode the link was first asked in:
  `client_reference_id` on a Checkout Session, `metadata[taskto_ref]` on a Payment Link, the reference
  of every attempt, which the screen lists under each payment: each attempt's reference in a read-only
  monospace field with a copy button, the field to search for that kind of object, any object seen (and
  whether it was checked against the payment), the account and mode it was made under and when it was
  first sent, so the admin searches by reference and falls back to amount and time only where the
  provider can't be searched by it; a link that is found is left alone and the person who ran the action retries it, not the payer), and, once
  the wait ended, a "Confirm no link exists" button behind a confirmation that says it queues one more
  attempt, which may adopt a link that exists or fail, and plainly that if a link does exist the payer
  ends up with two and could pay twice. A lookup that stopped for another reason (the integration's
  account changed) is "Lookup stopped", with the reason in words and no button. Cancelling sends
  nothing. A retry the server refuses because the event isn't failed any more stays said in the events
  section after the list is read again. The screen asks only for the one integration's events
  and links, the workspace's spaces' names and the provider registry.
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
  server. It comes from [reconciliation polling](#reconciliation-polling), which is always on: the
  server asks Stripe about each open link and applies what it finds as a webhook's event is applied.

  The API version is pinned in code (`integrations.StripeAPIVersion`, sent in the
  `Stripe-Version` header; the version Stripe's documentation lists as current when the adapter was
  written) and is raised by a change of code with its tests, never by configuration.
- **Exact money.** Minor units from a closed currency table, never a rounded number: more decimals
  than the currency has, zero or less, above the integration's ceiling, or a currency the integration
  doesn't list are each refused, with a code that says which, before anything is made.
- **Once, through crashes: a write-ahead log of keys.** The entry's reference (`integration_refs`) is
  unique by the outbox entry and keeps the provider's answer. It has **keys**
  (`integration_ref_attempts`), one at a time: each a Stripe `Idempotency-Key` with a random token of
  its own, which the call puts in the object (`client_reference_id`, or a Payment Link's
  `metadata[taskto_ref]`) and Stripe's events echo back. The first key's token is the reference's;
  a payment of a later key finds the reference by that key's token. The owner's rule is that a
  reference never risks a second payable link, so the server never decides after the fact that a call
  didn't reach Stripe:
  - *Written ahead.* Before every call under a key, the call is recorded on the key (`sends`, and the
    time the key was first sent), durably, and that record is never taken back. A crash between the
    record and the call, or between the call and its answer being noted, leaves the key as a call
    that may have run.
  - *Sent once.* The HTTP client never resends the call by itself: Go's transport would resend a
    request with an `Idempotency-Key` on a new connection when a reused one breaks, and report the
    second copy's failed dial, though the first reached Stripe. The body is made one that can't be
    read again, which stops that, and whether a call may have left is told by whether a connection was
    ever handed to it, not by the error.
  - *Resolved only by evidence.* A key is resolved by an answer (`2xx`) whose object is then read at
    Stripe; by a lookup that finds an object carrying the key's own token (never another key's: an
    expired session of an earlier key says nothing of a later one); or by an answer Stripe documents
    as given before the call ran: a `429` whose code is `rate_limit` or `lock_timeout`
    (<https://docs.stripe.com/rate-limits#object-lock-timeouts>), a `409` whose code is
    `idempotency_key_in_use` (another request with the key is still running), and a call that never got
    a connection. Those count as clean (`clean_sends`), and the same key is asked again later, never a
    new one.
  - *Everything else leaves the outcome unknown*: any other `4xx` (Stripe saves the result of a request
    whose execution began under its key, a refusal included:
    <https://docs.stripe.com/error-low-level#content-errors>), a `5xx` (indeterminate:
    <https://docs.stripe.com/error-low-level#server-errors>), a `408`, a `409` or `429` of another kind,
    a timeout, a dropped connection, an answer that can't be read, an object whose state can't be read.
- **An unknown outcome is never answered with a second link.** Stripe says not to "retry with a new
  idempotency key until you've resolved the original outcome". A reference whose key's outcome is
  unknown **never gets a new key by itself**:
  - *Each try resolves the key if it can.* An object an answer named (kept on the key before its state
    is read) is read again; otherwise the account is listed (the Checkout Sessions of any status
    created since the reference, matched on `client_reference_id`, or the Payment Links, active or
    not, newest first, matched on `metadata[taskto_ref]`; neither has a filter for them and the search
    API doesn't cover them) for an object of the key's token. An object that is open (an active link)
    and asks the amount and the currency of the payment is adopted as if a call had answered it, after
    its state is read. One that doesn't ask them, a completed session and an inactive link (deactivated,
    or at its limit of payments) are never given to a payer and stop the entry
    (`integration_link_already_paid` or the provider's refusal): nothing else is made, and a person
    reconciles the payment.
  - *Then the same key is asked again*, with the same parameters, while Stripe still keeps it (23 hours
    from the key's first call; Stripe keeps keys at least 24): Stripe answers with what the first call
    made (an object, or the failure it kept), or runs the call if it never ran. Past that window the
    key is never called again, since Stripe may have forgotten it and would run it a second time; the
    window is checked again just before the call leaves, after it is written ahead, and bounds the
    call: one that hasn't left when the window closes never does. A clock that reads earlier than the
    key's first call relies on no window. A provider that documents no window (Square) is never asked
    again under a key whose outcome is unknown.
  - *Failures tell nothing and end nothing.* A lookup or a call that fails (a `5xx`, a timeout, a
    listing that isn't a whole list or holds more than a lookup reads: 20 pages of 100, newest first),
    and a failed read of whose account the key is or of an object's state, leave the outcome as it was,
    whatever the failure, the key refused (`401`) included: any page of the lookup, a Payment Link's
    line items, the read of the account and of the state alike. An object a lookup finds is kept on its
    key before its state is read, as an answer's is, and so is one it finds carrying the key's token and
    can't adopt, in every case: a Payment Link whose line items can't be read (its amount can't be
    checked), or an object that asks another amount or currency. Such an object is kept *unchecked*: it
    is never adopted as it is, the same key isn't asked again on top of it, and it blocks a new key and an
    admin's "none exists" as any object on record does. Only a later lookup that reads and checks it
    adopts it (an open object asking the payment's amount and currency); until then the entry waits as
    below, and then fails for a person, with the admins' list naming the object (`object_id`). The entry waits half
    the time the outcome has been unknown, at least 30 seconds and at most 30 minutes (it shows
    `integration_link_lookup_pending`; these waits don't use up its tries), for at most **6 hours**
    from when the outcome became unknown (`outcomeWait`), and nothing ends that early.
  - *Then a person decides.* If the 6 hours end unresolved, the entry fails with
    `integration_outcome_unknown`, which tells an admin to check the Stripe account for an object of
    the reference (a Payment Link's `metadata.taskto_ref`, a Checkout Session's `client_reference_id`)
    and then either
    - *adopt one*: if the link is there, the person who acted retries the entry; it looks again and
      adopts the link (it fails again at once if it still sees none); or
    - *confirm that none exists*: an admin of the workspace confirms "no link exists, create a new
      one" (`POST /api/workspaces/{workspace}/integrations/{integration}/payment-links/{effect}/confirm-no-link`,
      [api.md](api.md#confirm-that-no-payment-link-exists)), the only way to a new key for such a
      reference. The key is recorded as abandoned (its record stays), **exactly one** new key is
      allowed, the entry is queued again as it was (the person who acted, with the access they have
      then), and the confirmation is in the audit log (`integration_new_link_allowed`). It is refused
      (`integration_link_not_confirmable`), in the transaction that would allow the new key, when the
      server saw anything of the reference that contradicts "none exists" (a key with an object on
      record: an answer named one, or a lookup found one carrying its token, whether or not it could check
      it; the admins' list names that object, `object_id`, for a person to check at Stripe) or any key of it was already resolved (seen
      expired, or abandoned by an earlier confirmation). So it is possible once per reference, for its
      first key: if the new key's call too has an unknown outcome, the entry fails with
      `integration_outcome_unknown` again and a person reconciles it in the Stripe account. A key also
      moves on by itself (from one seen expired) only while every key of the reference left nothing a
      payer can use. The
      admins' list of what waits for a decision is
      `GET …/integrations/{integration}/payment-links/unresolved`, which lists each payment's attempts
      too (every key of the reference, oldest first: its reference, the kind of object, any object
      seen, the account and mode, when it was first sent; never a key of the provider's account or
      anything sealed). Both routes cover only the spaces the
      admin manages (a private space's payments are not another admin's to decide).
- **Nothing is saved as the link until its state is read.** What a call answers, and above all what
  Stripe replays for a key it kept, is the object as it was made: a Checkout Session replayed a day
  later still says it is open. So before an object is saved as the link, whether a call made it, a
  replay returned it or the lookup adopted it, it is retrieved (`GET /v1/checkout/sessions/{id}`,
  `GET /v1/payment_links/{id}`) and saved only if a payer can use it now: an `open` session, an
  `active` Payment Link. An expired session is never given; its key is accounted for (it made that
  session and nothing else), so the reference moves on to the next key by itself, and the call is made
  under it. A completed session and an inactive link stop the entry with
  `integration_link_already_paid`. If the state can't be read, the object stays on the key and the
  outcome stays unknown (above).
- **A saved link is read again before it is given again.** An entry done again after its link was
  saved (its try stopped before the record held the link, or the entry runs once more) gets the
  saved link only if its state, read again at the provider in the reference's own account and mode
  (another account's or mode's key is refused with `integration_account_changed` and reads nothing),
  still says a payer can use it. One that
  expired or was paid since, or that the reference's own verified events ended, is not given, and
  nothing else is made for the entry: it stops with `integration_link_already_paid` for a person to
  reconcile. A read that fails gives nothing and makes nothing, and the entry tries again.
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

**Square signs over the address.** Square's signature covers the notification URL as well as the
body, so a delivery verifies only against the exact address registered at Square. The server keeps
the address it showed when the admin saved the webhook signing secret (`webhook_registered_url`),
and verifies Square's deliveries against that one (or the current one, when none was recorded),
made from the public URL and never from the request's `Host`. When the server's public URL changes
afterwards, the integration's `webhook_url_changed` is `true` and the settings page says so: the
provider still sends to, and Square still signs over, the old address. The admin registers the new
address at the provider and saves the signing secret again (the same one is fine), which records
the new address and clears the warning. The same warning shows for Stripe, whose deliveries also
keep going to the old address.

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
   provider's timestamp tolerance applies (Stripe: five minutes; Square: the event's
   `created_at`, a day back and five minutes ahead, [below](#square)). A delivery for an integration is only ever checked
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
90). The apply step is [below](#applying-events): until the server sets one, events wait in
`pending`, untouched.

## Applying events

The event worker hands each stored event to the apps. It is the only code that reads what a
provider said and changes what an app holds, so it believes little:

- **The record is found through a token the server issued, and nothing else.** A ref has one token
  for each of its idempotency keys (`integration_ref_attempts`; the first key's is the ref's own),
  and a payment echoes the token of the key whose object was paid, which is a later key's after an
  earlier one expired or was given up, so any of them finds the ref. A token no key of any ref
  carries names nothing (`integration_event_ref_unknown`, ignored). The provider's metadata,
  descriptions and ids name no record, app or space. The ref
  (`integration_refs`) belongs to one integration, which must be the event's (another's token
  names nothing: `integration_event_ref_unknown`, ignored), and to one mode of the provider's
  account, which must be the mode of the event (`livemode`): money of the other mode is ignored
  (`integration_event_mode_mismatch`).
- **A refund carries no token of ours.** It is mapped through the payment (the payment intent)
  that the ref learned from the verified event that paid it, in the event's own integration.
  Stripe tells events in no set order, so a full refund can arrive before the payment it undoes. A
  refund of a payment no ref has been paid by **waits for that payment** (`pending`,
  `integration_event_payment_unknown`) when the payment may still be one of this integration's: a
  verified event of the integration told the payment with one of our tokens, or a ref of the
  integration that isn't paid yet asks exactly the amount, currency and mode the refund gives back
  (a ref is made before its payment, and the payment before its refund). It is tried again on a
  growing wait (30 seconds to 30 minutes) that uses up none of its tries, and the payment's own
  apply makes it due at once, so the invoice ends refunded. It waits for **72 hours** from when it
  arrived, the time Stripe retries a webhook it couldn't deliver; then it **fails** with the same
  code, and the admins are told. A payment that applies after that still tries its failed refund
  again. Any other refund is of a payment this integration never made a link for, and is ignored
  (`integration_event_ref_unknown`): it changes no record.
- **Money is checked exactly.** The amount (in the smallest unit) and the currency must equal what
  the ref asked for, else the event is **failed** with `integration_amount_mismatch`, not applied and
  shown to the admins; a refund must be in the ref's currency and not above what was paid. Only a
  refund of **all** of the payment moves the ref and the record to refunded: a partial one is ignored
  (`integration_event_partial_refund`), since the invoice isn't refunded yet, and the full one
  that completes it is applied (Stripe's `charge.refunded` reports the total refunded so far).
- **The ref's state only moves forward**: `creating`, `open` or `failed` (and `expired`, since money
  is never turned away for the state it finds) to `paid`; `creating` or `open` to `expired` or `failed`;
  `paid` to `refunded`. The change is a conditional `UPDATE … WHERE state IN (…)` made in the
  transaction that writes the record, so the state and the record change together or not at all. An
  event that finds the ref already there is a **duplicate** (`integration_event_duplicate`), one that
  finds it past there is **late** (`integration_event_late`), both ignored.
- **A ref whose link's outcome is unknown** (the provider never said whether a call under the ref's
  current key made the link, so the ref is `creating` with no object and its entry only looks for it;
  "unknown" is a property of that key's row, not of the ref) can still be paid: a payer may have used
  a link the server never heard of. A verified paid event for a token of any of the ref's keys is real
  money and applies once, as for any ref: the record is marked paid, the ref takes the event's object
  as its own if the event names one and no other ref has it, and the key the payment came through,
  if it had no outcome, becomes `settled` (the other keys keep theirs). The paid ref leaves the
  admin's list of unresolved links, and confirming that no link exists finds nothing to decide,
  whichever key the payment came through, so no new key is allowed. The entry that asked for the link
  then stops with `integration_link_already_paid` and makes no link: the record is paid, and there
  is no address to write. An end that is not money (`expired`, `failed`) is not applied to a ref with
  no object (`integration_event_late`): the entry's lookup may still adopt the link. Nor is one for an
  object that isn't the ref's link, such as the session of an earlier key that expired, since the
  ref has moved on to a link of a later key that is still open (`integration_event_late`).
- **A second payment** on a ref that was paid, by another payment, is never applied: it is
  recorded in `integration_ref_duplicates` (kept as long as the ref, unlike the event, once per
  payment) and the admins are told (`integration_event_second_payment`, ignored); the row and the
  event's end are one transaction, and the same second payment told again (after the purge) is a
  duplicate, with no second notification. Two paid events
  are the same payment when both name the same payment (the payment intent), or, when either names
  none, the same object; two that can't be told apart by either id are two payments, so a payment
  is never taken for a repeat because both lack an id.
- **Re-checked when it is applied**, in the transaction that writes, against the live release: the
  app is online and its space isn't archived, its live release declares the slot, the release's
  contract is approved, the integration is switched on and allowed in the space, and it is the
  integration the ref was made under. Otherwise the event is **failed** with
  `integration_event_unavailable`, `integration_event_integration_changed` or
  `integration_event_app_offline`, the ref stays as it was, and the admins are told. A record that is
  gone is `integration_event_record_gone`. A failed event is **retried** by an admin once what is
  wrong is fixed ([api.md](api.md#post-apiworkspacesworkspaceintegrationsintegrationeventseventretry)).
  Money that arrives is never dropped silently: an event ends applied, ignored with a reason, or
  failed with a reason an admin sees.
- **What is written** is the contract's handler for the event: its declared fields, its literals and
  the sources it names (`occurred_at`, `paid_at`, `amount`, `currency`, `payment_id`), under its
  `only_if` rule, as it holds of the record then. A rule that doesn't hold writes nothing; the ref is
  still settled, since the money arrived, and an admin is told when it was a payment
  (`integration_event_condition_unmet`, ignored). An event for which the app declares no handler
  settles the ref and writes nothing (`integration_event_no_handler`, ignored). `receipt_url` is
  never written: Stripe's checkout events carry none, and the adapter keeps no text. The write
  still goes through `Collection.Fits`; a record it would make invalid is refused with nothing
  changed (`integration_event_record_refused`, failed). The history's actor is the integration, the
  action the normalized event, and no effect is queued (there is no user to run it as).
- **Idempotent.** The store forgets an event when it is purged, and the provider's retry of it is
  stored again, so the same event can arrive a second time long after the first. The second finds the
  ref where the first left it, is a duplicate and changes nothing: a person's later edit of the record
  isn't undone by it. An event's own outcome (applied, or ignored because the app's rule didn't hold
  or it has no handler) is stored in the transaction that moves the ref and writes the record, so a
  worker that stops after that commit leaves an event that is ended, with its reason, and never one
  a later try would find settled and take for a duplicate. Only the claim that holds the event
  writes: a worker whose lease ran out has its write rolled back.
- **The admins** are told (an inbox notification of the kind `integration_event`, to each admin of
  the workspace, naming the integration and nothing of the payment) of every failed event, of a
  second payment and of a payment the app's rule left unrecorded. The event's end and the debt of
  its notification (one per end, so a later end never replaces an earlier one's) are stored
  together, and the notification is written by a step of its own, after every ten events and once
  the queue is empty: one that can't be written is tried again every minute until it is, and never
  holds back the event or the events after it. An event that still owes one is not purged. An admin lists an integration's
  events and retries a failed one through [the API](api.md#get-apiworkspacesworkspaceintegrationsintegrationevents); an
  event shows its kind, amount, state and code, never the app or record, since an admin has no read
  access to a private app's records. A notification to the record's creator is not sent.

## Reconciliation polling

A server with no public address can't receive webhooks, and one that has an address can miss one. So
a worker of its own (`StartPolling`, in the server process, always on, whether or not there is a public
URL) reads the objects of the refs that are still open and feeds what it finds through the same path a
webhook's event takes. It never reads an event feed: it retrieves the objects the server made.

- **Only an adapter that polls.** A provider is polled only when its adapter implements
  `PaymentPoller`. Square's doesn't: the worker holds a Square integration's refs and reads nothing of
  them, so a Square payment is learnt only from its webhook, which needs a public URL.
- **Same path, so once.** A finding is stored in `integration_events` as an event of the source `poll`
  with the id `poll:<object>:<state>` (`poll:cs_…:paid`, `poll:cs_…:expired`): once per integration, by
  the table's unique key. The event worker then applies it with [the same step](#applying-events) as a
  webhook's, so the same checks hold (the ref's token, mode, integration and exact money, the app's
  approval as of that moment) and the ref's conditional state change is what makes a payment apply
  once, whether the webhook, the poll or both tell it: the second is ignored as a duplicate. A poll
  doesn't move `last_event_at`, which tells an admin that webhooks arrive.
- **What it reads.** For a Checkout Session, its status and payment status (`complete` and `paid`:
  paid; `expired`: the link ended; a delayed payment that hasn't settled: nothing yet). For a Payment
  Link, the link's completed sessions (`payment_link` filter), each paid one a payment. The answer is
  checked against Stripe's shapes as a webhook's is, and nothing a payer typed is decoded or kept. A refund of a paid
  ref is not polled: a ref that is paid, expired, failed or refunded is no longer open, and the
  webhook tells a refund.
- **Dated when it happened.** A polled payment is dated by the provider's own time of the payment,
  read in the same request, as its webhook event is: a payment made at 23:59:30 and found at 00:00:10
  is `occurred_at` 23:59:30, and an invoice's `paid_at` is the day it was paid. For Stripe the read
  asks for the session's PaymentIntent and its latest Charge as objects (`expand[]` of
  `payment_intent.latest_charge`, `data.payment_intent.latest_charge` for a link's list), and the
  time is the Charge's `created`, when the payer's payment was made (for a card, when it succeeded;
  for a delayed method such as a bank debit, when the debit was started, which can be days before
  the webhook's `checkout.session.async_payment_succeeded`). Only when the provider gives no time (a
  session with no charge, or an adapter whose provider says none) is a payment dated by when the poll
  found it, and so is an expiry: Stripe gives no time of an expiry the server asked for early (a
  session's `expires_at` stays the time it was due to expire). The [contract
  suite](#the-end-to-end-isolation-and-contract-suites) requires the payment's time of every adapter that polls.
- **Read-only.** A poll never makes, changes or deletes anything at the provider.
- **Its own key, its own mode.** A ref is read with its own integration's key only, in that
  integration's workspace, and only while the key is of the mode the ref was made in (a test ref is
  never read with a live key, nor a live ref with a test key). A ref's app and its approval must still
  allow it (the integration on and allowed in the app's space, the app online with its approved
  contract bound to this integration); otherwise nothing is sent to Stripe for it and it is looked at
  again after five minutes. An integration that is disabled is not read for at all.
- **Polite.** After each read a ref waits longer: a minute, then double, up to 30 minutes; where
  webhooks arrive too (a public URL and a webhook secret) the poll is a safety net, 30 minutes doubling
  to six hours. An integration is read at most 20 times a minute, whatever the number of its open
  refs, the ones that waited longest first. A `429` holds all of the integration's reads for the wait
  Stripe's `Retry-After` asked for (a minute at least, an hour at most) and counts as no read of the
  ref. A new ref is read for the first time a minute after it was made.
- **It stops.** A ref that isn't open is never read. One older than 30 days is not read any more (a
  Payment Link doesn't expire by itself), though it stays open: a payment made after that arrives by
  webhook or not at all.

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

### Square

The adapter is `internal/integrations/square.go` and `square_webhook.go`, written against Square's
published API specification (`github.com/square/connect-api-specification`, release `2026-11-18`,
the source of [Square's API reference](https://developer.squareup.com/reference/square)) and its
guides, as read on the day it was written. The code comments cite each page; a change of any of it
is a change of code and of this section.

- **Version and hosts.** Every call sends `Square-Version: 2026-11-18`
  (`integrations.SquareAPIVersion`). The host is `connect.squareupsandbox.com` or
  `connect.squareup.com`, by the integration's `environment`, a choice and never a URL.
- **Payment links** ([CreatePaymentLink](https://developer.squareup.com/reference/square/checkout-api/create-payment-link)):
  one order of one ad hoc line (the app's description, quantity 1, the exact amount) at the
  integration's `location_id`, with tipping off. The key's token is the `idempotency_key`, the
  order's `reference_id` and the link's `payment_note`, which Square copies to the payment
  ("After processing the payment, Square adds this note to the resulting Payment as well as the
  post payment confirmation email"): the note is how a payment's events carry the token back, and
  the payer sees it, a 32-digit reference, in their confirmation email. With a public URL the payer
  is sent back to the server's done page (`checkout_options.redirect_url`; Square has no cancel
  page); without one they stay on Square's confirmation. A link is paid once ("The buyer can use
  the payment link only once"), its order going from `DRAFT` to `OPEN` with a tender, and never
  expires ([Quick Pay Checkout](https://developer.squareup.com/docs/checkout-api/quick-pay-checkout)).
  Its address must be on `sandbox.square.link` or `square.link`, by environment.
- **Its state** is read through the link's order (`GET /v2/orders/{id}`): `DRAFT` or `OPEN` with no
  tender is open; a tender, `COMPLETED` or `CANCELED` (a deleted link,
  [Manage checkout](https://developer.squareup.com/docs/checkout-api/manage-checkout)) is settled,
  for a person to reconcile.
- **Lookup by token.** Square's list of payment links has no filter, so a lookup reads every page
  (up to 20 of up to 1000 links) for the link whose `payment_note` is the key's token, and adopts it
  only if its order asks the same amount and currency with the same `reference_id`. More pages than
  that is `CallLookupIncomplete`, which tells nothing: the key stays unknown.
- **Idempotency** ([Idempotency](https://developer.squareup.com/docs/build-basics/common-api-patterns/idempotency)):
  the same key and request return the first successful answer, the same key with another request
  is `IDEMPOTENCY_KEY_REUSED`. Square publishes no period for which a key is kept, so a key whose
  outcome is unknown is never asked again: it is only looked for by its token, and never replaced
  by a new one without an admin ([Payment links](#payment-links)). A call that certainly never left is
  not unknown, and is sent again under the same key.
- **Errors** ([Handling errors](https://developer.squareup.com/docs/build-basics/handling-errors)):
  Square documents no answer as given before the request ran (not even `429`, after which "Square
  might temporarily stop processing your requests"), so every answer to a creation that isn't a
  success leaves its outcome unknown: `401` and `403` are the credentials, `408`, `429` and `5xx`
  may pass, any other `4xx` is a refusal. Only a request that never left is not executed.
- **Signature** ([Validate notifications](https://developer.squareup.com/docs/webhooks/step3validate)):
  `x-square-hmacsha256-signature` is the Base64 HMAC-SHA256, keyed with the subscription's
  signature key, of the notification URL followed by the exact body (Square's SDKs, whose
  known answers a test checks, do the same). It is compared in constant time, against two keys
  as Stripe's is.
- **Time.** No header carries a signed time; the event's `created_at`, inside the signed body, is
  read once the signature matched. Square retries a failed delivery "for up to 24 hours after the
  originating event" with the same `event_id`, in no guaranteed order
  ([Webhooks overview](https://developer.squareup.com/docs/webhooks/overview)), so a delivery is
  accepted up to 24 hours and 5 minutes after its event and 5 minutes ahead of it. A replay inside
  that window is stopped by the event's id, stored once, and by the ref's state, which only moves
  forward.
- **Mode.** Square's events don't say whether they are of the sandbox; each environment has its own
  subscriptions and signature keys, so a delivery that verified is of the integration's mode.
- **Events.** Square has no event per outcome, only a payment's creation and its updates, so the
  payment's own fields decide:

  | Normalized | Square's event | Notes |
  |---|---|---|
  | `payment.succeeded` | `payment.created` or `payment.updated` of a `COMPLETED` payment that has refunded nothing and whose `note` is a token the server issues | Square may tell one payment more than once; the ref's state change applies it once. The amount is the payment's `total_money`. |
  | `payment.refunded` | `payment.updated` of a `COMPLETED` payment with `refunded_money` above zero | What has been refunded so far, mapped to the ref through the payment's id. `refund.created` and `refund.updated` carry one refund's own amount, not the total, and are ignored. |

  `APPROVED` and `PENDING` payments aren't paid yet, and a `FAILED` or `CANCELED` one leaves the link
  open for the payer to try again, so none is an event; a Square link never expires. The buyer's
  email, address and card are never decoded.

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
  its binary links it.
- **A fake Square** (`testprovider/square.go`): payment links (create, list with a cursor and a page
  size a test sets, retrieve), orders and the merchant, at `Square-Version` `2026-11-18`, refusing
  another version, a wrong token, unknown fields and another location. It keeps idempotency keys as
  Square documents (the same request gets the first answer, another is `IDEMPOTENCY_KEY_REUSED`),
  fails on demand before or after doing the work, and plays the payer (pay once, refund in parts,
  delete a link) with the events Square sends for each, signed over the notification URL.

## The end-to-end, isolation and contract suites

Three groups of tests run in CI's Go job, with no real network:

- **Invoice to paid** (`internal/server/payments_e2e_test.go`): the server's whole mux, SQLite, the apps'
  outbox worker, the integrations' event worker and a clock the test moves, with the fake Stripe on a
  socket and its signed events sent over HTTP to the real `/hooks/` route. An admin connects the
  integration and approves an app's contract, a member sends an invoice, one payment link is made and
  written to the record by the integration, the payer pays, and the invoice is paid with the history
  saying the integration did it; then a replay, the same payment under another event id, an event that
  arrives late, a payment of another amount and a partial and a whole refund.
- **Isolation** (`internal/server/payments_isolation_test.go`): another workspace, a private space, an
  event of one integration or app with another's token, a revoked approval, a disabled integration, a
  test-mode event for a live reference, a definition with a URL, header, redirect or secret, a contract
  that wasn't approved, an amount above the admin's ceiling, and the secrets never being in an answer, a
  log or a row. Each shows that the thing that must not cross is there to cross: the record left alone is
  one that a payment would have changed.
- **The adapter contract** (`internal/integrations/contract_test.go`): what every payments adapter must do,
  written once in terms of the adapter interfaces and the normalized shapes: a link made once for a
  token and refused for the same token with other terms, found by its token, its state read live,
  a webhook's signature, time tolerance and rotation, and a payment, expiry and refund each read as one
  event with the ref's token and the exact amount, nothing a payer typed in it; and of an adapter that
  polls (`PaymentPoller`), a payment read back dated by the provider's time of it, the time its webhook
  event carries, not the time of the read. An adapter runs it by giving a `paymentsFixture` (its
  adapter, a client for its fake, the fake's payer, expiry and refund, and a clock it can move);
  `contract_stripe_test.go` is Stripe's and `contract_square_test.go` Square's, and
  `TestTheContractSuiteNoticesABrokenAdapter` runs the cases against adapters broken on purpose, to show
  they fail. A provider that signs its time inside the body (Square) re-signs a body with its time
  moved, and one that retries for a day gives its own window (`Tolerance`, `Ahead`).

## Trying an integration by hand

CI never calls a real provider: tests run against the fake. **Check** only looks at what the
server holds (the secrets are set and open, the settings are valid) and says nothing about
whether the provider accepts the credentials. To try a payment link against the real Stripe, by
hand and never in CI, use its **test mode** (Square: its **sandbox**, with a sandbox access token,
the sandbox location's id and a webhook subscription's signature key) with test keys of your own
that you create at the provider:
connect them in Settings → Integrations, set a public URL, allow a space, approve an invoicing app
and send an invoice; the link opens a hosted page that takes Stripe's test cards. Never use live
keys to try the software, and never paste a key into an issue, a chat or a commit.

## Not here, on purpose

- **No model-authored HTTP call.** An app, an effect or an action never holds a URL.
- **No card data in taskto.** Hosted checkout only; taskto will keep a link, a provider's
  object id and the amount it asked for.
- **No raw webhook bodies** are stored, only normalized fields and a hash.
- No refunds, payouts, subscriptions or customer records.
