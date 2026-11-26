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
