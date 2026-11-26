package browser

import (
	"context"
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
	if s := sessions[2]; s.ExternalID != "664f4567-e43b-497d-a2d4-aa827e02a801" || s.Title != "Muse · 664f4567" {
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
		`if (!ready) return { error: "not ready" };`,
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
	res, err = New(SiteMuse, gone).Send(ctx, cred, side.SessionRef(), provider.Outgoing{Text: "hi"}, provider.SendContext{})
	if err != nil || res.Status == provider.Sent || res.Error != provider.CodeNotFound {
		t.Fatalf("Send to a gone thread = %+v, err=%v", res, err)
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
