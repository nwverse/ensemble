# Installing the Persona extension from a local package

This guide installs Ensemble on a Persona hub from a package you build yourself, before it is published
in a market. Everything below runs on the hub's machine, as the user the hub runs as.

## What the package is

`make package` writes `dist/personal-agent-club-<version>.tar.gz` and its `.sha256`. The archive holds one directory,
`personal-agent-club/`, with:

- `persona-extension.json`, and the same manifest as `persona-plugin.json` for hubs from before extensions were
  renamed from plugins;
- `bin/personal-agent-club-<platform>` for `linux-amd64`, `linux-arm64`, `darwin-arm64` and `darwin-amd64`.

There is no source in it. The programs are built with `-trimpath` and without cgo; the packaging script refuses to
pack when a program contains a path of the machine that built it, and the archive's entries carry no owner name or
macOS metadata. The hub needs no Go toolchain or other runtime.

Before installing, check the archive against its hash:

```sh
shasum -a 256 -c personal-agent-club-<version>.tar.gz.sha256   # or: sha256sum -c …
```

To hold the program to the hub's own checks without a hub (manifest and package as at install, handshake, error
codes, every page drawable), run `scripts/persona-test.sh` with a `personad` on `PATH` (or `PERSONAD=/path/to/personad`).

## What 0.7.4 adds

Claude Duet and ChatGPT dots show their own pictures, no longer Claude's and ChatGPT's icons: Claude Duet the icon of
its own app (Claude Duet by Anthropic in the App Store), whole on a transparent margin so the round avatar shows all of
it; ChatGPT dots the dot chatgpt.com's dots page shows, on a gray ground so it reads on light and dark pages alike. Both
are https addresses the apps load without credentials; nothing is added to this repository, and no new permission,
network entry or setting.

## What 0.7.3 adds

Quota & usage across every account:
- Real usage readings are fetched read-only on sync (throttled to every 5 minutes, immediate on manual Sync) for Grok Bot (`DashboardService/GetSandUsageStatus` / `GetCurrentPeriodUsage`), Claude Duet (`claude.ai/api/organizations/<id>/usage`), ChatGPT Dots (`chatgpt.com/backend-api/wham/usage`), alongside Groq's live `x-ratelimit-*` headers and 429 cooldowns.
- Services that do not expose real-time quota numbers (Muse, Gemini, Cue, muse2api) show local 5-hour and 24-hour send-count estimates with their exact data source and estimation method.
- The home page, Overview card, Accounts list and each account's detail page show remaining quota, reset times, per-window progress bars, and low/exhausted badges.
- Sending to an account known to be exhausted stops before hitting the provider (`QUOTA_EXHAUSTED`) with a clear prompt and reset time across Agents, Inbox, Takeover, and assistant tools, instead of failing silently.

No new permissions or configuration fields are added.

## What 0.7.2 adds

Gemini and Claude Duet each have one main chat again, and sending to it works.
Gemini: the main chat is a conversation Ensemble starts in Gemini's 对话 (chat) mode on its first message and continues after;
your other Gemini conversations are its side chats (tasks stay tasks). A send now waits for Gemini's answer before leaving the
page (the new Gemini drops a message whose page is left before the answer is written), and is refused, not lost, while Gemini is
still answering in that conversation.
Claude Duet: the main chat is Duet's own (claude.ai/duet, "Everything"), no longer the most recently active channel; every
channel is a side chat of it. No new permission or setting.

## What 0.7.1 adds

Alerts: a reply that waits for you (choices, or a request to sign in or approve a link) is also pushed to the phone, once.
Replies already waiting at the upgrade are not alerted, only later ones.
This needs the hub scope `notify`, so the hub asks again at upgrade; without it everything else works and no alert is sent.
The setting `inbox_push` (on) turns the alerts off. The manifest also says `engines.persona` `>=202611160608`: the first
stable hub (v0.6.0) with the connector `catalog` capability, `fallback`, `inbox_item` pages and `push_level`, found from the
hub's release index and git history. An older hub will refuse the install, and a development build (number 0) is not checked.

## What 0.7.0 adds

A second connector, `ensemble` (chats as Inbox rows), a page for those rows, and four settings: `inbox_conversations` (off),
`inbox_reply` (on), `inbox_via_subscription` (off) and `triage_rules`. Nothing changes until `inbox_conversations` is turned on.
The hub may ask again about the new connector at upgrade. The connector declares `fallback` (every row carries a tier), so the
hub's "Message triage" model is not required: without one the rows come in as "worth knowing". The chats' text goes to that model
when there is one. See the README, "Chats in Persona's Inbox".

## Back up first

An install or upgrade touches two directories under the hub's state directory (`~/.persona` unless `PERSONA_HOME`
says otherwise):

| Directory | What it holds |
| --- | --- |
| `extensions/installed/personal-agent-club/<version>/` | The unpacked package; an upgrade keeps the current and the previous version |
| `extensions/data/personal-agent-club/` | The extension's own data (below): `club.key`, `club.lock` and `spaces/default/` with `club.db` and `credentials.enc` |

Copy both before an upgrade, keeping modes, for example from the state directory:

```sh
tar -C ~/.persona/extensions -cpzf personal-agent-club-backup.tgz \
  installed/personal-agent-club data/personal-agent-club
```

Treat the copy of the data directory like a
password store, since it holds the key next to what it protects. The hub also keeps the extension's settings and its
record in its database; back that up as you usually back up the hub.

Accounts, sessions, messages, read state and the outbox are kept on disk in the data directory and survive restarts,
upgrades and settings changes; the credentials are kept there encrypted. Quota readings are kept in memory only: after
a restart nothing is shown until a provider's next answer.

## Install

```sh
personad extension install file:/path/to/personal-agent-club-<version>.tar.gz
```

The command first shows what the extension contributes and asks for, then asks to continue (`--yes` skips the
question). It contributes:

- the agent source `pac`, shown as **Ensemble 的账号和对话** ("Ensemble's accounts and chats") among the extension's
  switches (its sessions appear on the Agents page as `pac:<sessionKey>`);
- the home page **我的 AI 助手** ("My AI assistants"): an entry in the navigation (icon `robot`) and a card on
  Overview, **我的 AI 助手 · 概况** (on the phones, above the list in Settings › Extensions) that opens it. The web's sidebar lists only the
  first few extension pages (three) and puts the rest under **更多** ("More"), so with other extensions installed the
  entry may be there; the card always opens it;
- the link **账号** ("Accounts") in **Settings › Extensions › Ensemble**: the same accounts and ways to add one,
  without the home's introduction and state; one line on top says so and that the overview, with what needs you, is
  **我的 AI 助手** in the navigation;
- the connector `club`, shown as **让 Persona 助手使用 Ensemble** ("Let the Persona assistant use Ensemble"), which
  carries the assistant's `club_agent_*` tools;

and asks for network access to `api.manus.ai` (Cue, the agents of a Manus account), `api2.cursor.sh` (Grok Bot) and
`api.groq.com` (Groq), and to the address you write in the setting `muse2api_url` (muse2api; nothing until you write
one). The settings page draws these permissions in the hub's own words: the network entries as the bare host names
(the manifest's `permissions.network` holds host names only, with no label) and `agent_runtimes` (the main
assistant bridge) in the hub's description of that permission (which names the node's `runtime.sock`); the manifest has only
`"agent_runtimes": true`, so none of that text comes from it.

**No hub restart is needed.** The hub starts the extension right after the install (it runs from startup on, as
agent sources must) and the source appears on the Agents page within seconds. `personad extension list` should show
it as `running`. Changing a setting restarts only the extension, not the hub.

To try it without any account, switch on the demo provider, which answers every message with its own text and
reaches nothing:

```sh
personad extension config personal-agent-club demo_echo=true
```

Then add a "演示账号" (demo account) on the home page with any text starting with `echo-`.

To use Muse through your own [muse2api](https://github.com/czg86389-hub/muse2api) server, write its address (http or
https; a server on the hub machine itself is `http://host.persona.internal:<port>`) and, if you like, the model:

```sh
personad extension config personal-agent-club muse2api_url=http://<host>:18610
personad extension config personal-agent-club muse2api_model=muse-spark
```

Then add "Muse 自建转接" (Muse, own relay) on the home page with the server's API key (`MUSE2API_KEY`).

The accounts through a computer's browser (Muse on the web, Claude Duet, Gemini, ChatGPT Dots) are on by default
since 0.6.0 (`browser_providers`; nothing is reached until one is added on a computer). To switch them off:

```sh
personad extension config personal-agent-club browser_providers=false
```

## Home page

The extension's pages are written for someone who has never read this guide; their copy is Chinese (English for an
English-language app) and avoids the program's own terms. The manifest itself (name, description, settings) is
written in Chinese, since the hub shows a manifest's own language and the package carries no translations.

The Overview card (**我的 AI 助手 · 概况**, "My AI assistants at a glance", so that the extension's settings page
does not list two contributions named alike) shows where things stand at a glance, without scrolling: how many accounts are
connected, how many need you (naming the first), how many the Persona assistant may use, and the one next step
(connect the first account, fix the broken one, let the assistant use them, or, when nothing needs you, where the
chats are). With no account it says so and offers to connect the first one. Tapping it opens the home.

The home (**我的 AI 助手**) shows, top to bottom:

- what the extension is for, in two sentences;
- **现在的状态** (where things stand): how many accounts are connected, how many need you, how many the Persona
  assistant may use, and the one next step: connect the first account, fix a broken one (it opens that account), or
  let the assistant use them through **助手指挥**;
- **已接入的账号** (your accounts): each with its service, its state in words and what the assistant may do with it;
- the ways to add an account, under one line (**怎么选**) naming every way the page offers, drawn from the same lists
  as the rows (the self-hosted Muse relay last, as a choice only for those who run one), grouped by how one connects:
  - **在浏览器里登录** (sign in in your browser): **Grok Bot**. Start the sign-in, approve on the page that opens in
    your browser, and the page checks the result every 30 seconds (or at once with *check now*).
  - **借用电脑上已登录的网页** (a website signed in on your computer): **Muse 网页版**, **Claude Duet**,
    **Gemini** and **ChatGPT Dots**, through the browser Persona drives on a computer connected to it (ego lite;
    pairing and every read run in a task space of its own inside that browser, with its sign-ins). The page lists
    your computers and says to sign in to the site in that browser first. With the setting `browser_providers` off, the rows and pages say
    so and where to switch it on.
  - **粘贴密钥** (paste a key): **Cue**, **Groq** and **Muse 自建转接** (your own muse2api server, which needs
    `muse2api_url` first; the row says so until it is written). The key field is a secret field: the apps never show
    it, the extension cannot fill it in, and the extension never shows, returns or logs a saved key.
  - **先试试** (try it first): the demo account, with `demo_echo` on.

An account's page starts with its state (and, when it needs you, what to do, *重新检查* / check again and, for a Grok
Bot account, *重新登录* / sign in again), then:

- the name field (**名字**): rename it (shown only in Persona);
- **助手指挥** (assistant control): *关闭* (off, the default), *只读* (read) or *可代发（逐次确认）* (confirm). An account set to *auto* shows *可代发（自动档）*;
  its sends are still confirmed on the phone every time, since the manifest lists `club_agent_*` in `confirm_always`;
- **主助手** (main assistant): the account itself answering as a Persona main assistant. With
  `assistant_runtime` off the page says to switch that on instead of offering a switch that does nothing;
- syncing (sync now, pause, resume; for a Grok Bot account that works, *重新登录* waits here with a line saying it is
  only for when its sign-in stops working), which allowance the avatar shows (only a sentence while the service has
  reported none), and for Grok Bot and Muse an advanced part that connects the agent's own cloud computer to Persona;
- removing it, keeping or deleting its chats.

Times read the same on every page, in the viewer's time zone: within 7 days relative (*刚刚*, *5 分钟前*, *3 小时前*,
*3 天前*; an allowance that comes back *3 小时后*), older as month, day and time (*11月14日 12:04*), with the year only
when it is not this year's.

Every change says what it changed (*重新检查* reads the account back and says whether it works now), and a refusal says what to do next, in words rather than error codes.

## The assistant's tools

The connector `club` (**让 Persona 助手使用 Ensemble** in Settings › Connectors) gives the assistant `club_agents_list`, `club_agent_read`,
`club_agent_send`, `club_agent_start` and `club_agent_interrupt`:

- An account the assistant may use is chosen on its page under **助手指挥** (assistant control): *关闭* (off, the
  default for every account: the assistant does not see it), *只读* (read: list and read) or *可代发（逐次确认）*
  (confirm: also write).
- Every write is confirmed on your phone, every time, whatever your gate rules say: the manifest lists the writes in
  `confirm_always`. A scheduled task or background follow-up, with nobody there to confirm, can only read.
- Starting a new session and interrupting a turn depend on the provider; where they are not available the tool says
  so before anything is asked.

## Sandbox and network

A package installed from a file runs **in the hub's sandbox** by default: it reads and writes only its own package and
data directory, and reaches the network only through the hub's egress proxy, which lets through only the hosts in
`permissions.network`. The hub hands the proxy to the program in `HTTPS_PROXY` / `HTTP_PROXY`, and the program's HTTP
client takes its proxy from there. Check the counters after the first sync:

```sh
personad extension show personal-agent-club   # "sandbox": {"active": true, "egress": {"allowed": …, "denied": …}}
```

- `denied` growing with `last_denied` naming a host means a provider reached somewhere the manifest does not list:
  report it rather than widening anything by hand.
- Keep the sandbox on. Turning it off (`personad extension sandbox personal-agent-club off`) lets the program read and
  write whatever the hub's user can, and the network list is then only shown, not enforced.
- On Linux the sandbox needs `bwrap` (bubblewrap) and, for memory and CPU limits, user systemd. Without `bwrap` the
  extension still runs, pointed at the proxy but not held to it, and its details say "No sandbox".
- If the hub itself must go through a proxy to reach the internet, set `HTTPS_PROXY` for the hub; the egress proxy
  goes out through it and the extension needs nothing more.
- Pictures need no network entry: an account's avatar is its own picture when the service gives one (a Grok Bot's
  bot avatar), else its product's official icon, else its letters (internal/names/avatar.go). The icons are https
  addresses the products publish (muse.ai, manus.im, groq.com, Google's branding path on www.gstatic.com for Gemini;
  for Grok Bot and Claude Duet the App Store artwork of each one's own app, whole on a transparent margin; for
  ChatGPT dots the dot chatgpt.com's dots page shows, from its image CDN images.ctfassets.net, padded on a gray ground)
  that the Persona app loads itself on the pages, and the hub fetches as a conversation's `avatarUrl` on the Agents
  page; the program never fetches one, and no address carries anything of your accounts. The self-hosted Muse relay
  is not the Muse app and shows its letters.

## Stopping just this extension

```sh
personad extension disable personal-agent-club   # its program stops; the hub and other extensions keep running
personad extension enable personal-agent-club    # started again with the same settings and data
```

`disable` keeps the settings and the data directory; the `pac` source leaves the Agents page until `enable`. Work
on the data directory while the program is stopped (an offline import, a restore) goes between the two. A hub
restart starts the extension again by itself.

## The data directory

The hub gives the program `PERSONA_EXTENSION_DATA`, which is `<state>/extensions/data/personal-agent-club/`, where
`<state>` is the hub's state directory (`~/.persona` of the user the hub runs as, or `PERSONA_HOME`). The program keeps
there:

| Path | What |
| --- | --- |
| `club.key` | the key the credential files are sealed with (AES-256-GCM), 0600; created at the first start and never rewritten |
| `club.lock` | held while the program runs: one process owns the directory at a time |
| `spaces/default/club.db` | the store of the one space this shell serves: SQLite (with its `-wal` and `-shm` files while it runs) |
| `spaces/default/credentials.enc` | that space's credentials, sealed with `club.key` |

This is the data directory layout `personal-agent-club serve` uses, with the key under the name `club.key`, so the
command's tools work on it while the extension is disabled: `personal-agent-club import -data <dir> -key-file
<dir>/club.key BUNDLE.json` imports a space exported from the Lark gateway ([import-bundle.md](import-bundle.md)), and
`personal-agent-club accounts -data <dir>` lists the accounts read-only (that one also works while the extension
runs). An import while the extension runs is refused (the lock), as is a second copy of the program. A key file that
does not open `credentials.enc` stops the program at start, and nothing is overwritten: keep `club.key` with the rest.

To see the path a running program actually has, on Linux:

```sh
pid=$(pgrep -f 'extensions/installed/personal-agent-club/.*/bin/personal-agent-club-' | head -1)
tr '\0' '\n' </proc/$pid/environ | grep '^PERSONA_EXTENSION_DATA='
```

Print only that line: the same environment holds the program's proxy credential.

Files put there by anything else (a restore, an import tool) must be owned by the user the hub runs as and be
readable and writable by that user; `0600` files in `0700` directories are what the extension writes itself. The
sandbox lets the program read and write everything under its data directory and nothing above it, and it does not
change owners or modes: a file left owned by root, or another user, cannot be opened by the program. Run such tools
as the hub's user while the extension is disabled, then enable it.

## Upgrade

```sh
personad extension upgrade personal-agent-club file:/path/to/personal-agent-club-<newer version>.tar.gz
```

The version must be higher than the installed one. The hub stops the old program and starts the new one, keeping the
settings and the data directory; when the new one does not come up within 30 seconds it goes back to the old one by
itself. An upgrade that asks for more (another host, another contribution place) shows what grew and asks again. No
hub restart.

A package with the **same version** as the installed one cannot be upgraded to. To run its programs anyway, remove
the installed one keeping its data, then install again; this drops the extension's settings (set `demo_echo` again
if you used it), not its data directory:

```sh
personad extension uninstall personal-agent-club --keep-data
personad extension install file:/path/to/personal-agent-club-<version>.tar.gz
```

## Roll back or remove

```sh
personad extension rollback personal-agent-club                 # back to the version before the last upgrade (one step)
personad extension disable personal-agent-club                  # stop it; settings and data stay
personad extension uninstall personal-agent-club --keep-data    # remove it but keep the data directory
```

`uninstall` without `--keep-data` deletes the data directory, key file included. To go back to a state you backed up,
uninstall with `--keep-data`, put the saved data directory back in place, and install the matching package again.
