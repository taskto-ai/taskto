# Single sign-on (OpenID Connect)

People can sign in with an identity provider instead of a password: Google, Authentik, or any
other OpenID Connect provider (Keycloak, Okta, Microsoft Entra…). The operator lists the
providers in a file; there is no admin screen for them.

- **Invitees only.** Signing in doesn't sign anyone up. It logs in people who already have an
  account, and makes an account only for an email address that has a pending invite. Everyone
  else is told to ask a workspace admin for an invite.
- **Passwords stay on** unless the operator turns them off ([below](#turning-passwords-off)).
- The server never sees the person's password at the provider. It gets an ID token it checks
  itself: signature, issuer, audience, expiry and a nonce.

## How it works

1. On the login screen, or an invite page, the person clicks "Continue with *name*". The web
   app calls `POST /api/sso/{provider}/start`. The server makes a random state, nonce and PKCE
   verifier, keeps them in memory for 10 minutes, sets a short-lived cookie with the state, and
   answers with the provider's authorization URL, which the web app opens.
2. The provider signs the person in and redirects the browser to
   `<public_url>/api/sso/{provider}/callback`. The server checks the state against the cookie
   and its memory (once only), trades the code for tokens with the PKCE verifier, verifies the
   ID token, and reads `sub`, `email`, `email_verified` and `name`. If the ID token has no
   email, it asks the provider's UserInfo endpoint.
3. In one database transaction, the server finds the account:
   1. the identity (issuer and `sub`) is already linked: that user logs in;
   2. otherwise a user has the verified email: the identity is linked to them and they log in;
   3. otherwise the email has a pending invite: the account is created with no password (an
      SSO-only account), named from the `name` claim (or the start of the email), it accepts
      **every** pending invite for that email, and the identity is linked;
   4. otherwise the login is refused: `sso_no_account`.

   With Google, steps 2 and 3 have one more condition ([below](#google-and-email-addresses-it-doesnt-own)).
4. The person gets a session and is sent back to the page they came from. A failure sends them
   to `/?sso_error=<code>`, which the login screen shows in their language. If the sign-in
   made the account from its invites and the page was the invite screen, they are sent to the
   conversations of a workspace they joined instead, since the invite link is used up: the one
   whose invite link they opened, if that link was one of the invites the sign-in accepted, or
   else the first invite accepted.

An identity is the issuer and the `sub` claim, never the email: a person who changes their
email at the provider still logs in as the same user. An email whose user already has another
identity from the same provider is refused (`sso_account_linked_elsewhere`), so a second or
recreated account at the provider can't take the account over by having the same address.

## Configuration

Three settings, each a flag, a `TASKTO_*` environment variable and a config file key (the
usual precedence, [README](../README.md#running)):

| Flag | Variable | Key | |
|---|---|---|---|
| `-sso-providers` | `TASKTO_SSO_PROVIDERS` | `sso_providers` | Path of the providers file. |
| `-public-url` | `TASKTO_PUBLIC_URL` | `public_url` | The server's external origin, such as `https://taskto.example.com`. Required with `sso_providers`. It must be `https`, with no path, query or credentials; `http` is accepted only for `localhost`, `127.x` and `[::1]` together with `insecure_cookies`, for development. |
| `-disable-password-login` | `TASKTO_DISABLE_PASSWORD_LOGIN` | `disable_password_login` | Turns passwords off. Needs `sso_providers`. |

`public_url` is what the redirect URIs you register at the provider are built on:
`<public_url>/api/sso/<id>/callback`.

### The providers file

Strict JSON: an unknown field is an error. Errors name the field and never quote a value. The file is read when the server
starts, so a mistake stops it there; restart to change it.

```json
{
  "providers": [
    {
      "id": "google",
      "name": "Google",
      "issuer": "https://accounts.google.com",
      "client_id": "1234567890-abc.apps.googleusercontent.com",
      "client_secret": { "file": "/run/secrets/taskto-google-secret" }
    },
    {
      "id": "authentik",
      "name": "Authentik",
      "issuer": "https://auth.example.com/application/o/taskto/",
      "client_id": "taskto",
      "client_secret": { "env": "AUTHENTIK_CLIENT_SECRET" }
    }
  ]
}
```

| Field | | |
|---|---|---|
| `id` | required | 1–32 characters of `a-z`, `0-9` and `-`, unique. It is in the URLs, including the redirect URI. |
| `name` | required | What the button says: "Continue with *name*". |
| `issuer` | required | The provider's issuer URL, whose `/.well-known/openid-configuration` is read. `https` (`http` only for a localhost provider). |
| `client_id` | required | |
| `client_secret` | required | Where the secret is: `{ "file": "/absolute/path" }` (read with surrounding whitespace trimmed) or `{ "env": "VARIABLE" }`. Never the secret itself. |
| `scopes` | optional | Default `["openid", "email", "profile"]`. Must include `openid`. |
| `trust_email` | optional | Default `false`: the ID token's `email_verified` must be `true`. See below. |

The providers aren't contacted at startup: each is discovered the first time someone uses it,
so a provider that is down doesn't stop the server, and works again once it is back. Everyone
who logs in while a provider is being asked shares the one request and waits at most its
15-second timeout; a failure is forgotten, so the next login asks again.

**Prefer `{"file": …}` for the client secret.** A secret named with `{"env": …}` has to be in
the server's environment, which child processes inherit and operators' tooling (`docker
inspect`, `/proc`, crash reports) can show. The server removes each variable it read from its
own environment at startup, before it starts the agent runtime or anything else, and the agent
runtime's environment is built without it; a file, though, is never in an environment at all,
and can be mode 600 and owned by the server's user alone.

Every endpoint the provider's discovery document names (authorization, token, keys, user info)
must be `https`: one that isn't makes the provider fail before any request carrying a
credential is made (`http` is accepted only when the issuer itself is a `localhost` one, for
development, and then only to loopback hosts: the exception belongs to that provider alone, not
to the others). Discovery, keys and user info follow a redirect only within the origin they
started from, so a redirect never leaves `https`, never goes to another origin, and the user
info's bearer token stays where it was sent. The token exchange, which carries the code, the
PKCE verifier and the client secret, follows no redirect at all.

### `trust_email`

An email address is what links a provider's identity to an existing account or to an invite,
so the server accepts it only when the provider says it checked it (`email_verified: true`).
Set `trust_email` to `true` only for a provider whose admins control every user's email
address, so nobody can claim an address that isn't theirs: for example Authentik when users
can't edit their own email. The better fix is to make the provider send
`email_verified: true` (for Authentik, a property mapping, below) and leave `trust_email`
off. Never set it for a provider where people can sign up themselves, or change their own
address without confirming it.

## Setting up Google

1. In the Google Cloud console, pick or create a project, then **APIs & Services → OAuth
   consent screen** and set it up. For a company, choose the **Internal** user type: only
   accounts of your Google Workspace can then sign in.
2. **APIs & Services → Credentials → Create credentials → OAuth client ID**, application type
   **Web application**.
3. Under **Authorized redirect URIs**, add `<public_url>/api/sso/google/callback`, for example
   `https://taskto.example.com/api/sso/google/callback`.
4. Copy the client ID and the client secret. Put the secret in a file only the server can read
   (mode 600) and name it in `client_secret`.
5. Add the provider with the issuer `https://accounts.google.com`, as in the example above.

Google sends `email_verified: true` for its accounts, so `trust_email` stays off (and it
wouldn't help: see the next section).

### Google and email addresses it doesn't own

Google can keep `email_verified: true` for an address that isn't Gmail after the mailbox changes
hands (a domain that lapsed and was bought, say). Following Google's guidance, when the issuer
is `https://accounts.google.com` the server links an identity to an existing account by its
email, and makes an account from an invite, **only if** the `email` claim's domain is `gmail.com`
(ASCII letters, either case: a Unicode look-alike is refused) or the ID token has an `hd` claim
that isn't blank (a Google Workspace account). Otherwise the login is refused
with `sso_google_email_untrusted`, and `trust_email` doesn't change that. An identity already
linked (by issuer and `sub`) keeps working without the check, so it matters only the first time.
Someone with a personal Google account on another domain can't use the first sign-in with
Google; they can sign in with another provider or with a password.

## Setting up Authentik

1. **Applications → Providers → Create**: an **OAuth2/OpenID Provider**. Client type
   **Confidential**. Note the client ID and client secret.
2. **Redirect URIs/Origins**: `<public_url>/api/sso/authentik/callback`, as a strict match.
3. **Signing Key**: pick one, so ID tokens are signed with RS256 (without a key Authentik signs
   with the client secret, which this server doesn't accept).
4. **Scopes**: the default `openid`, `email` and `profile` mappings.
5. **Applications → Applications → Create**: an application that uses this provider. Its slug
   is in the issuer.
6. Add the provider with the issuer `https://<authentik>/application/o/<slug>/` (keep the
   trailing slash), the client ID, and the secret.
7. **`email_verified`.** Authentik sends `email_verified: false` by default. Either edit the
   `email` scope mapping (**Customization → Property Mappings**) so it returns both claims:

   ```python
   return {
       "email": request.user.email,
       "email_verified": True,
   }
   ```

   which is right only if your Authentik admins control users' emails; or, with the same
   condition, set `"trust_email": true` for this provider.

## Who may sign in

taskto.ai makes accounts only for invited addresses, so who can get in is who an admin invites.
Within that:

- **Invites:** an admin invites `person@example.com` as always. When someone **without an
  account** signs in with the provider and the provider vouches for that address, the account is
  made and **every** pending invite for it is accepted at once, so nobody needs to pass on the
  invite link and a person invited to several workspaces joins them all at their first sign-in.
  Someone who **already has an account** is only signed in: they still open the invite link and
  press Join after signing in, to join the workspace.
- **Authentik:** bind a group or user policy to the application (**Policy / Group / User
  Bindings**) to decide which of your users can use it at all.
- **Google Workspace:** an **Internal** consent screen limits sign-in to your organization.

An existing account (an owner created at setup, or anyone with a password) is linked the first
time its owner signs in with a provider that vouches for the same address. After that, the
provider's `sub` is what counts.

## Turning passwords off

`disable_password_login = true` (or the flag, or the variable) turns password sign-in off:

- `POST /api/login` answers `403 password_login_disabled`.
- A new person can't create an account with a password from an invite (`403`); they sign in
  with a provider. Someone who is logged in can still join with their invite.
- Reset links can't be made or used (`403 password_login_disabled`), since nobody could use one:
  Settings → Members has no "Reset link" action, and a reset link that was made earlier opens a
  page that says password login is turned off.
- The login and invite screens show the provider buttons only.

First-run setup still creates the owner **with a password**, since there is no provider yet at
that point to vouch for them. With passwords off, the owner then signs in with the provider
using the same email, which links the identity to the account. Keep the provider's
configuration right before you turn passwords off: if no provider works, nobody can log in, and
the way back is to remove the setting and recreate the server. With Compose, change the
`.env` and run `docker compose up -d taskto`, which recreates the container with the new
environment; `docker compose restart` keeps the old one and changes nothing.

Accounts made by signing in have no password. They can't use the password login, and "change
password" refuses them. If passwords are on, an admin's reset link still lets such a person
set one.

## Two-factor login

taskto's own [two-factor login](api.md#two-factor-login) (a TOTP code after the password) is
for **password logins only**. Signing in with a provider makes a session as soon as the
provider vouches for the person, with no code asked: whether that sign-in needs a second
factor is for the provider to decide, and for you to turn on there (Google's 2-Step
Verification, an Authentik authentication flow with an authenticator stage). A person who has
two-factor login on and signs in with both a password and a provider is asked for a code
only on the password.

## Security notes

- **The callback is a state-changing `GET`**, the one such route, because the provider's
  redirect is a browser navigation. It is safe because it acts only on a state that is in
  the browser's own `__Host-taskto_sso` cookie (`HttpOnly`, `SameSite=Lax`) *and* in the
  server's memory, compared in constant time and used once: a forged link finds no matching
  cookie, and a replayed one finds no state. The authorization code is bound to the login by
  PKCE (S256), and the ID token by its nonce. The start is a `POST`, so the CSRF guard covers it.
- **Pending logins are bounded:** at most 10 000 at a time, held 10 minutes, and 30 starts per
  client address per 15 minutes.
- **No open redirect.** Where the person returns to is a path on this server: it starts with
  one `/`, and is rejected if it starts with `//` or `/\`, or has a backslash or a control
  character, or is longer than 2048 bytes. Anything else goes to `/`.
- **ID tokens are verified by [go-oidc](https://github.com/coreos/go-oidc)**: signature against
  the provider's published keys, issuer, audience (your client ID), expiry. The server then
  checks the nonce and that a subject is present, that the audience is your client ID and
  nothing else (an ID token also meant for another client is refused), and that `azp`, when
  present, is a string equal to your client ID (OpenID Connect Core 3.1.3.7): an empty or `null`
  `azp` is refused, not taken as absent.
- **Identities are the issuer and the subject**, not the email.
- **Secrets:** the client secret is read from a file or variable and is never logged, or put in
  an error. Tokens are never logged. Nor is what a provider sends back: a failure is logged as
  a fixed category and, when the provider answered, its HTTP status (and a standard OAuth error
  code such as `invalid_grant`), never its response body, `error_description` or other text,
  which could carry anything. The login's redirect never carries it either.
- **The cookie is never cleared by a callback.** It lives 10 minutes, as long as the state, which
  works once: a leftover cookie is harmless, and a slow or stale callback (an older sign-in in
  another tab, or one still waiting on the provider) can't take away the newer sign-in's. A
  callback to another provider than the login's consumes nothing.
- Calls to the provider (discovery, keys, tokens, UserInfo) use a 15-second timeout. They are
  not covered by the [egress policies](agent-providers.md), which are for model providers.
