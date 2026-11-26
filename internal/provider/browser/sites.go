package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
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

// siteScript is a node-side script: it opens gotoURL, defines ARGS, and runs body (which has page in scope and
// returns its answer). In-page code defines sitePrelude itself.
func siteScript(gotoURL string, args any, body string) string {
	a, _ := json.Marshal(args)
	if args == nil {
		a = []byte("{}")
	}
	return fmt.Sprintf(`
const ARGS = %s;
await page.goto(%q);
await page.waitForLoadState("domcontentloaded").catch(() => {});
%s
`, a, gotoURL, body)
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

func sessionURL(session provider.SessionRef, fallback string) string {
	if u := strings.TrimSpace(session.Ref["url"]); u != "" {
		return u
	}
	return fallback
}

// ---------------------------------------------------------------------------------------------------------------
// Muse (muse.ai): one main chat. Rows are [data-message-item][data-message-id][data-message-role]; each carries an
// sr-only surrogate with its full text, after who said it. A signed-out visitor gets a landing page with no
// chat log.

type museSite struct{}

const museComposerSel = `textarea[aria-label="消息"], textarea[aria-label="Message" i]`
const museSendSel = `button[aria-label="发送"], button[aria-label="Send" i]`

// museMessagesJS reads the chat's rows into messages (in page, with sitePrelude).
const museMessagesJS = `
const __museMessages = (limit) => {
  const rows = [...document.querySelectorAll('[data-message-item][data-message-id]')];
  const all = [];
  for (const n of rows) {
    const sur = n.querySelector('[data-message-accessibility-surrogate]');
    const role = (n.getAttribute('data-message-role') || '').toLowerCase() === 'user' ? 'user' : 'assistant';
    // The surrogate starts with who said it: "用户消息：" / "助手消息：" while the page loads, "你：" once it is drawn.
    const who = role === 'user' ? /^\s*(用户消息|你|Your message|You)\s*[:：]\s*/i : /^\s*(助手消息|Assistant message)\s*[:：]\s*/i;
    const text = String((sur ? sur.textContent : n.innerText) || "").replace(who, "").trim();
    if (!text) continue;
    all.push({ id: n.getAttribute('data-message-id'), role, text: text.slice(0, 8000) });
  }
  return all.slice(-limit);
};
`

const museSignedInJS = `!!document.querySelector('[role="log"][data-hatch-chat-column]')`

// museReadyJS tells the chat has drawn its rows (an empty chat never does: the wait then times out).
const museReadyJS = `!!document.querySelector('[role="log"][data-hatch-chat-column] [data-message-item]')`

func (museSite) verify(ctx context.Context, a *Adapter, credential string) (provider.RemoteAccount, error) {
	script := siteScript(a.site.AppURL, nil, fmt.Sprintf(`
await page.waitForFunction(() => %s, undefined, { timeout: 15000 }).catch(() => {});
const signedIn = await page.evaluate(() => %s);
// The title becomes "Chat — <agent name>" once the chat is drawn.
if (signedIn) await page.waitForFunction(() => document.title.includes("—"), undefined, { timeout: 3000 }).catch(() => {});
return { signedIn, title: await page.title() };
`, museReadyJS, museSignedInJS))
	var r struct {
		SignedIn bool   `json:"signedIn"`
		Title    string `json:"title"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemoteAccount{}, err
	}
	if !r.SignedIn {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	cred, _ := ParseCredential(credential)
	name := a.site.Label
	// The signed-in title is "Chat — <agent name>".
	if _, agent, ok := strings.Cut(r.Title, "—"); ok && strings.TrimSpace(agent) != "" {
		name = a.site.Label + " · " + strings.TrimSpace(agent)
	}
	return provider.RemoteAccount{ExternalAccountID: a.site.Provider + ":" + cred.MachineID, DisplayName: name}, nil
}

func (museSite) listSessions(_ context.Context, a *Adapter, _ string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{
		ExternalID:     "main",
		Title:          a.site.Label,
		Kind:           protocol.KindMain,
		LastActivityMs: time.Now().UnixMilli(),
		CanSend:        true,
		Ref:            map[string]string{"url": a.site.AppURL},
	}}, nil
}

func (museSite) listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	script := siteScript(sessionURL(session, a.site.AppURL), map[string]any{"limit": clampLimit(opts.Limit, 50)}, fmt.Sprintf(`
await page.waitForFunction(() => %s, undefined, { timeout: 15000 }).catch(() => {});
return await page.evaluate((ARGS) => {
%s
%s
  if (!(%s)) return { error: "auth" };
  return { messages: __fit(__museMessages(ARGS.limit), 40000) };
}, ARGS);
`, museReadyJS, sitePrelude, museMessagesJS, museSignedInJS))
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

// domSendJS is the node-side body of a DOM site's send: it waits for the composer, notes the owner's messages
// already shown, types and sends, then waits for a new owner message with the text. readJS is an in-page
// expression (with sitePrelude and helpers) giving the messages shown; signedInJS one telling a signed-in page,
// readyJS one telling the page has drawn its messages (waited for at most 20s).
func domSendJS(composer, send, helpers, readJS, signedInJS, readyJS string, wait time.Duration) string {
	return fmt.Sprintf(`
const __read = () => page.evaluate(() => {
%s
%s
  if (!(%s)) return { error: "auth" };
  return { messages: %s };
});
await page.waitForFunction(() => %s, undefined, { timeout: 20000 }).catch(() => {});
const before = await __read();
if (before.error) return { error: before.error };
const seen = new Set(before.messages.filter(m => m.role === "user").map(m => m.id));
const sel = %q;
const ready = await page.waitForSelector("loc=css:" + sel, { state: "visible", timeout: 5000 }).then(() => true).catch(() => false);
if (!ready) return { error: "no composer" };
await page.fill("loc=css:" + sel, ARGS.text);
await new Promise(r => setTimeout(r, 400));
const clicked = await page.click("loc=css:" + %q, { timeout: 3000 }).then(() => true).catch(() => false);
if (!clicked) await page.press("loc=css:" + sel, "Enter");
const norm = t => String(t || "").replace(/\s+/g, " ").trim();
const want = norm(ARGS.text);
const deadline = Date.now() + %d;
let hit = null;
while (!hit && Date.now() < deadline) {
  await new Promise(r => setTimeout(r, 1000));
  const now = await __read();
  if (now.error) continue;
  hit = now.messages.filter(m => m.role === "user" && !seen.has(m.id) && norm(m.text) === want).pop() || null;
}
if (!hit) return { sent: false, clicked };
// Let the page settle so an optimistic id has been replaced by the stored one.
await new Promise(r => setTimeout(r, 1500));
const after = await __read();
const settled = (after.messages || []).filter(m => m.role === "user" && !seen.has(m.id) && norm(m.text) === want).pop();
return { sent: true, clicked, id: (settled || hit).id };
`, sitePrelude, helpers, signedInJS, readJS, readyJS, composer, send, wait.Milliseconds())
}

func (museSite) send(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, msg provider.Outgoing) (provider.SendResult, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return provider.SendResult{}, errors.New("empty message")
	}
	script := siteScript(sessionURL(session, a.site.AppURL), map[string]any{"text": text},
		domSendJS(museComposerSel, museSendSel, museMessagesJS, "__museMessages(30)", museSignedInJS, museReadyJS, 20*time.Second))
	var r struct {
		Error string `json:"error"`
		Sent  bool   `json:"sent"`
		ID    string `json:"id"`
	}
	err := a.run(ctx, credential, script, &r)
	return sendOutcome(err, r.Error, r.Sent, r.ID)
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

// duetScript runs body in a claude.ai page (in page, async, with sitePrelude, duetPrelude and ARGS), mapping a
// thrown error to {error}.
func duetScript(gotoURL string, args any, body string) string {
	return siteScript(gotoURL, args, fmt.Sprintf(`
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
	name := a.site.Label
	if n := strings.TrimSpace(r.Name); n != "" {
		name += " · " + n
	}
	// Keyed by the computer, as the generic sites were, so an account paired before keeps its identity.
	cred, _ := ParseCredential(credential)
	return provider.RemoteAccount{ExternalAccountID: a.site.Provider + ":" + cred.MachineID, DisplayName: name}, nil
}

func (duetSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	// The sidebar is drawn after the page loads; wait for it before reading.
	script := siteScript(duetOrigin+"/duet", nil, fmt.Sprintf(`
if (/\/login\b/.test(await page.url())) return { error: "auth" };
await page.waitForFunction(() => !!document.querySelector('a[href^="/duet/chan_"]'), undefined, { timeout: 15000 }).catch(() => {});
return await page.evaluate(async () => {
%s
  try {
    const seen = new Set();
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
    return { channels: chans };
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
});
`, duetPrelude))
	var r struct {
		Error    string `json:"error"`
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
	out := make([]provider.RemoteSession, 0, len(r.Channels))
	for _, c := range r.Channels {
		out = append(out, provider.RemoteSession{
			ExternalID:     c.ID,
			Title:          c.Title,
			Kind:           protocol.KindSide,
			LastActivityMs: parseTimeMs(c.Last),
			CanSend:        true,
			Ref:            map[string]string{"url": duetOrigin + "/duet/" + c.ID},
		})
	}
	// The most recently active channel is the main one.
	best := -1
	for i := range out {
		if best < 0 || out[i].LastActivityMs > out[best].LastActivityMs {
			best = i
		}
	}
	if best >= 0 {
		out[best].Kind = protocol.KindMain
	}
	return out, nil
}

func duetChannel(session provider.SessionRef) string {
	id := strings.TrimSpace(session.ExternalID)
	if strings.HasPrefix(id, "chan_") {
		return id
	}
	return ""
}

func (duetSite) listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	ch := duetChannel(session)
	if ch == "" {
		return provider.RemotePage{}, &provider.Error{Code: provider.CodeNotFound}
	}
	script := duetScript(duetOrigin+"/duet", map[string]any{"chan": ch, "limit": clampLimit(opts.Limit, 50)}, `
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
	ch := duetChannel(session)
	if ch == "" {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeNotFound}
	}
	// Typed into the channel's composer, then read back from the timeline: sent once the owner's message with this
	// text shows up after the send started.
	script := siteScript(duetOrigin+"/duet/"+ch, map[string]any{"chan": ch, "text": text}, fmt.Sprintf(`
if (/\/login\b/.test(await page.url())) return { error: "auth" };
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
`, duetComposerSel, duetSendSel, sitePrelude, duetPrelude))
	var r struct {
		Error string `json:"error"`
		Sent  bool   `json:"sent"`
		ID    string `json:"id"`
	}
	err := a.run(ctx, credential, script, &r)
	return sendOutcome(err, r.Error, r.Sent, r.ID)
}
