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

1. checks the version is `vMAJOR.MINOR.PATCH` (the one grammar the updater uses: no leading
   zeros, no suffix, at most 100 characters) and newer than the last release, and that its
   tag doesn't exist;
2. checks CI passed on `main`'s head, and stops if it didn't, or hasn't finished;
3. looks the tag up through the API once more, right before the push, and stops if it is
   there (a push of an existing tag at the same commit would succeed, so the push alone
   isn't the check), then tags that very commit, with `git push origin
   <sha>:refs/tags/<version>`, which refuses a tag that exists at another commit, so a tag
   is never moved;
4. creates the GitHub Release for that tag (`--verify-tag`) with the notes, and the line
   `manual-upgrade: true` first when **manual_upgrade** is ticked, so the marker is in the
   release from the moment it is published and no server can see it without; the generated
   list of pull requests follows the notes;
5. starts the Images workflow on the tag, which publishes `ghcr.io/<owner>/taskto`,
   `taskto-cli-worker` and `taskto-backup` with the tag (`.github/workflows/images.yml`),
   each labelled with the commit it was built from.

Servers don't take the release before step 5 has finished: a release whose images aren't
there yet doesn't count, and they look again the next night. Everything a server installs is
taken from that one commit, and it refuses images that aren't labelled with it. A tag is never
pushed twice, so a version's images never change; a mistake is fixed with the next version,
never by moving a tag.

The first time, set each of the three packages to public in its settings
([install.md](install.md#prebuilt-images)): a server pulls them without a login.

## Edit the notes

The generated notes list the pull requests. Add what a person running a server should know,
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
<version>`, which installs the version they name, past the marker.

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
