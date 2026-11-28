# Data sources

An app can show data that is not its own records: first the viewer's storage use and quota, later
other usage and monitors. Like the [integrations](integrations.md), the app only **declares** a
slot of a **kind** and which of the kind's values it reads; a workspace admin approves exactly that,
by hash, before the app can be published.

## Who sees what

- A slot holds no query, no URL, no user and no id: a kind and a list of values. The server works
  out the **contract** (version, kind, scope, the sorted list of values) and its SHA-256.
- Every kind has a scope. `storage_usage` is `viewer`: the reader is given only the person who opened
  the app, so a value is always that person's own, never another's, whoever built or approved the
  app. A kind is added to the closed registry in `internal/datasource` and a new kind that reads
  anything but the viewer's own data needs its own decision about who may approve it.
- A request for approval is made by someone who may publish the app and names the hash of the draft's
  contract. A workspace admin approves, turns down or revokes it, and only an admin of the app's own
  workspace can: a request of another workspace is not found. Every request and decision is an audit
  event with the kind and the app, in the transaction of the change.
- Publishing, and rolling back, need an approved contract for every slot of the release that would
  become live. A changed list of values is a new hash and a new approval.
- The values are read when the surface or the values route is asked for, by someone who may view the
  app, and only while the contract of the shown release is approved: a revocation stops them at once,
  and a slot that is not approved or cannot be read is `{"_unavailable": true}` while the rest of the
  app draws as usual. The offline copy has no values.

Values are at `/_sources/<slot>/<value>` of the data model. The routes are in
[api.md](api.md#data-sources-of-an-app).

## Storage usage

`storage_usage` reads the viewer's private space, as the usage view does: `used_bytes`,
`quota_bytes` (`0` for no quota), `used_mb` and `quota_mb` (rounded to a tenth, for a screen to show
with its number format) and `used_percent` (not present without a quota). A person with no private
space in the workspace has no values.
