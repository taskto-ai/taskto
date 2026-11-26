# App templates

Three starter apps come with the server. A template is only a **definition** (the same JSON as
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

## The three templates

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

## Duplicate as template

On the Apps page, **Duplicate as template** copies an app's **definition** into a new app of a
space you may write (your private space by default). It is for anyone who may view the app: those
who may design or publish copy its draft, everyone else its live release. It copies the
definition and nothing else:

- no records, photos or signatures, no history, no deleted records;
- no roles from the source (the copy is unrestricted, so the space's own rules decide, and its
  creator has the roles the creator of any new app has), no Home placements or defaults, no
  releases;
- no effect's queued or finished work.

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
