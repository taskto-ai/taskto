# Dictation

Design note: speak instead of type. The web app records what you say, the
server turns it into text, and the text lands in the chat box for you to edit before you
send it. Nothing is sent to the agent until you press Send.

## What it does

- The chat box has a microphone button. Press it to record, press it again to stop (or
  Cancel). A recording is at most **10 minutes**; at 10 minutes it stops by itself.
- The browser records, downmixes to mono and resamples to 16 kHz, and sends the audio as a
  16-bit PCM WAV, base64 in a JSON body (the CSRF rules allow only JSON; uploads to the
  knowledge base work the same way). Ten minutes is 19.2 MB of WAV, about 26 MB of JSON.
- **Memory is bounded.** The transcription route reads at most 28 MiB, and the server
  takes at most 4 transcriptions at once (the 5th gets `429`, before its body is read), so
  dictations hold at most about 300 MB of audio in memory together. Each person has one
  dictation in flight at most. The upload must arrive within 2 minutes, and a
  transcription times out after 5 (`504`).
- The server transcribes it with the workspace's **engine** and answers the text. The
  text is inserted at the cursor in the chat box.
- The knowledge base's **glossary** (see below) improves the result.
- **In apps.** Free-text fields on an app's screen get a microphone beside them: the same recorder and the same route, for the workspace the app is in,
  so the workspace's settings and the person's membership decide. It needs a connection, since the speech
  goes to the server, and says so: offline, or with the engine off, the button is disabled with the reason, and
  the keyboard's own dictation (the microphone key of a phone's keyboard) still works in the field and is named
  in the note. The status is asked once per screen, not once per field.
- Solo and company mode use the same code: the settings are per workspace, the
  transcription runs for whoever is a member.

## Decisions

- **Open-source tools as much as possible, and local first.**
- The default local engine is **sherpa-onnx with Parakeet TDT 0.6B v3** (both open, and
  Parakeet has Portuguese). Cactus with Whistle stays selectable.
- The hosted API key stays **optional**: with no key and no account anywhere, dictation
  works on the server's own model.
- **The local engine stays the default even when an API key is set.** The hosted
  API is used only when an admin picks `api` as the engine; setting or keeping a key never
  switches to it. Before migration 0018 the default was `auto` (the API whenever a key was set);
  migration 0018 turned every `auto` workspace into `local`, so a workspace that wants the
  API picks it again in Settings. Where the operator turned the local model off
  (`dictation_local = off`), the default leaves dictation off, saying so, until an admin
  picks another engine.
- Desert Ant Voz stays an **optional add-on** the operator installs; it is never bundled.

## Engines

The engine is a small Go interface, so adding one is a type and a registry entry:

```go
type Engine interface {
	// Transcribe returns the text of audio (16 kHz mono PCM, at most 10 minutes).
	Transcribe(ctx context.Context, audio Audio, opts Options) (string, error)
}

type Options struct {
	Language string   // ISO 639-1, or "" to let the engine detect it
	Terms    []string // glossary terms, for engines that can bias towards them
}
```

A workspace admin picks one of:

| Setting | Engine | Where the audio goes |
|---|---|---|
| `local` (default) | the operator's local model, run by the server | nowhere: it stays on the server |
| `api` | a hosted speech-to-text API with the workspace's key | to that API |
| `desertant-voz` | the Desert Ant Voz add-on, when the operator has installed it | nowhere: it runs on the server |

### API key (`api`)

Any endpoint that speaks OpenAI's `POST {base_url}/audio/transcriptions` (multipart:
`file`, `model`, optional `language`; reply `{"text": …}`). That covers OpenAI, Groq, and
self-hosted servers such as speaches or a LiteLLM proxy. The workspace stores:

- `base_url` (default `https://api.openai.com/v1`) and `model` (default `whisper-1`);
- the **API key**, sealed with `serverkey` (associated data names the workspace and the
  column), write-only: the API answers `"set"` or `"not set"`, never the key. Same rules as
  provider keys: 8–4096 printable ASCII, `https` unless the host is this machine.

The call goes through the egress policy of workspaces' provider accounts
(`provider_egress_workspace`), checked when the setting is saved and on every connection.

CI tests it against a fake server. **It still needs one check with a real key.**

### Local (`local`)

The first plan was "the Cactus runtime with Parakeet v3 or Whisper". Checked on
2026-11-19:

- **Cactus runtime 4.0.0** (cactus-compute/cactus, Apache-2.0, binaries included) loads
  only `.cact` archives, and its speech path accepts only **Whistle** (Apache-2.0, 17 MB,
  7 languages: en de fr es it nl pl, so **no Portuguese**).
- The **Parakeet v3** and **Whisper** weights under Cactus-Compute on Hugging Face are
  CQ/int8 zip folders for the older `cactus-compute/engine`, which has a non-commercial
  licence. We don't use that engine.

So the local engine is one interface with two pinned runtimes, and the operator picks the
model (`dictation_local`). sherpa-onnx with Parakeet v3 is the default:

| `dictation_local` | Runtime | Model | Languages | Download |
|---|---|---|---|---|
| `parakeet-v3` (default) | sherpa-onnx 2.0.2 (Apache-2.0), `sherpa-onnx-offline` | Parakeet TDT 0.6B v3, int8 (CC-BY-4.0, NVIDIA) | 25 European, incl. pt | ~20–35 MB + 670 MB |
| `whistle` | Cactus runtime 4.0.0 (Apache-2.0), `cactus` | Whistle (Apache-2.0) | en de fr es it nl pl | ~1.5 MB + 17 MB |
| `off` | — | — | — | — |

When Cactus publishes a Parakeet v3 or Whisper `.cact` for its 4.x runtime, it is one more
row in the manifest.

- **Never in the repo or the image.** The manifest in Go pins each file by URL (a fixed
  commit or release) and **sha256**. Files are downloaded on first use into
  `<data>/cache/dictation/`, written to a temporary name, verified, then renamed. A file
  whose hash doesn't match is deleted and the engine reports the error. An operator with
  no internet can put the same files there by hand; they are verified the same way.
- **Platforms:** linux x86-64, linux arm64, macOS arm64. Elsewhere the local engine says
  it isn't available.
- **Loads on demand, unloads when idle.** Each transcription runs the runtime as a child
  process with the model, in its own process group, with a clean environment
  (`CACTUS_TELEMETRY=0`, `DO_NOT_TRACK=1`) and a timeout. The model is in memory for that
  dictation only (Parakeet: ~1.2 GB, ~0.6 s to load on an M-series Mac; Whistle: tiny) and
  freed when it ends, so an idle server holds nothing. sherpa-onnx's websocket server would
  keep the model warm, but it listens on every interface with no address option, so we
  don't run it. Pressing the microphone calls `warm`, which starts the download if needed.
- **One local job at a time** per server; others wait in line (with their request's
  timeout), so two people dictating can't use 2.4 GB at once.
- Long audio is cut into pieces of at most 30 s at the quietest moment near each cut, and
  the pieces are transcribed in one run of the runtime.
- Telemetry: Cactus's own telemetry lives in its Python package; the native runtime
  makes no network call (checked with `strings`), and we set `CACTUS_TELEMETRY=0` anyway.

A real-model test runs only with `TASKTO_DICTATION_IT=1` (it downloads the files); CI uses a
fake runtime. The whole path, browser included, has an opt-in test too: see
[End-to-end check](#end-to-end-check).

### Desert Ant Voz add-on (`desertant-voz`)

Voz (Desert Ant Labs) is source-available, not open source: it is **never bundled** in the
repo or the image, and no Desert Ant code or weights enter either. The operator installs it
and TaskTo only detects it and uses it.

- **Add-ons** are a small registry in Go: an ID, the vendor, what it does (here
  `speech-to-text`), its licence link, the notice to show before turning it on, and the
  attribution to show while it is on. Desert Ant's other models (Redact, Schemer, …) are
  more entries with other kinds.
- **Detection:** an executable at `<addons_dir>/desertant-voz/transcribe`
  (`addons_dir` defaults to `<data>/addons`). The operator writes it with Voz's own SDK
  (Swift on Linux, or Node with `onnxruntime-node`). The contract is TaskTo's, documented
  in [install.md](install.md#dictation): it reads a 16 kHz mono WAV on stdin, takes
  `--language <code>` (optional), and writes `{"text": "…"}` on stdout.
- **Its network traffic is its own.** TaskTo runs it as a child process with a clean
  environment, like the local runtimes, but the licence check is Voz's own connection to
  Desert Ant: the egress policy of provider accounts doesn't cover it (that policy is for
  endpoints people type in). An operator who wants to block it does so with the host's
  firewall; per the licence, Voz keeps running when the check fails.
- **Turning it on** is choosing it as a workspace's engine. Before that, the settings page
  says that its licence check contacts Desert Ant on every model load, with no opt-out,
  and that each server counts as an install, and links to the licence
  (<https://license.desertant.com/>, the Desert Ant Labs Source-Available License 1.1).
  Per the licence, the check sends an install ID, the app identifier, the model and SDK
  versions and the platform, never audio or text. The admin must confirm that notice;
  the server refuses the setting without it.
- **Attribution** (licence §9: credit Desert Ant Labs where users can find it, linked to
  <https://desertant.com>): while it is a workspace's engine, the workspace's About page
  shows "Speech recognition: Voz, powered by Desert Ant Labs", linked.

## The glossary

Each space can have a `glossary.md` at its root. Its terms are the list items' leading
words: `- **TaskTo**: …`, `- Acme — …`, `- Parakeet`. Bold or code markers are dropped, and
so is everything after the first `:`, ` — ` or ` - `.

For a dictation, the server reads the glossary of every space **the caller can read** in
the workspace (through `access.Checker`, never another member's private space), up to 500
terms of up to 60 characters each. Then:

1. Engines that run on the server and can bias towards words get them (Whistle's
   `keywords`).
2. **Every engine's** text is corrected on the server, conservatively, since a wrong
   replacement is worse than a miss:
   - a run of up to four words that equals a term ignoring case, accents, punctuation
     inside words and the spaces between them is replaced by the term ("task to" becomes
     "TaskTo", "sao paulo" becomes "São Paulo");
   - a run of **as many words as the term** that is within a small edit distance of it is
     replaced too, but only for terms of 6 letters or more (distance 1 for 6–9 letters, 2
     for 10 or more), and only when the first letter is the same ("Zansibar" becomes
     "Zanzibar"). Shorter terms must match exactly;
   - words never join across punctuation ("task, to" stays), and a match that would only
     change the case of a term under 4 letters is left alone, so a term like "Go" doesn't
     capitalize every "go".

Glossary terms are **not** sent to a hosted API, and that stays so (the project's choice:
local first). The knowledge base's other features send a space's text out only with the consent of
that space's owner or an editor (search's embeddings); a glossary prompt would need the same.
The server's correction above still applies to a hosted API's text.

The correction is conservative on purpose, and the end-to-end check shows its edge: Parakeet
heard "TaskTo" as "task too", which no rule joins (only an exact join, "task to", becomes
"TaskTo"; joining near matches would also turn "tasks to" into "TaskTo"), while a channel's
name heard one letter off was corrected.

## API

- `GET /api/workspaces/{workspace}/dictation`: any member. The engine in use, the local
  engine's state (`unavailable`, `not_downloaded`, `downloading` with progress, `ready`,
  `failed`), the detected add-ons with their notices, and the credits to show.
- `PATCH /api/workspaces/{workspace}/dictation`: admins. Engine, language, the API's base URL,
  model and key, and the add-on notice confirmation.
- `POST /api/workspaces/{workspace}/dictation/warm`: any member. Starts the local download
  if needed; answers the state.
- `POST /api/workspaces/{workspace}/dictation/transcriptions`: any member.
  `{"audio_base64": "…", "language": "pt"}` → `{"text": "…", "engine": "local"}`. Without a
  language here or in the workspace's settings, the engine gets the user's own language
  ([i18n.md](i18n.md#the-agent-and-dictation)).

Details are in [api.md](api.md#dictation). Every route is under the workspace and wrapped
in `RequireMember` or `RequireAdmin`; tests try each one from another workspace's member.

## Credits

Parakeet TDT 0.6B v3 is CC-BY-4.0: [credits.md](credits.md) and the web app's About page
give the attribution whenever it is the local model.

## End-to-end check

On 2026-11-19 the local path was checked end to end, with no API key and the default
engine, in place of a hand test with a real microphone: the server from `main` run as
`make dev-server` does, with the web app built and a fresh data directory; a real Chromium
(Playwright 1.56, Chromium 141, headless, macOS arm64) whose microphone was a WAV file
(`--use-fake-device-for-media-stream --use-file-for-fake-audio-capture`). For each clip it
signed in, pressed the chat box's microphone, recorded the whole clip, pressed stop, and
checked that the text was in the box and could be edited. The clips were made with macOS
`say` (Samantha, en-US; Luciana, pt-BR). Times are from pressing stop to the text in the
box, on an arm64 laptop.

| Clip | Length | Words wrong (WER) | Text after stop |
|---|---|---|---|
| English, a meeting request | 19 s | 0 of 52 (0%) | 2.0 s |
| Portuguese, the same request | 26 s | 0 of 54 (0%) | 2.0 s |
| Long: the Declaration of Independence and the Gettysburg Address | 9 min 16 s | 26 of 1,436 (1.8%) | 34.9 s |
| Glossary: a request naming a colleague, TaskTo and a team channel | 8 s | see below | 1.3 s |

- **First use.** The first press of the microphone started the model's download (670 MB,
  44 s here). Pressing stop before it finished showed "the local speech model is
  downloading (29%)" with **Try again**, which kept the recording; once the model was
  ready, Try again gave the text without speaking again.
- **Long clip.** The pieces were cut in pauses. The same audio transcribed outside the
  browser had 1.1% wrong; the difference is Parakeet's own: it sometimes skips a word or
  two in a 30 s piece ("a jurisdiction [foreign to] our constitution"), and which words
  flips with tiny changes in the audio. The browser lost nothing: the recorder got every
  buffer (no gap in `playbackTime`, samples equal to the time recorded), with the browser's
  echo cancellation, noise suppression and gain control on or off.
- **Glossary.** Without a `glossary.md` the model misspelt the colleague's uncommon name,
  wrote "task too" for TaskTo, and heard the channel's name one letter off. With a glossary
  listing the three, the channel's name was corrected; "task too" and the name, more than
  one letter off, stayed (see [the glossary](#the-glossary) for why).

To run it again: `make dictation-e2e` (`web/e2e/dictation.e2e.mjs`). It builds the server,
installs Playwright's Chromium, downloads the model (`TASKTO_DICTATION_IT_CACHE` keeps it
between runs) and dictates four clips: Kennedy's "ask not" from sherpa-onnx's test files
(English), a pt-BR clip from `say` where macOS has the Luciana voice, that English clip
repeated to 9 min 30 s, and the English clip again with "YourCountry" in a space's
`glossary.md`. It takes about 11 minutes, most of them recording in real time
(`TASKTO_DICTATION_E2E_LONG=0` skips the long clip). CI never runs it. Its first run
(Playwright 1.64.1, macOS arm64) passed all four: the English and Portuguese text in 1.1 s,
"your country" 234 times of 234 in 9 min 28 s of audio, 34.7 s after stop, and "your
country" joined into "YourCountry" by the glossary.

Still to check by hand: a real microphone in Safari and Firefox (the check
above is Chromium only), and a real hosted API key (optional).

## Open questions

Answered by the project's direction ("open source and local first"), or kept as they were
because they don't conflict with it:

1. **Parakeet on sherpa-onnx instead of on Cactus.** Answered: sherpa-onnx with Parakeet
   v3 stays the default local engine; Cactus with Whistle stays selectable.
2. **Glossary to a hosted API.** Answered: no. Glossary terms stay on the server; the
   server's correction applies to every engine's text.
3. **The glossary's place and format.** Kept: `glossary.md` at a space's root, terms from
   its list items.
4. **Who turns Voz on.** Kept: the operator installs it (nothing is bundled, so nothing
   runs until they do), and a workspace admin chooses it after the notice.
5. **Language.** Kept: a workspace default (empty = detect; Parakeet detects it). A
   per-person choice can come later.
6. **ScriptProcessorNode or AudioWorklet.** Kept: the recorder stays on a
   ScriptProcessorNode. The end-to-end check found no lost audio over 9 minutes, so the
   move (a module asset and a CSP decision) waits until a browser drops it.
