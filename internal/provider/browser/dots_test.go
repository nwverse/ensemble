package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// hubOutput is a browser.run answer as Persona's node gives it: the script's return value under "value".
func hubOutput(t *testing.T, value any) RunResult {
	t.Helper()
	b, err := json.Marshal(map[string]any{"value": value, "url": "https://chatgpt.com/", "title": "ChatGPT",
		"console": "[notice] something printed"})
	if err != nil {
		t.Fatal(err)
	}
	return RunResult{Backend: "ego", Output: string(b)}
}

func TestScriptValueReadsHubAndPrintedResults(t *testing.T) {
	for _, tc := range []struct{ name, output, want string }{
		{"hub value", `{"value":{"sent":true},"url":"u","title":"t"}`, `{"sent":true}`},
		{"hub console fallback", `{"value":null,"console":"[notice]\n{\"sent\":true}"}`, `{"sent":true}`},
		{"printed line", "[notice] ok\n{\"sent\":true}\n", `{"sent":true}`},
		{"nothing", `{"value":null,"url":"u"}`, `{"value":null,"url":"u"}`},
	} {
		if got := scriptValue(tc.output); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestDotsReadsThroughTheHubFormat(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})
	const dot, room = "4f33~6abe", "d029"
	fd := &fakeDriver{name: "ego", answer: func(req RunRequest) (RunResult, error) {
		switch {
		case strings.Contains(req.Script, "/backend-api/messaging/rooms/"):
			return hubOutput(t, map[string]any{"messages": []map[string]any{
				{"id": "m1", "created_at": "2026-11-26T04:01:54.197762Z", "owner": true, "text": "af1有吗"},
				{"id": "m2", "created_at": "2026-11-26T04:01:58.239156Z", "owner": false, "text": "我查一下"},
				{"id": "m3", "created_at": "2026-11-26T04:02:00Z", "owner": false, "text": "  "},
			}}), nil
		case strings.Contains(req.Script, "return { userId"):
			return hubOutput(t, map[string]any{"userId": "user-X", "accountId": "acc-1", "name": "Ada", "dots": 1}), nil
		case strings.Contains(req.Script, "/backend-api/tbo?"):
			return hubOutput(t, map[string]any{"items": []map[string]any{
				{"id": dot, "display_name": "", "messaging_room_id": room, "status": "active", "updated_at": "2026-11-26T04:01:54.680727Z"},
				{"id": "gone", "messaging_room_id": "r2", "status": "deleted"},
			}}), nil
		}
		return RunResult{}, errors.New("unexpected script")
	}}
	a := New(SiteDots, fd)

	acct, err := a.Verify(ctx, cred)
	if err != nil || acct.ExternalAccountID != "dots:user-X:acc-1" || acct.Alias != "Ada" {
		t.Fatalf("Verify = %+v, %v", acct, err)
	}
	ss, err := a.ListSessions(ctx, cred)
	if err != nil || len(ss) != 1 {
		t.Fatalf("ListSessions = %+v, %v", ss, err)
	}
	s := ss[0]
	if s.ExternalID != dot || s.Title != "Dot" || s.Kind != protocol.KindMain || !s.CanSend || s.Ref["room"] != room ||
		s.Ref["url"] != "https://chatgpt.com/dots/"+dot || s.LastActivityMs == 0 {
		t.Fatalf("session = %+v", s)
	}
	pg, err := a.ListMessages(ctx, cred, s.SessionRef(), provider.ListOptions{Limit: 100})
	if err != nil || len(pg.Messages) != 2 {
		t.Fatalf("ListMessages = %+v, %v", pg, err)
	}
	if m := pg.Messages[0]; m.Role != protocol.RoleUser || m.ExternalID != "m1" || m.CreateTimeMs != 1795665714197 || m.Position != m.CreateTimeMs {
		t.Fatalf("first = %+v", m)
	}
	if m := pg.Messages[1]; m.Role != protocol.RoleAssistant || m.Blocks[0].Text != "我查一下" {
		t.Fatalf("second = %+v", m)
	}
	// The room's endpoint answers 422 above 32.
	last := fd.gotReqs[len(fd.gotReqs)-1].Script
	if !strings.Contains(last, `"limit":32`) || !strings.Contains(last, `"room":"d029"`) {
		t.Fatalf("messages script args: %s", last[len(last)-200:])
	}
}

func TestDotsVerifyRefusesSignedOutAndDotless(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1"})
	for _, tc := range []struct {
		value any
		code  string
	}{
		{map[string]any{"error": "auth"}, provider.CodeAuthFailed},
		{map[string]any{"userId": "user-X", "dots": 0}, provider.CodeNotFound},
		{map[string]any{"error": "http 429"}, provider.CodeRateLimited},
	} {
		a := New(SiteDots, &fakeDriver{answer: func(RunRequest) (RunResult, error) { return hubOutput(t, tc.value), nil }})
		if _, err := a.Verify(ctx, cred); provider.CodeOf(err) != tc.code {
			t.Errorf("%v: %v, want %s", tc.value, err, tc.code)
		}
	}
}

func TestDotsSend(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1"})
	ref := provider.SessionRef{ExternalID: "4f33~6abe", Ref: map[string]string{"room": "d029"}}
	for _, tc := range []struct {
		value  any
		status provider.SendStatus
		id     string
		code   string
	}{
		{map[string]any{"sent": true, "id": "m9"}, provider.Sent, "m9", ""},
		{map[string]any{"sent": false, "clicked": true}, provider.Unconfirmed, "", ""},
		{map[string]any{"error": "auth"}, "", "", provider.CodeAuthFailed},
	} {
		fd := &fakeDriver{answer: func(RunRequest) (RunResult, error) { return hubOutput(t, tc.value), nil }}
		res, err := New(SiteDots, fd).Send(ctx, cred, ref, provider.Outgoing{Text: `hi "there"`}, provider.SendContext{})
		if tc.code != "" {
			if provider.CodeOf(err) != tc.code {
				t.Errorf("%v: %v", tc.value, err)
			}
			continue
		}
		if err != nil || res.Status != tc.status || res.ExternalID != tc.id {
			t.Errorf("%v: %+v, %v", tc.value, res, err)
		}
		if sc := fd.gotReqs[0].Script; !strings.Contains(sc, `https://chatgpt.com/dots/4f33~6abe`) || !strings.Contains(sc, `"text":"hi \"there\""`) {
			t.Errorf("script does not target the Dot with the text")
		}
	}
	// A driver failure after the page may have taken the text is unconfirmed, never an error that retries.
	fd := &fakeDriver{answer: func(RunRequest) (RunResult, error) { return RunResult{Error: "timeout"}, nil }}
	if res, err := New(SiteDots, fd).Send(ctx, cred, ref, provider.Outgoing{Text: "x"}, provider.SendContext{}); err != nil || res.Status != provider.Unconfirmed {
		t.Fatalf("%+v, %v", res, err)
	}
}

// dotWebp is a 4x4 WebP as the page's canvas encodes it (data URL).
const dotWebp = "data:image/webp;base64,UklGRgACAABXRUJQVlA4WAoAAAAgAAAAAwAAAwAASUNDUMgBAAAAAAHIAAAAAAQwAABtbnRyUkdCIFhZWiAH4AABAAEAAAAAAABhY3NwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAA9tYAAQAAAADTLQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAlkZXNjAAAA8AAAACRyWFlaAAABFAAAABRnWFlaAAABKAAAABRiWFlaAAABPAAAABR3dHB0AAABUAAAABRyVFJDAAABZAAAAChnVFJDAAABZAAAAChiVFJDAAABZAAAAChjcHJ0AAABjAAAADxtbHVjAAAAAAAAAAEAAAAMZW5VUwAAAAgAAAAcAHMAUgBHAEJYWVogAAAAAAAAb6IAADj1AAADkFhZWiAAAAAAAABimQAAt4UAABjaWFlaIAAAAAAAACSgAAAPhAAAts9YWVogAAAAAAAA9tYAAQAAAADTLXBhcmEAAAAAAAQAAAACZmYAAPKnAAANWQAAE9AAAApbAAAAAAAAAABtbHVjAAAAAAAAAAEAAAAMZW5VUwAAACAAAAAcAEcAbwBvAGcAbABlACAASQBuAGMALgAgADIAMAAxADZWUDhMEQAAAC8DwAAAB1DVztSs/4GI6H8AAA=="

// A Dot's own picture (tbo's avatar_url, a signed address only the signed-in page loads) reaches its session: an
// https address that needs nothing as the page gave it, a picture the page scaled to a data URL, kept by its key so
// the next sync's page skips it; none, an oversized one (past names' 64 KB) or a plain http one leaves the session
// without, so it shows the dots icon. One the page named but could not make is pending (core.keptAvatars). What
// the page itself does (fetching, scaling, its budget) was checked against chatgpt.com; here the page is a fake.
func TestDotsOwnAvatars(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1"})
	const key = "sediment://file_00000000d0a8"
	big := "data:image/webp;base64," + strings.Repeat("A", 64*1024)
	item := func(id, avatar, key string) map[string]any {
		return map[string]any{"id": id, "messaging_room_id": "r-" + id, "status": "active", "avatar": avatar, "avatar_key": key}
	}
	first := true
	fd := &fakeDriver{answer: func(req RunRequest) (RunResult, error) {
		scaled := dotWebp
		if !first {
			scaled = "" // the page skips a key the Adapter named
		}
		first = false
		return hubOutput(t, map[string]any{"items": []map[string]any{
			item("pet", scaled, key),
			item("public", "https://cdn.example.com/dot.png", ""),
			item("plain", "", ""),
			item("big", big, "sediment://file_big"),
			item("http", "http://cdn.example.com/dot.png", ""),
			item("lost", "", "sediment://file_never_made"),
		}}), nil
	}}
	a := New(SiteDots, fd)
	want := map[string]string{"pet": dotWebp, "public": "https://cdn.example.com/dot.png", "plain": "", "big": "", "http": "", "lost": ""}
	for round := 0; round < 2; round++ {
		ss, err := a.ListSessions(ctx, cred)
		if err != nil || len(ss) != len(want) {
			t.Fatalf("round %d: ListSessions = %+v, %v", round, ss, err)
		}
		for _, s := range ss {
			if s.AvatarURL != want[s.ExternalID] {
				t.Errorf("round %d: %s avatar %.60q, want %.60q", round, s.ExternalID, s.AvatarURL, want[s.ExternalID])
			}
			// Only a Dot whose picture the page named but did not make is pending: the club keeps the one it has.
			if s.AvatarPending != (s.ExternalID == "lost") {
				t.Errorf("round %d: %s pending %v", round, s.ExternalID, s.AvatarPending)
			}
		}
	}
	if len(fd.gotReqs) != 2 {
		t.Fatalf("%d runs", len(fd.gotReqs))
	}
	if s := fd.gotReqs[0].Script; !strings.Contains(s, `"have":[]`) || !strings.Contains(s, `"max":16384`) ||
		!strings.Contains(s, "__dotAvatar(") || !strings.Contains(s, "OffscreenCanvas") {
		t.Errorf("the first list does not make the pictures")
	}
	if s := fd.gotReqs[1].Script; !strings.Contains(s, `"have":["`+key+`"]`) {
		t.Errorf("the second list does not name the picture kept: %s", s[len(s)-200:])
	}
}

func TestPictureCacheDropsTheOldest(t *testing.T) {
	c := newPictureCache(2)
	c.put("a", "1")
	c.put("b", "2")
	c.put("a", "1'")
	c.put("c", "3")
	if _, ok := c.get("a"); ok {
		t.Errorf("a outlived two newer keys")
	}
	if v, _ := c.get("c"); v != "3" || strings.Join(c.keys(), ",") != "b,c" {
		t.Errorf("keys %v", c.keys())
	}
	c.put("", "x")
	c.put("d", "")
	if len(c.keys()) != 2 {
		t.Errorf("an empty key or picture was kept: %v", c.keys())
	}
}
