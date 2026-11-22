# Security policy

TaskTo.AI holds people's documents, their agents' logins and their API keys, so we take
reports seriously and thank everyone who sends one.

## Reporting a vulnerability

**Please don't open a public issue, discussion or pull request for a vulnerability.**

Report it privately through GitHub: on the repository's **Security** tab, choose **Report a
vulnerability** (a private security advisory). Only you and the maintainers can see it.

Please include:

- what an attacker can do, and what they need first (an account, a member of a workspace,
  network access, a crafted document or web page…);
- the steps to reproduce it, or a proof of concept, against the version or commit you
  tested;
- anything you know about a fix.

What to expect:

- an acknowledgement within a few working days;
- our assessment, and a plan or a reason if we don't consider it a vulnerability;
- a fix in a release, and an advisory that credits you, unless you'd rather not be named.

Please give us a reasonable time to fix it before you disclose it. We won't take legal
action against research done in good faith that respects people's data and this policy.

## Supported versions

TaskTo.AI is in early development. Fixes go to `main` and to the latest release; older
releases aren't patched. Run a current version.

## Scope

In scope: the server, the web app, the agent runtime, the CLI worker, the container images
and the install files in this repository. Of particular interest:

- one user, workspace or space reaching another's data, conversations, logins or browser;
- an agent, or text an agent reads (a prompt injection), escaping the boundaries in
  [docs/agent-safety.md](docs/agent-safety.md): root, another user's files, a secret in
  its environment, an address the egress policy forbids;
- a sealed secret (an API key, `server.key`) returned, logged or put in an error;
- the backups' append-only guarantee broken from the server's side.

Out of scope: an instance you don't own, denial of service by volume, findings that need a
compromised host or an operator's development switches (`-insecure-cookies`,
`-unsafe-privileged-agents`, `local-unsafe` CLI workers), and issues in third-party
software that are already public (report those upstream; tell us if TaskTo.AI should pin
or update something).
