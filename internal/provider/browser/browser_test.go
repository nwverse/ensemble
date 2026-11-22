package browser

import (
	"context"
	"strings"
	"testing"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

type fakeDriver struct {
	name     string
	machines []Machine
	gotReqs  []RunRequest
	answer   func(RunRequest) (RunResult, error)
}

func (f *fakeDriver) Name() string                                { return f.name }
func (f *fakeDriver) Machines(context.Context) ([]Machine, error) { return f.machines, nil }
func (f *fakeDriver) Run(_ context.Context, req RunRequest) (RunResult, error) {
	f.gotReqs = append(f.gotReqs, req)
	return f.answer(req)
}

func TestBrowserAdapterVerifyListReadSend(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mm-2", Backend: "ego"})

	// 1. Muse (single-session) + Verify + ListMessages + Send.
	fd := &fakeDriver{
		name: "ego",
		answer: func(req RunRequest) (RunResult, error) {
			switch {
			case strings.Contains(req.Script, "signedOut"):
				return RunResult{Backend: "ego", Output: "[notice] ok\n" + `{"url":"https://muse.ai/","title":"Chat — Glint","signedOut":false}`}, nil
			case strings.Contains(req.Script, "messages: msgs"):
				return RunResult{Backend: "ego", Output: `{"messages":[{"id":"m1","role":"user","text":"ping"},{"id":"m2","role":"assistant","text":"pong"}]}`}, nil
			case strings.Contains(req.Script, "sent: true"):
				return RunResult{Backend: "ego", Output: `{"sent":true}`}, nil
			default:
				return RunResult{Backend: "ego", Output: `{}`}, nil
			}
		},
	}
	muse := New(SiteMuse, fd)
	if muse.Name() != "muse" || muse.Connection() != protocol.ConnectionBrowser {
		t.Fatalf("unexpected adapter metadata: %s %s", muse.Name(), muse.Connection())
	}
	acct, err := muse.Verify(ctx, cred)
	if err != nil || acct.DisplayName != "Chat — Glint" || acct.ExternalAccountID != "muse:mm-2" {
		t.Fatalf("Verify = %+v, err=%v", acct, err)
	}
	sessions, err := muse.ListSessions(ctx, cred)
	if err != nil || len(sessions) != 1 || sessions[0].ExternalID != "main" {
		t.Fatalf("ListSessions = %+v, err=%v", sessions, err)
	}
	page, err := muse.ListMessages(ctx, cred, sessions[0].SessionRef(), provider.ListOptions{Limit: 10})
	if err != nil || len(page.Messages) != 2 || page.Messages[1].Blocks[0].Text != "pong" {
		t.Fatalf("ListMessages = %+v, err=%v", page, err)
	}
	sendRes, err := muse.Send(ctx, cred, sessions[0].SessionRef(), provider.Outgoing{ClientID: "c1", Text: "hello"}, provider.SendContext{})
	if err != nil || sendRes.Status != provider.Sent {
		t.Fatalf("Send = %+v, err=%v", sendRes, err)
	}
	if len(fd.gotReqs) != 3 || fd.gotReqs[0].Slot != "pac-muse" || fd.gotReqs[0].Backend != "ego" {
		t.Fatalf("driver requests = %+v", fd.gotReqs)
	}

	// 2. Signed-out detection returns CodeAuthFailed.
	signedOut := &fakeDriver{
		name: "cdp",
		answer: func(RunRequest) (RunResult, error) {
			return RunResult{Backend: "cdp", Output: `{"url":"https://claude.ai/login","title":"Claude","signedOut":true}`}, nil
		},
	}
	duet := New(SiteClaudeDuet, signedOut)
	if _, err := duet.Verify(ctx, EncodeCredential(Credential{MachineID: "cdp-box", Backend: "cdp"})); err == nil {
		t.Fatal("expected auth error when signed out")
	}

	// 3. Gemini Tasks preset uses its own slot and multi-session listing.
	geminiDriver := &fakeDriver{
		name: "ego",
		answer: func(RunRequest) (RunResult, error) {
			return RunResult{Backend: "ego", Output: `{"sessions":[{"id":"g1234","title":"Trip plan","url":"https://gemini.google.com/app/g1234"}]}`}, nil
		},
	}
	gemini := New(SiteGeminiTasks, geminiDriver)
	gSessions, err := gemini.ListSessions(ctx, cred)
	if err != nil || len(gSessions) != 1 || gSessions[0].ExternalID != "g1234" {
		t.Fatalf("Gemini ListSessions = %+v, err=%v", gSessions, err)
	}
}
