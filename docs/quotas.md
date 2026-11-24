# Storage quotas and usage

The operator can cap how much a workspace stores, and how much each member's private space
does. Nothing is capped by default: a server that sets neither behaves as it always did.

## Setting them

Two settings, each in bytes or with `KiB`, `MiB` or `GiB` (the same forms as the knowledge
base's limits), by flag, `TASKTO_*` variable or the config file, in that order of
precedence:

| Flag | Variable | Config key | Caps |
|---|---|---|---|
| `-storage-quota-workspace` | `TASKTO_STORAGE_QUOTA_WORKSPACE` | `storage_quota_workspace` | what all of a workspace's spaces store together, shared and private |
| `-storage-quota-user` | `TASKTO_STORAGE_QUOTA_USER` | `storage_quota_user` | what each member's private space stores |

Leaving one out, or never setting it, is unlimited (zero is not a value to write). Both are
the same for every workspace and every member of the server; they are defaults the operator
sets once, not settings admins change. A member who belongs to several workspaces has a
private space in each, and each has the allowance.

A private space is bounded by both: the private quota for itself, and the workspace's for
everything in the workspace, theirs included. A shared space is bounded by the workspace's.

## What counts

Per space:

- the knowledge base's **versioned** bytes: git's repository, every version of every
  document, which deleting a document doesn't free (history keeps it; an admin can take a
  file's old versions out, [api.md](api.md#taking-a-files-old-versions-out-of-history));
- its **unversioned** files: large files, audio and video, whose bytes sit beside git
  ([api.md](api.md#unversioned-files)); deleting or replacing one frees them;
- the apps' **photos**, with their thumbnails. An upload counts from the moment it starts,
  at the size it declared, so two uploads in flight can't both take the last bytes.

What isn't counted: apps' records, boards, conversations and everything else in the
database. The quotas are about files.

The knowledge base's figures are an estimate of git's side, a few hundred bytes of
bookkeeping short of what `du` reports.

## What is refused

A write that adds content to a space and would take it, or its workspace, over a quota:

| Where | Checked |
|---|---|
| Knowledge base: creating or replacing a document or an upload (the web app, the REST API, the agent's tools and the effects of apps on documents) | before the change, inside the space's write lock, so two writers can't both fit; an unversioned file is checked at its own size, less the file it replaces |
| App photos | when the upload starts, at its declared size, and when it is finalized, for what is kept (the picture without its metadata, and its thumbnail) beyond what was declared |

The answer is `507` with the code `quota_user` (the private space's quota) or
`quota_workspace` and `max_mib`, the quota in MiB; the web app shows it translated, and an
agent's tool is refused with a `507` and the error's English text. Nothing is written, and the upload that
started is the only trace of one refused at finalize, which can be finished once there is
room, or abandoned.

A change that adds no content is never refused for a quota: deleting, moving and restoring
a document. A full space can always make room. (A document delete frees the bytes of an
unversioned file at once, but not a versioned one's, whose history stays.)

Lowering a quota below what a space already stores refuses new content there until it is
under, and never removes or blocks anything it holds.

## Seeing usage

Settings → Storage. Admins see the workspace's total against its quota and every space,
the biggest first: the shared ones, and each member's private space under its owner's name,
split into documents, files and photos, with a bar against the private quota. A member sees
their own private space only. The routes are in [api.md](api.md#storage-usage); a member
never reads another's usage, and an admin reads only their own workspace's.

## How the figures are kept

A request for usage, and each write's check, never walks git:

- The knowledge base's writer, which holds the space's lock, keeps `.git`'s size as it
  already did for its repository limit, and the unversioned files' as a count it drops when
  bytes are adopted, removed or moved. At the end of every write it publishes the two
  figures for the space (`knowledge.Store.Usage`).
- A space nobody has written since the server started has none published: the first reader
  measures its files once, without any lock, and keeps the result unless a write published
  meanwhile.
- Photos are one indexed query per workspace (`app_attachments` by space).

The workspace's quota is checked by adding up its spaces' published figures, so a check
costs a lookup per space and a query, not a scan. It takes no space's lock but the
writer's own.
