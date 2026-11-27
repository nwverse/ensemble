package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// The sites below replace the generic DOM scripts, whose selectors no longer matched the pages (2026-11): Muse's
// send button selector matched three buttons and its message selector the whole log, Claude Duet's composer is a
// textarea and its rows are div[role=article]. Each send is reported sent only once the owner's message reads back
// from the page or the site's own API; otherwise it is unconfirmed, as Dots does.

// sitePrelude defines helpers inside the page: __norm(text) and __fit(messages, budget), which keeps the newest
// messages within a byte budget (browser.run answers at most 60000 bytes).
const sitePrelude = `
const __norm = t => String(t || "").replace(/\s+/g, " ").trim();
const __fit = (all, budget) => {
  const enc = new TextEncoder();
  const out = [];
  for (let i = all.length - 1; i >= 0; i--) {
    const n = enc.encode(JSON.stringify(all[i])).length;
    if (n > budget) {
      if (out.length === 0) { all[i].text = all[i].text.slice(0, Math.floor(budget / 4)); out.unshift(all[i]); }
      break;
    }
    budget -= n;
    out.unshift(all[i]);
  }
  return out;
};
`

// siteMessage is one message a site script read.
type siteMessage struct {
	ID        string `json:"id"`
	Role      string `json:"role"` // user | assistant
	Text      string `json:"text"`
	CreatedAt string `json:"created_at,omitempty"`
}

// siteMessagesPage converts a site script's messages, oldest first, to a RemotePage.
func siteMessagesPage(msgs []siteMessage) provider.RemotePage {
	out := make([]provider.RemoteMessage, 0, len(msgs))
	for i, m := range msgs {
		if m.ID == "" || strings.TrimSpace(m.Text) == "" {
			continue
		}
		role := protocol.RoleAssistant
		if m.Role == "user" {
			role = protocol.RoleUser
		}
		ms := parseTimeMs(m.CreatedAt)
		pos := int64(i + 1)
		if ms > 0 {
			pos = ms
		}
		out = append(out, provider.RemoteMessage{
			ExternalID:   m.ID,
			Role:         role,
			Blocks:       []protocol.Block{{Type: "text", Text: m.Text}},
			CreateTimeMs: ms,
			Position:     pos,
		})
	}
	return provider.RemotePage{Messages: out}
}

// siteScript is a node-side script: it opens gotoURL (unless the tab is on a loaded page matching stay, openJS),
// defines ARGS, and runs body (which has page in scope and returns its answer). In-page code defines sitePrelude
// itself.
func siteScript(gotoURL, stay string, args any, body string) string {
	a, _ := json.Marshal(args)
	if args == nil {
		a = []byte("{}")
	}
	return fmt.Sprintf(`
const ARGS = %s;
`, a) + openJS(gotoURL, stay) + body + "\n"
}

// sendOutcome turns a send script's answer into a SendResult.
func sendOutcome(err error, errMsg string, sent bool, id string) (provider.SendResult, error) {
	if err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Code == provider.CodeAuthFailed {
			return provider.SendResult{}, err
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: provider.CodeOf(err)}, nil
	}
	if errMsg == "auth" {
		return provider.SendResult{}, dotsError(errMsg)
	}
	if !sent {
		code := provider.CodeUpstreamError
		if errMsg != "" {
			code = provider.CodeOf(dotsError(errMsg))
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: code}, nil
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: id}, nil
}

func clampLimit(limit, max int) int {
	if limit <= 0 || limit > max {
		return max
	}
	return limit
}

// ---------------------------------------------------------------------------------------------------------------
// Claude Duet (claude.ai/duet): one session per channel. The page reads through claude.ai's /v1/code API with its
// own cookies (and an anthropic-version header):
//   - /v1/code/channel-groups: 200 for an account with Duet.
//   - /v1/code/channels/<chan>/timeline?limit=N: {data: [{id, body, created_at, author_account_id |
//     author_agent_id, seq}], next_cursor}, newest first. A body that is JSON ({"kind":"activity"…}, {"card":…}) is
//     a tool activity or card, not a message.
// The channel list comes from the page's sidebar (a[href^="/duet/chan_"]): the channels endpoint answers nothing
// for the owner's own channels.
//
// The main chat is Duet's own, at claude.ai/duet (the sidebar's "Everything", composer "Message Claude Duet"): a
// channel of its own (template "aibo") that the sidebar does not link. The page loading /duet asks for its tasks
// (/v1/code/aibo/channels/<chan>/tasks), which names it; /v1/code/channels/<chan> says its template. It is the
// session "main" (its channel in the session's ref "chan"); every sidebar channel is a side chat of it.

type duetSite struct{}

const duetOrigin = "https://claude.ai"
const duetComposerSel = `textarea[data-cds="ChatComposerEditor"]`
const duetSendSel = `button[aria-label="Send"], button[aria-label="发送"]`

const duetPrelude = `
const __api = async (path) => {
  const r = await fetch(path, { credentials: "include", headers: { "anthropic-version": "2023-06-01" } });
  if (r.status === 401 || r.status === 403) throw new Error("auth");
  if (!r.ok) throw new Error("http " + r.status);
  return r.json();
};
// __duetMain is the main chat's channel, named by the requests a page at /duet made ("" when none did).
const __duetMain = async () => {
  const ids = [];
  for (const e of performance.getEntriesByType("resource")) {
    const m = String(e.name).match(/\/v1\/code\/aibo\/channels\/(chan_[A-Za-z0-9]+)\//);
    if (m && !ids.includes(m[1])) ids.push(m[1]);
  }
  for (const id of ids) {
    const c = await __api("/v1/code/channels/" + encodeURIComponent(id));
    if (c && c.channel && c.channel.template === "aibo") return id;
  }
  return "";
};
const __timeline = async (chan, limit) => {
  const t = await __api("/v1/code/channels/" + encodeURIComponent(chan) + "/timeline?limit=" + limit);
  const all = [];
  for (const m of (t.data || []).slice().reverse()) {
    const body = String(m.body || "").trim();
    if (!body) continue;
    if (body.startsWith("{")) { try { JSON.parse(body); continue; } catch (e) {} }
    all.push({ id: m.id, role: m.author_account_id ? "user" : "assistant", text: body.slice(0, 8000), created_at: m.created_at || "" });
  }
  return all;
};
`

// duetStay is a claude.ai page other than its sign-in, where the API answers from the page (openJS).
const duetStay = `^https://claude\.ai/(?!login)`

// duetScript runs body in a claude.ai page (in page, async, with sitePrelude, duetPrelude and ARGS), mapping a
// thrown error to {error}. It only calls the API from the page, so any signed-in claude.ai page will do.
func duetScript(gotoURL string, args any, body string) string {
	return duetScriptPre(gotoURL, args, "", body)
}

// duetScriptPre is duetScript with pre run node side first (duetMainChanJS).
func duetScriptPre(gotoURL string, args any, pre, body string) string {
	return siteScript(gotoURL, duetStay, args, pre+fmt.Sprintf(`
if (/\/login\b/.test(await page.url())) return { error: "auth" };
return await page.evaluate(async (ARGS) => {
%s
%s
  try {
%s
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
}, ARGS);
`, sitePrelude, duetPrelude, body))
}

func (duetSite) verify(ctx context.Context, a *Adapter, credential string) (provider.RemoteAccount, error) {
	script := duetScript(duetOrigin+"/duet", nil, `
    const r = await fetch("/api/account", { credentials: "include" });
    if (r.status === 401 || r.status === 403) throw new Error("auth");
    const acct = r.ok ? await r.json() : null;
    if (!acct || !acct.uuid) throw new Error("auth");
    await __api("/v1/code/channel-groups");
    return { uuid: acct.uuid, name: acct.display_name || acct.full_name || "" };
`)
	var r struct {
		Error string `json:"error"`
		UUID  string `json:"uuid"`
		Name  string `json:"name"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemoteAccount{}, err
	}
	if err := dotsError(r.Error); err != nil {
		return provider.RemoteAccount{}, err
	}
	if r.UUID == "" {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	// Keyed by the computer, as the generic sites were, so an account paired before keeps its identity.
	cred, _ := ParseCredential(credential)
	return provider.RemoteAccount{ExternalAccountID: a.site.Provider + ":" + cred.MachineID, Alias: strings.TrimSpace(r.Name)}, nil
}

func (duetSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	// The sidebar is drawn after the page loads; wait for it before reading. The list is read from the page, not an
	// API, so the page is loaded fresh for it.
	script := siteScript(duetOrigin+"/duet", "", nil, fmt.Sprintf(`
if (/\/login\b/.test(await page.url())) return { error: "auth" };
await page.waitForFunction(() => !!document.querySelector('a[href^="/duet/chan_"]'), undefined, { timeout: 15000 }).catch(() => {});
await page.waitForFunction(() => performance.getEntriesByType("resource").some(e => /\/v1\/code\/aibo\/channels\/chan_/.test(e.name)), undefined, { timeout: 10000 }).catch(() => {});
return await page.evaluate(async () => {
%s
  try {
    const main = await __duetMain();
    let mainLast = "";
    if (main) {
      try {
        const t = await __api("/v1/code/channels/" + encodeURIComponent(main) + "/timeline?limit=1");
        mainLast = ((t.data || [])[0] || {}).created_at || "";
      } catch (e) {
        if (String(e.message) === "auth") throw e;
      }
    }
    const seen = new Set([main]);
    const chans = [];
    for (const el of document.querySelectorAll('a[href^="/duet/chan_"]')) {
      const m = (el.getAttribute('href') || '').match(/^\/duet\/(chan_[A-Za-z0-9]+)(?:$|[?#])/);
      if (!m || seen.has(m[1])) continue;
      seen.add(m[1]);
      chans.push({ id: m[1], title: (el.innerText || '').trim().split('\n')[0] || m[1] });
      if (chans.length >= 30) break;
    }
    await Promise.all(chans.map(async c => {
      try {
        const t = await __api("/v1/code/channels/" + encodeURIComponent(c.id) + "/timeline?limit=1");
        c.last = ((t.data || [])[0] || {}).created_at || "";
      } catch (e) {
        if (String(e.message) === "auth") throw e;
        c.last = "";
      }
    }));
    return { main, mainLast, channels: chans };
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
});
`, duetPrelude))
	var r struct {
		Error    string `json:"error"`
		Main     string `json:"main"`
		MainLast string `json:"mainLast"`
		Channels []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			Last  string `json:"last"`
		} `json:"channels"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, err
	}
	if err := dotsError(r.Error); err != nil {
		return nil, err
	}
	mainRef := map[string]string{"url": duetOrigin + "/duet"}
	if strings.HasPrefix(r.Main, "chan_") {
		mainRef["chan"] = r.Main
	}
	out := make([]provider.RemoteSession, 0, len(r.Channels)+1)
	out = append(out, provider.RemoteSession{
		ExternalID:     "main",
		Title:          a.site.Label,
		Kind:           protocol.KindMain,
		LastActivityMs: parseTimeMs(r.MainLast),
		CanSend:        true,
		Ref:            mainRef,
	})
	for _, c := range r.Channels {
		if c.ID == r.Main {
			continue
		}
		out = append(out, provider.RemoteSession{
			ExternalID:       c.ID,
			Title:            c.Title,
			Kind:             protocol.KindSide,
			ParentExternalID: "main",
			LastActivityMs:   parseTimeMs(c.Last),
			CanSend:          true,
			Ref:              map[string]string{"url": duetOrigin + "/duet/" + c.ID},
		})
	}
	return out, nil
}

// duetChannel is the channel a session reads and sends to: a sidebar channel's own id, or for the main chat the
// channel its ref names ("" when the ref does not: the script finds it from a page at /duet).
func duetChannel(session provider.SessionRef) (ch string, main bool) {
	id := strings.TrimSpace(session.ExternalID)
	if id == "main" {
		if c := strings.TrimSpace(session.Ref["chan"]); strings.HasPrefix(c, "chan_") {
			return c, true
		}
		return "", true
	}
	if strings.HasPrefix(id, "chan_") {
		return id, false
	}
	return "", false
}

// duetMainChanJS sets ARGS.chan to the main chat's channel when the session did not name it, from a page loaded at
// /duet (node side, before the in-page reads).
const duetMainChanJS = `
if (ARGS.main && !ARGS.chan) {
  if (!/^https:\/\/claude\.ai\/duet\/?(?:[?#]|$)/.test(String(await page.url()))) {
    await page.goto("https://claude.ai/duet");
    await page.waitForLoadState("domcontentloaded").catch(() => {});
  }
  if (/\/login\b/.test(await page.url())) return { error: "auth" };
  await page.waitForFunction(() => performance.getEntriesByType("resource").some(e => /\/v1\/code\/aibo\/channels\/chan_/.test(e.name)), undefined, { timeout: 15000 }).catch(() => {});
  ARGS.chan = await page.evaluate(async () => {
%s
%s
    try { return await __duetMain(); } catch (e) { return ""; }
  }).catch(() => "");
  if (!ARGS.chan) return { error: "no thread" };
}
`

func (duetSite) listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	ch, main := duetChannel(session)
	if ch == "" && !main {
		return provider.RemotePage{}, &provider.Error{Code: provider.CodeNotFound}
	}
	pre := ""
	if ch == "" {
		pre = fmt.Sprintf(duetMainChanJS, sitePrelude, duetPrelude)
	}
	script := duetScriptPre(duetOrigin+"/duet", map[string]any{"chan": ch, "main": main, "limit": clampLimit(opts.Limit, 50)}, pre, `
    return { messages: __fit(await __timeline(ARGS.chan, ARGS.limit), 40000) };
`)
	var r struct {
		Error    string        `json:"error"`
		Messages []siteMessage `json:"messages"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemotePage{}, err
	}
	if err := dotsError(r.Error); err != nil {
		return provider.RemotePage{}, err
	}
	return siteMessagesPage(r.Messages), nil
}

func (duetSite) send(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, msg provider.Outgoing) (provider.SendResult, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return provider.SendResult{}, errors.New("empty message")
	}
	ch, main := duetChannel(session)
	if ch == "" && !main {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeNotFound}
	}
	// Typed into the channel's composer (the main chat's is claude.ai/duet's own), then read back from the timeline:
	// sent once the owner's message with this text shows up after the send started.
	at := duetOrigin + "/duet/" + ch
	if main {
		at = duetOrigin + "/duet"
	}
	script := siteScript(at, "", map[string]any{"chan": ch, "main": main, "text": text}, fmt.Sprintf(`
if (/\/login\b/.test(await page.url())) return { error: "auth" };
%s
const sel = %q;
const ready = await page.waitForSelector("loc=css:" + sel, { state: "visible", timeout: 20000 }).then(() => true).catch(() => false);
if (!ready) return { error: "no composer" };
const startedAt = Date.now();
await page.fill("loc=css:" + sel, ARGS.text);
await new Promise(r => setTimeout(r, 400));
const clicked = await page.click("loc=css:" + %q, { timeout: 3000 }).then(() => true).catch(() => false);
if (!clicked) await page.press("loc=css:" + sel, "Enter");
let found = null;
for (let i = 0; i < 20 && !found; i++) {
  await new Promise(r => setTimeout(r, 1000));
  found = await page.evaluate(async (ARGS) => {
%s
%s
    try {
      const want = __norm(ARGS.text);
      const hit = (await __timeline(ARGS.chan, 10)).filter(m => m.role === "user" && __norm(m.text) === want && Date.parse(m.created_at) >= ARGS.since - 60000).pop();
      return hit ? { id: hit.id } : null;
    } catch (e) {
      return { error: String((e && e.message) || e) };
    }
  }, { ...ARGS, since: startedAt });
  if (found && found.error) break;
}
return found ? (found.error ? { clicked, error: found.error } : { sent: true, clicked, id: found.id }) : { sent: false, clicked };
`, fmt.Sprintf(duetMainChanJS, sitePrelude, duetPrelude), duetComposerSel, duetSendSel, sitePrelude, duetPrelude))
	var r struct {
		Error string `json:"error"`
		Sent  bool   `json:"sent"`
		ID    string `json:"id"`
	}
	err := a.runSend(ctx, credential, script, &r)
	return sendOutcome(err, r.Error, r.Sent, r.ID)
}
