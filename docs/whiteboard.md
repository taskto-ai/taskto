# Whiteboards

People in a space draw together on a whiteboard (sticky notes, rectangles, ellipses, arrows, text,
freehand) and see each other's changes live. A whiteboard is a document of the space, so it has the
space's access, history, export and backups like any other. This page is the server's side; the
canvas in the web app, search over a board's text and cursors come after it.

## The document

A file whose name ends `.whiteboard.json` (any case), in any folder of a space:

```json
{"version":1,"shapes":[{"id":"a1","type":"sticky","x":10,"y":20,"w":120,"h":80,"rotation":0,"text":"Ship it","color":"amber"}]}
```

- `type` is `sticky`, `rect`, `ellipse`, `arrow`, `text` or `draw`; `points` (pairs of numbers) only
  on an `arrow` or a `draw`.
- `color` is one of the label colour roles (`amber`, `blue`, `green`, `pink`, `teal`, `violet`), never
  a literal, so every theme applies.
- Shapes are listed back to front. `id` is 1 to 64 letters, digits, `-` or `_`; coordinates and sizes
  stay within one million in either direction; a size is never negative.
- Limits: 2000 shapes, 64 KiB of text in all, 1 MiB as a file. A file that breaks them, or has a field
  this version doesn't know, is not a whiteboard: `internal/knowledge` refuses to write it
  (`whiteboard_invalid`), and opening a file that became one some other way (a rename) says so.

## Operations

A batch is 1 to 100 operations that are applied in order and either all take effect or none does.

| Operation | Effect |
|---|---|
| `{"op":"add","shape":{…}}` | Appends a shape. An id that exists is left as it is, so a resent batch does nothing twice. |
| `{"op":"update","id":"…","set":{…}}` | Sets `x`, `y`, `w`, `h`, `rotation`, `text`, `color` or `points`; `id` and `type` never change. |
| `{"op":"delete","id":"…"}` | Removes the shape. |
| `{"op":"order","id":"…","to":"front"\|"back"}` | Moves the shape to the end or the start of the list. |

Naming a shape that no longer exists does nothing (someone deleted it first). There is no CRDT:
the server takes batches one at a time in the order they arrive, numbers each with `seq`, and the
last writer wins per property. Two people moving one shape at once is rare on a whiteboard, and
harmless: the later position stays.

## How a board is kept

- **In memory and in `whiteboard_ops`.** The first read or write of a board loads the document,
  validates it, and replays the log of batches not yet saved onto it. Each accepted batch is
  written to the log in one transaction before it is applied in memory and announced, so a crash
  loses nothing that was acknowledged.
- **A snapshot** is committed through `internal/knowledge` (the only code that writes a space's
  repository) two minutes after the first unsaved batch, and on shutdown: one commit, "Whiteboard
  edited", by the last person who edited, with the document's version as its base. The log up to it
  is then deleted and the board leaves memory. A commit the server didn't make while the board
  waited (a restore of an older version, an upload, a rename, a deletion) wins: at the next
  snapshot, or at the next start, the unsaved batches are dropped, never laid over the other
  document, and the board's streams end so their clients open them again and read the board as it
  now is.
- **Access.** Every read checks read access to the space, every batch write access, again when it
  is applied. A stream's access check is the space check plus the board still existing.
- **Not yet:** a snapshot when the last person leaves (only the timer and shutdown), a prompt end of
  streams when the document is replaced while no batch is waiting (they end at the next snapshot or
  load), a canvas, search text, cursors and presence.
