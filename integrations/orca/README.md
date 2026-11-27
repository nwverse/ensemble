# Ensemble for Orca

An [Orca](https://github.com/stablyai/orca) plugin that puts Ensemble's personal agents (Muse, Gemini
Tasks, Grok Bot, Claude Duet, Dots…) in Orca's right sidebar. It talks to your Persona hub, not to the
agents: whatever the Ensemble extension has paired shows up here.

- **Panel:** accounts with their conversations, unread counts, a conversation view and a reply box.
  Opening a conversation marks it read.
- **Notifications:** when an agent replies, Orca shows a desktop notification. The status bar
  item shows the unread count, and pressing it refreshes.
- **Settings** (Orca › Settings › Plugins › Ensemble): the hub URL, how often to check (30 s by
  default), and whether to notify.

## Connect

1. In Persona, open **Settings › Devices** and create a device credential with the scopes
   `agents.read` and `agents.control`, and nothing else. It can read personal agents, reply to them and
   mark them read. It cannot reach Persona's settings or other data, and you can revoke it there.
2. In Orca, open the Ensemble panel, enter the hub URL (for example `https://persona.example.ts.net`)
   and paste the credential. The plugin checks it against the hub before saving it. The credential is
   kept in Orca's encrypted plugin secrets (the OS keyring), never in settings.

## How it works

The panel has no network access in Orca. Every call goes through the plugin's worker
(`src/worker.ts`), which uses these routes with `Authorization: Bearer <credential>`:

| Route | Use |
|---|---|
| `GET /api/v1/agents` | accounts (display name, provider, last error) |
| `GET /api/v1/agents/sessions` | conversations, unread counts |
| `GET /api/v1/agents/sessions/{id}/messages` | a conversation |
| `POST /api/v1/agents/sessions/{id}/messages` | a reply, `{clientId, text, origin: "orca"}` |
| `POST /api/v1/agents/sessions/{id}/read` | mark read |

The panel makes one UUID `clientId` per message and reuses it on retry, so a send that times out and is
sent again is never delivered twice. The worker's first check after it starts only records the unread
counts, so you are not notified about replies that were already waiting.

## Develop

No build step: Orca loads the `.ts`/`.tsx` files as written.

```sh
ORCA=/Applications/Orca.app/Contents/Resources/bin/orca
$ORCA plugin test . --json      # src/*.test.ts(x): worker against a stub hub, panel against a stub worker
$ORCA plugin check . --json     # renders the scenarios in src/panel.check.ts
$ORCA plugin validate . --json
$ORCA plugin dev watch . --check --shot .orca/shots --json   # needs Orca running; asks for approval first
```
