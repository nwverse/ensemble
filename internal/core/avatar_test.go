package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/names"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// pictured is a provider under a real id whose conversations carry the pictures in avatars, one main conversation
// each (none when the list is empty).
type pictured struct {
	*fake
	id      string
	avatars []string
}

func (p pictured) Name() string { return p.id }
func (p pictured) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	out := []provider.RemoteSession{{ExternalID: "task:1", Title: "Task", Kind: protocol.KindTask, AvatarURL: "https://images.example.com/task.png"}}
	if len(p.avatars) == 0 {
		return append(out, provider.RemoteSession{ExternalID: "main", Title: "Main", Kind: protocol.KindMain}), nil
	}
	for i, a := range p.avatars {
		out = append(out, provider.RemoteSession{ExternalID: "bot" + string(rune('a'+i)), Title: "Bot", Kind: protocol.KindMain, AvatarURL: a})
	}
	return out, nil
}

const svgShape = "data:image/svg+xml;base64,PHN2Zy8+"

// What Persona is handed follows names.Avatar: an account's own picture is the one its main conversations share
// (agentsrc.status avatarUrl, club's addition), and each conversation's avatarUrl is its own picture, else the
// product's official icon, else nothing (Persona draws the title's first letter). Grok Bot never gets an icon of
// xAI's Grok, and a refused own picture is dropped.
func TestAvatarsHandedToPersona(t *testing.T) {
	cases := []struct {
		provider        string
		avatars         []string
		account, mainAv string
	}{
		{"grok_bot", []string{"https://images.example.com/bot_alpha.png"}, "https://images.example.com/bot_alpha.png", "https://images.example.com/bot_alpha.png"},
		{"grok_bot", []string{svgShape}, svgShape, svgShape},
		{"grok_bot", nil, "", ""},
		{"grok_bot", []string{"https://images.example.com/a.png", "https://images.example.com/b.png"}, "", "https://images.example.com/a.png"},
		{"grok_bot", []string{"http://images.example.com/plain.png"}, "", ""},
		{"claude_duet", nil, "", "https://claude.ai/apple-touch-icon.png"},
		{"cue", nil, "", "https://manus.im/apple-icon.png"},
		{"groq", nil, "", "https://groq.com/apple-touch-icon.png"},
		{"muse", nil, "", "https://muse.ai/favicon.ico"},
		{"dots", nil, "", "https://chatgpt.com/cdn/assets/favicon-180x180.9e0103ce69.png"},
		{"gemini_tasks", nil, "", "https://www.gstatic.com/images/branding/product/2x/gemini_96dp.png"},
		{"echo", nil, "", ""},
	}
	for _, c := range cases {
		reg := provider.NewRegistry(pictured{fake: newFake(), id: c.provider, avatars: c.avatars})
		club := New(Options{Registry: reg, FollowUps: []time.Duration{}, WatchLimit: -1})
		ctx := context.Background()
		space, _ := club.Space(ctx, DefaultSpace)
		if _, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: c.provider, Credential: "fixture-credential"}); err != nil {
			t.Fatal(err)
		}
		space.SyncAll(ctx)
		st, _ := space.Status(ctx)
		if got := st.Accounts[0].AvatarURL; got != c.account {
			t.Errorf("%s %v: account avatar %q, want %q", c.provider, c.avatars, got, c.account)
		}
		list, _ := space.Sessions(ctx)
		for _, s := range list.Sessions {
			if strings.Contains(s.AvatarURL, "x.ai") || strings.Contains(s.AvatarURL, "grok.com") {
				t.Errorf("%s: a conversation shows %s", c.provider, s.AvatarURL)
			}
			switch s.Kind {
			case protocol.KindTask:
				if s.AvatarURL != "https://images.example.com/task.png" {
					t.Errorf("%s: a task's own picture became %q", c.provider, s.AvatarURL)
				}
			case protocol.KindMain:
				if s.AvatarURL != c.mainAv && !(len(c.avatars) > 1 && s.AvatarURL == c.avatars[1]) {
					t.Errorf("%s %v: conversation avatar %q, want %q", c.provider, c.avatars, s.AvatarURL, c.mainAv)
				}
			}
		}
		club.Close()
	}
	if names.Icon("grok_bot") != "" {
		t.Errorf("Grok Bot has an icon: %s", names.Icon("grok_bot"))
	}
}
