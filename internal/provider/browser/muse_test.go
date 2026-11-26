package browser

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

const museSideID = "146dc3a5-0099-4c72-be90-8b1c033752db"

func TestMuseSite(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mm-2", Backend: "ego"})
	d := siteDriver(t, map[string]any{
		"signedIn, title": map[string]any{"signedIn": true, "title": "Chat — Glint"},
		"threads.push": map[string]any{"threads": []map[string]any{
			{"id": museSideID, "title": "联系商家打电话", "when": "周日"},
			{"id": "664F4567-E43B-497D-A2D4-AA827E02A801", "title": "", "when": "11月8日"},
			{"id": "not-a-thread", "title": "bogus", "when": ""},
		}},
		"__museMessages(ARGS.limit)":     map[string]any{"messages": []map[string]any{{"id": "u1", "role": "user", "text": "ping"}, {"id": "assistant-msg-1", "role": "assistant", "text": "pong"}}},
		"const before = await __read();": map[string]any{"sent": true, "clicked": true, "id": "u2"},
	})
	a := New(SiteMuse, d)
	acct, err := a.Verify(ctx, cred)
	if err != nil || acct.ExternalAccountID != "muse:mm-2" || acct.DisplayName != "Muse · Glint" {
		t.Fatalf("Verify = %+v, err=%v", acct, err)
	}
	sessions, err := a.ListSessions(ctx, cred)
	if err != nil || len(sessions) != 3 {
		t.Fatalf("ListSessions = %+v, err=%v", sessions, err)
	}
	// The main chat keeps the identity it had before side chats were listed.
	if m := sessions[0]; m.ExternalID != "main" || m.Kind != protocol.KindMain || m.Ref["url"] != "https://muse.ai/" || !m.CanSend {
		t.Fatalf("main session = %+v", m)
	}
	side := sessions[1]
	if side.ExternalID != museSideID || side.Title != "联系商家打电话" || side.Kind != protocol.KindSide || side.ParentExternalID != "main" ||
		side.Ref["url"] != "https://muse.ai/thread/"+museSideID || !side.CanSend || side.LastActivityMs <= 0 {
		t.Fatalf("side session = %+v", side)
	}
	if s := sessions[2]; s.ExternalID != "664f4567-e43b-497d-a2d4-aa827e02a801" || s.Title != museSideNames([]string{s.ExternalID})[s.ExternalID] || !museSideTitleRe.MatchString(s.Title) {
		t.Fatalf("untitled side session = %+v", s)
	}
	if sc := d.gotReqs[1].Script; !strings.Contains(sc, "thread_id") || strings.Contains(sc, "page.click") {
		t.Fatal("the session list does not read thread ids from the rows without clicking them")
	}

	for _, s := range sessions[:2] {
		page, err := a.ListMessages(ctx, cred, s.SessionRef(), provider.ListOptions{})
		if err != nil || len(page.Messages) != 2 || page.Messages[0].Role != protocol.RoleUser || page.Messages[1].Role != protocol.RoleAssistant ||
			page.Messages[0].Position >= page.Messages[1].Position {
			t.Fatalf("ListMessages %s = %+v, err=%v", s.ExternalID, page, err)
		}
	}
	read := d.gotReqs[len(d.gotReqs)-1].Script
	if !strings.Contains(read, `await page.goto("https://muse.ai/thread/`+museSideID+`")`) || !strings.Contains(read, `"thread":"`+museSideID+`"`) ||
		!strings.Contains(read, `if (!__museOn(ARGS.thread)) return { error: "no thread" };`) {
		t.Fatal("a side chat is not read on its own thread")
	}

	res, err := a.Send(ctx, cred, side.SessionRef(), provider.Outgoing{Text: "hi"}, provider.SendContext{})
	if err != nil || res.Status != provider.Sent || res.ExternalID != "u2" {
		t.Fatalf("Send = %+v, err=%v", res, err)
	}
	send := d.gotReqs[len(d.gotReqs)-1].Script
	for _, want := range []string{
		`await page.goto("https://muse.ai/thread/` + museSideID + `")`,
		`if (!ready) return { error: "not ready", unsent: true };`,
		`await page.fill("loc=css:" + sel, "");`,
		`if ((await draft()) !== ARGS.text) {`,
		`const still = await __read();`,
	} {
		if !strings.Contains(send, want) {
			t.Fatalf("send script lacks %q", want)
		}
	}
	// Nothing is typed before the page shows the thread with its messages.
	if strings.Index(send, `if (!ready) return`) > strings.Index(send, `page.fill(`) {
		t.Fatal("the send types before the thread is ready")
	}
	for _, r := range d.gotReqs {
		if r.Slot != "pac-muse" {
			t.Fatalf("slot %q", r.Slot)
		}
	}

	// A session that is not Muse's is not found, and nothing runs for it.
	n := len(d.gotReqs)
	if _, err := a.ListMessages(ctx, cred, provider.SessionRef{ExternalID: "chan_1"}, provider.ListOptions{}); provider.CodeOf(err) != provider.CodeNotFound {
		t.Fatalf("ListMessages on a foreign session = %v, want NOT_FOUND", err)
	}
	if _, err := a.Send(ctx, cred, provider.SessionRef{ExternalID: "../x"}, provider.Outgoing{Text: "hi"}, provider.SendContext{}); provider.CodeOf(err) != provider.CodeNotFound {
		t.Fatalf("Send to a foreign session = %v, want NOT_FOUND", err)
	}
	if len(d.gotReqs) != n {
		t.Fatal("a foreign session reached the browser")
	}

	// Signed out (a landing page, no chat log): AUTH_FAILED. A send whose message never reads back: unconfirmed.
	out := siteDriver(t, map[string]any{
		"signedIn, title":                map[string]any{"signedIn": false, "title": "Muse — Your Personal AI Agent from Meta"},
		"threads.push":                   map[string]any{"error": "auth"},
		"const before = await __read();": map[string]any{"sent": false, "clicked": true},
	})
	if _, err := New(SiteMuse, out).Verify(ctx, cred); !isAuthFailed(err) {
		t.Fatalf("Verify signed out = %v, want AUTH_FAILED", err)
	}
	if _, err := New(SiteMuse, out).ListSessions(ctx, cred); !isAuthFailed(err) {
		t.Fatalf("ListSessions signed out = %v, want AUTH_FAILED", err)
	}
	res, err = New(SiteMuse, out).Send(ctx, cred, sessions[0].SessionRef(), provider.Outgoing{Text: "hi"}, provider.SendContext{})
	if err != nil || res.Status != provider.Unconfirmed {
		t.Fatalf("Send not read back = %+v, err=%v", res, err)
	}
	auth := siteDriver(t, map[string]any{"const before = await __read();": map[string]any{"error": "auth"}})
	if _, err := New(SiteMuse, auth).Send(ctx, cred, sessions[0].SessionRef(), provider.Outgoing{Text: "hi"}, provider.SendContext{}); !isAuthFailed(err) {
		t.Fatalf("Send signed out = %v, want AUTH_FAILED", err)
	}

	// A side chat that is gone (Muse redirects it to the main chat): NOT_FOUND for a read, nothing sent for a send.
	gone := siteDriver(t, map[string]any{
		"__museMessages(ARGS.limit)":     map[string]any{"error": "no thread"},
		"const before = await __read();": map[string]any{"error": "no thread"},
	})
	if _, err := New(SiteMuse, gone).ListMessages(ctx, cred, side.SessionRef(), provider.ListOptions{}); provider.CodeOf(err) != provider.CodeNotFound {
		t.Fatalf("ListMessages on a gone thread = %v, want NOT_FOUND", err)
	}
	gone = siteDriver(t, map[string]any{"const before = await __read();": map[string]any{"error": "no thread", "unsent": true}})
	res, err = New(SiteMuse, gone).Send(ctx, cred, side.SessionRef(), provider.Outgoing{Text: "hi"}, provider.SendContext{})
	if provider.CodeOf(err) != provider.CodeNotFound || !provider.IsRefusal(err) || res.Status == provider.Sent {
		t.Fatalf("Send to a gone thread = %+v, err=%v, want a NOT_FOUND refusal", res, err)
	}
}

// A send stopped before the click sent nothing: it is refused (the core records it failed), never unconfirmed. One
// stopped after the click stays unconfirmed.
func TestMuseSendOutcomes(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mm-2", Backend: "ego"})
	ref := provider.SessionRef{ExternalID: museSideID}
	for _, msg := range []string{"not ready", "no composer", "not sent"} {
		d := siteDriver(t, map[string]any{"const before = await __read();": map[string]any{"error": msg, "unsent": true}})
		res, err := New(SiteMuse, d).Send(ctx, cred, ref, provider.Outgoing{Text: "hi"}, provider.SendContext{})
		if !provider.IsRefusal(err) || provider.CodeOf(err) != provider.CodeRequestFailed || res.Status == provider.Unconfirmed {
			t.Fatalf("Send %q = %+v, err=%v, want a REQUEST_FAILED refusal", msg, res, err)
		}
	}
	d := siteDriver(t, map[string]any{"const before = await __read();": map[string]any{"error": "not sent", "clicked": true}})
	res, err := New(SiteMuse, d).Send(ctx, cred, ref, provider.Outgoing{Text: "hi"}, provider.SendContext{})
	if err != nil || res.Status != provider.Unconfirmed {
		t.Fatalf("Send not sent after the click = %+v, err=%v, want unconfirmed", res, err)
	}
	send := d.gotReqs[0].Script
	// Every stop before the click is marked unsent; none after it is.
	click := strings.Index(send, `page.click("loc=css:"`)
	if click < 0 || strings.Count(send[:click], "unsent: true") != 5 || strings.Contains(send[click:], "unsent") {
		t.Fatal("the send script does not mark exactly its stops before the click as unsent")
	}
	// A read-back counts only rows after the last one shown before the click.
	if !strings.Contains(send, `msgs.slice(last + 1)`) || strings.Contains(send, `!seen.has(`) {
		t.Fatal("the send reads back a message the page showed before, or one drawn above it")
	}
}

// The row's thread id is read without leaving the row, and an id more than one row resolves to is nobody's.
func TestMuseThreadRows(t *testing.T) {
	for _, want := range []string{`if (sn && sn !== row && sn instanceof Element) break;`, `return n === 1;`} {
		if !strings.Contains(museThreadJS, want) {
			t.Fatalf("museThreadJS lacks %q", want)
		}
	}
	d := siteDriver(t, map[string]any{"threads.push": map[string]any{"threads": []any{}}})
	if _, err := New(SiteMuse, d).ListSessions(context.Background(), EncodeCredential(Credential{MachineID: "mm-2", Backend: "ego"})); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(d.gotReqs[0].Script, `count.get(t.id) !== 1`) {
		t.Fatal("the session list keeps a thread id that more than one row resolves to")
	}
}

func TestMuseActivityMs(t *testing.T) {
	loc := time.FixedZone("PST", -8*3600)
	now := time.Date(2026, 11, 26, 10, 30, 0, 0, loc) // a Thursday
	endOf := func(y int, m time.Month, d int) int64 { return time.Date(y, m, d, 23, 59, 59, 0, loc).UnixMilli() }
	for label, want := range map[string]int64{
		"5m":           now.Add(-5 * time.Minute).UnixMilli(),
		"2h":           now.Add(-2 * time.Hour).UnixMilli(),
		"3 小时前":        now.Add(-3 * time.Hour).UnixMilli(),
		"刚刚":           now.UnixMilli(),
		"今天":           now.UnixMilli(),
		"昨天":           endOf(2026, 11, 25),
		"3d":           endOf(2026, 11, 23),
		"周日":           endOf(2026, 11, 22),
		"周四":           endOf(2026, 11, 19),
		"Mon":          endOf(2026, 11, 23),
		"11月8日":        endOf(2026, 11, 8),
		"9月29日 10:45":  endOf(2026, 9, 29),
		"12月30日":       endOf(2025, 12, 30),
		"2025年9月29日":   endOf(2025, 9, 29),
		"2025/1/2":     endOf(2025, 1, 2),
		"Nov 8":        endOf(2026, 11, 8),
		"Dec 30, 2025": endOf(2025, 12, 30),
		"9:13":         time.Date(2026, 11, 26, 9, 13, 0, 0, loc).UnixMilli(),
		"23:00":        time.Date(2026, 11, 25, 23, 0, 0, 0, loc).UnixMilli(),
		"":             0,
		"2月30日":        0,
		"whenever":     0,
	} {
		if got := museActivityMs(label, now); got != want {
			t.Errorf("museActivityMs(%q) = %v, want %v", label, time.UnixMilli(got).In(loc), time.UnixMilli(want).In(loc))
		}
	}
}

var museSideTitleRe = regexp.MustCompile(`^旁聊 · [a-z]{8,}$`)

// A side chat the panel gives no title is named after its own id, never after where it sits in the panel: when
// another chat is deleted, falls past the list's cap or gets a title, the rest keep their names. No name reads like
// an id fragment and no two are equal; a titled chat keeps the panel's title.
func TestMuseUntitledSideChats(t *testing.T) {
	cred := EncodeCredential(Credential{MachineID: "mm-2", Backend: "ego"})
	ids := make([]string, museMaxThreads+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("%08x-0000-4000-8000-%012x", 0xa0000000+i, i)
	}
	// list is the sessions for the panel's rows, the most recent first; titled says which ids have a title.
	list := func(rows []string, titled map[string]string) map[string]string {
		t.Helper()
		threads := []map[string]any{}
		for _, id := range rows {
			threads = append(threads, map[string]any{"id": id, "title": titled[id], "when": "周日"})
		}
		d := siteDriver(t, map[string]any{"threads.push": map[string]any{"threads": threads}})
		got, err := New(SiteMuse, d).ListSessions(context.Background(), cred)
		if err != nil {
			t.Fatal(err)
		}
		titles := map[string]string{}
		seen := map[string]string{}
		for _, s := range got[1:] {
			titles[s.ExternalID] = s.Title
			if other, ok := seen[s.Title]; ok {
				t.Errorf("%s and %s are both titled %q", other, s.ExternalID, s.Title)
			}
			seen[s.Title] = s.ExternalID
			if _, ok := titled[s.ExternalID]; ok {
				continue
			}
			if !museSideTitleRe.MatchString(s.Title) {
				t.Errorf("%s titled %q", s.ExternalID, s.Title)
			}
			for i := 0; i+4 <= len(s.ExternalID); i++ {
				if strings.Contains(s.Title, s.ExternalID[i:i+4]) {
					t.Errorf("%s titled %q, a fragment of its id", s.ExternalID, s.Title)
					break
				}
			}
		}
		return titles
	}
	// The panel lists museMaxThreads chats, one of them titled.
	before := list(ids[:museMaxThreads], map[string]string{ids[3]: "周末计划"})
	if len(before) != museMaxThreads || before[ids[3]] != "周末计划" {
		t.Fatalf("first list = %v", before)
	}
	keep := func(step string, after map[string]string, changed ...string) {
		t.Helper()
		for id, title := range after {
			if slices.Contains(changed, id) {
				continue
			}
			if was, ok := before[id]; ok && was != title {
				t.Errorf("%s: %s went from %q to %q", step, id, was, title)
			}
		}
	}
	// One deleted (gone from the panel).
	rows := slices.Delete(slices.Clone(ids[:museMaxThreads]), 5, 6)
	keep("delete", list(rows, map[string]string{ids[3]: "周末计划"}))
	// A new chat at the top pushes the oldest past the cap.
	rows = ids[len(ids)-1:]
	rows = append(rows, ids[:museMaxThreads-1]...)
	after := list(rows, map[string]string{ids[3]: "周末计划"})
	if _, ok := after[ids[museMaxThreads-1]]; ok {
		t.Fatal("the oldest chat is still listed past the cap")
	}
	keep("past the cap", after)
	// One untitled chat gets a title; the titled one loses its title.
	after = list(ids[:museMaxThreads], map[string]string{ids[7]: "新标题"})
	if after[ids[7]] != "新标题" || !museSideTitleRe.MatchString(after[ids[3]]) {
		t.Fatalf("titled/untitled = %q / %q", after[ids[7]], after[ids[3]])
	}
	keep("titled", after, ids[7], ids[3])
	// The same chat is named the same whatever else is listed, and in any order.
	if a, b := museSideNames([]string{ids[0], ids[1]}), museSideNames([]string{ids[1]}); a[ids[1]] != b[ids[1]] || a[ids[0]] == a[ids[1]] {
		t.Fatalf("names = %v / %v", a, b)
	}
}

// Two chats whose four-syllable words agree are both named longer, each from its own hash, so they differ; listed
// apart, each has the short name.
func TestMuseSideNamesTellClashesApart(t *testing.T) {
	const a, b = "00000416-0000-4000-8000-000000000416", "00001279-0000-4000-8000-000000001279"
	if x, y := museSideNames([]string{a})[a], museSideNames([]string{b})[b]; x != y || x != "旁聊 · lenonavu" {
		t.Fatalf("alone: %q, %q (the fixture no longer clashes)", x, y)
	}
	both := museSideNames([]string{b, a, ids0})
	if both[a] != "旁聊 · lenonavuso" || both[b] != "旁聊 · lenonavuko" || both[ids0] != museSideNames([]string{ids0})[ids0] {
		t.Fatalf("together: %v", both)
	}
}

const ids0 = "a0000000-0000-4000-8000-000000000000"

// Cloudflare's "Verify you are human" in front of muse.ai is "human check", a signed-out landing page "auth", and a
// signed-in page with its chat log neither, as the page scripts see them.
func TestMusePageTellsHumanCheckApart(t *testing.T) {
	expr := `(` + museSignedInJS + `) ? "signed in" : ` + museSignedOutJS
	for _, tc := range []struct {
		name string
		page map[string]any
		want string
	}{
		{"interstitial form", map[string]any{"url": "https://muse.ai/", "selectors": []string{"#challenge-form"}}, `"human check"`},
		{"turnstile frame", map[string]any{"url": "https://muse.ai/thread/" + museSideID, "selectors": []string{`iframe[src*="challenges.cloudflare.com"]`}}, `"human check"`},
		{"challenge script alone", map[string]any{"url": "https://muse.ai/", "selectors": []string{`script[src*="/cdn-cgi/challenge-platform/"]`}}, `"auth"`},
		{"signed-out landing", map[string]any{"url": "https://muse.ai/", "title": "Just a moment...", "text": "Verify you are human"}, `"auth"`},
		{"signed in with a frame", map[string]any{"url": "https://muse.ai/", "selectors": []string{`[role="log"][data-hatch-chat-column]`, `iframe[src*="challenges.cloudflare.com"]`}}, `"signed in"`},
	} {
		if got := runPageJS(t, challengeJS, tc.page, expr); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	for name, script := range map[string]string{"list": "threads.push", "messages": "__museMessages(ARGS.limit), 40000", "send": "const before = await __read();"} {
		d := siteDriver(t, map[string]any{"threads.push": map[string]any{"threads": []any{}}, "__museMessages(ARGS.limit), 40000": map[string]any{}, "const before = await __read();": map[string]any{"error": "unsent", "unsent": true}, "signedIn, title": map[string]any{"signedIn": true}})
		a := New(SiteMuse, d)
		cred := EncodeCredential(Credential{MachineID: "mm-2", Backend: "ego"})
		switch name {
		case "list":
			_, _ = a.ListSessions(context.Background(), cred)
		case "messages":
			_, _ = a.ListMessages(context.Background(), cred, provider.SessionRef{ExternalID: "main"}, provider.ListOptions{})
		default:
			_, _ = a.Send(context.Background(), cred, provider.SessionRef{ExternalID: "main"}, provider.Outgoing{Text: "hi"}, provider.SendContext{})
		}
		sc := d.gotReqs[0].Script
		if !strings.Contains(sc, script) || !strings.Contains(sc, "const __challenged") || !strings.Contains(sc, "return { error: "+museSignedOutJS+" }") {
			t.Errorf("the %s script does not tell a human check from signed out", name)
		}
	}
}

// Muse's "human check" reaches the owner as HUMAN_CHECK_REQUIRED (problems.go's wording), signed out as AUTH_FAILED,
// at pairing, sync and send, through the same mapper as Dots and Gemini; a send it stopped sent nothing.
func TestMuseHumanCheckIsItsOwnCode(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mm-2", Backend: "ego"})
	for _, tc := range []struct{ msg, code string }{{"human check", provider.CodeHumanCheck}, {"auth", provider.CodeAuthFailed}} {
		d := siteDriver(t, map[string]any{
			"signedIn, title":                map[string]any{"signedIn": false, "challenged": tc.msg == "human check"},
			"threads.push":                   map[string]any{"error": tc.msg},
			"__museMessages(ARGS.limit), 4":  map[string]any{"error": tc.msg},
			"const before = await __read();": map[string]any{"error": tc.msg, "unsent": true},
		})
		a := New(SiteMuse, d)
		if _, err := a.Verify(ctx, cred); provider.CodeOf(err) != tc.code {
			t.Errorf("verify %q: %v, want %s", tc.msg, err, tc.code)
		}
		if _, err := a.ListSessions(ctx, cred); provider.CodeOf(err) != tc.code {
			t.Errorf("list %q: %v, want %s", tc.msg, err, tc.code)
		}
		if _, err := a.ListMessages(ctx, cred, provider.SessionRef{ExternalID: museSideID}, provider.ListOptions{}); provider.CodeOf(err) != tc.code {
			t.Errorf("messages %q: %v, want %s", tc.msg, err, tc.code)
		}
		res, err := a.Send(ctx, cred, provider.SessionRef{ExternalID: museSideID}, provider.Outgoing{Text: "hi"}, provider.SendContext{})
		if provider.CodeOf(err) != tc.code || !provider.IsRefusal(err) || res.Status != "" {
			t.Errorf("send %q: %+v, %v, want a %s refusal", tc.msg, res, err, tc.code)
		}
	}
}
