package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// geminiSite drives Gemini (gemini.google.com) for one Google account: its main chat, its other conversations and
// its tasks (the composer's "任务" / "Tasks" mode: Gemini works on a task with tools, and the conversation carries the
// task's status). It reads through the batchexecute RPCs the signed-in page itself calls, with the page's own token
// (WIZ_global_data.SNlM0e, never leaving the browser), and sends through the conversation's composer.
//
// The main chat is one conversation Ensemble started: the first send to "main" opens a new conversation in the
// composer's 对话 (chat) mode and the page keeps its id (localStorage, geminiMainKey); later sends to main continue
// it, and main reads it. The account's other conversations are side chats of main, its tasks are tasks.
//
// Gemini keeps a turn only once its answer is written: a page left (navigated, closed) before then loses the turn,
// and while an answer the page left half-written is still being written, the conversation takes no other message.
// So a send waits for the answer, a run never leaves a page that is writing one if it can wait, and a tab is not
// recycled under one.
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

// geminiMaxChats is the most plain conversations listed as side chats.
const geminiMaxChats = 30

// geminiMainKey is where the page keeps the main chat's conversation id (localStorage on gemini.google.com).
const geminiMainKey = "ensemble.gemini.main"

// geminiMainTitle is the main chat's title.
const geminiMainTitle = "Gemini"

// geminiSendTimeout bounds a send: the page, the wait for an answer Gemini is still writing there, the typing, and
// the wait for this message's answer (geminiAnswerWaitMs), within the node's two-minute limit.
const geminiSendTimeout = 110 * time.Second

// geminiAnswerWaitMs is how long a send waits at most for its answer to be written before it leaves the page (the
// answer is then still being written in the tab, which no run leaves while it is).
const geminiAnswerWaitMs = 60000

// The page's parts the scripts use (testdata/gemini holds recorded pages they are tested on).
const (
	// geminiComposerSel is the composer.
	geminiComposerSel = `rich-textarea div.ql-editor[contenteditable="true"]`
	// geminiSendSel is the send button, shown while the composer holds text.
	geminiSendSel = `button[aria-label="发送"]:not([disabled]), button[aria-label="Send message" i]:not([disabled]), button.send-button:not([disabled])`
	// geminiStopSel is the stop button, shown in place of the send button while an answer is being written.
	geminiStopSel = `button[aria-label="停止回答"], button[aria-label="Stop response" i], button.stop`
	// geminiQuerySel is one of the user's turns on the page.
	geminiQuerySel = `user-query`
	// geminiQueryTextSel is a turn's text (its lines; the turn also carries a screen reader's label with "你说").
	geminiQueryTextSel = `.query-text-line`
	// geminiChatModeSel is the composer's 对话 (chat) mode, on a new conversation's page (任务 is data-value 40).
	geminiChatModeSel = `chat-spark-toggle gem-segmented-button[data-value="0"] button`
)

// geminiHumanCheckJS defines __humanCheck() inside the page: whether Google put its "unusual traffic" check in front
// of Gemini (www.google.com/sorry/index, a reCAPTCHA in #captcha-form) instead of the app. That is neither signed out
// nor a rate limit: only a person solving the check lets the browser through again. A signed-in Gemini page (it has
// its token) is never taken for one, so a conversation that mentions "unusual traffic" is not.
const geminiHumanCheckJS = `
const __humanCheck = () => {
  if (location.pathname.startsWith("/sorry")) return true;
  const w = window.WIZ_global_data || {};
  if (location.hostname === "gemini.google.com" && w.SNlM0e) return false;
  if (document.querySelector('#captcha-form, form[action*="/sorry"], .g-recaptcha, iframe[src*="/recaptcha/"], iframe[title*="reCAPTCHA" i]')) return true;
  const t = String((document.body && document.body.innerText) || "").slice(0, 4000);
  return /unusual traffic|not a robot|异常流量|我不是机器人|不是自动程序/i.test(t);
};
`

// geminiPrelude defines __rpc(id, args) inside the page: one batchexecute call with the page's token, answering the
// RPC's decoded payload. It throws "human check" when Google's human verification stands in front of Gemini, "auth"
// when the page is not signed in and "http <status>" on a failed request.
const geminiPrelude = geminiHumanCheckJS + `
const __wiz = () => {
  if (__humanCheck()) throw new Error("human check");
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
  // An RPC Google answers with its unusual-traffic page (redirected to /sorry, or a 429 carrying it) is the human
  // check too; a bare 429 is a rate limit.
  if (/\/sorry\//.test(r.url || "")) throw new Error("human check");
  if (r.status === 429) {
    const body = await r.text().catch(() => "");
    throw new Error(/\/sorry\/|unusual traffic|recaptcha/i.test(body) ? "human check" : "http 429");
  }
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
const __busy = () => !!document.querySelector(` + "`" + geminiStopSel + "`" + `);
const __mainID = () => { try { return String(localStorage.getItem("` + geminiMainKey + `") || ""); } catch (e) { return ""; } };
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
// the script's value. The RPCs answer on any of Gemini's pages, so a loaded page already on gemini.google.com is not
// moved (openJS): a sync then never navigates away under a send waiting in the same tab for its answer. Nor is a
// Gemini page writing an answer ever reloaded, and its answer says busy, so the tab is not recycled under it.
func geminiScript(gotoURL string, args any, body string) string {
	a, _ := json.Marshal(args)
	return fmt.Sprintf(`
const __geminiBusy = await page.evaluate((stop) => location.hostname === "gemini.google.com" && !!document.querySelector(stop), %q).catch(() => false);
if (!__geminiBusy) {
%s
}
const __out = await page.evaluate(async (ARGS) => {
%s
  try {
%s
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
}, %s);
if (__out && typeof __out === "object" && !__out.error) {
  __out.busy = await page.evaluate((stop) => !!document.querySelector(stop), %q).catch(() => false);
}
return __out;
`, geminiStopSel, openJS(gotoURL, geminiStay), geminiPrelude, body, a, geminiStopSel)
}

// geminiStay is any gemini.google.com page (openJS).
const geminiStay = `^https://gemini\.google\.com/`

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
	return provider.RemoteAccount{ExternalAccountID: "gemini:" + email, Alias: name}, nil
}

type geminiTask struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	At     int64  `json:"at"`
	Status string `json:"status"`
}

// listSessions is the main chat, the account's other conversations (side chats of main) and its tasks.
func (geminiSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	var r struct {
		Error string       `json:"error"`
		Main  string       `json:"main"`
		Chats []geminiTask `json:"chats"`
		Tasks []geminiTask `json:"tasks"`
	}
	script := geminiScript(geminiOrigin+"/app", map[string]any{"chats": geminiMaxChats}, `
    return { main: __mainID(), chats: await __tasks([1], ARGS.chats), tasks: await __allTasks(30) };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, err
	}
	if err := geminiError(r.Error); err != nil {
		return nil, err
	}
	mainID := strings.TrimPrefix(strings.TrimSpace(r.Main), "c_")
	mainURL := geminiOrigin + "/app"
	if mainID != "" {
		mainURL += "/" + mainID
	}
	out := []provider.RemoteSession{{
		ExternalID: "main",
		Title:      geminiMainTitle,
		Kind:       protocol.KindMain,
		CanSend:    true,
		Ref:        map[string]string{"url": mainURL},
	}}
	seen := map[string]bool{"main": true}
	for _, t := range r.Chats {
		id := strings.TrimSpace(t.ID)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if id == mainID {
			// The main chat's own conversation is main, not a side chat of it.
			out[0].LastActivityMs = t.At
			continue
		}
		title := strings.TrimSpace(t.Title)
		if title == "" {
			title = "Gemini 对话"
		}
		out = append(out, provider.RemoteSession{
			ExternalID:       id,
			Title:            title,
			Kind:             protocol.KindSide,
			ParentExternalID: "main",
			LastActivityMs:   t.At,
			CanSend:          true,
			Ref:              map[string]string{"url": geminiOrigin + "/app/" + id},
		})
	}
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

// geminiTurnsJS reads ARGS.task's (the main chat's for "main") newest ARGS.turns turns into messages (oldest first,
// within a byte budget, as dotsMessagesJS does); a running task's newest answer is left out until it is done, so a
// half-written answer is never kept.
const geminiTurnsJS = `
    // The main chat reads the conversation the page keeps for it, and nothing before its first send.
    const isMain = ARGS.task === "main";
    const task = isMain ? __mainID() : ARGS.task;
    if (!task) return { messages: [] };
    const v = await __rpc("hNvQHb", ["c_" + task, ARGS.turns, null, 1, [0], [4], null, 1]);
    const turns = ((v && v[0]) || []).filter(t => Array.isArray(t) && Array.isArray(t[0]));
    if (!v || (!turns.length && !isMain)) throw new Error("no task");
    const running = ARGS.checkRunning && !isMain ? (await __tasks([2], 30)).some(t => t.id === task) : false;
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
	if id == "" {
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

func (geminiSite) send(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, msg provider.Outgoing) (provider.SendResult, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return provider.SendResult{}, errors.New("empty message")
	}
	id := geminiTarget(session)
	if id == "" {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeNotFound}
	}
	var r struct {
		Error    string `json:"error"`
		Sent     bool   `json:"sent"`
		Clicked  bool   `json:"clicked"`
		Answered bool   `json:"answered"`
		ID       string `json:"id"`
	}
	if err := a.runSend(ctx, credential, geminiSendScript(id, text), &r); err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Code == provider.CodeAuthFailed {
			return provider.SendResult{}, err
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: provider.CodeOf(err)}, nil
	}
	switch r.Error {
	case "auth", "no task":
		return provider.SendResult{}, geminiError(r.Error)
	case "human check", "http 429":
		// Refused before anything was typed: nothing went out.
		return provider.SendResult{}, geminiError(r.Error)
	case "busy", "not sent":
		// Gemini still writing an answer in the conversation, or the message still in the composer after the wait
		// (cleared from it): it did not go, and can be sent again.
		return provider.SendResult{}, &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	if !r.Sent {
		code := provider.CodeUpstreamError
		if r.Error != "" {
			code = provider.CodeOf(geminiError(r.Error))
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: code}, nil
	}
	ext := ""
	if r.ID != "" {
		ext = r.ID + ":u" // as listMessages names the user's turn
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: ext}, nil
}

// geminiSendScript sends text into conversation id ("main" for the main chat). Each look is its own short evaluate.
//
//  1. A Gemini page in the tab still writing an answer is left only once it is written (or the wait is over): the
//     page leaving would lose that turn.
//  2. The conversation is opened: id's, the main chat's kept one, or a new conversation in 对话 mode for a main chat
//     that has none yet (or whose conversation is gone).
//  3. An answer still being written there is waited for; still writing after the wait, the send is refused (busy):
//     Gemini takes no other message in the conversation meanwhile.
//  4. The text is typed into a composer cleared of any draft Gemini kept for the conversation, and sent once the
//     composer holds exactly it. It went once the page shows a new turn of the user's with this text; still in the
//     composer a few seconds later (Gemini did not take the click), it is sent again with Enter; still there after
//     the wait, it did not go: the draft is cleared and the send refused (not sent).
//  5. A new main conversation's id is kept as the main chat's. The send then waits for the answer to be written
//     (geminiAnswerWaitMs) and, once it is, reads the turn back from Gemini's own record to name it.
func geminiSendScript(id, text string) string {
	args, _ := json.Marshal(map[string]any{"text": text, "id": id, "answerWaitMs": geminiAnswerWaitMs,
		"budgetMs": int((geminiSendTimeout - 10*time.Second) / time.Millisecond)})
	return fmt.Sprintf(`
const ARGS0 = %s;
const MAIN_KEY = %q, SEL = %q, SEND = %q, STOP = %q, QUERY = %q, QUERY_TEXT = %q, CHAT_MODE = %q;
const sleep = ms => new Promise(r => setTimeout(r, ms));
// Every wait is held to what is left of the run (ARGS0.budgetMs from its start), so it ends with an answer.
const T0 = Date.now();
const left = () => Math.max(0, ARGS0.budgetMs - (Date.now() - T0));
const norm = t => String(t || "").replace(/\s+/g, " ").trim();
const onGemini = async () => /^https:\/\/gemini\.google\.com\//.test(String(await Promise.resolve().then(() => page.url()).catch(() => "")));
const busy = () => page.evaluate((stop) => !!document.querySelector(stop), STOP).catch(() => false);
const waitIdle = async (ms) => {
  const end = Date.now() + Math.min(ms, left());
  while (await busy()) {
    if (Date.now() >= end) return false;
    await sleep(1000);
  }
  return true;
};
// 1. Not under an answer being written in this tab: still writing after the wait, the page is not left (that
// would lose the turn it answers) and the send is refused.
if ((await onGemini()) && !(await waitIdle(30000))) return { error: "busy" };
// 2. The conversation.
let target = ARGS0.id === "main" ? "" : ARGS0.id;
if (ARGS0.id === "main") {
  if (!(await onGemini())) {
    await page.goto("https://gemini.google.com/app");
    await page.waitForLoadState("domcontentloaded").catch(() => {});
  }
  target = await page.evaluate((k) => { try { return String(localStorage.getItem(k) || ""); } catch (e) { return ""; } }, MAIN_KEY).catch(() => "");
}
const fresh = !target;
const here = String(await Promise.resolve().then(() => page.url()).catch(() => "")).replace(/[?#].*$/, "");
if (fresh || here !== "https://gemini.google.com/app/" + target) {
  await page.goto("https://gemini.google.com/app" + (target ? "/" + target : ""));
  await page.waitForLoadState("domcontentloaded").catch(() => {});
}
const signedIn = await page.evaluate(() => {
%s
  return __humanCheck() ? "human check" :
    location.hostname === "gemini.google.com" && !!(window.WIZ_global_data && window.WIZ_global_data.SNlM0e);
});
if (signedIn === "human check") return { error: "human check" };
if (!signedIn) return { error: "auth" };
const look = () => page.evaluate(([sel, query, queryText]) => {
  const qs = [...document.querySelectorAll(query)];
  const lastQ = qs[qs.length - 1];
  const lines = lastQ ? [...lastQ.querySelectorAll(queryText)] : [];
  return {
    turns: qs.length,
    last: lines.length ? lines.map(l => l.innerText).join("\n") : ((lastQ || {}).innerText || ""),
    draft: ((document.querySelector(sel) || {}).innerText) || "",
    path: String(location.pathname),
  };
}, [SEL, QUERY, QUERY_TEXT]);
await page.waitForSelector("loc=css:" + SEL, { state: "visible", timeout: 15000 });
let before = await look();
if (!fresh) {
  for (let i = 0; i < 15 && before.turns === 0 && left() > 0; i++) {
    await sleep(1000);
    before = await look();
  }
  if (before.turns === 0) {
    if (ARGS0.id !== "main") return { error: "no task" };
    // The main chat's conversation is gone: a new one.
    await page.goto("https://gemini.google.com/app");
    await page.waitForLoadState("domcontentloaded").catch(() => {});
    await page.waitForSelector("loc=css:" + SEL, { state: "visible", timeout: 15000 });
    before = await look();
    before.fresh = true;
  }
}
const isNew = fresh || !!before.fresh;
if (isNew) {
  // A new conversation is a chat, not a task.
  await page.click("loc=css:" + CHAT_MODE, { timeout: 3000 }).catch(() => {});
}
// 3. Not while Gemini still writes an answer in the conversation.
if (!(await waitIdle(20000))) return { error: "busy" };
// 4. Gemini puts back a draft it kept for the conversation a moment after the page shows: wait for the composer to
// settle, empty it, type, and send only when it holds exactly this text.
const settle = async () => {
  let prev = null;
  for (let i = 0, same = 0; i < 12 && same < 2 && left() > 0; i++) {
    const d = (await look()).draft;
    same = d === prev ? same + 1 : 0;
    prev = d;
    await sleep(400);
  }
};
const setDraft = async (t) => {
  await page.click("loc=css:" + SEL).catch(() => {});
  await page.evaluate((sel) => {
    const el = document.querySelector(sel);
    if (!el) return;
    el.focus();
    const r = document.createRange();
    r.selectNodeContents(el);
    const s = getSelection();
    s.removeAllRanges();
    s.addRange(r);
    document.execCommand("delete");
  }, SEL);
  if (t) await page.keyboard.insertText(t);
  await sleep(400);
  return norm((await look()).draft) === norm(t);
};
await settle();
let ready = await setDraft(ARGS0.text);
if (!ready) {
  await settle();
  ready = await setDraft(ARGS0.text);
}
if (!ready) {
  await setDraft("");
  return { error: "not sent" };
}
const want = norm(ARGS0.text);
before = { ...(await look()), fresh: before.fresh };
const went = (now) => (now.turns > before.turns || (isNew && now.turns > 0)) && norm(now.last) === want;
const clicked = await page.click("loc=css:" + SEND, { timeout: 3000 }).then(() => true).catch(() => false);
if (!clicked) await page.press("loc=css:" + SEL, "Enter");
let now = before, pressed = !clicked;
for (let i = 0; i < 20 && left() > 0; i++) {
  await sleep(1000);
  now = await look();
  if (went(now)) break;
  if (!pressed && i >= 3 && norm(now.draft).includes(want) && !(await busy())) {
    // Gemini did not take the click: the text is still in the composer.
    pressed = true;
    await page.press("loc=css:" + SEL, "Enter").catch(() => {});
  }
}
if (!went(now)) {
  if (norm(now.draft).includes(want)) {
    await setDraft("");
    return { clicked, error: "not sent" };
  }
  return { sent: false, clicked };
}
// 5. Sent. A new main conversation's id is kept as soon as the page shows its address, before the answer is waited
// for, so a run cut short still keeps it.
const pathID = async () => ((await look()).path.match(/\/app\/([0-9a-f]{8,})/) || [])[1] || "";
let kept = "";
if (isNew && ARGS0.id === "main") {
  for (let i = 0; i < 10 && !kept && left() > 0; i++) {
    kept = await pathID();
    if (!kept) await sleep(500);
  }
  if (kept) await page.evaluate(([k, v]) => { try { localStorage.setItem(k, v); } catch (e) {} }, [MAIN_KEY, kept]).catch(() => {});
}
// Then the answer is waited for (leaving the page before it is written loses the turn), and the turn named from
// Gemini's record.
const answered = await waitIdle(Math.min(ARGS0.answerWaitMs, left() - 8000));
const found = await page.evaluate(async ([want, path, fresh]) => {
%s
  const n = t => String(t || "").replace(/\s+/g, " ").trim();
  const turnOf = async (conv) => {
    const v = await __rpc("hNvQHb", ["c_" + conv, 3, null, 1, [0], [4], null, 1]);
    const t = ((v && v[0]) || []).find(t => Array.isArray(t) && Array.isArray(t[0]) && n(t[2] && t[2][0] && t[2][0][0]).includes(want));
    return t ? String(t[0][1] || "") : "";
  };
  try {
    let conv = (String(location.pathname).match(/\/app\/([0-9a-f]{8,})/) || [])[1] || "";
    if (conv) return { conv, turn: await turnOf(conv) };
    if (!fresh) return { conv: "", turn: "" };
    // A new conversation the address does not show yet: the newest chat with this turn.
    for (const c of await __tasks([1], 3)) {
      const turn = await turnOf(c.id);
      if (turn) return { conv: c.id, turn };
    }
  } catch (e) {}
  return { conv: "", turn: "" };
}, [want, "", isNew]).catch(() => ({ conv: "", turn: "" }));
const conv = found.conv, turn = found.turn;
if (isNew && ARGS0.id === "main" && conv && conv !== kept) {
  await page.evaluate(([k, v]) => { try { localStorage.setItem(k, v); } catch (e) {} }, [MAIN_KEY, conv]).catch(() => {});
}
return { sent: true, clicked, answered, id: turn, conversation: conv };
`, args, geminiMainKey, geminiComposerSel, geminiSendSel, geminiStopSel, geminiQuerySel, geminiQueryTextSel, geminiChatModeSel,
		geminiHumanCheckJS, geminiPrelude)
}

// readQuota reads Gemini's real usage windows (Current usage + Weekly limit and their reset times) from
// https://gemini.google.com/usage?hl=en inside an isolated temporary page (task.newPage) so the active Gemini chat
// tab is never navigated away or disturbed while answering.
func (geminiSite) readQuota(ctx context.Context, a *Adapter, credential string) ([]provider.QuotaReading, error) {
	script := fmt.Sprintf(`
if (typeof task === "undefined" || !task || typeof task.newPage !== "function") {
  return { unavailable: true };
}
const __busy = async () => page.evaluate((stop) => location.hostname === "gemini.google.com" && !!document.querySelector(stop), %q).catch(() => false);
const qp = await task.newPage();
let out = { unavailable: true };
try {
  await qp.goto("https://gemini.google.com/usage?hl=en");
  await qp.waitForFunction(() => {
    const el = document.querySelector("main") || document.body;
    const t = (el && el.innerText) || "";
    return /%%\s*used|已使用\s*\d|Usage limits|用量限额/i.test(t);
  }, undefined, { timeout: 10000 }).catch(() => {});
  out = await qp.evaluate(() => {
    const url = String(location.href || "");
    if (/accounts\.google\.com/i.test(url)) return { error: "auth" };
    if (/\/sorry\b/i.test(String(location.pathname || ""))) return { error: "human check" };
    const main = document.querySelector("main") || document.body;
    const text = String((main && main.innerText) || "");
    const now = new Date();
    const parseReset = (line) => {
      if (!line) return 0;
      let m = line.match(/Resets(?:\s+tomorrow)?\s+at\s+(\d{1,2}):(\d{2})(?:\s*(AM|PM))?/i);
      if (m) {
        let h = parseInt(m[1], 10), min = parseInt(m[2], 10);
        const ap = (m[3] || "").toUpperCase();
        if (ap === "PM" && h < 12) h += 12;
        if (ap === "AM" && h === 12) h = 0;
        const d = new Date(now.getFullYear(), now.getMonth(), now.getDate(), h, min, 0, 0);
        if (/tomorrow/i.test(line) || d.getTime() <= now.getTime()) d.setDate(d.getDate() + 1);
        return d.getTime();
      }
      m = line.match(/Resets(?:\s+on)?\s+([A-Za-z]{3,9})\s+(\d{1,2})(?:,?\s*(\d{4}))?\s+at\s+(\d{1,2}):(\d{2})(?:\s*(AM|PM))?/i);
      if (m) {
        const months = { jan:0,feb:1,mar:2,apr:3,may:4,jun:5,jul:6,aug:7,sep:8,oct:9,nov:10,dec:11 };
        const mo = months[m[1].slice(0, 3).toLowerCase()];
        if (mo !== undefined) {
          let h = parseInt(m[4], 10), min = parseInt(m[5], 10);
          const ap = (m[6] || "").toUpperCase();
          if (ap === "PM" && h < 12) h += 12;
          if (ap === "AM" && h === 12) h = 0;
          let yr = m[3] ? parseInt(m[3], 10) : now.getFullYear();
          let d = new Date(yr, mo, parseInt(m[2], 10), h, min, 0, 0);
          if (!m[3] && d.getTime() < now.getTime() - 86400000) d = new Date(yr + 1, mo, parseInt(m[2], 10), h, min, 0, 0);
          return d.getTime();
        }
      }
      return 0;
    };
    const lines = text.split("\n").map(s => s.trim()).filter(Boolean);
    const readBlock = (headerRe) => {
      const idx = lines.findIndex(l => headerRe.test(l));
      if (idx < 0) return null;
      let used = null, resetsAt = 0;
      for (let i = idx + 1; i < Math.min(lines.length, idx + 6); i++) {
        const l = lines[i];
        if (/^(Weekly limit|Current usage|Get |Usage settings|每周限额|当前用量|用量设置)/i.test(l)) break;
        const pm = l.match(/(\d+(?:\.\d+)?)\s*%%\s*used/i) || l.match(/已使用\s*(\d+(?:\.\d+)?)\s*%%/);
        if (pm && used === null) used = parseFloat(pm[1]);
        if (!resetsAt && /Resets/i.test(l)) resetsAt = parseReset(l);
      }
      if (used === null) return null;
      return { used, resetsAt };
    };
    const current = readBlock(/^(Current usage|当前用量)$/i);
    const weekly = readBlock(/^(Weekly limit|每周限额)$/i);
    if (!current && !weekly) return { unavailable: true };
    return { current, weekly };
  });
} finally {
  await qp.close().catch(() => {});
}
// Tell recycleScript if the main Gemini chat tab is still answering so tab recycling never drops a live reply.
if (out && typeof out === "object") out.busy = await __busy();
return out;
`, geminiStopSel)
	var r struct {
		Error       string             `json:"error"`
		Unavailable bool               `json:"unavailable"`
		Current     *geminiUsageWindow `json:"current"`
		Weekly      *geminiUsageWindow `json:"weekly"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, nil
	}
	if r.Error == "http 429" {
		return nil, dotsError(r.Error)
	}
	if r.Error != "" || r.Unavailable {
		return nil, nil
	}
	var out []provider.QuotaReading
	appendWindow := func(w *geminiUsageWindow, window string) {
		if w == nil || w.Used == nil {
			return
		}
		used := *w.Used
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		rem := 100 - used
		out = append(out, provider.QuotaReading{
			Kind:       protocol.QuotaKindWindow,
			Window:     window,
			Limit:      100,
			Remaining:  rem,
			ResetsAt:   int64(w.ResetsAt),
			Exhausted:  rem <= 0,
			SourceNote: "gemini:usage",
		})
	}
	appendWindow(r.Current, "5h")
	appendWindow(r.Weekly, "week")
	return out, nil
}

type geminiUsageWindow struct {
	Used     *float64 `json:"used"`
	ResetsAt float64  `json:"resetsAt"`
}
