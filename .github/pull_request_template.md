## What

<!-- What changed, in a few lines. -->

## Why

<!-- The problem it solves. Link the issue it closes, if any ("Closes" and its number). -->

## Rules this PR keeps

<!--
What must hold after this change, one rule per line, each with the test that fails when
its guard is removed (you removed the guard and saw it fail). For example:
- After sign-out, no request carries the old session: `signOutRaces.test.tsx`, "…".
- A space removed while its sync runs leaves nothing in its replacement: `TestASyncPaused…`.
Reviewers check the change against these rules. Write "None: <why>" for a change with no
behaviour to guard (docs, a rename).
-->

## How it was checked

<!-- Tests added or run (make check), and anything checked by hand. -->

## Checklist

- [ ] The title is conventional (`feat:`, `fix:`, `docs:`, `chore:`, …).
- [ ] At most 800 lines of production code changed (CI's "Pull request size" check counts them), or split.
- [ ] New API routes check the caller's access and have a cross-user test.
- [ ] Every string people see is translated (a key in `en.json`).
- [ ] Docs are updated (`docs/api.md` for the API).
- [ ] No secrets, private hostnames or personal names in the change.
- [ ] A new dependency, if any, has its reason here.
