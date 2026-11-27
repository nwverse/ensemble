package browser

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

func TestGeminiTasksReadsThroughTheHubFormat(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})
	fd := &fakeDriver{name: "ego", answer: func(req RunRequest) (RunResult, error) {
		switch {
		case strings.Contains(req.Script, `"checkRunning":true`):
			return hubOutput(t, map[string]any{"messages": []map[string]any{
				{"id": "r_1:u", "at": 1795681464152, "owner": true, "text": "查一下今天旧金山的天气"},
				{"id": "rc_1", "at": 1795681464153, "owner": false, "text": "今天多云，最高 18°C。"},
				{"id": "r_2:u", "at": 1795681500000, "owner": true, "text": " "},
			}}), nil
		case strings.Contains(req.Script, "return { email"):
			return hubOutput(t, map[string]any{"email": "Ada@Example.com", "name": "Ada", "tasks": 0}), nil
		case strings.Contains(req.Script, "return { main"):
			return hubOutput(t, map[string]any{"main": "c0ffee", "chats": []map[string]any{
				{"id": "c0ffee", "title": "kept", "at": 1795681000000},
				{"id": "aa11", "title": "", "at": 1795681100000},
			}, "tasks": []map[string]any{
				{"id": "8857", "title": "旧金山实时天气查询", "at": 1795681477385, "status": "ready"},
				{"id": "9001", "title": "", "at": 1795681400000, "status": "running"},
				{"id": "8857", "title": "dup", "at": 1, "status": "waiting"},
			}}), nil
		}
		return RunResult{}, errors.New("unexpected script")
	}}
	a := New(SiteGeminiTasks, fd)

	acct, err := a.Verify(ctx, cred)
	if err != nil || acct.ExternalAccountID != "gemini:ada@example.com" || acct.Alias != "Ada" {
		t.Fatalf("Verify = %+v, %v", acct, err)
	}
	if !strings.Contains(fd.gotReqs[0].Script, `startsWith("/sorry")`) {
		t.Fatalf("verify script does not tell Google's unusual-traffic page from signed out")
	}
	if !strings.Contains(fd.gotReqs[0].Script, openJS("https://gemini.google.com/app", `^https://gemini\.google\.com/`)) {
		t.Fatalf("a read moves a page already on Gemini (it would pull a waiting send's task away)")
	}
	if !strings.Contains(fd.gotReqs[0].Script, `"MaZiqc"`) || !strings.Contains(fd.gotReqs[0].Script, "https://gemini.google.com/app") {
		t.Fatalf("verify script does not read the tasks on gemini.google.com")
	}
	ss, err := a.ListSessions(ctx, cred)
	if err != nil || len(ss) != 4 {
		t.Fatalf("ListSessions = %+v, %v", ss, err)
	}
	// The main chat (its kept conversation not listed again as a side chat), the other conversations as its side
	// chats, then the tasks.
	if m := ss[0]; m.ExternalID != "main" || m.Kind != protocol.KindMain || !m.CanSend || m.Ref["url"] != "https://gemini.google.com/app/c0ffee" || m.LastActivityMs != 1795681000000 {
		t.Fatalf("main = %+v", m)
	}
	if s := ss[1]; s.ExternalID != "aa11" || s.Kind != protocol.KindSide || s.ParentExternalID != "main" || s.Title != "Gemini 对话" || !s.CanSend {
		t.Fatalf("side = %+v", s)
	}
	ss = ss[2:]
	if s := ss[0]; s.ExternalID != "8857" || s.Title != "旧金山实时天气查询" || s.Kind != protocol.KindTask || !s.CanSend ||
		s.Ref["url"] != "https://gemini.google.com/app/8857" || s.Ref["status"] != "ready" || s.LastActivityMs != 1795681477385 {
		t.Fatalf("session = %+v", s)
	}
	if ss[1].Title != "Gemini 任务" || ss[1].Ref["status"] != "running" {
		t.Fatalf("untitled running task = %+v", ss[1])
	}
	pg, err := a.ListMessages(ctx, cred, ss[0].SessionRef(), provider.ListOptions{Limit: 100})
	if err != nil || len(pg.Messages) != 2 {
		t.Fatalf("ListMessages = %+v, %v", pg, err)
	}
	if m := pg.Messages[0]; m.Role != protocol.RoleUser || m.ExternalID != "r_1:u" || m.CreateTimeMs != 1795681464152 || m.Position != m.CreateTimeMs {
		t.Fatalf("first = %+v", m)
	}
	if m := pg.Messages[1]; m.Role != protocol.RoleAssistant || m.ExternalID != "rc_1" || m.Blocks[0].Text != "今天多云，最高 18°C。" {
		t.Fatalf("second = %+v", m)
	}
	last := fd.gotReqs[len(fd.gotReqs)-1].Script
	if !strings.Contains(last, `"hNvQHb"`) || !strings.Contains(last, `"task":"8857"`) || !strings.Contains(last, `"turns":20`) {
		t.Fatalf("messages script args: %s", last[len(last)-200:])
	}
}

func TestGeminiTasksVerifyRefusesSignedOut(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1"})
	for _, tc := range []struct {
		value any
		code  string
	}{
		{map[string]any{"error": "auth"}, provider.CodeAuthFailed},
		{map[string]any{"email": ""}, provider.CodeAuthFailed},
		{map[string]any{"error": "http 429"}, provider.CodeRateLimited},
		{map[string]any{"error": "human check"}, provider.CodeHumanCheck},
	} {
		a := New(SiteGeminiTasks, &fakeDriver{answer: func(RunRequest) (RunResult, error) { return hubOutput(t, tc.value), nil }})
		if _, err := a.Verify(ctx, cred); provider.CodeOf(err) != tc.code {
			t.Errorf("%v: %v, want %s", tc.value, err, tc.code)
		}
	}
	a := New(SiteGeminiTasks, &fakeDriver{answer: func(RunRequest) (RunResult, error) {
		return hubOutput(t, map[string]any{"error": "no task"}), nil
	}})
	if _, err := a.ListMessages(ctx, cred, provider.SessionRef{ExternalID: "gone"}, provider.ListOptions{}); provider.CodeOf(err) != provider.CodeNotFound {
		t.Errorf("a gone task: %v", err)
	}
}

func TestGeminiTasksSend(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1"})
	ref := provider.SessionRef{ExternalID: "8857"}
	for _, tc := range []struct {
		value  any
		status provider.SendStatus
		id     string
		code   string
	}{
		{map[string]any{"sent": true, "clicked": true}, provider.Sent, "", ""},
		{map[string]any{"sent": false, "clicked": true}, provider.Unconfirmed, "", ""},
		{map[string]any{"error": "auth"}, "", "", provider.CodeAuthFailed},
		// A rate limit, or Google's human check, before anything was typed: refused, nothing went out.
		{map[string]any{"error": "human check"}, "", "", provider.CodeHumanCheck},
		{map[string]any{"error": "http 429"}, "", "", provider.CodeRateLimited},
		// Still in the composer after the wait: it did not go, so it is refused and can be sent again.
		{map[string]any{"error": "not sent", "clicked": true}, "", "", provider.CodeUpstreamError},
		{map[string]any{"error": "no task"}, "", "", provider.CodeNotFound},
	} {
		fd := &fakeDriver{answer: func(RunRequest) (RunResult, error) { return hubOutput(t, tc.value), nil }}
		res, err := New(SiteGeminiTasks, fd).Send(ctx, cred, ref, provider.Outgoing{Text: `hi "there"`}, provider.SendContext{})
		if tc.code != "" {
			if provider.CodeOf(err) != tc.code {
				t.Errorf("%v: %v", tc.value, err)
			}
			continue
		}
		if err != nil || res.Status != tc.status || res.ExternalID != tc.id {
			t.Errorf("%v: %+v, %v", tc.value, res, err)
		}
		if sc := fd.gotReqs[0].Script; !strings.Contains(sc, `"id":"8857"`) || !strings.Contains(sc, `"text":"hi \"there\""`) ||
			!strings.Contains(sc, `document.execCommand("delete")`) || !strings.Contains(sc, `if (!ready)`) {
			t.Errorf("script does not clear the draft and type the text into the task")
		}
	}
	fd := &fakeDriver{answer: func(RunRequest) (RunResult, error) { return RunResult{Error: "timeout"}, nil }}
	if res, err := New(SiteGeminiTasks, fd).Send(ctx, cred, ref, provider.Outgoing{Text: "x"}, provider.SendContext{}); err != nil || res.Status != provider.Unconfirmed {
		t.Fatalf("%+v, %v", res, err)
	}
}

func TestGeminiTasksRunsInItsOwnSlot(t *testing.T) {
	d := &fakeDriver{name: "ego", answer: func(RunRequest) (RunResult, error) {
		return RunResult{Backend: "ego", Output: `{"value":{"tasks":[{"id":"g1234","title":"Trip plan","status":"ready"}]}}`}, nil
	}}
	ss, err := New(SiteGeminiTasks, d).ListSessions(context.Background(), EncodeCredential(Credential{MachineID: "mac-2", Backend: "ego"}))
	if err != nil || len(ss) != 2 || ss[0].ExternalID != "main" || ss[1].ExternalID != "g1234" || d.gotReqs[0].Slot != "pac-gemini" {
		t.Fatalf("ListSessions = %+v, %v (slot %q)", ss, err, d.gotReqs[0].Slot)
	}
}
