# Offline use of apps

A person can **download an app for offline use**, open it without a connection, and add records
that wait on the device until the server has them. It is explicit, it is bounded, and it never
pretends: what is on the device says it is on the device, and a record is "synced" only once the
server said so.

What works offline, and what doesn't:

| | Offline |
|---|---|
| Open a downloaded app, look around its screens | yes |
| Add a record (an `insert` action), with photos | yes, kept on the device and sent later |
| Change or delete a record, restore, history, effects' progress | no: they need the server's records as they are |
| Build or change an app with AI, publish, roll back, share, roles | no: online only, and the panel says so |
| Search, a new server query, the records explorer | no |

The server's side is [`GET …/apps/{app}/offline`](api.md#get-appsappoffline) and the ordinary
action route; everything else is in the web app (`web/src/offline/`).

## Downloading

**Download for offline use** (the app's page, *Offline use*) asks the server for the app's **live
release** and the **records the person may read**, and keeps them in the browser's IndexedDB. The
download is the person's own view: the same filter every screen uses (their records only, if the
app's roles say so; none deleted; each record as the screen shows it, with no field the collection
doesn't declare), at most the latest 2000 records. It holds no action's effects (their targets are
the server's) and no one else's data.

The panel shows what the person is relying on:

- **Readiness:** *Ready for offline use*, the release number, when it was downloaded, and how many
  of how many records are on the device (and that only the latest, or only their own, were
  downloaded). When a newer release is live it says so: download again to get it.
- **Storage:** how much the site uses of what the browser allows (`navigator.storage.estimate()`),
  and whether the browser agreed to keep the data (`persist()`, requested after a download). A
  persistence request improves the odds and promises nothing: a browser may still clear a site's
  data under pressure, so the panel says which of the two it is, and the app works the same.
- **A write that fails** (the storage is full, it is not available) is shown as the failure it is,
  and no copy is kept as if it were.

A release that **requires a capability this client lacks** (a newer component, a newer definition
format) isn't downloaded, and one already on the device shows *update required* instead of a broken
app, with the same button online does.

**Admins can turn offline downloads off for a shared space** (Settings → Spaces → *Allow offline
downloads*, `offline_downloads` on the space): for data that shouldn't stay on devices. New
downloads of the space's apps are refused (`403 offline_downloads_disabled`), and a device that
already holds a copy removes it the next time the app opens with the server reachable (the
device asks whether it may still keep each copy), and when sending its queue is refused. It is on for a new space, and always on for a private space, which is its owner's
alone.

## Using it offline

When the server can't be reached and someone signed in on this browser before, the app opens the
**offline view** instead of an error: their downloaded apps, and the queue. (The session is the
browser's cookie, which can't be checked offline; this view shows only that user's own database,
and everything that matters is decided by the server when the queue is sent.) Each app is drawn
from the downloaded release by the same renderer, from the service worker's precache.

**Numbers are labelled.** A list or a total drawn offline covers the downloaded records and the
records queued on the device, and the view says so, with the counts: *Offline copy: lists and
numbers cover the records downloaded {when} (downloaded: N) and those saved on this device (saved
here: M). They are not the server's totals.* The summaries (count, sum, with their `where`) are
recomputed from those records with the same rules the server uses
([`rules.ts`](../web/src/apps/rules.ts)).

## The outbox

An insert made offline is a **queue item**, written to IndexedDB **together with its photos' bytes
in one transaction**. It is shown as *Saved on this device* only after that transaction has
completed; if the write fails (the quota, a closed database) the person is told it was **not**
saved and the form keeps what they typed. An item carries:

- who and where: the user, workspace, space and app ids, and the base of its routes;
- the **release** it was drawn from and that release's definition version (the server checks the
  insert against the live release, and an older one it can't take is refused: `app_release_outdated`);
- a **stable operation id**, made once with the item, sent with every attempt: the server's
  idempotency key;
- the record's local id (negative) and its revision (none until the server has it);
- the action as the screen sent it, and its **attachment dependencies**: each photo's blob, its
  upload id once the server made one, and the attachment id once it is finalized.

States, each shown on the item:

| State | Meaning |
|---|---|
| *Draft on this device* | kept, and not sent until the person chooses *Send* (*Keep as draft* holds an item) |
| *Saved on this device, waiting to sync* | queued: it is sent when the network is there |
| *Syncing…* | being sent now |
| *Synced* | **the server acknowledged the record** (its answer is what makes it so) |
| *Needs attention* | not sent, and not retried until the person says so; the input is kept |

A record's photos have a state of their own (*Photos: 1 of 2 sent*), since they can still be on
their way when the record is not.

### Sending (replay)

The queue is sent in the order it was made, **as the signed-in user, with the session the browser
holds then**: nothing about who may do what is remembered from when a record was made, so the
server's access checks run again on every send. It starts when the app opens, when the network
comes back, when the page returns to the foreground, and when an item's wait is over. Where the
browser has Web Locks, one tab at a time sends.

1. **Photos first.** Each photo is uploaded through the resumable upload routes (create → chunks →
   finalize). The upload's id is saved as soon as it exists, so an upload interrupted by the
   network, or by closing the page, **resumes from where the server is** the next time. The
   **record is sent only when every photo is finalized**: a record never refers to media that isn't
   there (the server also refuses one that names an unfinished upload, `app_attachment_bad`).
2. **The record**, with its operation id and release. The server writes the record **and the
   operation's receipt in one transaction**, so sending it again (the answer was lost, the page was
   reopened, two tabs) changes nothing and is answered as the first send was: **a record is made
   exactly once**. The same id for a different request is refused (`app_operation_reused`).

What the answer means:

| Answer | What happens |
|---|---|
| 2xx | *Synced*; the photos' copies on the device are released |
| 401, the session ended | **sending pauses** (the item waits, no attempt is counted) until someone signs in again; then it resumes |
| no answer, 5xx, 408, 425, 429 | transient: try again with backoff (2 s doubling to 5 min, with jitter); after 8 attempts, *Needs attention* with **Try again now** |
| 403, 404 | access lost: *Needs attention*. If asking for the app itself is also refused, the revocation is confirmed: **replay is blocked and the downloaded copy is removed**; the queued input stays, for the person to deal with |
| 409 archived, offline, outdated; 400/422 | the server refused it for good: *Needs attention* with the server's reason; never retried by itself |

The person's controls, on every item: *Try again now*, *Keep as draft* / *Send*, *Discard* (asks),
and *Sync now* for the whole queue.

## Accounts, signing out and revocation

Everything is kept in **one IndexedDB database per user**, `taskto-offline-<user id>`: downloads,
the queue and the photos. Another account on the same browser never opens it:

- **Signing in** claims the browser for that account, like the unsent drafts do: any other
  account's database with nothing waiting is deleted; one with unsent work keeps only that work,
  sealed under its owner's id (its downloaded records are removed, and nobody else's code opens it).
- **Signing out** asks first if records on the device haven't reached the server: *Sync now*,
  *Discard and sign out*, or *Cancel*. Then the account's database is **deleted**: downloads, queue
  and photos.
- **An ended session** (it expired, or was ended elsewhere) is not signing out: the queue stays
  and sending pauses until the person signs in again.
- **Revocation** (removed from the space or the app, the app moved or deleted, an admin turned
  downloads off) is confirmed by the server when the app opens or on the next send, and removes the copy
  of the records that person may no longer read.

What this doesn't do: the data is not encrypted at rest on the device, and someone with the
unlocked device and the browser profile can read it, as with the browser's other site data. Use
the admin switch for spaces where that is not acceptable.

## Updates and the schema

The database is **versioned** (`SCHEMA_VERSION` in `web/src/offline/store.ts`) and migrated step by
step, each version's migration run once in the upgrade's own transaction; a migration is added,
never edited. A service worker update waits for the person ([pwa.md](pwa.md)), so a page and its
database change version together. If a database is **newer than the page** (an installed version
newer than this tab), it is left untouched and the page asks to be updated rather than risk the
queue. The queue's items name the release and definition version they were made against, so an
update never reinterprets them: the server decides again.

The service worker never stores or answers anything under `/api`: the downloaded records are in
IndexedDB, not in a cache the worker keeps. It precaches the renderer's chunks and the offline
view with the rest of the app ([pwa.md](pwa.md)).

## What the tests don't cover

The tests run in jsdom with an in-memory IndexedDB (`fake-indexeddb`, used by the tests only). Only
real devices show: that a browser's quota and eviction behave as `estimate()` and `persist()` say
(Safari's seven-day rule for sites without a home-screen install; Chrome's eviction under
pressure); that a phone's camera photo, held in IndexedDB as a blob, survives the app being killed
in the background; that Web Locks and `online` events fire the way the code expects after a
phone's sleep; that the offline view opens after a device restart with no network; and the
durability of a write on a phone that loses power right after "Saved on this device".
