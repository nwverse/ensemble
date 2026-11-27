package browser

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// Muse (muse.ai): the main chat at muse.ai/ and its side chats (旁聊) at muse.ai/thread/<uuid>. Muse talks to its
// backend over an encrypted WebSocket, so everything is read from the rendered page:
//   - Messages are [data-message-item][data-message-id][data-message-role] rows in the chat log; each carries an
//     sr-only surrogate with its full text, after who said it. There are no per-message times (only date
//     separators in the log), so messages are ordered by position.
//   - The left panel lists the threads as [data-testid="hatch-thread-row"] rows: the main chat first, then the side
//     chats with a title and a relative date. A row has no href; its thread id is in the analytics metadata React
//     passes it (actionAnalyticsMetadata.thread_id, is_primary_thread). Rows are never clicked to learn an id: a
//     click navigates in-page and shows the previous thread's rows under the new URL for a while. The id is looked
//     for only within the row's own React fibers (up to the first DOM element above it). A row whose id cannot be
//     read, or whose id another row also resolves to, is left out of the list, not guessed.
//   - A thread that no longer exists redirects to muse.ai/ and shows the main chat, so every read and send checks
//     the page is on its thread (URL and highlighted row) and answers NOT_FOUND otherwise.
// A signed-out visitor gets a landing page with no chat log.

type museSite struct{}

const museComposerSel = `textarea[aria-label="消息"], textarea[aria-label="Message" i]`
const museSendSel = `button[aria-label="发送"], button[aria-label="Send" i]`

// museMaxThreads caps the side chats listed (the panel shows the most recent first).
const museMaxThreads = 30

var museThreadIDRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

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

// museThreadJS defines, in page, __museRowThread(row): the {id, primary} of a thread row, from the analytics
// metadata React passes it (null when none is found), and __museOn(thread): whether the page shows that thread
// ("" for the main chat) — its URL and, for a side chat, the highlighted row, whose id no other row has.
// The fiber walk stays within the row: it stops at the first DOM element above the row, so an object with a
// thread_id held by the panel or the app (the current thread, a route) is never read as the row's.
const museThreadJS = `
const __museRowThread = (row) => {
  const fk = Object.keys(row).find(k => k.startsWith("__reactFiber"));
  let f = fk ? row[fk] : null;
  for (let d = 0; f && d < 40; d++, f = f.return) {
    const sn = f.stateNode;
    if (sn && sn !== row && sn instanceof Element) break;
    const p = f.memoizedProps;
    if (!p || typeof p !== "object") continue;
    for (const v of Object.values(p)) {
      if (v && typeof v === "object" && !Array.isArray(v) && typeof v.thread_id === "string" && v.thread_id) {
        return { id: v.thread_id.toLowerCase(), primary: v.is_primary_thread === true };
      }
    }
  }
  return null;
};
const __museOn = (thread) => {
  if (!thread) return location.pathname === "/";
  if (location.pathname.toLowerCase() !== "/thread/" + thread) return false;
  const cur = document.querySelector('[data-testid="hatch-thread-row"][aria-current="page"]');
  const t = cur ? __museRowThread(cur) : null;
  if (!t || t.id !== thread) return false;
  let n = 0;
  for (const row of document.querySelectorAll('[data-testid="hatch-thread-row"]')) {
    const r = __museRowThread(row);
    if (r && r.id === thread) n++;
  }
  return n === 1;
};
`

const museSignedInJS = `!!document.querySelector('[role="log"][data-hatch-chat-column]')`

// museSignedOutJS is the error a page without the chat log answers (in page, with challengeJS): "human check" when
// Cloudflare's "Verify you are human" stands in front of Muse, "auth" otherwise (a signed-out landing page). Both
// reach the owner through the shared codes (dotsError), as Dots and Gemini do.
const museSignedOutJS = `(__challenged() ? "human check" : "auth")`

// museReadyJS tells the chat has drawn its rows (an empty chat never does: the wait then times out).
const museReadyJS = `!!document.querySelector('[role="log"][data-hatch-chat-column] [data-message-item]')`

// museWaitJS is node-side: it waits (at most ms) for the page to show ARGS.thread with its rows drawn, and is true
// when it did.
func museWaitJS(ms int) string {
	return fmt.Sprintf(`await page.waitForFunction((ARGS) => {
%s
  return __museOn(ARGS.thread) && %s;
}, ARGS, { timeout: %d }).then(() => true).catch(() => false)`, museThreadJS, museReadyJS, ms)
}

// museThread is the thread a session reads ("" for the main chat) and the URL it is at. A session that is neither
// the main chat nor a side chat's id is not Muse's.
func museThread(a *Adapter, session provider.SessionRef) (thread, url string, err error) {
	id := strings.ToLower(strings.TrimSpace(session.ExternalID))
	switch {
	case id == "main":
		return "", a.site.AppURL, nil
	case museThreadIDRe.MatchString(id):
		return id, "https://muse.ai/thread/" + id, nil
	default:
		return "", "", &provider.Error{Code: provider.CodeNotFound}
	}
}

func (museSite) verify(ctx context.Context, a *Adapter, credential string) (provider.RemoteAccount, error) {
	script := siteScript(a.site.AppURL, "", nil, fmt.Sprintf(`
await page.waitForFunction(() => %s, undefined, { timeout: 15000 }).catch(() => {});
const signedIn = await page.evaluate(() => %s);
const challenged = signedIn ? false : await page.evaluate(() => {
%s
  return __challenged();
});
// The title becomes "Chat — <agent name>" once the chat is drawn.
if (signedIn) await page.waitForFunction(() => document.title.includes("—"), undefined, { timeout: 3000 }).catch(() => {});
return { signedIn, title: await page.title(), challenged };
`, museReadyJS, museSignedInJS, challengeJS))
	var r struct {
		SignedIn   bool   `json:"signedIn"`
		Challenged bool   `json:"challenged"`
		Title      string `json:"title"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemoteAccount{}, err
	}
	if !r.SignedIn {
		if r.Challenged {
			return provider.RemoteAccount{}, dotsError("human check")
		}
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	cred, _ := ParseCredential(credential)
	// The signed-in title is "Chat — <agent name>": the agent is the account's alias.
	_, agent, _ := strings.Cut(r.Title, "—")
	return provider.RemoteAccount{ExternalAccountID: a.site.Provider + ":" + cred.MachineID, Alias: strings.TrimSpace(agent)}, nil
}

// listSessions is the main chat (ExternalID "main", as before side chats were listed, so its synced messages keep
// their session) and the side chats the panel lists, read from one page load without clicking any row.
func (museSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	script := siteScript(a.site.AppURL, "", map[string]any{"max": museMaxThreads}, fmt.Sprintf(`
await page.waitForFunction(() => %s && !!document.querySelector('[data-testid="hatch-thread-row"]'), undefined, { timeout: 15000 }).catch(() => {});
return await page.evaluate((ARGS) => {
%s
%s
%s
  if (!(%s)) return { error: %s };
  // An id that more than one row resolves to is not any one row's: every row with it is left out.
  const rows = [...document.querySelectorAll('[data-testid="hatch-thread-row"]')].map(row => ({ row, t: __museRowThread(row) }));
  const count = new Map();
  for (const { t } of rows) if (t) count.set(t.id, (count.get(t.id) || 0) + 1);
  const threads = [];
  for (const { row, t } of rows) {
    if (!t || t.primary || count.get(t.id) !== 1) continue;
    const named = row.querySelector('span[title]');
    const title = __norm(named ? named.getAttribute('title') : String(row.innerText || '').split('\n')[0]);
    // The relative date ("5m", "周日", "11月8日") is the first other span with text.
    let when = '';
    for (const s of row.querySelectorAll('span')) {
      if (s === named || s.getAttribute('role') === 'status' || s.querySelector('span')) continue;
      const v = __norm(s.textContent);
      if (v && v !== title) { when = v; break; }
    }
    threads.push({ id: t.id, title, when });
    if (threads.length >= ARGS.max) break;
  }
  return { threads };
}, ARGS);
`, museSignedInJS, sitePrelude, challengeJS, museThreadJS, museSignedInJS, museSignedOutJS))
	var r struct {
		Error   string `json:"error"`
		Threads []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			When  string `json:"when"`
		} `json:"threads"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, err
	}
	if err := dotsError(r.Error); err != nil {
		return nil, err
	}
	now := time.Now()
	out := []provider.RemoteSession{{
		ExternalID:     "main",
		Title:          a.site.Label,
		Kind:           protocol.KindMain,
		LastActivityMs: now.UnixMilli(),
		CanSend:        true,
		Ref:            map[string]string{"url": a.site.AppURL},
	}}
	for _, t := range r.Threads {
		id := strings.ToLower(strings.TrimSpace(t.ID))
		if !museThreadIDRe.MatchString(id) {
			continue
		}
		title := strings.TrimSpace(t.Title)
		if title == "" {
			title = museUntitled
		}
		out = append(out, provider.RemoteSession{
			ExternalID:       id,
			Title:            title,
			Kind:             protocol.KindSide,
			ParentExternalID: "main",
			LastActivityMs:   museActivityMs(t.When, now),
			CanSend:          true,
			Ref:              map[string]string{"url": "https://muse.ai/thread/" + id},
		})
	}
	return out, nil
}

// museUntitled is the title of every side chat the panel gives none. The row carries nothing else that is both
// stable and readable (only a last-activity date), so it is not numbered by its place in the panel, which moves,
// nor named after its id: chats may share it, and the Agents list tells them apart by their preview and time. It
// is the same in every locale.
const museUntitled = "未命名旁聊"

func (museSite) listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	thread, url, err := museThread(a, session)
	if err != nil {
		return provider.RemotePage{}, err
	}
	script := siteScript(url, "", map[string]any{"thread": thread, "limit": clampLimit(opts.Limit, 50)}, fmt.Sprintf(`
%s;
return await page.evaluate((ARGS) => {
%s
%s
%s
%s
  if (!(%s)) return { error: %s };
  if (!__museOn(ARGS.thread)) return { error: "no thread" };
  return { messages: __fit(__museMessages(ARGS.limit), 40000) };
}, ARGS);
`, museWaitJS(15000), sitePrelude, challengeJS, museMessagesJS, museThreadJS, museSignedInJS, museSignedOutJS))
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

// museSendJS is the node-side body of a send. Nothing is typed until the page shows the thread with its messages
// drawn. It notes the messages already shown, empties the composer (Muse kept no draft when tried, but a leftover
// one must not go out with the text), types, and clicks send only when the composer holds exactly the text and the
// page is still on the thread. Any stop before the click answers {error, unsent: true}: nothing went out. It is sent
// once a new owner message with the text reads back on the thread after the last message the page showed before:
// an older row drawn late (Muse may still be loading them) is above that and never counts. A text still in the
// composer afterwards was not sent and is cleared.
func museSendJS(wait time.Duration) string {
	return fmt.Sprintf(`
const __read = () => page.evaluate((ARGS) => {
%s
%s
%s
%s
  if (!(%s)) return { error: %s };
  if (!__museOn(ARGS.thread)) return { error: "no thread" };
  return { messages: __museMessages(30) };
}, ARGS);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const ready = %s;
const before = await __read();
if (before.error) return { error: before.error, unsent: true };
if (!ready) return { error: "not ready", unsent: true };
const shown = new Set(before.messages.map(m => m.id));
const sel = %q;
const visible = await page.waitForSelector("loc=css:" + sel, { state: "visible", timeout: 5000 }).then(() => true).catch(() => false);
if (!visible) return { error: "no composer", unsent: true };
const draft = () => page.evaluate((sel) => { const el = document.querySelector(sel); return el ? String(el.value || "") : null; }, sel);
// Let a draft the page puts back show before emptying the composer.
for (let i = 0, prev = null, same = 0; i < 8 && same < 2; i++) {
  const d = await draft();
  same = d === prev ? same + 1 : 0;
  prev = d;
  await sleep(250);
}
await page.fill("loc=css:" + sel, "");
await page.fill("loc=css:" + sel, ARGS.text);
await sleep(400);
if ((await draft()) !== ARGS.text) {
  await page.fill("loc=css:" + sel, "").catch(() => {});
  return { error: "not sent", unsent: true };
}
const still = await __read();
if (still.error) {
  await page.fill("loc=css:" + sel, "").catch(() => {});
  return { error: still.error, unsent: true };
}
const clicked = await page.click("loc=css:" + %q, { timeout: 3000 }).then(() => true).catch(() => false);
if (!clicked) await page.press("loc=css:" + sel, "Enter");
const norm = t => String(t || "").replace(/\s+/g, " ").trim();
const want = norm(ARGS.text);
// The new owner message with the text, among the rows after the last one the page showed before (none when none of
// those rows is shown any more: then where the new rows start is not known).
const fresh = (msgs) => {
  let last = -1;
  msgs.forEach((m, i) => { if (shown.has(m.id)) last = i; });
  if (last < 0 && shown.size) return null;
  return msgs.slice(last + 1).filter(m => m.role === "user" && norm(m.text) === want).pop() || null;
};
const deadline = Date.now() + %d;
let hit = null;
while (!hit && Date.now() < deadline) {
  await sleep(1000);
  const now = await __read();
  if (now.error) continue;
  hit = fresh(now.messages);
}
if (!hit) {
  if (norm(await draft().catch(() => "")) === want) {
    await page.fill("loc=css:" + sel, "").catch(() => {});
    return { clicked, error: "not sent" };
  }
  return { sent: false, clicked };
}
// Let the page settle so an optimistic id has been replaced by the stored one.
await sleep(1500);
const after = await __read();
const settled = after.error ? null : fresh(after.messages);
return { sent: true, clicked, id: (settled || hit).id };
`, sitePrelude, challengeJS, museMessagesJS, museThreadJS, museSignedInJS, museSignedOutJS, museWaitJS(15000), museComposerSel, museSendSel, wait.Milliseconds())
}

// museUnsent is the refusal for a send stopped before the click: nothing was typed into a sent message, so it
// failed (a clean resend is safe), never unconfirmed. A thread that is gone is NOT_FOUND, a signed-out or blocked
// page is AUTH_FAILED or HUMAN_CHECK_REQUIRED; the rest are refusals that a retry of the same send would not change.
func museUnsent(msg string) error {
	switch msg {
	case "auth", "human check", "no thread":
		return dotsError(msg)
	default:
		return &provider.Error{Code: provider.CodeRequestFailed}
	}
}

func (museSite) send(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, msg provider.Outgoing) (provider.SendResult, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return provider.SendResult{}, errors.New("empty message")
	}
	thread, url, err := museThread(a, session)
	if err != nil {
		return provider.SendResult{}, err
	}
	script := siteScript(url, "", map[string]any{"thread": thread, "text": text}, museSendJS(18*time.Second))
	var r struct {
		Error  string `json:"error"`
		Unsent bool   `json:"unsent"`
		Sent   bool   `json:"sent"`
		ID     string `json:"id"`
	}
	err = a.runSend(ctx, credential, script, &r)
	if err == nil && r.Unsent && r.Error != "" {
		return provider.SendResult{}, museUnsent(r.Error)
	}
	return sendOutcome(err, r.Error, r.Sent, r.ID)
}

var (
	museAgoRe     = regexp.MustCompile(`^(\d+)\s*(s|sec|秒|秒钟|m|min|mins|分|分钟|h|hr|hrs|小时|d|天|w|周)(?:\s*(?:前|ago))?$`)
	museCNDateRe  = regexp.MustCompile(`^(?:(\d{4})\s*年\s*)?(\d{1,2})\s*月\s*(\d{1,2})\s*日`)
	museISODateRe = regexp.MustCompile(`^(\d{4})[/.-](\d{1,2})[/.-](\d{1,2})`)
	museClockRe   = regexp.MustCompile(`^(\d{1,2}):(\d{2})$`)
	museWeekdays  = map[string]time.Weekday{
		"周日": time.Sunday, "周一": time.Monday, "周二": time.Tuesday, "周三": time.Wednesday, "周四": time.Thursday,
		"周五": time.Friday, "周六": time.Saturday, "星期日": time.Sunday, "星期天": time.Sunday, "星期一": time.Monday,
		"星期二": time.Tuesday, "星期三": time.Wednesday, "星期四": time.Thursday, "星期五": time.Friday, "星期六": time.Saturday,
		"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
		"fri": time.Friday, "sat": time.Saturday, "sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
		"wednesday": time.Wednesday, "thursday": time.Thursday, "friday": time.Friday, "saturday": time.Saturday,
	}
)

// museActivityMs reads a side chat row's relative date ("5m", "2h", "昨天", "周日", "11月8日", "2025年9月29日",
// "Nov 8") as of now; 0 when it is not one. A day is read as its end (no later than now), so a chat active that day
// counts as changed since a sync earlier that day.
func museActivityMs(label string, now time.Time) int64 {
	s := strings.ToLower(strings.TrimSpace(label))
	if s == "" {
		return 0
	}
	endOf := func(t time.Time) int64 {
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, now.Location())
		if end.After(now) {
			end = now
		}
		return end.UnixMilli()
	}
	day := func(y int, m time.Month, d int, yearGiven bool) int64 {
		t := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
		if t.Month() != m || t.Day() != d {
			return 0
		}
		if !yearGiven && t.After(now) {
			t = t.AddDate(-1, 0, 0)
		}
		return endOf(t)
	}
	switch s {
	case "刚刚", "now", "just now", "今天", "today":
		return now.UnixMilli()
	case "昨天", "yesterday":
		return endOf(now.AddDate(0, 0, -1))
	}
	if wd, ok := museWeekdays[s]; ok {
		back := (int(now.Weekday()) - int(wd) + 7) % 7
		if back == 0 {
			back = 7
		}
		return endOf(now.AddDate(0, 0, -back))
	}
	if m := museAgoRe.FindStringSubmatch(s); m != nil {
		n, _ := strconv.Atoi(m[1])
		switch m[2] {
		case "s", "sec", "秒", "秒钟":
			return now.Add(-time.Duration(n) * time.Second).UnixMilli()
		case "m", "min", "mins", "分", "分钟":
			return now.Add(-time.Duration(n) * time.Minute).UnixMilli()
		case "h", "hr", "hrs", "小时":
			return now.Add(-time.Duration(n) * time.Hour).UnixMilli()
		case "d", "天":
			return endOf(now.AddDate(0, 0, -n))
		default:
			return endOf(now.AddDate(0, 0, -7*n))
		}
	}
	if m := museClockRe.FindStringSubmatch(s); m != nil {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		if h > 23 || mi > 59 {
			return 0
		}
		t := time.Date(now.Year(), now.Month(), now.Day(), h, mi, 0, 0, now.Location())
		if t.After(now) {
			t = t.AddDate(0, 0, -1)
		}
		return t.UnixMilli()
	}
	if m := museCNDateRe.FindStringSubmatch(s); m != nil {
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		if m[1] != "" {
			y, _ := strconv.Atoi(m[1])
			return day(y, time.Month(mo), d, true)
		}
		return day(now.Year(), time.Month(mo), d, false)
	}
	if m := museISODateRe.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[1])
		mo, _ := strconv.Atoi(m[2])
		d, _ := strconv.Atoi(m[3])
		return day(y, time.Month(mo), d, true)
	}
	for _, layout := range []string{"Jan 2, 2006", "January 2, 2006"} {
		if t, err := time.Parse(layout, strings.TrimSpace(label)); err == nil {
			return day(t.Year(), t.Month(), t.Day(), true)
		}
	}
	for _, layout := range []string{"Jan 2", "January 2"} {
		if t, err := time.Parse(layout, strings.TrimSpace(label)); err == nil {
			return day(now.Year(), t.Month(), t.Day(), false)
		}
	}
	return 0
}

var (
	museUsedPctRe = regexp.MustCompile(`(?i)(?:已使用\s*([0-9]+(?:\.[0-9]+)?)\s*%|([0-9]+(?:\.[0-9]+)?)\s*%\s*used)`)
	museResetCNRe = regexp.MustCompile(`重置时间[：:]\s*([0-9]{1,2})月([0-9]{1,2})日`)
	museResetENRe = regexp.MustCompile(`(?i)resets?(?:\s+on)?\s+([A-Za-z]{3,9}\s+[0-9]{1,2})`)
)

// readQuota reads Muse's real weekly usage and extra credit allowance from Settings -> General -> Usage on muse.ai,
// intercepting the Next.js server action payload (with exact percentUsed, resetsAt, quotaStatus, topupBalance/Total)
// and falling back to the rendered dialog text when React serves cached state. Always closes the dialog before returning.
func (museSite) readQuota(ctx context.Context, a *Adapter, credential string) ([]provider.QuotaReading, error) {
	script := siteScript(a.site.AppURL, `^https://muse\.ai/`, nil, fmt.Sprintf(`
await page.waitForFunction(() => %s, undefined, { timeout: 6000 }).catch(() => {});
const signedIn = await page.evaluate(() => {
%s
  return { ok: %s, err: %s };
});
if (!signedIn.ok) return { error: signedIn.err };
await page.evaluate(() => {
  if (!window.__pacOrigFetch) {
    window.__pacOrigFetch = window.fetch;
    window.__pacSub = null;
    window.fetch = async function(...args) {
      const res = await window.__pacOrigFetch.apply(this, args);
      try {
        const reqBody = args[1] && typeof args[1].body === "string" ? args[1].body : "";
        if (reqBody.includes("includeAgreement")) {
          const text = await res.clone().text().catch(() => "");
          for (const line of text.split("\n")) {
            const idx = line.indexOf("{");
            if (idx < 0) continue;
            try {
              const obj = JSON.parse(line.slice(idx));
              if (obj && obj.subscription) { window.__pacSub = obj.subscription; break; }
            } catch (e) {}
          }
        }
      } catch (e) {}
      return res;
    };
  } else {
    window.__pacSub = null;
  }
});
let out = { unavailable: true };
try {
  const btnSel = 'loc=css:button[aria-label="设置"], button[aria-label="Settings" i]';
  await page.waitForSelector(btnSel, { state: "visible", timeout: 4000 }).catch(() => {});
  await page.click(btnSel).catch(() => {});
  await new Promise(r => setTimeout(r, 400));
  await page.evaluate(() => {
    const match = (el) => {
      const t = (el.innerText || "").trim();
      return t.startsWith("设置") || /^Settings(?:\s|$)/i.test(t);
    };
    const primary = [...document.querySelectorAll('[role="menu"] [role="menuitem"], [role="menu"] button')];
    let hit = primary.find(match);
    if (!hit) {
      const leaves = [...document.querySelectorAll('[role="menu"] div')].filter(el => el.children.length === 0);
      hit = leaves.find(match);
    }
    if (hit) hit.click();
  });
  await page.waitForFunction(() => {
    if (window.__pacSub) return true;
    const d = document.querySelector('[role="dialog"]');
    return !!(d && /已使用\s*\d|%%\s*used/i.test(d.innerText || ""));
  }, undefined, { timeout: 6000 }).catch(() => {});
  out = await page.evaluate(() => {
    const sub = window.__pacSub;
    const d = document.querySelector('[role="dialog"]');
    const text = d ? String(d.innerText || "").slice(0, 2000) : "";
    if (sub && sub.usage) {
      return {
        source: "action",
        percentUsed: typeof sub.usage.percentUsed === "number" ? sub.usage.percentUsed : null,
        resetsAt: typeof sub.usage.resetsAt === "number" ? sub.usage.resetsAt : 0,
        quotaStatus: String(sub.usage.quotaStatus || ""),
        topupBalance: typeof sub.topupBalance === "number" ? sub.topupBalance : 0,
        topupTotal: typeof sub.topupTotal === "number" ? sub.topupTotal : 0,
      };
    }
    if (text) return { source: "dom", text };
    return { unavailable: true };
  });
} finally {
  await page.keyboard.press("Escape").catch(() => {});
  await new Promise(r => setTimeout(r, 150));
  await page.keyboard.press("Escape").catch(() => {});
}
return out;
`, museSignedInJS, challengeJS, museSignedInJS, museSignedOutJS))

	var r struct {
		Error        string   `json:"error"`
		Unavailable  bool     `json:"unavailable"`
		Source       string   `json:"source"`
		PercentUsed  *float64 `json:"percentUsed"`
		ResetsAt     float64  `json:"resetsAt"`
		QuotaStatus  string   `json:"quotaStatus"`
		TopupBalance float64  `json:"topupBalance"`
		TopupTotal   float64  `json:"topupTotal"`
		Text         string   `json:"text"`
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
	return parseMuseQuota(r.Source, r.PercentUsed, r.ResetsAt, r.QuotaStatus, r.TopupBalance, r.TopupTotal, r.Text, time.Now()), nil
}

func parseMuseQuota(source string, pctUsed *float64, resetsAt float64, quotaStatus string, topupBal, topupTotal float64, domText string, now time.Time) []provider.QuotaReading {
	var used float64
	hasUsed := false
	var resetMs int64
	if source == "action" && pctUsed != nil {
		used = *pctUsed
		hasUsed = true
		if resetsAt >= 1e11 {
			resetMs = int64(resetsAt)
		} else if resetsAt > 0 {
			resetMs = int64(resetsAt * 1000)
		}
	} else if domText != "" {
		if m := museUsedPctRe.FindStringSubmatch(domText); m != nil {
			raw := m[1]
			if raw == "" {
				raw = m[2]
			}
			if v, err := strconv.ParseFloat(raw, 64); err == nil {
				used = v
				hasUsed = true
			}
		}
		if m := museResetCNRe.FindStringSubmatch(domText); m != nil {
			mo, _ := strconv.Atoi(m[1])
			d, _ := strconv.Atoi(m[2])
			resetMs = nextMonthDayMs(now, time.Month(mo), d)
		} else if m := museResetENRe.FindStringSubmatch(domText); m != nil {
			for _, layout := range []string{"Jan 2", "January 2"} {
				if t, err := time.Parse(layout, strings.TrimSpace(m[1])); err == nil {
					resetMs = nextMonthDayMs(now, t.Month(), t.Day())
					break
				}
			}
		}
	}
	if !hasUsed && topupTotal <= 0 {
		return nil
	}
	if used < 0 {
		used = 0
	}
	if used > 100 {
		used = 100
	}
	statusUpper := strings.ToUpper(strings.TrimSpace(quotaStatus))
	statusExhausted := statusUpper == "EXHAUSTED" || statusUpper == "DEPLETED" || statusUpper == "EXCEEDED" || statusUpper == "LIMITED"
	hasExtraCredit := topupTotal > 0 && topupBal > 0

	var out []provider.QuotaReading
	if hasUsed {
		rem := 100 - used
		exhausted := (statusExhausted || rem <= 0) && !hasExtraCredit
		// When weekly allowance is 100% used but non-expiring extra credits remain, omit the 0% weekly bucket so
		// core does not falsely block sends while extra credits are still available.
		if rem > 0 || !hasExtraCredit {
			out = append(out, provider.QuotaReading{
				Kind:       protocol.QuotaKindWindow,
				Window:     "week",
				Limit:      100,
				Remaining:  rem,
				ResetsAt:   resetMs,
				Exhausted:  exhausted,
				SourceNote: "muse:settings/usage",
			})
		}
	}
	if hasExtraCredit {
		rem := topupBal
		if rem > topupTotal {
			rem = topupTotal
		}
		out = append(out, provider.QuotaReading{
			Kind:       protocol.QuotaKindCredits,
			Limit:      topupTotal,
			Remaining:  rem,
			SourceNote: "muse:settings/usage",
		})
	}
	return out
}

func nextMonthDayMs(now time.Time, mo time.Month, day int) int64 {
	if mo < 1 || mo > 12 || day < 1 || day > 31 {
		return 0
	}
	t := time.Date(now.Year(), mo, day, 23, 59, 59, 0, now.Location())
	if t.Month() != mo || t.Day() != day {
		return 0
	}
	if !t.After(now) {
		t = time.Date(now.Year()+1, mo, day, 23, 59, 59, 0, now.Location())
		if t.Month() != mo || t.Day() != day {
			return 0
		}
	}
	return t.UnixMilli()
}
