# Ensemble (personal-agent-club)

Ensemble (extension id `personal-agent-club`, "club" in its code and protocol) keeps your personal agents in one
place: accounts you pair at agent services, their sessions and messages, an outbox for what you send them, read
state and search. It is one Go program that runs in either of two ways:

- **As a Persona extension.** A Persona hub starts the program with no arguments and talks to it over the extension
  protocol on stdin and stdout. It contributes the agent source `pac`, so its sessions show up on Persona's Agents
  page (session ids `pac:<sessionKey>`).
- **Standalone, with `serve`.** The program serves the same protocol over HTTP on a unix socket (and, with tokens, on
  TCP), for the Lark gateway's agent bridge or any other client.

Both shells call the same core and answer the same protocol.

## Status

**Usable with the API providers.** `serve` keeps each space in SQLite with its credentials encrypted on disk, and
reaches three providers through their APIs: Cue (Manus API v2), Grok Bot (its desktop client's protocol) and Groq
(OpenAI-compatible), and Muse through the owner's own [muse2api](https://github.com/czg86389-hub/muse2api) server
(OpenAI-compatible) once its address is configured. An exported space of the Lark gateway's agent bridge can be imported with its ids, and the
accounts can be listed read-only beside a running serve. The browser-based providers (Muse, Dots, Claude Duet) are not
in this version. The provider adapters are built from documentation and recorded fixtures and still need calibrating
against real traffic (`internal/provider/testdata`). The Persona extension serves the same core from its own data
directory, with an accounts page and the assistant's tools. See the roadmap below.

License: not decided yet. Until a license is added, no rights beyond viewing the source are granted.

## Running

```sh
make build                     # bin/personal-agent-club
bin/personal-agent-club version
```

### The data directory

`serve` and `import` keep everything in one data directory (`-data`, default `~/.personal-agent-club`), created with
mode 0700:

| Path | What |
| --- | --- |
| `key` | the key of the credential files (64 hex digits, 0600; created on first start; `-key-file` puts it elsewhere) |
| `club.lock` | held by the one process that owns the directory: a serve, an import, or the Persona extension |
| `club.sock` | serve's default unix socket |
| `spaces/<space>/club.db` | the space's SQLite store (WAL): the gateway's `agent_accounts`, `agent_sessions`, `agent_messages` and `agent_outbox` tables, plus settings |
| `spaces/<space>/credentials.enc` | the space's credentials, AES-256-GCM under the key, bound to the space's name |

A second owner is refused (`in use by another personal-agent-club process`). A key file that does not open a space's
credentials is refused when the space opens, before serve listens, and nothing is overwritten. Keep the key out of
backups of the directory, or the encryption protects nothing. Quota readings are kept in memory only.

### serve

```sh
bin/personal-agent-club serve                                  # ~/.personal-agent-club, socket ~/.personal-agent-club/club.sock
bin/personal-agent-club serve -data /srv/club -poll 2m
bin/personal-agent-club serve -listen 127.0.0.1:7655 -tokens tokens.json
```

| Flag | |
| --- | --- |
| `-data DIR` | the data directory |
| `-key-file F` | the key file (default `<data>/key`) |
| `-socket PATH` | the unix socket (default `<data>/club.sock`; `none` for TCP only) |
| `-listen ADDR` | a TCP address as well; needs `-tokens` |
| `-tokens F` | the tokens file |
| `-poll D` | how often every account is synced (default `60s`; `0` turns polling off; at least `15s`) |
| `-demo-echo` | add the in-memory echo provider (demos and tests only) |

At start serve opens every space already in the directory: their outboxes recover (sends that were in flight become
`unconfirmed`, sends still pending go out once) and their polling starts. Without polling, accounts are synced when
paired, on `club.accounts.sync` and after sends.

By default `serve` listens only on the unix socket, created with mode 0600; whoever can open the socket is trusted. To
listen on TCP as well, give `-listen` and a tokens file; without tokens, `serve` refuses to start:

```json
{"read": ["<read token>"], "write": ["<write token>"], "admin": ["<admin token>"]}
```

Tokens are at least 32 characters and sent as `Authorization: Bearer <token>`. Levels:

| Level | May call |
| --- | --- |
| `read` | `agentsrc.status`, `sessions`, `messages`, `search`, the event stream |
| `write` | also `agentsrc.send`, `delete_message`, `mark_read` |
| `admin` | also every `club.*` method |

`mark_read` needs `write` because it changes unread state, as with the Lark gateway's read-only agent token.

Try it with curl:

```sh
S=~/.personal-agent-club/club.sock
curl -s --unix-socket $S http://club/v1/spaces/default/rpc -d '{"method":"club.info"}'
curl -s --unix-socket $S http://club/v1/spaces/default/rpc \
  -d '{"method":"club.accounts.add","params":{"provider":"groq","credential":"<your Groq API key>"}}'
curl -s --unix-socket $S http://club/v1/spaces/default/rpc -d '{"method":"agentsrc.sessions"}'
curl -sN --unix-socket $S http://club/v1/spaces/default/events
```

### import

```sh
bin/personal-agent-club import [-data DIR] [-key-file F] [-space default] [-only-provider NAME]... [-dry-run] [-json] BUNDLE.json
```

Imports a space exported from the Lark gateway (format: [docs/import-bundle.md](docs/import-bundle.md)), keeping every
id, skipping the accounts of providers this build cannot reach, never sending anything, and then verifying each
imported account read-only. Stop serve first; `-dry-run` writes nothing (not even the directory or the key) and works
beside a running serve. Exit codes: 0 done, 1 error, 2 usage, 3 imported but an imported account did not verify.

### accounts

```sh
bin/personal-agent-club accounts [-data DIR] [-space default] [-json]
```

Lists a space's accounts (id, provider, name, status, last synced, last error, sessions, messages, assistant control,
quota display) straight from the store, read-only: no lock, nothing created, the credentials never opened. It works
while serve runs.

### Environment

| Variable | |
| --- | --- |
| `PERSONAL_AGENT_CLUB_PROXY` | every provider request goes through this proxy (`socks5://`, `socks5h://`, `http://` or `https://` with host and port), and fails rather than going direct when it is down; unset, `HTTP_PROXY` / `HTTPS_PROXY` / `NO_PROXY` apply |
| `PERSONAL_AGENT_CLUB_GROQ_MODEL` | the model Groq accounts ask (default `openai/gpt-oss-120b`); a pairing is refused when the key cannot use it |
| `PERSONAL_AGENT_CLUB_MUSE2API_URL` | the address of your muse2api server (`http://<host>:18610`, with or without `/v1`); unset, muse2api is listed as unavailable; not an http(s) URL, serve refuses to start |
| `PERSONAL_AGENT_CLUB_MUSE2API_MODEL` | the model muse2api accounts ask (default `muse-spark`; any of the server's aliases works too) |

Provider requests never follow a redirect, so a credential header never goes to another host.

### As a Persona extension

`persona-extension.json` is the manifest: extension id `personal-agent-club`, the agent source `pac` ("Ensemble 的账号和对话"; providers Cue,
Grok Bot, Groq and muse2api), the home (`ui` `home`, a navigation entry, and its Overview card `home-card`), the Accounts
page (`ui` `accounts`) and the connector `club` ("让 Persona 助手使用 Ensemble") with the assistant's `club_agent_*` tools, `activation_events: on_startup`, and network access to the three providers' API hosts and the muse2api address the user writes
(`config:muse2api_url:http`) only. `make package`
builds `dist/personal-agent-club-<version>.tar.gz` (manifest and the four programs), `scripts/persona-test.sh` holds
it to the hub's own checks, and [docs/persona-install.md](docs/persona-install.md) installs it on a hub. The
configuration switch `demo_echo` adds the echo provider, and `muse2api_url` (with `muse2api_model`) adds muse2api. The extension syncs every account every 60 s and keeps its
one space (`default`) in the data directory the hub gives it, laid out as serve's with the key in `club.key`, so
`import` and `accounts` work on it too; its providers go out through the HTTP client that takes the hub's egress
proxy. Without a data directory (run by hand) it keeps everything in memory.

#### Chats in Persona's Inbox

Off by default. The setting `inbox_conversations` ("把 AI 对话放进收件箱") makes each chat of the paired accounts one row in
Persona's Inbox, through a second connector, `ensemble` (it gives the assistant no tools; `club` and its `club_agent_*` tools,
every send confirmed on the phone, are unchanged). Each account's page has its own switch (on unless turned off), separate from
"助手指挥".

- **The row**: thread = the session key; title = account · chat name; preview = the newest reply; unread follows the chat's
  unread count; `last` = the newest message's id, so a repeated sync adds nothing. A chat whose newest message is the owner's own
  is filed as replied. Chats quiet for a day or more when the switch went on come in already read, and only the last 8 days (at most
  100 chats) are listed. A chat that backs the Persona assistant itself is left out.
- **Load**: building the rows reads only what club has already synced. It never calls a provider and never syncs, so browser-backed
  accounts get no extra page loads. The hub is told (`connector.changed`, at most every 5 s) only when a row really changed.
- **Choices**: when the newest reply ends in a list of choices (`A：…` / `B：…`, or `1.` / `2.` after a question, plus up to two
  short closing lines such as "leave it for now"), the row is marked Needs You and its detail page (`ui` `inbox-chat`) draws each
  choice as a button. A tap asks first, then sends that text as the owner's next message through the chat's outbox
  (`internal/extension/options.go`).
- **Sign-in and authorization requests** (`internal/extension/auth.go`): a reply that asks the owner to sign in or approve
  an authorization also marks the row Needs You, even in a chat that cannot be replied to. Recognised: a sentence that
  puts it to the owner ("请", "需要你", "please", "you", an instruction) and tells them to act on a sign-in link or to enter
  a device code, the ask being allowed to stand in the sentence before a bare link; or, in a message of at most 700
  characters, a request in words alone ("请你先登录", "please log in", "you need to sign in"). A sign-in link is a sign-in
  path of a known host, an OAuth `…/authorize` endpoint, or a device page (`/activate`, `/device`). Left alone: the AI
  saying it must or did sign in itself, conditions and advice ("if…", "once…", how-tos), instructions for other people,
  "no need to log in", approvals that are not a sign-in ("please approve the PR"), code blocks, quoted lines and phrases,
  and links that merely have "oauth" or "device" in their path. It prefers missing a request to a false alert, and real
  prompts worded another way will be missed. Nothing in the text is opened or followed.
- **Phone alerts** (`inbox_push`, on; `internal/extension/inbox_push.go`): the Inbox does not push an alert for a
  connector's row by itself (only its count changes), so a reply that waits for the owner (choices, or a sign-in request)
  is also sent to the hub's `POST /api/v1/notify`, at push level P0. One goroutine sends them, a round after each change
  and again a minute later while an alert failed or more than 5 were due. Each reply is alerted once: what was alerted is
  kept in the space's settings and read back after a restart (if an alert goes out and the program dies before that is
  written, the hub replaces the earlier alert with the same id). Never for a row read, answered, or older than 6 hours;
  the rows are read again before every send. What already waits when alerts start (an upgrade, the switch turned on) is
  remembered and not alerted. It uses the token this run's hub gave it and only with the scope `notify`; without it nothing is sent or retried. The alert names
  the account and its service: no chat title and nothing the AI wrote.
- **Replying**: `inbox_reply` (on) lets the owner answer from the Inbox; the Send press is the confirmation, as for every Inbox
  source, and it goes out through the same outbox as any send (one client id per reply, so a retry sends once). Switched off, rows
  are read-only and a reply has to go through the assistant, which asks on the phone each time.
- **Subscription** (advanced): `inbox_via_subscription` turns off the Inbox's own reading and serves the same rows from the
  read-only catalog tool `conversations` of source `ensemble`, for a `things` subscription:

  ```json
  {"mode": "things", "tool": "conversations", "args": {}, "items_path": "$.things", "kind": "sub_message", "group_by": "thread"}
  ```

  Only the Inbox takes a `things` subscription's rows (not the knowledge base).

## Providers

| | Cue | Grok Bot | Groq | muse2api |
| --- | --- | --- | --- | --- |
| Name | `cue` | `grok_bot` | `groq` | `muse2api` |
| Connects with | API key (`x-manus-api-key`) | device sign-in (`club.logins.grok_bot.*`, approved in the user's browser) or a pasted access token | API key (`Authorization: Bearer`) | the server's API key (`MUSE2API_KEY`, `Authorization: Bearer`), checked against its model list; the server's address is configuration |
| History | from the provider: each agent's main task and its subtasks | from the provider: each bot's main session and side sessions | local only: Groq keeps no conversation; club stores it and sends the latest 20 turns (24k characters) with each message | local only, as Groq |
| Send | yes, into a main task; subtasks are read-only | yes; the client id is the message id, so a repeat is a duplicate, not a second message | yes; the answer comes back in the same call | yes; the answer comes back in the same call (up to 110 s: the server drives muse.ai's web chat) |
| Send confirmation (of an `unconfirmed` send) | no: it stays unconfirmed | yes: the send status by message id, asked at the follow-ups | no: it stays unconfirmed | no: it stays unconfirmed |
| Live state (`club.live`) | no (`supported: false`) | yes: running and composing, per session | no (`supported: false`) | no (`supported: false`) |
| Push | no: polling | after a send, the transcript watch (at most 90 s) reads the session as soon as it changes | not needed | not needed |
| Interrupt | no | no | no | no |
| Start a session | no | no | no | no |
| Credential refresh | no | yes, for signed-in credentials (refresh token), once per refused token; a pasted token cannot be refreshed | no | no |
| Quota | a 429 (with `Retry-After`) | a 429 (with `Retry-After`) | every answer's `x-ratelimit-*` headers (requests per day, tokens per minute; the model list's too), and a 429 | a 429 (with `Retry-After`); the server spreads load over its own Muse accounts |
| Hosts | `api.manus.ai` | `api2.cursor.sh` (the sign-in page the user opens is on `cursor.com`) | `api.groq.com` | the configured address only |

`muse`, `dots` and `claude_duet` (browser-based) are not in this version: listed as unavailable, refused when paired
(`unsupported`, `PROVIDER_UNAVAILABLE`), skipped by an import. `club.sessions.start` and `club.sessions.interrupt`
answer `unsupported` for every provider.

## Protocol

Calls are `{method, params}`; results and errors follow Persona's agent source protocol field for field (results in
camelCase, parameters in snake_case), so the extension shell passes them through unchanged. Over HTTP:
`POST /v1/spaces/{space}/rpc` with `{"method", "params"}` → `{"result"}` or `{"error": {"code", "message", "reason"?}}`.

| Method | Params | Result |
| --- | --- | --- |
| `agentsrc.status` | — | `{enabled, setup?, providers, accounts}` |
| `agentsrc.sessions` | — | `{sessions: [...]}` |
| `agentsrc.messages` | `session_key`, `cursor?`, `limit?` | `{session?, messages, hasMore, nextCursor?}` |
| `agentsrc.send` | `session_key`, `client_id` (UUID), `text` | the message as queued, with `local {clientId, status, error?}` |
| `agentsrc.delete_message` | `session_key`, `client_id` | `{}`: cancels a send that has not gone out |
| `agentsrc.mark_read` | `session_key` | `{}` |
| `agentsrc.search` | `q` | `{hits: [...]}` (words, and CJK by n-grams) |
| `club.info` | — | `{version, protocolVersion, providers, browser}` |
| `club.accounts.add` | `provider`, `credential`, `display_name?` | the account (verified with the provider, then synced once) |
| `club.accounts.update` | `account_id`, `paused?`, `display_name?`, `assistant_control?`, `show_quota?` | the account |
| `club.accounts.replace_credential` | `account_id`, `credential` | the account (the same remote account only) |
| `club.accounts.remove` | `account_id`, `delete_data?` | `{}` |
| `club.accounts.sync` | `account_id` | the account after one sync round |
| `club.live` | `session_key` | `{supported, running, composing}` (asked at most every 3 s per session) |
| `club.settings.get` / `set` | — / `browser_identity` | `{browserIdentity}` (kept only, in this version) |
| `club.outbox.dismiss` | `session_key`, `client_id` | `{}`: removes a failed send, releasing its client id |
| `club.logins.grok_bot.start` | `account_id?` (sign that account in again) | `{loginId, loginUrl}` (10 minutes; at most two waiting) |
| `club.logins.grok_bot.poll` | `login_id`, `display_name?` | `{status: pending}` or `{status: done, account}` |
| `club.logins.grok_bot.cancel` | `login_id` | `{}` |
| `club.import` | `path`, `only_providers?`, `dry_run?` | `{accounts, skipped, dryRun?}` ([docs/import-bundle.md](docs/import-bundle.md)) |
| `club.sessions.start`, `club.sessions.interrupt` | | `unsupported` (`PROVIDER_UNAVAILABLE`): no provider in this build can |
| `club.export`, `club.logins.browser.*` | | `unsupported`: not in this version |

Error codes: `not_found`, `invalid`, `conflict`, `unsupported`; other errors carry only a message. Over HTTP they come
with 404, 400, 409, 501 (502 for the rest). `reason`, when present, is the Lark gateway's agent bridge error code
(`AGENT_SESSION_NOT_FOUND`, `ACCOUNT_PAUSED`, `LOGIN_LIMIT`, …) or a provider's (`AUTH_FAILED`, `RATE_LIMITED`, …).

Event `agentsrc.updated {session_keys?}`: those sessions changed; no `session_keys` means the list itself changed. Over
HTTP: `GET /v1/spaces/{space}/events`, server-sent events.

Additions Persona hubs ignore: sessions carry `previewText`, `previewIsMine` and `onTopRankTimeMs`; a message's
`local` keeps `error`; accounts and sessions carry `quota` when something is known.

Rules the core keeps:

- **Spaces** isolate accounts, data and credentials (`/v1/spaces/{space}`); the Persona shell uses `default`.
- **Ids**: account ids are `a` and 12 hex digits; session keys `agent:<provider>:<accountId>:<externalId>`.
- **Outbox**: sends are deduplicated by `client_id`. A send whose outcome is unknown is marked `unconfirmed` and is
  never sent again automatically (a provider that can confirm is asked, read-only); after a restart, sends that were
  in flight become `unconfirmed` too, and sends still pending go out once.
- **Sync** follows the gateway's schedule: at most 20 sessions per round (those that moved, those active in the last
  30 minutes, then the three read least recently); a refused credential is retried every 15 minutes, after one
  refresh where the provider has one.
- **Credentials** go in and never come out: no method returns one, nothing logs one.
- **Quota** combines real provider readings (Groq's `x-ratelimit-*` headers, Grok Bot's `DashboardService/GetSandUsageStatus` and `GetCurrentPeriodUsage`, Claude Duet's `/api/organizations/<id>/usage`, ChatGPT Dots' `/backend-api/wham/usage`, optional Manus/muse2api rate-limit headers, and any 429) with honest local 5h/24h send-count estimates (`account.usage`) for services that do not publish real-time quota (Muse, Gemini, Cue, muse2api). `state` is `ok` (more than 20% left), `low` (20% or less) or `exhausted` (none left, provider refusal flag, or a 429 until `Retry-After` or five minutes); the tightest dimension is on top, all windows in `buckets`; `stale` after 15 minutes without an answer; `show` mirrors the account's `show_quota`. Unknown is no `quota` at all. A change of state or staleness is announced with `agentsrc.updated`. Sends to an account freshly known to be exhausted are refused before hitting the provider (`conflict`, `QUOTA_EXHAUSTED`) with the reset time so callers warn instead of failing silently; manual sync (`club.accounts.sync`) refreshes the reading immediately.
- **Assistant control** per account (`off`, `read`, `confirm`, `auto`; `off` by default, and for every imported
  account): what the Persona assistant may do with that account's agents. With nobody present (scheduled tasks,
  background follow-ups) the assistant may only read, whatever the level.
- **Remote terminal & shell entry** per account: SSH-reachable cloud environments (Muse, Grok Bot, Echo) support saving a non-secret SSH / Tailscale SSH target (no passwords or private keys ever stored), running a fixed read-only status probe (`uname -srm && uptime`), and running commands only after explicit two-step staging + owner confirmation (`20 s` timeout, `16 KB` output cap, ANSI stripping). Grok Bot also reads cloud sandbox state read-only (`SandBoxService/GetSandBoxRunState`) without waking sleeping sandboxes, and Muse/Grok Bot offer a two-step confirmed chat-assisted sandbox runner. Hosted web/API services without a shell clearly label themselves and link to `nwverse/persona#16741` for the Persona core interfaces needed for native/embedded webshell support.

## Layout

| Path | What |
| --- | --- |
| `cmd/personal-agent-club` | the program: extension mode, `serve`, `import`, `accounts`, `version`; the whole-program tests |
| `scripts` | `package.sh` (the Persona package, `make package`) and `persona-test.sh` (the hub's own checks) |
| `internal/protocol` | wire types and method names, pinned by golden JSON |
| `internal/provider` | the provider adapter interface, optional capabilities, the registry; `cue/`, `grokbot/`, `groq/`, `muse2api/` the API providers, `echo/` the demo one, `web/` their shared HTTP client, `providertest/` a fake upstream |
| `internal/providers` | the registry both shells build: the API providers on one HTTP client and its proxy |
| `internal/store` | the store interface, SQLite (with CJK search) and in-memory implementations, one test suite for both |
| `internal/vault` | credentials: the encrypted file and an in-memory one |
| `internal/datadir` | the data directory: lock, key, spaces, read-only access |
| `internal/core` | spaces, accounts, sync, outbox, read state, search, events, quota, import, logins, assistant control |
| `internal/serve` | the HTTP shell and its token levels |
| `internal/extension` | the Persona extension shell: the agent source, the accounts page, the assistant's `club_agent_*` tools, its data directory |
| `internal/conformance` | one protocol test suite, run against both shells |

## Development

```sh
make check    # gofmt, go vet, go test -race
make dist     # the four platform binaries
make package # the Persona extension package under dist/
scripts/persona-test.sh   # the hub's own checks (needs personad)
```

## Roadmap

1. The Lark gateway draws an internal boundary around its built-in agent bridge (there; no behavior change).
2. **This repository**: core and `serve` shell, protocol conformance tests, SQLite storage, the encrypted vault, the API
   providers with their recorded fixtures, quota from answers, assistant control in accounts, import. *(Done; the
   providers still need calibrating against real traffic.)* Interrupt and new sessions wait for a provider that can.
3. The Lark gateway gains a club backend behind an optional setting, off by default.
4. Migration of paired accounts into club (`import`, with verification), then rollback and export.
5. The Persona extension: agent source, accounts page and the assistant's `club_agent_*` tools on disk *(done)*, then
   gateway and launcher quota sources.
6. Browser-based providers, after checking that a browser can run in the extension sandbox.
7. Whether club becomes the Lark gateway's default backend is decided later.
