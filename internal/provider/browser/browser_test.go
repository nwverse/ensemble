package browser

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

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

// genericSite is a site without its own scripts: it runs the generic DOM ones.
var genericSite = Site{
	Provider:       "generic",
	Label:          "Generic",
	AppURL:         "https://agent.example/",
	Slot:           "pac-generic",
	SingleSession:  true,
	ComposerSel:    "textarea",
	SendButtonSel:  "button.send",
	MessageLogSel:  "[data-message-id]",
	SignedOutHosts: []string{"login"},
}

func TestGenericSiteVerifyListReadSend(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-2", Backend: "ego"})
	fd := &fakeDriver{
		name: "ego",
		answer: func(req RunRequest) (RunResult, error) {
			switch {
			case strings.Contains(req.Script, "signedOut"):
				return RunResult{Backend: "ego", Output: "[notice] ok\n" + `{"url":"https://agent.example/","title":"Agent","signedOut":false,"composer":true}`}, nil
			case strings.Contains(req.Script, "messages: msgs"):
				return RunResult{Backend: "ego", Output: `{"messages":[{"id":"m1","role":"user","text":"ping"},{"id":"m2","role":"assistant","text":"pong"}]}`}, nil
			case strings.Contains(req.Script, "sent: true"):
				return RunResult{Backend: "ego", Output: `{"sent":true}`}, nil
			default:
				return RunResult{Backend: "ego", Output: `{}`}, nil
			}
		},
	}
	a := New(genericSite, fd)
	if a.Name() != "generic" || a.Connection() != protocol.ConnectionBrowser {
		t.Fatalf("unexpected adapter metadata: %s %s", a.Name(), a.Connection())
	}
	acct, err := a.Verify(ctx, cred)
	if err != nil || acct.Alias != "Agent" || acct.ExternalAccountID != "generic:mac-2" {
		t.Fatalf("Verify = %+v, err=%v", acct, err)
	}
	sessions, err := a.ListSessions(ctx, cred)
	if err != nil || len(sessions) != 1 || sessions[0].ExternalID != "main" {
		t.Fatalf("ListSessions = %+v, err=%v", sessions, err)
	}
	page, err := a.ListMessages(ctx, cred, sessions[0].SessionRef(), provider.ListOptions{Limit: 10})
	if err != nil || len(page.Messages) != 2 || page.Messages[1].Blocks[0].Text != "pong" {
		t.Fatalf("ListMessages = %+v, err=%v", page, err)
	}
	sendRes, err := a.Send(ctx, cred, sessions[0].SessionRef(), provider.Outgoing{ClientID: "c1", Text: "hello"}, provider.SendContext{})
	if err != nil || sendRes.Status != provider.Sent {
		t.Fatalf("Send = %+v, err=%v", sendRes, err)
	}
	if len(fd.gotReqs) != 3 || fd.gotReqs[0].Slot != "pac-generic" || fd.gotReqs[0].Backend != "ego" {
		t.Fatalf("driver requests = %+v", fd.gotReqs)
	}

	// A signed-out redirect, and a landing page that does not redirect (no composer), are not a signed-in account.
	for _, out := range []string{
		`{"url":"https://agent.example/login","title":"Agent","signedOut":true,"composer":true}`,
		`{"value":{"url":"https://agent.example/","title":"Welcome","signedOut":false,"composer":false}}`,
		`{"value":{"url":"https://agent.example/","title":"Welcome","signedOut":false}}`,
	} {
		out := out
		d := &fakeDriver{name: "ego", answer: func(RunRequest) (RunResult, error) { return RunResult{Backend: "ego", Output: out}, nil }}
		if _, err := New(genericSite, d).Verify(ctx, cred); !isAuthFailed(err) {
			t.Fatalf("Verify on %s = %v, want AUTH_FAILED", out, err)
		}
	}
}

// siteDriver answers each of a site's scripts by the first marker it contains.
func siteDriver(t *testing.T, answers map[string]any) *fakeDriver {
	return &fakeDriver{name: "ego", answer: func(req RunRequest) (RunResult, error) {
		for marker, v := range answers {
			if strings.Contains(req.Script, marker) {
				return hubOutput(t, v), nil
			}
		}
		t.Fatalf("no answer for script:\n%s", req.Script)
		return RunResult{}, nil
	}}
}

func TestClaudeDuetSite(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-2", Backend: "ego"})
	d := siteDriver(t, map[string]any{
		"/api/account": map[string]any{"uuid": "acct-1", "name": "Rowan"},
		"channels: chans": map[string]any{"channels": []map[string]any{
			{"id": "chan_old", "title": "old", "last": "2026-11-20T00:00:00Z"},
			{"id": "chan_new", "title": "new", "last": "2026-11-25T00:00:00Z"},
		}},
		"__timeline(ARGS.chan, ARGS.limit)": map[string]any{"messages": []map[string]any{
			{"id": "cmsg_1", "role": "user", "text": "ping", "created_at": "2026-11-25T00:00:00Z"},
			{"id": "cmsg_2", "role": "assistant", "text": "pong", "created_at": "2026-11-25T00:00:05Z"},
		}},
		"since: startedAt": map[string]any{"sent": true, "clicked": true, "id": "cmsg_3"},
	})
	a := New(SiteClaudeDuet, d)
	acct, err := a.Verify(ctx, cred)
	if err != nil || acct.ExternalAccountID != "claude_duet:mac-2" || acct.Alias != "Rowan" {
		t.Fatalf("Verify = %+v, err=%v", acct, err)
	}
	sessions, err := a.ListSessions(ctx, cred)
	if err != nil || len(sessions) != 2 || sessions[1].Kind != protocol.KindMain || sessions[0].Kind != protocol.KindSide ||
		sessions[1].Ref["url"] != "https://claude.ai/duet/chan_new" {
		t.Fatalf("ListSessions = %+v, err=%v", sessions, err)
	}
	page, err := a.ListMessages(ctx, cred, sessions[1].SessionRef(), provider.ListOptions{Limit: 500})
	if err != nil || len(page.Messages) != 2 || page.Messages[1].CreateTimeMs == 0 || page.Messages[1].Role != protocol.RoleAssistant {
		t.Fatalf("ListMessages = %+v, err=%v", page, err)
	}
	if !strings.Contains(d.gotReqs[len(d.gotReqs)-1].Script, `"limit":50`) {
		t.Fatal("the read limit is not clamped to 50")
	}
	res, err := a.Send(ctx, cred, sessions[1].SessionRef(), provider.Outgoing{Text: "hi"}, provider.SendContext{})
	if err != nil || res.Status != provider.Sent || res.ExternalID != "cmsg_3" {
		t.Fatalf("Send = %+v, err=%v", res, err)
	}
	// A session that is not a channel is not found; a refused API is AUTH_FAILED.
	if _, err := a.ListMessages(ctx, cred, provider.SessionRef{ExternalID: "main"}, provider.ListOptions{}); err == nil {
		t.Fatal("ListMessages on a non-channel session succeeded")
	}
	auth := siteDriver(t, map[string]any{"/api/account": map[string]any{"error": "auth"}})
	if _, err := New(SiteClaudeDuet, auth).Verify(ctx, cred); !isAuthFailed(err) {
		t.Fatalf("Verify signed out = %v, want AUTH_FAILED", err)
	}
}

func TestLiveEgoSmoke(t *testing.T) {
	if os.Getenv("PAC_LIVE_EGO") != "1" {
		t.Skip("set PAC_LIVE_EGO=1 to run live ego-browser smoke test")
	}
	ctx := context.Background()
	drv := &LocalEgoDriver{}
	cred := EncodeCredential(Credential{MachineID: "local", Backend: "ego"})
	sites := []Site{SiteMuse, SiteClaudeDuet, SiteGeminiTasks, SiteDots}
	if only := os.Getenv("PAC_LIVE_EGO_SITES"); only != "" {
		sites = sites[:0]
		for _, s := range []Site{SiteMuse, SiteClaudeDuet, SiteGeminiTasks, SiteDots} {
			if strings.Contains(","+only+",", ","+s.Provider+",") {
				sites = append(sites, s)
			}
		}
	}
	for _, site := range sites {
		a := New(site, drv)
		acct, err := a.Verify(ctx, cred)
		if err != nil {
			t.Fatalf("%s Verify failed: %v", site.Provider, err)
		}
		t.Logf("%s verified: %+v", site.Provider, acct)
		sessions, err := a.ListSessions(ctx, cred)
		if err != nil || len(sessions) == 0 {
			t.Fatalf("%s ListSessions = %d sessions, err=%v", site.Provider, len(sessions), err)
		}
		main := sessions[0]
		for _, s := range sessions {
			if s.Kind == protocol.KindMain {
				main = s
			}
		}
		// PAC_LIVE_EGO_SESSION=<title> reads (and sends to) that session instead of the main one.
		if want := os.Getenv("PAC_LIVE_EGO_SESSION"); want != "" {
			for _, s := range sessions {
				if s.Title == want || s.ExternalID == want {
					main = s
				}
			}
		}
		for _, s := range sessions {
			t.Logf("%s session %s %q kind=%s last=%d", site.Provider, s.ExternalID, s.Title, s.Kind, s.LastActivityMs)
		}
		t.Logf("%s: %d sessions, main %q (%s)", site.Provider, len(sessions), main.Title, main.ExternalID)
		page, err := a.ListMessages(ctx, cred, main.SessionRef(), provider.ListOptions{Limit: 10})
		if err != nil {
			t.Fatalf("%s ListMessages failed: %v", site.Provider, err)
		}
		t.Logf("%s: %d messages in %q", site.Provider, len(page.Messages), main.Title)
		// PAC_LIVE_EGO_SEND=<text> also sends it to that session and waits for a reply that reads back.
		if text := os.Getenv("PAC_LIVE_EGO_SEND"); text != "" {
			res, err := a.Send(ctx, cred, main.SessionRef(), provider.Outgoing{Text: text}, provider.SendContext{})
			if err != nil || res.Status != provider.Sent || res.ExternalID == "" {
				t.Fatalf("%s Send = %+v, err=%v", site.Provider, res, err)
			}
			t.Logf("%s: sent %s", site.Provider, res.ExternalID)
			var reply string
			for i := 0; i < 12 && reply == ""; i++ {
				time.Sleep(10 * time.Second)
				p, err := a.ListMessages(ctx, cred, main.SessionRef(), provider.ListOptions{Limit: 10})
				if err != nil {
					t.Logf("%s ListMessages after send: %v", site.Provider, err)
					continue
				}
				after := false
				for _, m := range p.Messages {
					if m.ExternalID == res.ExternalID {
						after = true
						continue
					}
					if after && m.Role == protocol.RoleAssistant && len(m.Blocks) > 0 {
						reply = m.Blocks[0].Text
					}
				}
			}
			if reply == "" {
				t.Fatalf("%s: no reply read back after %s", site.Provider, res.ExternalID)
			}
			if len(reply) > 200 {
				reply = reply[:200]
			}
			t.Logf("%s: reply %q", site.Provider, reply)
		}
	}
}

func isAuthFailed(err error) bool {
	var pe *provider.Error
	return errors.As(err, &pe) && pe.Code == provider.CodeAuthFailed
}
