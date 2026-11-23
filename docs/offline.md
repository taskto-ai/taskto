# Offline use of apps

A person can **download an app for offline use**, open it without a connection, and add, change
and delete records that wait on the device until the server has them. It is explicit, it is bounded, and it never
pretends: what is on the device says it is on the device, and a record is "synced" only once the
server said so.

What works offline, and what doesn't:

| | Offline |
|---|---|
| Open a downloaded app, look around its screens | yes |
| Add a record (an `insert` action), with photos, a signature, a location or a scanned code | yes, kept on the device and sent later |
| Change or delete a record that was downloaded (an `update` or `delete` action), for the records the server said the person may change | yes, kept on the device and sent later, against the revision they were based on ([Changing and deleting](#changing-and-deleting-records)) |
| Restore a deleted record, history, effects' progress, new photos on a record that exists | no: they need the server's records as they are |
| Build or change an app with AI, publish, roll back, share, roles | no: online only, and the panel says so |
| Search, a new server query, the records explorer | no |

The server's side is [`GET …/apps/{app}/offline`](api.md#get-appsappoffline) and the ordinary
action route, with its revision check and its soft deletion; everything else is in the web app
(`web/src/offline/`). The server decides every change again when it is sent, as the person who
made it: what the device does is only what saves a wait.

## Downloading

**Download for offline use** (the app's page, *Offline use*) asks the server for the app's **live
release** and the **records the person may read**, and keeps them in the browser's IndexedDB. The
download is the person's own view: the same filter every screen uses (their records only, if the
app's roles say so; none deleted; each record as the screen shows it, with its `_revision` and no
field the collection doesn't declare), at most the latest 2000 records. It says which of those the
person may change or delete (`modifiable`: from their role's modify scope, none in a read-only
space). It holds no action's effects (their targets are the server's) and no one else's data. It also says which of the records the person's own
operations made (`operations`, the keys that [replay](#sending-replay) sends), so that a record
this device sent and a download that already holds it are the same record: see
[Refreshing a download](#refreshing-a-download).

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
already holds a copy removes it the next time it asks the server whether it may still keep each
copy, and when sending its queue is refused. It is on for a new space, and always on for a private
space, which is its owner's alone.

**A copy is asked about again** whenever the server can be reached after the device may have
missed a change: when the app opens, when the same person signs in again after the app opened
offline (the app's *Reconnect*), when the network comes back (in any open tab, before the queue
is sent), and on each fresh start. Every one of these requests (the download, the check of the
space and of the app) names the account whose database it is for (`X-Taskto-Expected-User`), and
the server refuses it for any other session. The answer is written only while this browser is
still signed in as that account: a page that learned of another account's sign-in meanwhile
writes nothing, and a revalidation that was refused as the wrong account decides nothing (the
copy stays as it is). The server's answers decide: the person was removed from the space or the app, or it was moved or deleted
(the copy is removed); downloads were turned off (removed); the person now reads fewer records
than the copy holds, all of the app's where they now read only their own (the copy is
downloaded again, which holds what they read now, and removed if that can't be done). What was
queued for the app stays.

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

An insert, an edit or a deletion made offline is a **queue item**, written to IndexedDB **together
with its photos' bytes in one transaction**. It is shown as *Saved on this device* only after that transaction has
completed; if the write fails (the quota, a closed database) the person is told it was **not**
saved and the form keeps what they typed. An item carries:

- who and where: the user, workspace, space and app ids, and the base of its routes;
- the **release** it was drawn from and that release's definition version (the server checks the
  insert against the live release, and an older one it can't take is refused: `app_release_outdated`);
- a **stable operation id**, made once with the item, sent with every attempt: the server's
  idempotency key;
- its **kind**: `insert`, `update` or `delete`;
- the record: an insert's local id (negative, and no revision until the server has it), or an
  update's or a delete's **server id and the revision the change was based on**, which is
  mandatory (`expectedRevision`; see [Changing and deleting](#changing-and-deleting-records)), and
  the record's fields as they were downloaded, to show beside the server's if there is a conflict;
- the action as the screen sent it, and its **attachment dependencies**: each photo's blob, its
  upload id once the server made one, and the attachment id once it is finalized (an insert's:
  adding photos to a record that exists needs the connection).

States, each shown on the item:

| State | Meaning |
|---|---|
| *Draft on this device* | kept, and not sent until the person chooses *Send* (*Keep as draft* holds an item) |
| *Saved on this device, waiting to sync* | queued: it is sent when the network is there |
| *Syncing…* | being sent now |
| *Synced* | **the server acknowledged the record** (its answer is what makes it so) |
| *Needs attention* | not sent, and not retried until the person says so; the input is kept |

The capture fields work offline. The device checks a **location** (a position within the globe, its accuracy,
a real `captured_at` from 2000 on and not ahead of the clock), a **barcode** (no control characters, its length
and, for `ean_8`, `ean_13` and `upc_a`, its check digits) and a **signature** (one picture's id) as the server
does (`web/src/offline/local.ts` mirrors `internal/apps/capture.go`), so a form that requires one isn't refused
for lack of a value the offline parser didn't know. A **signature's picture is a queued photo**: staged on the
device when the person signs, written with the record, uploaded first (for the signature field, as a PNG) and
replaced by the server's id in the record that is sent. The server still checks everything again.

A record's photos have a state of their own (*Photos: 1 of 2 sent*), since they can still be on
their way when the record is not.

### One writer: a single tab changes existing records

Several tabs of one account share one queue, and what they would have to do to write it together
(merge, re-check, never undo one another's choices) is far more than what a person needs. So
**changes to existing records are made by one tab at a time, the offline writer**:

- Only the writer **queues an edit or a deletion** of a downloaded record, **makes a choice** about
  a change that needs attention (*Keep the server's version*, *Apply my change on top*, *Edit and try
  again*, *Restore it and apply my change*, the repair), **discards** an item, holds or sends a draft,
  tries an item again, and clears the synced entries from the list. Inserts are independent items
  with keys of their own, and any tab may still queue them; every tab sends, as always
  ([sending](#sending-replay)).
- The other tabs of the account **show the queue and the records read-only**. They say *Changes are
  being made in another tab*, hide those buttons, answer a press that would change a record with the
  same words, and have a button, *Make changes here*, that **takes the right over**: it asks the
  holder to let go and waits for the lock; the holder becomes a reader at once. Only the first tab
  of an account is the writer without asking.
- **Who holds it is the database's word, not what a tab believes.** A row in the database's `meta`
  store names the writer's identity, and every write only the writer makes (`changeRecord`,
  `resolve`, `discardItem`, the retry, hold, release and clear of an item) reads that row in its
  own transaction and is **refused** (`NotWriter`) unless it names this tab. A tab that lost the
  right while a write was on its way, a frozen tab that woke up, is refused by the database. The
  row's **lease is compared with the clock in the same transaction**: a guarded write is also
  refused (`NotWriter`) once the lease has run out, until the holder's heartbeat renews it (or another
  tab replaces the row), so a frozen tab never writes in the interval before another tab takes over.
- **How it is held: the row is the one source of truth.** The row carries a **lease** that its
  holder renews with a heartbeat, and it is taken only by a compare-and-set in one transaction
  (`claimWriter`): when no one holds it, or the lease has run out. That is the same with and without
  Web Locks, so a tab that has them and a tab that hasn't, or that holds the right by the lease
  alone, respect one another: neither takes the right from the other's unexpired lease, and both
  report holding only when the row says so. **Web Locks only serialize acquisition**: a tab takes the
  lock named for the account, then tries the row, so tabs with Web Locks never race for it, and a
  row left by a holder whose lock is gone (its tab was closed or crashed) is free to the tab that
  holds the lock now, without waiting for the lease. A lock is held only while the tab holds the
  row: when the row is lost the lock is let go, and a lock that is granted to a tab that stopped
  meanwhile (it was closed while it waited, or while the database answered its claim) is let go at
  once, with the row if it got it. A tab that closed lets go at once; one that froze loses the right
  when the lease ends, if another tab asks.
- **A screen is bound to the session it was drawn in.** Each time a tab becomes the writer it starts
  a new **session**. A button that changes a record is accepted only on a screen drawn in the
  writer's current session, and only if the versions of the record's items are the ones the screen
  was drawn from (see [Versions](#versions-what-makes-two-tabs-agree)): a screen drawn before a
  hand-over, which doesn't show what the previous writer queued, can't merge a change into it. It
  is refused (*This record changed on this device…*) and drawn again, and the next press is made on
  what it shows.

### Versions: what makes two tabs agree

Several tabs of one account, and a page that was closed, work on one queue, so no part of it is
ever changed from a copy read earlier. Every item has a **version**, up from 1, that goes up with
each change made to it by a person or a screen (coalescing a change into it, a choice, a hold, a
release, a retry). The engine's own writes leave it: the claim on the item and its renewal, a
photo's progress, and what a send's answer made of it (synced, needing attention, waiting for
another try). Whatever of those a person is shown next, they are shown after an action of theirs
that raised it (a retry, a choice), so a choice made on what was on screen before another tab's
action is still refused, while an item that is merely being retried never makes an approval fail.
The rules:

- **Every change is a compare-and-set in one IndexedDB transaction**, against what its maker read.
  An engine's write is a function of the stored item (`updateItem`), a new change to a record is
  decided on the record's items as the transaction reads them (`changeRecord`), and a person's
  choice, discard or approval carries the **version and operation key of the item they were shown**
  (`Seen`). One that no longer matches is **refused**, with *This changed in another tab or window.
  Review it again.*, and nothing is applied: the other tab's work is never overwritten by a
  confirmation made on what was on screen before it. A **discard** is bound to the item as it was
  when the question was **asked**, its version, its operation, its state and **how many times it was
  sent** (`sends`, raised each time a send claims the item), not as it is drawn when the answer is
  given (`discardItem`): an item that went out and came back to the queue (queued, syncing, queued:
  the same version and key) may be on the server now, so a confirmation made before that is refused
  and the person is asked again about what is there. It never removes an entry the server
  acknowledged (those leave with a download that holds their change, or are only cleared from the
  list). Discarding wakes the queue: a later change to the same record that waited for it is sent
  at once.
- **Signing out loses only what was shown.** The records the person is told they would lose are
  listed with their versions, and *Discard and sign out* approves exactly those: an item that was
  changed since (a newer edit coalesced into it) or made since is not agreed to, nothing is
  deleted, and they are asked again about what is there now.
- **One base per item.** A new local change to a record joins an item that is waiting only when it
  was made on the **same base** (the same revision, the same release and definition version) and
  is the **same action** (the same name: what an action sets going on boards and documents is the
  server's to know, so a different action keeps its own item and its own identity, and a deletion
  replaces nothing). A different action is queued on its own, based on the same revision, so it is
  a visible conflict if the earlier one is sent first; a change on another base, or to a record
  another tab deleted meanwhile, is **refused** (*This record changed on this device… It is shown
  as it is now*, and the screen is drawn again): values and revisions of different bases are never
  put together. A screen drawn from another revision than the device holds now is refused the same
  way, so an item never pairs the revision a person saw with the values of another.
- **A press is bound to the screen it was made on.** The transport keeps, with each screen it
  draws (the first load, a navigation, and after each change it saved), the writer's session and the
  items of the app's queue the screen showed. A press that changes a record is refused
  (`offline_record_changed`) when the session is another one, or when the versions of the record's
  items are not those of the screen: the transaction's own check of the queue then guards what
  happens between the press's read and its write. A transport that has drawn nothing has nothing to
  be stale against.
- **A button is resolved in the release its screen was drawn from.** The transport keeps the
  release `load` drew and resolves the pressed action in its definition, queues the item for that
  release, and never in a newer one downloaded meanwhile, where the same name may be another
  action. The records it acts on are always the copy's as it is now. A download that brings
  **another release** draws the screen again (the app's view is remounted, keyed by the release),
  so the button on screen is always one of the release its transport resolves in.
- **What was read for a copy is written only into that copy.** Each downloaded copy has a
  **generation** (a counter of the database, raised whenever a copy is put or removed, never by
  a change to the copy it is). A choice about a conflict takes the generation before it reads the
  server, and the transaction that applies it **refuses** (`offline_copy_changed`) if the copy is
  not that generation any more: a revalidation that narrowed what the person may read, or a new
  download, was applied meanwhile, and the record that was read is never written back into the
  copy that replaced it. What is shown is read again.
- **The server's word is authoritative for values.** When an answer says the record's current
  revision (the answer to an update, including a replay of one the server already had, or one that
  set what was set already and so made no revision), the item keeps the record **as the server
  answered it** (`ack`), and the screen shows that record from then on: the server's revision is
  never paired with the values this device made. An answer that doesn't say leaves the record as
  the person left it, with no revision to base a change on.
- **Reading the server again at each choice.** The choices that show or reuse the server's data
  (*Keep the server's version*, *Apply my change on top*, *Edit and try again*, and the review
  itself) read the record **at that moment**, as the person who is signed in now may read it
  (`records/{id}/history`), instead of trusting what the conflict was answered with: that may be a
  record they can no longer read (it is dropped, and never written back into the download), or
  have been shown before the app changed. A choice that builds on the server's record is applied
  only if it is at the revision the person was shown; if it moved on, nothing is applied and the
  review is drawn again with the record as it is (*The record changed again meanwhile*).

### Sending (replay)

The queue is sent in the order it was made, **as the signed-in user, with the session the browser
holds then**: nothing about who may do what is remembered from when a record was made, so the
server's access checks run again on every send. It starts when the app opens, when the network
comes back, when the page returns to the foreground, when an item is added, and when an item's
wait is over. A pass goes on until nothing is left that can be sent: a record added while it ran
is sent by it.

**Only as the account that made them.** The browser's session can be another account's by the
time a queue is sent (someone else signed in in another tab). So every request the offline code
makes (a replay's photo uploads, its record and the check of the app, and the downloads and
revalidation above) carries the id of the account the database belongs to
(`X-Taskto-Expected-User`), and the server refuses it with `409 session_account_changed` for any
other session, on any `/api` route and before it looks at anything else. The page also
stops before it asks: it sends only while the account this browser says is signed in is the
queue's. Either way **sending pauses** (*Sync is paused: this browser is signed in as someone
else*), nothing is counted against the item, and it goes on when its owner signs in.

**One sender per item.** Where the browser has Web Locks, one tab at a time sends. Everywhere,
an item is claimed in its own IndexedDB transaction before it is sent (a lease that is renewed
while it goes on and that runs out if its page is closed), so two tabs, or a tab and a page that
was just closed, never send one item at once. Each claim carries the **attempt token** of the send
it was made for: what a send writes (its progress, its failure, **the acknowledgement**) is written
only while the item is still *syncing*, under **the operation key it sent** and **that attempt's
claim**. An answer that comes late, after the item was made another request by a choice or taken
by another send, finds that and writes nothing: it never marks a different operation synced, and
never replaces the acknowledgement or state of a newer attempt (the other send's own answer, the
same operation's, gets its own); a tab that finds another one sending looks again
shortly. Where there are no Web Locks the claim is kept alive for as long as the item's request is
in flight, on a timer of its own (a third of the lease), whether or not the request makes
progress: a photo upload whose chunk stalls still holds its claim, so no other tab starts it
again. If the claim is lost anyway (another page took it, the item was discarded) the request is
aborted at once and nothing of it is written.

**Changes to one record are answered in the order they were made.** An item for a record is not
sent while an earlier item for the same record has no answer for good (it is queued, being sent,
waiting for its retry, held as a draft, or *needs attention* and was left unconfirmed): a deletion
therefore never overtakes the edit before it, an edit is never reported *deleted* because its later
deletion got there first, and the ordinary outcome of two changes made on one revision is a visible
conflict on the second. The order is enforced **in the claim's own transaction**
(`OfflineStore.claimItem`): an item is claimed only if, in that transaction, no earlier item for the
record lacks an answer for good, so an earlier item that the person put back to waiting between the
pass's reading of the queue and its claim is sent first, and the later one after it. An answer for good is the server's acknowledgement or its verdict (a
conflict, a missing record, a field that doesn't fit). The revisions are not chained: the second
item is still based on the revision the person saw.

**Nothing is written or deleted on the strength of an answer that came too late.** Every request
of a pass (the photos, the record, and the check of the app after a 403 or 404) carries the pass's
`AbortSignal`, which stopping the engine or losing the claim aborts. A body cut short by an abort
is an `AbortError` (`web/src/api.ts`), never an empty answer. After each wait and before it writes
or deletes anything (an acknowledgment, the item's new state, the removal of a downloaded copy) the
send checks that the engine still runs, the signal is not aborted and the account signed in here is
the queue's owner; if not, it lets its claim go (the item is the next sender's at once, no attempt
counted) and writes nothing. A pass also waits for a revalidation that is in flight, whoever asked
for the pass (the network coming back, the page returning to the foreground, a timer, *Sync now*),
so nothing is sent before the server has said what may be kept. A tab that found another one
sending looks again after no less than `LOCK_RETRY_MS`, whatever an item's own wait says. A change
queued behind an earlier change to its record is **not woken by its own wait** (it can't be sent
before the earlier one is answered, and a zero-delay timer for it would find nothing, for ever): it
is looked for again when the earlier one is answered in the pass, or when the person's choice
(a discard) asks for one. What a send found out is acted on only while the item is still claimed by
that very send, its operation key and attempt token read in a transaction first: a reply to a send
that lost its claim, a revocation (a 403 or 404 that the app itself confirms) included, removes no
download and changes nothing.
 The photos' ids the server finalized are kept on the item, first one wins, so a send made
again is the same request, and an item the server acknowledged is never put back to waiting.

1. **Photos first.** Each photo is uploaded through the resumable upload routes (create → chunks →
   finalize). The upload's id is saved as soon as it exists, so an upload interrupted by the
   network, or by closing the page, **resumes from where the server is** the next time. The
   **record is sent only when every photo is finalized**: a record never refers to media that isn't
   there (the server also refuses one that names an unfinished upload, `app_attachment_bad`).
2. **The record**, with its operation id and release. The server writes the record **and the
   operation's receipt in one transaction**, so sending it again (the answer was lost, the page was
   reopened, two tabs) changes nothing and is answered with a 2xx (**also when what the person may do
   changed meanwhile**: a request the caller made that committed is answered with its receipt, an
   empty answer, where the access checks would now refuse it, so a device that lost the answer
   learns that the change is in, **also after the app was archived**; only the space's own route
   check, before the server reads the request, can still refuse a replay, and a receipt answers only
   on the route of the space and app it was made in) (the rows as they are *now*, not the first
   answer's body: the server keeps the request's receipt, not its answer, and a record deleted since
   is simply not among them), which is what the device needs: **a record is made exactly once**. The same id for a different request is refused (`app_operation_reused`).

What the answer means:

| Answer | What happens |
|---|---|
| 2xx | *Synced*; the photos' copies on the device are released |
| 401, the session ended | **sending pauses** (the item waits, no attempt is counted) until someone signs in again; then it resumes |
| 409 `session_account_changed` | the session is another account's: **sending pauses** as above, the item waits as it was |
| no answer, 5xx, 408, 425, 429 | transient: try again with backoff (2 s doubling to 5 min, with jitter), and the items behind it wait; after 8 attempts, *Needs attention* with **Try again now**, and the items behind it are sent (a record that gave up doesn't hold the queue) |
| 403, 404 | access lost: *Needs attention*. If asking for the app itself is also refused, the revocation is confirmed: **replay is blocked and the downloaded copy is removed**; the queued input stays, for the person to deal with. Such a refusal says nothing of an earlier send of the same change: a change that **may already be on the server** stays *unconfirmed* (it takes no further edit, and is never reused for another request). That includes a send by a tab that was lost: an item found *syncing* by the tab that claims it next was sent by one that never answered for good, so it is unconfirmed, and a refusal of the route by the next claimant leaves it so |
| 409 `app_record_conflict` (an update or a delete) | the record changed since the change was based on it: *Needs attention*, a **conflict**, with the record as the server has it now if the person may read it; never retried by itself ([Conflicts](#conflicts-and-what-doesnt-fit)) |
| 422 `app_row_not_found` (an update or a delete) | the record is gone (deleted, or no longer the person's to change): *Needs attention*, **deleted**; the change never brings it back |
| 422 or 400 for a field the app no longer takes (removed, retyped, now required, a choice that is no longer one) | *Needs attention*, **schema**: the input is kept, with what doesn't fit, to repair or discard |
| 409 archived, offline, outdated; 400/422 | the server refused it for good: *Needs attention* with the server's reason; never retried by itself. Only the server's **verdict on the operation** (a conflict, a missing record, a field that doesn't fit, a release it can't take) says it doesn't have the change; a refusal made before it looked (access, an archived or unpublished app, a photo) leaves a change that was unconfirmed unconfirmed |

The person's controls, on every item: *Try again now*, *Keep as draft* / *Send*, *Discard* (asks),
and *Sync now* for the whole queue. A **permanent failure stops retrying**: every state that
*Needs attention* waits for the person, and *Sync now* leaves alone what only they can settle
(conflicts, deleted records, what no longer fits). An item that needs a decision has *Review*.

## Changing and deleting records

A button that **changes** (`update`) or **deletes** a record the person downloaded queues an item
like an insert does. What is defined, and why:

- **The revision it is based on is mandatory.** The screen's button says the `_revision` of the
  record it was shown (as it does online), the item keeps it as `record.revision`, and the send
  carries it as `expectedRevision`. A copy that doesn't say which records the person may change
  (made by an earlier version), a record not in the server's `modifiable` list, a button that
  doesn't give a revision, and a record that is only on this device (a negative id: change or
  delete it once it is synced, or discard it from the list) are refused, in words, and nothing is
  queued. The app's own rules (a field that must be a number, a required field) are checked as an
  insert's are.
- **Shown optimistically, and labelled.** The offline screen draws a changed record as the person
  left it (`_pending`), with the `_revision` it is based on, and leaves a deleted record out; the
  queue says it is a change the server doesn't have yet, and the screen says how many are waiting.
  Counts and sums are over what is shown. A change is shown only on **the revision it was based
  on**: over a copy that holds the record at another revision (a download was taken since) the
  record is shown **as downloaded, marked for review** (`_pending: 'review'`), never with the
  change's values and a revision they were not made on; and what the server acknowledged shows
  only if no newer copy of the record is held (a no-op that left no revision of its own never
  covers a newer download).
- **Several changes of one action to one record are one item while none was sent**, on the same
  base ([versions](#versions-what-makes-two-tabs-agree)). The later values win; the item keeps its
  place in the queue and the revision it was first based on, and is sent under a new operation key
  (nothing was ever sent under the old one). A deletion, or another action, is an item of its
  own. **Once a send was started and not answered for good** (no answer, a 5xx, the page was
  closed: the change may be on the server) the item is marked and the record takes no further
  change until it is known (*An earlier change to this record is being sent*): the key is never
  reused for a different request, and a change never goes out based on a revision that another
  one of ours may have moved. **Only a definite answer clears the mark**: the server's
  acknowledgement, or its verdict on the operation (a conflict, a missing record, a field that
  doesn't fit). An answer that says the session ended (401), that the account is another's, or that
  access is lost or the app archived (403, 404: the server refused before it looked the operation
  up) says nothing of an earlier send that went unanswered, and leaves the mark as it was before
  that send.
- **The whole queue of the record is looked at, in the transaction.** `changeRecord` reads **every**
  item for the record, not the last one: any that may be on the server blocks the change (*being
  sent*), any deletion (answered or not) refuses an edit, and the queue the screen was drawn from
  (the items with their versions and states, as the transport read them for what it shows) must be
  what the transaction reads: an item added, answered or changed between is a *record changed*
  refusal, and the screen is drawn again, so a change is never based on a revision that an item
  acknowledged meanwhile has moved.
- **After the server acknowledged it**, the record is shown as the server answered it, with its
  revision, and the next change to the record is based on that revision. The screen is drawn again
  with it.
- **Deleting is the server's soft deletion**: the record is a tombstone there, kept with its
  history and restorable. A fresh download holds no deleted record, so it removes the copy of one
  (a change waiting for it stays, to be settled below, and a synced change to a record the download
  no longer holds goes with it); a download that was cut at 2000 records doesn't say a record that
  isn't in it is gone.

## Conflicts and what doesn't fit

Nothing is ever overwritten, and nothing is brought back to life, silently: the server's answer
decides, and where it can't apply a change the item **needs attention** and keeps the person's
input. *Review* shows what to choose, with the record as the server has it **only when the person
may read it**, read when it opens. A choice is made in the item's own transaction, together with
the downloaded copy it changes, and only to an item that still needs attention and is still the
item that was shown (its version and operation key): another tab that settled or changed it first
wins, and the choice is refused ([versions](#versions-what-makes-two-tabs-agree)).

| Situation | What the person sees | Choices |
|---|---|---|
| **Conflict**: two devices (or two people) changed the record from the same revision | a table of each field as it was when they started, their change, and the server's value now | *Apply my change on top* (the same change on the revision just shown, under a new key; for a deletion, *Delete it anyway*); *Keep the server's version* (their change is discarded and the copy holds the server's record); *Edit and try again* (a form of the record's fields; **only the fields the person changed** are applied, onto the record as the server has it now, so what they didn't touch is never written: the form remembers just what was edited, and when the record is read again while it is open every other field shows the new values); *Discard* |
| **Conflict, record no longer readable** | that there is a newer version they can't read | *Keep the server's version* (discard) |
| **Deleted** on the server while it was changed here | who deleted it and when, from the list of deleted records | *Restore it and apply my change* (the restore is sent first, then the change on the revision it gives; a restore whose answer was lost is recognised as the same restore; a restore that finds the record deleted again at a newer revision is a deleted record still, and is offered again at the revision it has now), *Discard my change*. A deletion of a record already deleted has nothing left to send: *Clear this*. The restore is the person's own step and needs the right to modify; if it is refused the item says so |
| **Schema**: a field removed, retyped, now required, a choice narrowed | what doesn't fit, field by field, checked against the app as it is live now (the copy is downloaded again to know it), and the input as entered | *Fix and try again* (a form of the live fields; the item is then made for the live release and its definition version, on a new key), *Discard* |
| **Outdated, refused** | the server's reason | the same repair, or *Discard* |
| **Access lost, archived**, an app taken offline | the reason; the copy is removed if the app can't be opened any more | *Discard*; retry from the list once access is back |

Fixing never silently drops input: a value that doesn't fit the live app is refused in the form
with the server's words and the item stays as it was. A field the app has removed is listed as
such; it is left out of the new request only when the person saves the repaired one. Any other
refusal is shown with the server's own reason.

**The same guarantees as an insert's.** Every request of a change or of a choice (the send, the
restore, the check of the deleted records, the download that repairs) names the account the
database belongs to and is written only while the browser is still signed in as them; a different
account signed in pauses sending without counting anything against the item; a stopped engine or a
lost claim writes nothing; unsent changes count as unsent for signing out and for another account
signing in (sealed, never deleted by a sign-in), and are purged only by the one conditional step.
What a send finds out after any wait (the answer, a restore, a finished upload) is written only
while the account that made the queue is still the one signed in here (the account epoch): a
success that comes in after another account signed in changes nothing in the old account's queue,
and the item stays as it was, unconfirmed, to be sent again, which the server answers with the
same result. The check is made **in the transaction that writes**: the store asks, after what it
read and before what it puts, whether the account signed in here is the database's (and refuses
the write, `AccountLapsed`, if not), so a sign-in that comes between the engine's own check and the
write can't be written under the account that has left. *Clear synced records* takes the entries out of the list, and an entry stays in the
queue (cleared) until a download that holds its change lets it go, so a deleted record is never
shown again from a copy that doesn't know of the deletion yet.

## Accounts, signing out and revocation

Everything is kept in **one IndexedDB database per user**, `taskto-offline-<user id>`: downloads,
the queue and the photos. A database's name is not a boundary on its own, so another account
signing in is dealt with when it signs in:

- **Signing in** makes the browser that account's (the *last user* marker moves first, so a
  database that can't be opened is never replaced by the previous account's). If the marker can't
  be written (the storage is full or blocked) it is removed, so an offline start never opens the
  previous account's database; if it can't be removed either, offline starts are **refused**, and
  the refusal is kept where the next page finds it (a flag in `localStorage`, one in
  `sessionStorage`, and one in a small IndexedDB database of its own: whichever can be written),
  so no page opens the last account's database from a marker that survived while another account
  was the last to sign in. A later sign-in that can move the marker ends the refusal. (Only a
  browser whose storage can't be written at all can't keep it; the tests can't tell what a real
  one does.) **The account epoch:** who the marker names is checked again after every wait while
  a database is being opened (before the sweep, between the other accounts' databases, before
  and after opening this one): if another tab signed in as someone else meanwhile, the sweep stops
  and this tab shows *The account changed* instead of publishing the database it was opening.
  What the last
  account kept is deleted at once: its downloads, its records that reached the server and their
  photos, and the whole database if nothing was left unsent. If **records of another account have
  not reached the server**, those alone stay, **sealed**: nothing of them is shown or sent, and
  the person signing in is **asked first, with nothing else on screen**: *This browser holds N
  unsent records of another account. Sign in as that account to send them, or discard them.*
  *Discard them* (asks again) deletes everything of that account on this device; signing out
  leaves them for their owner to sign in and send. A database that can't be opened, read or cut
  down at that point (written by a newer version, or a storage error that may pass) is **never
  deleted by the sign-in**: it stays as it is, sealed, and is looked at again at the next
  sign-in. When the app can't tell whether a database holds unsent work, it never deletes it
  silently and never shows it to another account.)
- **A database that can't be opened** (it is of a newer version, the storage isn't available) is
  said so, in words: the page never falls back to another account's database, and the offline
  view opens the signed-in account's own or none.
- **Signing out**, from anywhere (the menu, the invite page), asks first if records on the device
  haven't reached the server: *Sync now*, *Discard and sign out*, or *Cancel*. *Sync now* retries
  what needs a retry, which only the writer does: in a tab that isn't, it asks for the right first
  (as the take-over button does), and if the tab doesn't get it says so in words and tries nothing.
  Then the account's
  database is **deleted**: downloads, queue and photos, whether or not it had been opened in this
  page. Counting and deleting are **one step**: in a single IndexedDB transaction the database is
  checked for unsent records other than those the person was shown (and agreed to lose) and, only
  if there are none, emptied and **marked as being deleted**; from then on a record committed to
  it (a stale tab, another tab of the same account) is refused and reported as not saved, never
  lost without a word. If an unsent record turned up after the person was asked, nothing is
  deleted and nobody is signed out: they are asked again about what is there now. The same step
  decides the other accounts' databases when someone signs in, so a stale tab's record committed
  during that sweep is counted, never deleted. The sign-out request names the account it is for
  (`X-Taskto-Expected-User`), so a stale tab never signs out the account that is signed in now:
  the server refuses it with `409 session_account_changed` and the tab shows *The account
  changed*. If the queue **can't be opened or counted** (a newer version wrote it, the storage isn't
  available) the count is not taken to be zero: the person is told that unsent records may exist
  that this version can't check, and chooses *Keep them and sign out* (nothing is deleted; the
  database stays for the version or the moment that can read it) or *Discard and sign out*
  (asks again).
- **Another tab changes the account.** A tab learns that the account signed in on this browser
  changed (another tab signed in as someone else, or out: the browser's `storage` event and a
  BroadcastChannel; or the server answered `409 session_account_changed` to one of its requests)
  and lets go at once: its engine stops, what it holds in memory is dropped (the offline view
  unmounts, with the records and photos on screen), and its connection to the database is closed
  as soon as what the engine was writing is. It shows only *The account changed*, with a button to
  reload the page, until it is reloaded as whoever is signed in. Nothing is deleted by it.
- **An ended session** (it expired, or was ended elsewhere) is not signing out: the queue stays
  and sending pauses until the person signs in again.
- **Revocation** (removed from the space or the app, the app moved or deleted, an admin turned
  downloads off, a narrower read scope) is confirmed by the server when the app opens, when the
  same person returns online, or on the next send, and removes the copy of the records that
  person may no longer read ([Downloading](#downloading)).

## Refreshing a download

**Downloads are ordered by when they began.** Before its request is made, a download takes a
**ticket** from a counter in the database (one for every tab of the account), and the copy is written
only if no download that began after it is held, and the app's copy was not removed after it began,
decided in the writing transaction (`putDownload`, `StaleDownload`). An older answer that arrives
late (the revalidation's, a second tab's) is never written over a newer copy: the record that the
newer one no longer holds (deleted, with the device's note of the deletion let go together with it)
can't come back from it, and a copy removed for a revocation is not brought back by a download
that was already on its way. The caller is answered with the copy the device holds.

A download is a snapshot, and a record this device sent may be in the next one. The download says
which of its records the person's own operations made, and a record whose operation key is among
them is the server's: the offline screen shows it **once**, and a fresh download removes the
device's own entry for it. Until a download includes it, the entry is shown, labelled as saved
here, and counted in "saved here"; the numbers never count it twice.

What this doesn't do: the data is not encrypted at rest on the device, and someone with the
unlocked device and the browser profile can read it, as with the browser's other site data. Use
the admin switch for spaces where that is not acceptable.

## Updates and the schema

The database is **versioned** (`SCHEMA_VERSION` in `web/src/offline/store.ts`) and migrated step by
step, each version's migration run once in the upgrade's own transaction; a migration is added,
never edited. Version 2 added the item's kind: what version 1 queued was only inserts, so its items
are marked as such when the database is upgraded, and are sent exactly as before (same operation
key, same request); a copy downloaded by version 1 keeps working and offers no change until it is
downloaded again. A service worker update waits for the person ([pwa.md](pwa.md)), so a page and its
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
pressure); that an edit made on one device and a conflicting one made on another are told apart
the way the tests' stand-in server does (the Go tests cover the real server's side, the web tests
a stand-in for it); that a person reads the conflict table and the repair form comfortably on a
phone-sized screen; that a phone's camera photo, held in IndexedDB as a blob, survives the app being killed
in the background; that Web Locks and `online` events fire the way the code expects after a
phone's sleep, and that real tabs hand the offline writer over as the tests' small stand-in for Web Locks and
for the channel between tabs does (the database's own refusal of a write by a tab that isn't the writer is tested); that the offline view opens after a device restart with no network; and the
durability of a write on a phone that loses power right after "Saved on this device".
