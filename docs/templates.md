# App templates

Four starter apps come with the server. A template is only a **definition** (the same JSON as
any app's, checked by the same catalog): installing one makes a new app of your own in the space
you choose, as a draft, with no records, photos or history, and nobody's roles but the ones the
template names for you. Nothing is live until you try the draft and publish it, the ordinary way.
Installing the same template twice makes two apps that share nothing.

Where to start: **Apps → Start from a template** (also on the builder's first page). Pick a
template, give it a name, choose where it lives (your **private space** unless you choose a
shared one) and, for a template that makes cards, a board you may write. The draft opens with a
preview on sample data, a board picker if one is still needed, and the publish panel that says
who will be able to use the app and what it can read and change.

The bundled files are in [`internal/apps/templates/`](../internal/apps/templates/): JSON with a
`template_version`, an `id`, its own `version`, a `name`, a `description`, a
`recommended_space`, optional `roles` and the `definition`. A Go test checks every one against the
catalog, so a template that doesn't validate fails the build. Their screens' text is English, like
any app's; the names and descriptions in the picker are translated. The API is in
[docs/api.md](api.md#templates-and-copies).

Each template draws a few **icons**: one beside the list's heading, a plus in the button that
starts a new record and an arrow in each Back button. Like every app's, they are SVG paths in the
definition on a 24 by 24 view box ([docs/api.md](api.md#the-definition), Icons), drawn in the text's colour, and
decoration beside a label, so they need no name of their own. Version 2 of each template added
them; an app installed from version 1 has none, and keeps working as it was.

## The four templates

### Expense tracker (for your private space)

Description, amount, date, a category and up to three **photos of the receipt**. Screens: the
list with this month's total, a form, and a record's detail with the receipt. A **Home widget**
shows the month's total and the all-time total (the `month_of` summary counts the rows dated in
the current calendar month, UTC); the app also offers a navigation pin.

Who sees what: the receipt is read as its record is. In a private space that is you alone. In a
shared space the install gives you the builder, publisher and user roles and every other member
the user role, so **each person reads and changes only their own expenses and photos**: another
member's request for your photo is answered as if it weren't there.

### Equipment checkout (for a shared space)

A **barcode** field for the item's tag (scanned with the camera where the browser can, and always
typed), who took it, the date, its condition, and a status of *out* or *returned* with the date
it came back (required once it is returned). A Home widget counts the items checked out.

Who sees what: the install gives you the builder, publisher and data manager roles, and every
other member the user role. A **user** reads and changes their own records only, and the counts
they see are over those; a **data manager** reads and changes them all. Change who holds which
role afterwards in the app's **Roles** panel. If the installer later leaves the space and nobody
else holds the builder role, the app has nobody to manage its roles: a workspace admin can then
open the **Roles** panel and give someone the builder role (and, if needed, the data manager
role). Once the app has a builder again, the admin has only the roles assigned to them.

### Inspection (for a shared space, with a board)

A site, a result (*pass* or *fail*), notes that are **required when it fails** (`required_when`),
the **place** (a location taken when you submit) and the inspector's **signature**. A failed
inspection runs an **effect** that makes exactly one card, in the *To do* column of the board you
chose at install time, with the site, the notes and the record's number: however often the
submit is repeated, and never for a pass. A **Home widget** counts the open failures and the
inspections done; *Mark as fixed* closes a failure, which lowers the count.

The board must be one you may write, in a space of the same workspace; the server refuses any
other, and checks again when the card is made (the effect runs as the person who submits, and
only if they may write the board then). The roles are the space's own: everyone who may write it
reads and adds inspections.

### Invoicing (for your private space, with a payments account)

Invoices with a number, a customer, an amount, a currency (EUR, USD, GBP or BRL) and a status:
*Draft*, *Sent*, *Paid* or *Refunded*. Screens: the list with the count waiting for payment, a form,
and an invoice's detail. **Send invoice** makes a payment page at the payment provider and puts its
address on the invoice; when the customer pays, the invoice becomes *Paid* with the date, and when
the payment is refunded in full, *Refunded*. A sent invoice offers **Replace an expired link**, which
makes a new page only once the old one has expired at the provider (until then it puts the same
address back). Editing an invoice in the records view never asks for a link: the template has an edit
action without effects, which the records view uses. A **Home widget** counts the invoices waiting
for payment and the paid ones.

It is the one template that reaches outside TaskTo.AI, and it does so without naming anything: its
definition declares a **slot** called `payments` of the kind `payments` ([integrations.md](integrations.md#apps-and-approval)),
two actions with a `create_payment_link` effect on that slot (sending, and replacing an expired link,
both only for a *Sent* invoice), and what a payment may write (the status
and the date, only when the invoice is *Sent*; *Refunded*, only when it is *Paid*). It holds no
provider, no address, no account, no key and no limit, which are the admin's. So:

- **Installing it binds nothing.** The draft opens with the slot unapproved. Until a workspace admin
  has connected a payments account, allowed the space and approved the app's contract, the app
  can't be published; the builder's preview works at once, on sample data, and never calls the
  provider.
- **A copy is the same.** Installing it twice, or duplicating an installed one, gives apps with no
  approval, no records, no payment links and no secrets, each asking for its own approval.
- The roles are the space's own: the template gives nobody an app role, so in a shared space it is
  a good fit for a team that is fine with everyone who can write the space seeing every invoice, and
  otherwise one to install in a private space.
- The currencies it offers must also be on the admin's list for the account: an invoice in another
  currency is refused when it is sent, with a message that names the currency.

The whole setup, from the provider's keys to the first paid invoice, is in
[integrations.md](integrations.md#taking-payments-a-guide). The template's contract hash is pinned in
`internal/apps/templates_invoicing_test.go`, so a change to what it sends or writes is made on purpose.
Such a change reaches new installs, and apps whose definition is updated to the new one (whose next
release then asks for a new approval); an app installed before keeps its own definition and its
approval, and doesn't change by itself.

## Duplicate as template

On the Apps page, **Duplicate as template** copies an app's **definition** into a new app of a
space you may write (your private space by default). It is for anyone who may view the app: those
who may design or publish copy its draft, everyone else its live release. It copies the
definition and nothing else:

- no records, photos or signatures, no history, no deleted records;
- no roles from the source (the copy is unrestricted, so the space's own rules decide, and its
  creator has the roles the creator of any new app has), no Home placements or defaults, no
  releases;
- no effect's queued or finished work;
- no approval of an outside service, payment link or payment event: an app that asks for a
  [payments account](integrations.md#apps-and-approval) asks an admin again.

An effect aimed at a board you can't write, or in another workspace, is **cleared**: the effect stays, and the copy can't be
published until you choose a board (the same picker as for the inspection template). Boards you
can write are kept.

## Trying them end to end

You need a signed-in user, and, for the inspection, a board (Boards → a board of your private or a
shared space). A new user can do all of this with no setup beyond an account:

1. **Apps → Start from a template → Expense tracker → Install as a draft.** Try the preview, add
   an expense, then **Publish**. Open the app, add an expense with a receipt photo, and put the
   **Expenses** widget on your Home page. In a shared space, a second member who adds their own
   expense can't see yours nor fetch your photo.
2. **Equipment checkout** in a shared space with a second member: each of you checks out an item;
   you (a data manager) see both, they see their own; return one from its detail screen.
3. **Inspection**: choose the board while installing, publish, submit a failing inspection with
   notes, a place and a signature, and watch exactly one card appear on the board and the widget's
   *Open failures* go up. Submit one that passes: no card, the count doesn't move. Mark the failure
   as fixed: the count goes down.
4. **Duplicate as template** on any of them, into your private space, and check the copy is empty.
5. **Invoicing** needs a payments account, so it is tried by hand with the provider's test mode, as
   [integrations.md](integrations.md#trying-an-integration-by-hand) says; without one you can install
   it, try the preview and see that publishing asks for an admin's approval.

What needs a **real device**: the camera (receipt photos, and scanning the equipment tag), the
position and the signature pad's touch drawing. In a desktop browser the photo comes from a file,
the position can be typed, the signature typed as a name and the tag typed: the automated tests
(`go test ./internal/apps`, `pnpm --filter @taskto/web test`) cover those paths with uploaded
pictures and stored values, and not the hardware. The position and the camera also need HTTPS
(see [docs/pwa.md](pwa.md)) on anything but `localhost`.

## For agents

The agent has `list_app_templates`, `install_app_template` and `duplicate_app`
([agent-tools.md](agent-tools.md#the-tools)), which do what the routes above do for the
conversation's owner and with exactly their access: a new **draft**, never published, with no
records, roles or secrets of a source, and effect targets the owner can't write cleared. A
template's own role assignments go to the owner as the template defines, and the agent has no
tool to choose or change roles; `show_app_roles` only reads them. Only a person with the publish
capability publishes. It can also read an app's effects, releases and roles to diagnose it
(`list_app_effects`, `list_app_releases`, `show_app_roles`), as far as the owner's own roles let
them. The agent still builds apps of its own with `create_app` and `update_app`, and the bundled
templates' definitions are good examples to ask it for.
