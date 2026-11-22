# Agent runtime: model providers

The agent sidecar (`agent/`) reads its model providers from a JSON file. The Go server writes
the file (see [Provider accounts](#provider-accounts)) and passes its path in
`TASKTO_AGENT_PROVIDERS`. Without the variable, the sidecar runs with only the scripted offline
model, as in its own tests.

The file is read at startup and again on `POST /providers/reload`. An invalid file at startup
stops the sidecar with exit code 2; on a reload it is refused with `400` and the sidecar keeps
the providers it had. Either way the message names the problem field but never quotes a value.

## Schema

```json
{
  "providers": [
    {
      "id": "local",
      "kind": "openai-compatible",
      "name": "Ollama on this machine",
      "baseUrl": "http://127.0.0.1:11434/v1",
      "models": [
        { "id": "llama3.1:8b", "name": "Llama 3.1 8B", "contextWindow": 131072, "maxTokens": 8192 }
      ]
    },
    {
      "id": "gateway",
      "kind": "openai-compatible",
      "baseUrl": "https://llm-gateway.example.com/v1",
      "apiKey": { "file": "/var/lib/taskto/keys/gateway" },
      "compat": { "supportsStore": false },
      "models": [{ "id": "some-model", "reasoning": true }]
    },
    { "id": "script", "kind": "script" }
  ]
}
```

**`providers`** is a non-empty list. The first model of the first provider is the default for new
conversations. Unknown fields anywhere are refused, so a key pasted into the wrong field stops
the start instead of being kept.

Each provider has:

| Field | | |
|---|---|---|
| `id` | required | 1–64 characters of `a-z 0-9 . _ -`, unique. The `provider` of a model choice. |
| `kind` | required | `openai-compatible` or `script`. |
| `name` | optional | Display name; defaults to `id`. |

**`openai-compatible`**: any server that speaks OpenAI's Chat Completions API with streaming:
Ollama, vLLM, SGLang, llama.cpp, LM Studio, LiteLLM and other gateways. It uses pi-ai's
`openai-completions` API with the given base URL.

| Field | | |
|---|---|---|
| `baseUrl` | required | `http` or `https` URL up to the version path, such as `http://127.0.0.1:11434/v1`. No credentials in it. A provider with an `apiKey` needs `https`, unless the host is this machine (`localhost`, `127.x.x.x`, `[::1]`), so a key never crosses the network in cleartext. |
| `apiKey` | optional | Where the key is: `{ "file": "/absolute/path" }` or `{ "env": "VAR_NAME" }`. Never the key itself. Leave it out for a server without auth. |
| `models` | required | Non-empty list of `{ id, name?, contextWindow?, maxTokens?, reasoning? }`. `id` is what the server calls the model. Defaults: `contextWindow` 128000, `maxTokens` 8192, `reasoning` false. |
| `compat` | optional | pi-ai's `OpenAICompletionsCompat` flags for a server's quirks (see pi-ai's README). The sidecar starts from `supportsDeveloperRole: false` and `supportsReasoningEffort: false`, which suit local servers; set them to `true` for a server that supports them. |
| `egress` | optional | Where its requests may connect: a non-empty list of `"public"`, host names, IP addresses and CIDR prefixes, as in the server's egress policies ([install.md](install.md#where-provider-accounts-may-connect)). Left out, anywhere. The server writes each account's from its policy. |

**`script`**: the scripted offline model (`agent/src/model.ts`) with one model, `script-1`. No
keys, no network, the same answer for the same input. For tests and development.

**`cli`**: a CLI worker ([cli-workers.md](cli-workers.md)), the official Claude Code, Codex or
Grok Build CLI on the conversation owner's own login. Only the server writes these, as
`cli-claude`, `cli-codex` and `cli-grok`, after the operator's providers, while CLI workers are
on; an operator's file can't use the `cli-` prefix.

| Field | | |
|---|---|---|
| `cli` | required | `claude`, `codex` or `grok`. |
| `models` | required | Non-empty list of `{ id, name? }`: the CLI's own model names, and `default` for the CLI's own choice. |

No keys, URLs or compat settings: the CLI uses its own login. Its model makes no model call
(`agent/src/cli.ts`): it turns the user's new input into one `cli_turn` tool call and the tool's
result into the answer. The tool runs the turn on the server's tools socket (`POST /cli/turn`,
with the conversation's capability), streams the CLI's work into its output, and returns the
CLI's answer. It is replay-safe: it memoises a turn ID before it calls the server, so after a
sidecar restart it reads the same turn from the worker instead of running the CLI again. An
expired login doesn't fail it: it waits (`POST /cli/wait`) until the owner signs in again, then
goes on.

## Egress

A provider with `egress` sends its requests through a fetch of its own (`agent/src/egress.ts`):
pi-ai's `fetch` option, backed by an undici `Agent` whose connector resolves names with a
`lookup` that drops every address the list doesn't allow, and refuses the connection when none
is left. It runs for every new connection, so a name that answers an allowed address first and
another later (DNS rebinding) is refused the second time, before the socket connects. An IP
address in the URL is never looked up, so it is checked before the request. The fetch follows
no redirect. A refused request fails the run with `the server's egress policy doesn't allow
this endpoint: <host> resolves to <address>, <why>`, instead of the OpenAI client's
`Connection error.`, and the sidecar logs it. The address ranges are the same as the Go
server's (`internal/egress`).

pi-ai is pinned, and its `fetch` option is a documented hook (`ProviderRequestOptions.fetch`),
so neither a fork nor a proxy in front of the sidecar is needed. `undici` is the library Node's
own fetch is built on; it is pinned to the version pi-ai's OpenAI client already brings.

## API keys

- A key lives in a file (read with surrounding whitespace trimmed) or an environment variable of
  the sidecar, never in the config. The Go server should write key files owner-only (mode 600).
- Each key is read at startup and on every reload, so a missing or empty one stops the start
  (exit 2) or refuses the reload, naming the provider and the file or variable, not the key.
  It is read again for every request, so a rewritten key file takes effect without a restart.
- Keys must be at least 8 characters long.
- The sidecar never returns a key and never logs one. Every key it has read is replaced by
  `[redacted]` in every response, every log line and every model error before it is stored, in
  case an upstream server quotes a key back in an error message.
- `GET /models` lists provider IDs, names, kinds and models only: no base URLs or key sources.

## API

```
GET  /models                    → { providers: [{ id, name, kind, models: [{ id, name }] }], default: { provider, modelId } }
POST /providers/reload          → the new GET /models body; 400 { error } and no change for a refused file
POST /conversations             { model?: { provider, modelId } } → { id, model }
GET  /conversations/:id/model   → { provider, modelId }
PUT  /conversations/:id/model   { provider, modelId } → { provider, modelId }
```

A model that isn't in the config is refused with 400 `unknown model`. A switch (`PUT`) uses Pi
Durable's `configure()`: it takes effect from the conversation's next model request, also in the
middle of a run. A conversation whose model was later removed from the config fails its next
run with `no_model` until it is switched to a configured model.

A reload checks the whole file and reads every key before it changes anything, then registers
the new providers and removes the ones that are gone. It doesn't touch conversations: a run in
progress goes on, and each model request resolves its provider afresh, so a changed endpoint or
key applies from the next request and a removed model fails the next run with `no_model`.

The Go client (`internal/agentrt`) has `Models`, `ReloadProviders`, `CreateConversationOn`,
`Model` and `SetModel`; refusals are a `*StatusError` with the HTTP status in `Code`.

## Tests

`internal/agentrt/providers_test.go` runs the real sidecar against a fake OpenAI-compatible
server (Go `httptest`, streaming SSE on loopback), so no test reaches a real provider. Run it with
`make agent-it`. It checks a full streamed answer, the default model, switching between models
and providers, 400 for unknown models, that neither key appears in any response or in the
sidecar's output even when the upstream quotes it, and that bad config files exit 2 without
quoting a value. `internal/agentrt/egress_test.go` checks each kind of `egress` entry, with a
resolver it controls (a module Node preloads in place of `dns.lookup`): refusals by address
and by name, a key never reaching a refused endpoint, and DNS rebinding.

`internal/providers/feed_it_test.go` checks reloads against the real sidecar: accounts appear and
disappear, a run in progress finishes across reloads, a refused file changes nothing, and every
compat setting the API accepts passes the sidecar. `cmd/taskto/providers_it_test.go` adds an
account through the server's API and checks the running sidecar lists it and answers through it
(same process, no restart), that a new key applies from the next request, and that no key shows
up in the server's output.

## Provider accounts

Users and workspaces add their own providers through the API ([api.md](api.md#model-providers)),
stored in the server's database with their keys encrypted. With the agent runtime on, the
server (`internal/providers`, `Feed`) turns them into the sidecar's file, in the sidecar's
directory `<data>/agent/`:

- `providers.json` (mode 600): the operator's providers first, then every account, workspaces'
  before users'. Without an operator file, the scripted offline model comes first instead, so
  the sidecar's default model is never someone's account. Each account has the `egress` of
  the server's egress policy for its scope (none for `any`).
- `keys/<provider>.key` (mode 600, in a mode 700 directory): one file per account with a key,
  the only place it is ever decrypted. A key file that's no longer used (key removed, account
  deleted) is deleted.

Every file is replaced in one rename. The server writes them before it starts the sidecar, and
again after every change through the API, followed by a reload. If the sidecar doesn't answer
(it is starting or restarting), the server retries the reload in the background; a sidecar that
restarts reads the current file anyway. If the sidecar refuses the file, the server logs it and
the sidecar keeps its previous providers. An account whose key can't be decrypted (a replaced
`server.key`) is left out, with a log line naming the account.

**Why a reload, not a restart.** A restart would be safe, since Pi Durable conversations are
durable, but it interrupts every run in progress for everyone, and makes every account change
cost a sidecar start. The reload swaps pi-ai's providers in place and leaves the conversations
alone.

### Provider IDs and who may use them

An account's provider ID says who owns it: `ws<workspace>-<name>` for a workspace's,
`u<user>-<name>` for a user's. The sidecar doesn't know about users and checks nothing, so the
server must, whenever it passes a model choice to the sidecar (creating a conversation or
switching its model):

- a `ws<N>-…` provider only for a conversation in workspace N;
- a `u<N>-…` provider only for a conversation of user N;
- any other ID (the operator's providers) for anyone.

`providers.Usable(provider, user, workspace)` is that rule. The conversation API
([api.md](api.md#conversations)) calls it for every model choice, and answers a refused one
like a model that doesn't exist.

### With `-agent-providers`

The operator's file (`-agent-providers`) and the accounts combine: the operator's providers
come first, exactly as written, then the accounts. So an operator can offer a gateway to
everyone (its key in a file or an environment variable of the server, which the sidecar
inherits), and its first model is the default. The server reads the operator's file once, at
startup: restart the server to change it. It refuses to start if the file isn't JSON, has no
providers, or uses an ID shaped like an account's (`ws<N>-…`, `u<N>-…`); the sidecar checks the
rest at its start. The model list (`GET /api/workspaces/{workspace}/models`) shows these
providers with scope `instance`.

The operator's file supports all of pi-ai's compat settings. Through the API, accounts get the
flat ones (see [api.md](api.md#model-providers)); the server checks them itself, and an
integration test checks the sidecar accepts every one, to catch a pi-ai upgrade that changes
them.

## Not here yet

pi-ai's other provider kinds (Anthropic, Google, subscription logins), and the model picker in
the web app.
