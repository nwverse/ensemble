package browser

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// Muse (muse.ai): the main chat at muse.ai/ and its side chats (旁聊) at muse.ai/thread/<uuid>. Muse talks to its
// backend over an encrypted WebSocket, so everything is read from the rendered page:
//   - Messages are [data-message-item][data-message-id][data-message-role] rows in the chat log; each carries an
//     sr-only surrogate with its full text, after who said it. There are no per-message times (only date
//     separators in the log), so messages are ordered by position.
//   - The left panel lists the threads as [data-testid="hatch-thread-row"] rows: the main chat first, then the side
//     chats with a title and a relative date. A row has no href; its thread id is in the analytics metadata React
//     passes it (actionAnalyticsMetadata.thread_id, is_primary_thread). Rows are never clicked to learn an id: a
//     click navigates in-page and shows the previous thread's rows under the new URL for a while. A row whose id
//     cannot be read is left out of the list, not guessed.
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
// ("" for the main chat) — its URL and, for a side chat, the highlighted row.
const museThreadJS = `
const __museRowThread = (row) => {
  const fk = Object.keys(row).find(k => k.startsWith("__reactFiber"));
  let f = fk ? row[fk] : null;
  for (let d = 0; f && d < 40; d++, f = f.return) {
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
  return !!t && t.id === thread;
};
`

const museSignedInJS = `!!document.querySelector('[role="log"][data-hatch-chat-column]')`

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

// listSessions is the main chat (ExternalID "main", as before side chats were listed, so its synced messages keep
// their session) and the side chats the panel lists, read from one page load without clicking any row.
func (museSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	script := siteScript(a.site.AppURL, map[string]any{"max": museMaxThreads}, fmt.Sprintf(`
await page.waitForFunction(() => %s && !!document.querySelector('[data-testid="hatch-thread-row"]'), undefined, { timeout: 15000 }).catch(() => {});
return await page.evaluate((ARGS) => {
%s
%s
  if (!(%s)) return { error: "auth" };
  const seen = new Set();
  const threads = [];
  for (const row of document.querySelectorAll('[data-testid="hatch-thread-row"]')) {
    const t = __museRowThread(row);
    if (!t || t.primary || seen.has(t.id)) continue;
    seen.add(t.id);
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
`, museSignedInJS, sitePrelude, museThreadJS, museSignedInJS))
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
			title = a.site.Label + " · " + id[:8]
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

func (museSite) listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	thread, url, err := museThread(a, session)
	if err != nil {
		return provider.RemotePage{}, err
	}
	script := siteScript(url, map[string]any{"thread": thread, "limit": clampLimit(opts.Limit, 50)}, fmt.Sprintf(`
%s;
return await page.evaluate((ARGS) => {
%s
%s
%s
  if (!(%s)) return { error: "auth" };
  if (!__museOn(ARGS.thread)) return { error: "no thread" };
  return { messages: __fit(__museMessages(ARGS.limit), 40000) };
}, ARGS);
`, museWaitJS(15000), sitePrelude, museMessagesJS, museThreadJS, museSignedInJS))
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
// drawn. It notes the owner's messages already shown, empties the composer (Muse kept no draft when tried, but a
// leftover one must not go out with the text), types, and clicks send only when the composer holds exactly the
// text and the page is still on the thread. It is sent once a new owner message with the text reads back on the
// thread; a text still in the composer afterwards was not sent and is cleared.
func museSendJS(wait time.Duration) string {
	return fmt.Sprintf(`
const __read = () => page.evaluate((ARGS) => {
%s
%s
%s
  if (!(%s)) return { error: "auth" };
  if (!__museOn(ARGS.thread)) return { error: "no thread" };
  return { messages: __museMessages(30) };
}, ARGS);
const sleep = ms => new Promise(r => setTimeout(r, ms));
const ready = %s;
const before = await __read();
if (before.error) return { error: before.error };
if (!ready) return { error: "not ready" };
const seen = new Set(before.messages.filter(m => m.role === "user").map(m => m.id));
const sel = %q;
const visible = await page.waitForSelector("loc=css:" + sel, { state: "visible", timeout: 5000 }).then(() => true).catch(() => false);
if (!visible) return { error: "no composer" };
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
  return { error: "not sent" };
}
const still = await __read();
if (still.error) {
  await page.fill("loc=css:" + sel, "").catch(() => {});
  return { error: still.error };
}
const clicked = await page.click("loc=css:" + %q, { timeout: 3000 }).then(() => true).catch(() => false);
if (!clicked) await page.press("loc=css:" + sel, "Enter");
const norm = t => String(t || "").replace(/\s+/g, " ").trim();
const want = norm(ARGS.text);
const deadline = Date.now() + %d;
let hit = null;
while (!hit && Date.now() < deadline) {
  await sleep(1000);
  const now = await __read();
  if (now.error) continue;
  hit = now.messages.filter(m => m.role === "user" && !seen.has(m.id) && norm(m.text) === want).pop() || null;
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
const settled = (after.messages || []).filter(m => m.role === "user" && !seen.has(m.id) && norm(m.text) === want).pop();
return { sent: true, clicked, id: (settled || hit).id };
`, sitePrelude, museMessagesJS, museThreadJS, museSignedInJS, museWaitJS(15000), museComposerSel, museSendSel, wait.Milliseconds())
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
	script := siteScript(url, map[string]any{"thread": thread, "text": text}, museSendJS(18*time.Second))
	var r struct {
		Error string `json:"error"`
		Sent  bool   `json:"sent"`
		ID    string `json:"id"`
	}
	err = a.run(ctx, credential, script, &r)
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
