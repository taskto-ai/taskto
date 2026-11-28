# Skills

A **skill** is a named, described set of instructions: "how we write a quote", "our support
tone", "closing the month". People teach the agent how their business does something once, and
the agent is meant to use it in every conversation after that. This page is about storing and managing
skills. The routes are in [the API](api.md#skills).

> **Status:** saved skills are stored and managed here, and are not yet used in conversations or in
> the CLI workers. The agent does not apply them in this release.

## What a skill is

| Field | What |
|---|---|
| `name` | a short identifier: a lowercase letter, then 1 to 39 lowercase letters, digits or hyphens (`quote-writer`). Unique among one owner's skills |
| `title` | what people read in lists, 1 to 100 characters on one line |
| `description` | **when to use it**, 1 to 300 characters on one line. It is what the agent sees in its index of skills, so it says when the skill applies, not what it contains |
| `body` | the instructions, Markdown, up to 32 KiB |
| `enabled` | a disabled skill is kept and is not used |
| `origin` | `""` for a skill someone wrote here; set when a skill came from a gallery |

## What a skill is not

A skill is **text only**: no scripts, no files, no tools and no permissions. It cannot widen
what the agent may do. It is advice in the prompt, under the same permission prompts and the same
checks of who may read or write what as anything a person types to the agent. A skill that says
"delete every document" makes the agent ask to delete a document, and the person says no. Where a
skill and a person disagree, the person's own words win, as with any instruction.

A skill's text is user content, like a document: it is never written to the server's log, and
never to the [audit log](audit.md).

## Who owns a skill, who manages it and who uses it

| Scope | Managed by | Used by |
|---|---|---|
| a person's own (`user`) | that person; nobody else sees it, an admin included | that person |
| a shared space's (`space`) | people with write access to the space (not in an archived space) | everyone who can read the space |
| the workspace's (`workspace`) | the workspace's admins | every member |

A person's own skills belong to one workspace, and leave with their membership of it: a person
who is removed and invited again starts with none. A space's skills go with the space. A skill
of a space is tied to the space's generation, the number that tells a space from another one that
later has its ID, so a removed space's skills are never read as a new space's.

A private space has no skills of its own: its owner's own skills are the personal ones.

Every read and write of skills checks again, in its own transaction, that the person is still an
enabled member of the workspace and may read or manage the owner (an admin for the workspace's, a
writer for a space's): access that was revoked after a request was admitted is not used. A person's
own skill is stored only for a member of its workspace, which the schema also keeps.

## What the agent gets

`skills.Store.ForUser(workspace, user)` is the one answer to "which skills may this person use
now?": the enabled skills of the workspace, of every shared space the person can read at that
moment (asked of the access checker, never worked out apart from it), and the person's own.
Someone who is not a member of the workspace, or whose account is disabled, gets none. Conversations and
the CLI workers do not use these skills yet.

## Audit and export

Making, changing and removing a space's or the workspace's skills are audit events
(`skill_created`, `skill_updated`, `skill_removed`: the scope, the space and the skill's name,
never its text), because they change what the agent is told for other people. A person's own
skills are private and change nobody else's access, so they record nothing.

A person's [export](api.md#export-my-data) holds their own skills as Markdown files under
`skills/`, disabled ones too. Skills of a space or of the workspace belong to it, not to a
person, and are not in it.

## Limits

An owner (a person, a space, the workspace) holds at most 200 skills.
