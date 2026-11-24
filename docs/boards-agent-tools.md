# Board tools for the agent

The contract between the boards (`internal/boards`) and the agent: which board tools the
agent gets, what they do, and when it may use them. The sidecar's tools call these Go
methods through the server's tools socket ([agent-tools.md](agent-tools.md)), which builds
the `Actor` from the conversation's owner. The HTTP routes in [api.md](api.md#boards) are the same
operations for the web app and other clients.

## Who the agent acts for

- **The agent always works for one user**: the one whose conversation it is in. It has
  exactly that user's access, no more: the spaces they may read, and writes only where
  they may write. It never acts for a card's assignee, its author, or an admin.
- **Every tool call names the user.** The Go methods take an `Actor{UserID, Agent: true}`
  built by the server from the conversation's owner, never from anything the model says.
  Each method asks package access about that user and that space first, as
  `RequireSpace` does for a request: a space, board or card the user can't read is
  `ErrNotFound`, the same as one that doesn't exist; a write they can't make is
  `ErrReadOnly` (a viewer, an archived space) or `ErrBoardArchived`.
- **Changes are the user's, marked as the agent's.** Cards, edits and comments record the
  user as their author with `agent: true`, so people see who asked for the change and that
  the agent made it.
- **Assigned to the agent** (`assignee: {"kind": "agent"}`) means "for the agent to do".
  It gives the agent no access: the card is worked when a user who can write it asks the
  agent to, in their conversation, with their access.
- **Assigned to a person** (`assignee: {"kind": "user", "id": …}`) takes the `id` that
  `list_members` returns (`list_members` with the card's `space_id`, for people who can see
  the space). That tool lists only what the user can read, and the board methods check that the
  assignee is a member of the workspace who can see the space.
- **Updates carry versions.** The agent sends the `version` of the card it last read;
  `ErrStale` (409 over HTTP) means someone changed the card meanwhile: show it again,
  then decide. Tools never retry a stale update blindly.

## The tools

All take the space and board IDs; the IDs come from earlier tool results (the user's
spaces from `list_spaces`, then `list_boards`). Errors are the ones above,
plus `ErrInvalid` with a message the model can act on.

| Tool | Go method (`*boards.Service`) | HTTP equivalent | Does |
|---|---|---|---|
| `list_boards` | `ListBoards` / `ListAllBoards(ctx, actor, space)` | `GET …/boards` | The space's boards that aren't archived; with `archived: true`, the archived ones too, last (`archived_at` is set). |
| `list_cards` | `ListCards(ctx, actor, space, board, Filter)` | `GET …/cards` | Cards in board order, or by due date. `Filter`: `Column`, `Assignee` (`user` + `AssigneeID`, `agent`, `none`), `Blocked`, `Text`, `Labels` (all of them), `DueBefore` and `DueAfter` (dates, inclusive), `Overdue` (with `Today`, the user's date; default today in UTC) and `Sort` (`board` or `due`). Each card carries `due` and `labels`. |
| `show_card` | `ShowCard(ctx, actor, space, board, card)` | `GET …/cards/{card}` | The card with its thread. |
| `create_card` | `CreateCard(ctx, actor, space, board, NewCard)` | `POST …/cards` | A new card; `Title` required; column, position, assignee, blocked state, docs, conversations, due date and labels optional. |
| `update_card` | `UpdateCard(ctx, actor, space, board, card, CardChanges)` | `PATCH …/cards/{card}` | Edits title, body, assignee, blocked state, docs, conversations, due date (`null` clears it) and labels (the whole list); needs `Version`. |
| `move_card` | `MoveCard(ctx, actor, space, board, card, Move)` | `POST …/cards/{card}/move` | To a column (not `archived`) at a 0-based position; needs `Version`. |
| `archive_card` | `ArchiveCard(ctx, actor, space, board, card, version)` | `POST …/cards/{card}/archive` | Takes a card out of the columns, remembering its column; needs `Version`. It stays in `list_cards` with `column: "archived"`. |
| `restore_card` | `RestoreCard(ctx, actor, space, board, card, version, column)` | `POST …/cards/{card}/restore` | Brings an archived card back, last in the column it came from or in `column`; needs `Version`. |
| `create_board` | `CreateBoard(ctx, actor, space, name)` | `POST …/boards` | A team board in a shared space (a private space has only its personal board); `name` 1 to 100 characters. |
| `update_board` | `UpdateBoard(ctx, actor, space, board, BoardChanges)` | `PATCH …/boards/{board}` | Renames a board and archives (`archived: true`) or unarchives it; needs `Version`. A personal board can't be archived; an archived board takes no change but being unarchived (`ErrBoardArchived`). |
| `comment_card` | `Comment(ctx, actor, space, board, card, body)` | `POST …/cards/{card}/comments` | Adds to the thread. |
| `set_blocked` | `SetBlocked(ctx, actor, space, board, card, version, reason, blockedBy)` | `PATCH …/cards/{card}` | Sets why the card is blocked and the cards (same board) it waits for; a nil reason and no cards unblock it. |

The write tools above are not read-only: the CLI's permission prompt asks the owner first
(MCP clients see no `readOnlyHint`). They need the right to write the space, as the HTTP
routes do (`ErrReadOnly` for a viewer or an archived space). Nothing is deleted: cards and
boards are archived, never removed, and both can be brought back. A board records the user
who made a change, not whether the agent made it (cards and comments also record that).

The tool schemas the sidecar registers mirror the Go types' JSON (`NewCard`,
`CardChanges`, `Move`, `Filter`), and tool results are the JSON of `Board`, `Card`,
`Thread` and `Comment`, the same shapes as the HTTP replies.

## A conversation that works a card

1. The user asks the agent to work card 12 on the team board.
2. `show_card` reads it and its thread (the user can read it, or the agent gets "not
   found").
3. `move_card` to `doing` with the version just read, then work in the conversation,
   adding the conversation's ID (the conversation API's `id`) to the card's
   `conversations` with `update_card`. Only the user's own conversations can be added.
4. `comment_card` with what was done, `set_blocked` if it is stuck, or `move_card` to
   `review` when done. The user's own rights decide each step; for a viewer, every write
   fails with `ErrReadOnly`.
