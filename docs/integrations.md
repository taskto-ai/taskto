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

Nothing calls a provider yet: no payment link is made, no stored event is applied to a record
and no app can name an integration. Those arrive as separate changes, in this order: the
declaration in an app's definition with the admin's approval of its contract; the payment
link; applying a payment to a record; polling the provider for payments the webhook missed.
The rules below are the ones those changes have to keep.

## Concepts

| Term | Meaning |
|---|---|
| **Provider** | An entry of the registry in Go (`stripe`, `square`): the secrets and settings it needs, its fixed API hosts, the kinds it implements. |
| **Kind** | A closed class of operations an app may use. Today `payments`: create a payment link, and four normalized events (`payment.succeeded`, `payment.expired`, `payment.failed`, `payment.refunded`). |
| **Integration** | A workspace admin's connection to one provider account: sealed secrets, settings, limits, the spaces that may use it, and a random `public_id` for its webhook address. |
| **Slot** | (To come.) A name in an app's definition (`payments`) of a kind. It holds no id and no secret; the admin binds it to an integration when approving the app. |
| **Contract** | (To come.) What the server derives from an app's draft for one slot: what it sends out and which fields the provider's events may write. An admin approves one by its hash. |

An app will never name a provider or an integration, and nothing in a definition, an action or
an effect will hold a URL, a header, a method, a body, a secret or a limit. The providers are a
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
   `integration_webhook_secret_reused`, which says nothing of whose it is. An id with no
   usable secret is verified against a random secret of the server's own that nobody holds, so
   the work is the same and the result is thrown away.
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
  balance call. It answers with the shapes of Stripe's API at the version the adapter pins
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
- **No raw webhook bodies** are stored, only normalized fields and a hash.
- No refunds, payouts, subscriptions or customer records.
