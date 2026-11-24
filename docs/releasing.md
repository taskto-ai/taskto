# Releasing

A release is a GitHub Release named `vMAJOR.MINOR.PATCH` with the three images published for
its tag. Servers that follow the release channel update themselves to the newest one
([Automatic updates](install.md#automatic-updates)), so cutting a release is how a change
reaches them. This page is for the person who cuts it.

## Cut one

In GitHub, **Actions → Release → Run workflow**, on `main`, with the version. The workflow
(`.github/workflows/release.yml`, the workflow's own `GITHUB_TOKEN` only, on a GitHub-hosted
runner):

1. checks the version is `vMAJOR.MINOR.PATCH` and newer than the last release, and that
   its tag doesn't exist;
2. checks CI passed on `main`'s head, and stops if it didn't, or hasn't finished;
3. tags that commit and creates the GitHub Release with generated notes;
4. starts the Images workflow on the tag, which publishes `ghcr.io/<owner>/taskto`,
   `taskto-cli-worker` and `taskto-backup` with the tag (`.github/workflows/images.yml`).

Servers don't take the release before step 4 has finished: a release whose images aren't
there yet doesn't count, and they look again the next night. A tag is never pushed twice, so
a version's images never change; a mistake is fixed with the next version, never by moving a
tag.

The first time, set each of the three packages to public in its settings
([install.md](install.md#prebuilt-images)): a server pulls them without a login.

## Edit the notes

The generated notes list the pull requests. Add what a person running a server should know.
Don't publish a draft or tick **pre-release** for a version you want servers to take: drafts
and pre-releases are never installed automatically (a pre-release is the way to let someone try a
candidate with `taskto-update --to`).

### When a release needs a person: `manual-upgrade: true`

Migrations only go forward, and most releases need nothing from the operator. When one
does (a new required setting in `.env`, a change to the host, a step that mustn't be
automatic), put this line on a line of its own in the release notes, and say what to do:

```
manual-upgrade: true
```

The updater doesn't install that release, nor any later one while this install is older than
it: it records why in its status and the journal, and waits. The operator follows the notes
and then runs `taskto-update --to <version>`, which installs the version they name, past the
marker. Add or remove the line by editing the release; the next run sees it.

## Migrations only go forward

A released database migration is never edited ([AGENTS.md](../AGENTS.md)), and a server
refuses to start on a database a newer version has migrated. So an update can't be undone in
place: the updater's way back is the backup it takes first, and anything written since is
lost. Keep that in mind for a release that changes the schema: it should have run on real
data before it is cut, and a migration that can't be reversed belongs in a version whose
notes say so.

## The main channel

An install can follow `main` instead (`TASKTO_UPDATE_CHANNEL=main`): the newest commit whose
CI passed, that descends from the install's, and whose images (tagged with the commit's full
SHA) are published. It ignores releases and `manual-upgrade`: it is for a server you use to
try what is about to be released.
