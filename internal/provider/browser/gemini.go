package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// geminiSite drives Gemini's tasks (gemini.google.com, the composer's "任务" / "Tasks" mode: Gemini works on a task
// with tools, and the conversation carries the task's status). It reads through the batchexecute RPCs the signed-in
// page itself calls, with the page's own token (WIZ_global_data.SNlM0e, never leaving the browser), and sends through
// the task conversation's composer, then reads the turn back to confirm it arrived.
//
// What the page answers (2026-11):
//   - MaZiqc [n, null, [null,null,1,null,null,null,null,null,null,1,null,[statuses]]]: the sidebar's conversations
//     with those task statuses, [null, token, [[ "c_<id>", title, …, [sec, nanos] (5), …]]]. The sidebar's filter
//     asks [2] for 进行中 (running), [3,5] for 需要注意 (waiting for the user), [4,1] for 准备就绪; tasks are in
//     2 to 5, plain chats in 1.
//   - hNvQHb ["c_<id>", turns, null, 1, [0], [4], null, 1]: the conversation's turns, newest first,
//     [[ ["c_<id>", "r_<id>"], null, [[user text], …], [[ ["rc_<id>", [answer markdown], …] ]], [sec, nanos] (4) ]].
type geminiSite struct{}

const geminiOrigin = "https://gemini.google.com"

// Task statuses, as the sidebar's filter asks for them.
const (
	geminiRunning = "running"
	geminiWaiting = "waiting"
	geminiReady   = "ready"
)

// geminiMaxTurns is the most turns one read asks for.
const geminiMaxTurns = 20

// geminiPrelude defines __rpc(id, args) inside the page: one batchexecute call with the page's token, answering the
// RPC's decoded payload. It throws "auth" when the page is not signed in and "http <status>" on a failed request.
const geminiPrelude = `
const __wiz = () => {
  // Google's "unusual traffic" check (www.google.com/sorry) is a rate limit, not a signed-out page.
  if (location.pathname.startsWith("/sorry")) throw new Error("http 429");
  const w = window.WIZ_global_data || {};
  if (location.hostname !== "gemini.google.com" || !w.SNlM0e) throw new Error("auth");
  return w;
};
const __rpc = async (id, args) => {
  const w = __wiz();
  const qs = new URLSearchParams({ rpcids: id, "source-path": location.pathname || "/app", bl: w.cfb2h || "", "f.sid": w.FdrFJe || "",
    hl: document.documentElement.lang || "en", _reqid: String(100000 + Math.floor(Math.random() * 800000)), rt: "c" });
  const body = new URLSearchParams({ "f.req": JSON.stringify([[[id, JSON.stringify(args), null, "generic"]]]), at: w.SNlM0e });
  const r = await fetch("/_/BardChatUi/data/batchexecute?" + qs, { method: "POST", body, credentials: "include",
    headers: { "content-type": "application/x-www-form-urlencoded;charset=UTF-8" } });
  if (r.status === 401 || r.status === 403) throw new Error("auth");
  if (!r.ok) throw new Error("http " + r.status);
  const text = await r.text();
  for (const line of text.split("\n")) {
    if (!line.includes('"wrb.fr"')) continue;
    const frame = JSON.parse(line).find(f => f[0] === "wrb.fr" && f[1] === id);
    if (!frame) continue;
    return frame[2] ? JSON.parse(frame[2]) : null;
  }
  throw new Error("http 500");
};
const __ms = t => Array.isArray(t) ? t[0] * 1000 + Math.floor((t[1] || 0) / 1e6) : 0;
const __tasks = async (statuses, n) => {
  const v = await __rpc("MaZiqc", [n || 30, null, [null, null, 1, null, null, null, null, null, null, 1, null, statuses]]);
  return ((v && v[2]) || []).filter(c => typeof c[0] === "string").map(c => ({ id: c[0].replace(/^c_/, ""), title: c[1] || "", at: __ms(c[5]) }));
};
const __allTasks = async n => {
  const out = [];
  for (const [status, codes] of [["running", [2]], ["waiting", [3, 5]], ["ready", [4]]]) {
    for (const t of await __tasks(codes, n)) out.push({ ...t, status });
  }
  return out;
};
`

// geminiScript wraps body (an async function body of (ARGS) run inside the page) so its answer, or its failure, is
// the script's value. The RPCs answer on any of Gemini's pages, so a page already on gemini.google.com is not moved:
// a sync then never navigates away under a send waiting in the same tab for its task to show.
func geminiScript(gotoURL string, args any, body string) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`
if (!String(await page.url()).startsWith("https://gemini.google.com/")) {
  await page.goto(%q);
  await page.waitForLoadState("domcontentloaded").catch(() => {});
}
const __out = await page.evaluate(async (ARGS) => {
%s
  try {
%s
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
}, %s);
return __out;
`, gotoURL, geminiPrelude, body, a)
}

// geminiError maps a script's error string to the provider's error, as dotsError does.
func geminiError(msg string) error {
	if msg == "no task" {
		return &provider.Error{Code: provider.CodeNotFound}
	}
	return dotsError(msg)
}

func (geminiSite) verify(ctx context.Context, a *Adapter, credential string) (provider.RemoteAccount, error) {
	var r struct {
		Error string `json:"error"`
		Email string `json:"email"`
		Name  string `json:"name"`
		Tasks int    `json:"tasks"`
	}
	script := geminiScript(geminiOrigin+"/app", nil, `
    const w = __wiz();
    // "Google 账号： Name \n(email), Google One 会员" / "Google Account: Name (email)" on the account button.
    const label = [...document.querySelectorAll('a[href*="SignOutOptions"], [aria-label*="@"]')].map(e => e.getAttribute("aria-label") || "").find(l => l.includes("@")) || "";
    const m = label.match(/[:：]\s*([^()]*?)\s*\(([^()\s]+@[^()\s]+)\)/);
    const email = (m && m[2]) || w.oPEP7c || "";
    if (!email) throw new Error("auth");
    const tasks = (await __allTasks(30)).length;
    return { email, name: (m && m[1]) || "", tasks };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemoteAccount{}, err
	}
	if err := geminiError(r.Error); err != nil {
		return provider.RemoteAccount{}, err
	}
	email := strings.ToLower(strings.TrimSpace(r.Email))
	if email == "" {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = email
	}
	// No task yet is fine: the account is paired and its tasks show up as they are made.
	return provider.RemoteAccount{ExternalAccountID: "gemini:" + email, DisplayName: a.site.Label + " · " + name}, nil
}

type geminiTask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	At     int64  `json:"at"`
	Status string `json:"status"`
}

func (geminiSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	var r struct {
		Error string       `json:"error"`
		Tasks []geminiTask `json:"tasks"`
	}
	script := geminiScript(geminiOrigin+"/app", nil, `
    return { tasks: await __allTasks(30) };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, err
	}
	if err := geminiError(r.Error); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]provider.RemoteSession, 0, len(r.Tasks))
	for _, t := range r.Tasks {
		id := strings.TrimSpace(t.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		title := strings.TrimSpace(t.Title)
		if title == "" {
			title = "Gemini 任务"
		}
		out = append(out, provider.RemoteSession{
			ExternalID:     id,
			Title:          title,
			Kind:           protocol.KindTask,
			LastActivityMs: t.At,
			CanSend:        true,
			Ref:            map[string]string{"url": geminiOrigin + "/app/" + id, "status": t.Status},
		})
	}
	return out, nil
}

type geminiMessage struct {
	ID    string `json:"id"`
	At    int64  `json:"at"`
	Owner bool   `json:"owner"`
	Text  string `json:"text"`
}

// geminiTurnsJS reads ARGS.task's newest ARGS.turns turns into messages (oldest first, within a byte budget, as
// dotsMessagesJS does); a running task's newest answer is left out until it is done, so a half-written answer is
// never kept.
const geminiTurnsJS = `
    const v = await __rpc("hNvQHb", ["c_" + ARGS.task, ARGS.turns, null, 1, [0], [4], null, 1]);
    const turns = ((v && v[0]) || []).filter(t => Array.isArray(t) && Array.isArray(t[0]));
    if (!v || (!turns.length && !ARGS.allowEmpty)) throw new Error("no task");
    const running = ARGS.checkRunning ? (await __tasks([2], 30)).some(t => t.id === ARGS.task) : false;
    const all = [];
    turns.slice().reverse().forEach((t, i, arr) => {
      const at = __ms(t[4]);
      const turnId = String(t[0][1] || "");
      const user = t[2] && t[2][0] && t[2][0][0];
      if (turnId && typeof user === "string") all.push({ id: turnId + ":u", at, owner: true, text: user });
      const cand = t[3] && t[3][0] && t[3][0][0];
      const answer = cand && cand[1] && cand[1][0];
      const newest = i === arr.length - 1;
      if (cand && typeof answer === "string" && !(newest && running)) all.push({ id: String(cand[0] || turnId + ":a"), at: at + 1, owner: false, text: answer });
    });
    const enc = new TextEncoder();
    let budget = ARGS.budget || 40000;
    const messages = [];
    for (let i = all.length - 1; i >= 0; i--) {
      const n = enc.encode(JSON.stringify(all[i])).length;
      if (n > budget) {
        if (messages.length === 0) { all[i].text = all[i].text.slice(0, Math.floor(budget / 4)); messages.unshift(all[i]); }
        break;
      }
      budget -= n;
      messages.unshift(all[i]);
    }
`

func geminiTarget(session provider.SessionRef) string {
	return strings.TrimPrefix(strings.TrimSpace(session.ExternalID), "c_")
}

func (geminiSite) listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	id := geminiTarget(session)
	if id == "" || id == "main" {
		return provider.RemotePage{}, &provider.Error{Code: provider.CodeNotFound}
	}
	turns := (opts.Limit + 1) / 2
	if turns <= 0 || turns > geminiMaxTurns {
		turns = geminiMaxTurns
	}
	var r struct {
		Error    string          `json:"error"`
		Messages []geminiMessage `json:"messages"`
	}
	script := geminiScript(geminiOrigin+"/app", map[string]any{"task": id, "turns": turns, "checkRunning": true},
		geminiTurnsJS+`
    return { messages };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemotePage{}, err
	}
	if err := geminiError(r.Error); err != nil {
		return provider.RemotePage{}, err
	}
	out := make([]provider.RemoteMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		if m.ID == "" || strings.TrimSpace(m.Text) == "" {
			continue
		}
		role := protocol.RoleAssistant
		if m.Owner {
			role = protocol.RoleUser
		}
		out = append(out, provider.RemoteMessage{
			ExternalID:   m.ID,
			Role:         role,
			Blocks:       []protocol.Block{{Type: "text", Text: m.Text}},
			CreateTimeMs: m.At,
			Position:     m.At,
		})
	}
	return provider.RemotePage{Messages: out}, nil
}

const geminiComposerSel = `rich-textarea div.ql-editor[contenteditable="true"]`
const geminiSendSel = `button[aria-label="发送"]:not([disabled]), button[aria-label="Send message" i]:not([disabled]), button.send-button:not([disabled])`

func (geminiSite) send(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, msg provider.Outgoing) (provider.SendResult, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return provider.SendResult{}, errors.New("empty message")
	}
	id := geminiTarget(session)
	if id == "" || id == "main" {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeNotFound}
	}
	args, _ := json.Marshal(map[string]any{"text": text})
	// Typed into the task's own conversation once it is on the page (typed earlier, the text stays a draft that is
	// never sent), into a composer cleared of any draft Gemini kept for the conversation, then sent: sent once the
	// page shows a new turn of the user's with this text. Each look is its own short evaluate, and the whole send
	// stays well inside the driver's limit. Still in the composer after the wait, it did not go: the draft is cleared
	// and the send refused, so it can be sent again.
	script := fmt.Sprintf(`
const ARGS0 = %s;
await page.goto(%q);
await page.waitForLoadState("domcontentloaded").catch(() => {});
const signedIn = await page.evaluate(() => location.pathname.startsWith("/sorry") ? "limited" :
  location.hostname === "gemini.google.com" && !!(window.WIZ_global_data && window.WIZ_global_data.SNlM0e));
if (signedIn === "limited") {
  return { error: "http 429" };
} else if (!signedIn) {
  return { error: "auth" };
} else {
  const sel = %q;
  const norm = t => String(t || "").replace(/\s+/g, " ").trim();
  const look = () => page.evaluate((sel) => ({
    turns: document.querySelectorAll("user-query").length,
    last: (([...document.querySelectorAll("user-query .query-text, user-query")].pop() || {}).innerText) || "",
    draft: ((document.querySelector(sel) || {}).innerText) || "",
  }), sel);
  await page.waitForSelector("loc=css:" + sel, { state: "visible", timeout: 15000 });
  let before = await look();
  for (let i = 0; i < 15 && before.turns === 0; i++) {
    await new Promise(r => setTimeout(r, 1000));
    before = await look();
  }
  if (before.turns === 0) {
    return { error: "no task" };
  } else {
    await page.click("loc=css:" + sel);
    await page.keyboard.press("ControlOrMeta+A");
    await page.keyboard.press("Backspace");
    await page.keyboard.insertText(ARGS0.text);
    await new Promise(r => setTimeout(r, 500));
    const clicked = await page.click("loc=css:" + %q, { timeout: 3000 }).then(() => true).catch(() => false);
    if (!clicked) await page.press("loc=css:" + sel, "Enter");
    const want = norm(ARGS0.text);
    let now = before;
    for (let i = 0; i < 15; i++) {
      await new Promise(r => setTimeout(r, 1000));
      now = await look();
      if (now.turns > before.turns && norm(now.last).includes(want)) break;
    }
    if (now.turns > before.turns && norm(now.last).includes(want)) {
      return { sent: true, clicked };
    } else if (norm(now.draft).includes(want)) {
      await page.click("loc=css:" + sel).catch(() => {});
      await page.keyboard.press("ControlOrMeta+A").catch(() => {});
      await page.keyboard.press("Backspace").catch(() => {});
      return { clicked, error: "not sent" };
    } else {
      return { sent: false, clicked };
    }
  }
}
`, args, geminiOrigin+"/app/"+id, geminiComposerSel, geminiSendSel)
	var r struct {
		Error   string `json:"error"`
		Sent    bool   `json:"sent"`
		Clicked bool   `json:"clicked"`
		ID      string `json:"id"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Code == provider.CodeAuthFailed {
			return provider.SendResult{}, err
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: provider.CodeOf(err)}, nil
	}
	switch r.Error {
	case "auth", "no task":
		return provider.SendResult{}, geminiError(r.Error)
	case "http 429":
		// Refused before anything was typed: nothing went out.
		return provider.SendResult{}, geminiError(r.Error)
	case "not sent":
		// Still in the composer after the wait, and cleared from it: it did not go, and can be sent again.
		return provider.SendResult{}, &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	if !r.Sent {
		code := provider.CodeUpstreamError
		if r.Error != "" {
			code = provider.CodeOf(geminiError(r.Error))
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: code}, nil
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: r.ID}, nil
}
