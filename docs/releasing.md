# Releasing

A release is a GitHub Release named `vMAJOR.MINOR.PATCH` with the three images published for
its tag. Servers that follow the release channel update themselves to the newest one
([Automatic updates](install.md#automatic-updates)), so cutting a release is how a change
reaches them. This page is for the person who cuts it.

## Cut one

In GitHub, **Actions → Release → Run workflow**, on `main`, with the version, the notes to
put first, and **manual_upgrade** ticked when the release needs a person (below). The
workflow (`.github/workflows/release.yml`, the workflow's own `GITHUB_TOKEN` only, on a
GitHub-hosted runner, the inputs reaching shell commands only as environment variables):

1. checks the version, the whole input and not just its first line, is `vMAJOR.MINOR.PATCH`
   (the one grammar the updater uses: no leading zeros, no suffix, at most 100 characters)
   and newer than the last release, and that its tag doesn't exist;
2. checks CI passed on `main`'s head, and stops if it didn't, or hasn't finished;
3. tags that very commit through the API's create-a-reference call (`POST
   /repos/{owner}/{repo}/git/refs` with `ref=refs/tags/<version>` and the checked `sha`), which
   GitHub answers with an error when the reference exists, whoever made it and at whatever
   commit, even in the moment between the checks and this call: so a tag is never moved, nor
   taken over (a `git push` of a tag that exists at the same commit would succeed, which is
   why the push isn't used). The job has `contents: write` for this and the release, and
   `actions: write` to start the images, and no other permission;
4. creates the GitHub Release for that tag (`--verify-tag`) with the notes, and the line
   `manual-upgrade: true` first when **manual_upgrade** is ticked, so the marker is in the
   release from the moment it is published and no server can see it without; an ordinary
   release has the generated list of pull requests after the notes, grouped by their titles'
   prefixes: `feat:` under **Features**, `fix:` under **Fixes**, everything else under
   **Other** (`.github/scripts/release-notes.sh`, since GitHub's own grouping goes by labels
   only), so a pull request's title is what the release notes say of it (a **manual_upgrade**
   release is published with the notes you gave and the marker, and no generated list);
5. starts the Images workflow on the tag, which publishes `ghcr.io/<owner>/taskto`,
   `taskto-cli-worker` and `taskto-backup` with the tag (`.github/workflows/images.yml`),
   each labelled with the commit it was built from.

Servers don't take the release before step 5 has finished: a release whose images aren't
there yet doesn't count, and they look again the next night. Everything a server installs is
taken from that one commit, and it refuses images that aren't labelled with it. Git's own
reference creation stops a second tag of the same name, and the Images workflow skips an
image the registry already has, so a rerun doesn't push it again. That second check isn't
atomic: two Images runs for one tag that overlap could both push, so don't start Images by
hand for a tag whose run is still going. A mistake is fixed with the next version, never by
moving a tag.

The first time, set each of the three packages to public in its settings
([install.md](install.md#prebuilt-images)): a server pulls them without a login.

## Edit the notes

An ordinary release's notes end with the generated list of pull requests, in Features, Fixes
and Other by their titles. Add what a person running a server should know,
in the **notes** input when you cut it, or by editing the release after. Don't publish a draft
or tick **pre-release** for a version you want servers to take: drafts and pre-releases are
never installed automatically (a pre-release is the way to let someone try a candidate with
`taskto-update --to`).

### When a release needs a person: `manual-upgrade: true`

Migrations only go forward, and most releases need nothing from the operator. When one
does (a new required setting in `.env`, a change to the host, a step that mustn't be
automatic), tick **manual_upgrade** when you cut it and say what to do in the **notes**
(required then): the workflow puts this line first on its own line in the release notes:

```
manual-upgrade: true
```

Don't put the line in the notes yourself: the workflow refuses notes that have one, so the
marker is always the input's. (Editing a release later and adding or removing the line works
too; the next run sees it.)

The updater doesn't install that release, nor any later one while this install is older than
it: it records why in its status and the journal, and waits. It reads every page of the
release list down to the installed version, so a marker is never missed, and if a page can't
be read it installs nothing. The operator follows the notes and then runs `taskto-update --to
<version>`, which installs the version they name, past the marker. It goes past every marker
between the installed version and that one, not only the one the skip message names: when
several releases in between are marked, the operator reviews and completes the steps of each
of them (their notes) before running the command.

A change to `deploy/worker-firewall.sh` needs no marker: the updater sees it itself, skips the
release and tells the operator to update the firewall by hand (runbook step 8) first.

## Migrations only go forward

A released database migration is never edited ([AGENTS.md](../AGENTS.md)), and a server
refuses to start on a database a newer version has migrated. So an update can't be undone in
place: the way back is the backup the updater takes first, restored by a person (the
updater itself never restores data), and anything written since is lost. Keep that in mind for a release that changes the schema: it should have run on real
data before it is cut, and a migration that can't be reversed belongs in a version whose
notes say so.

## The main channel

An install can follow `main` instead (`TASKTO_UPDATE_CHANNEL=main`): the newest commit whose
CI passed, that descends from the install's, and whose images (tagged with the commit's full
SHA) are published. It ignores releases and `manual-upgrade`: it is for a server you use to
try what is about to be released.
