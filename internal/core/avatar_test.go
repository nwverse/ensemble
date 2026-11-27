package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
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

// dotWebp is a Dot's own picture as the page's canvas encodes it: a 4x4 WebP.
const dotWebp = "data:image/webp;base64,UklGRgACAABXRUJQVlA4WAoAAAAgAAAAAwAAAwAASUNDUMgBAAAAAAHIAAAAAAQwAABtbnRyUkdCIFhZWiAH4AABAAEAAAAAAABhY3NwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAA9tYAAQAAAADTLQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAlkZXNjAAAA8AAAACRyWFlaAAABFAAAABRnWFlaAAABKAAAABRiWFlaAAABPAAAABR3dHB0AAABUAAAABRyVFJDAAABZAAAAChnVFJDAAABZAAAAChiVFJDAAABZAAAAChjcHJ0AAABjAAAADxtbHVjAAAAAAAAAAEAAAAMZW5VUwAAAAgAAAAcAHMAUgBHAEJYWVogAAAAAAAAb6IAADj1AAADkFhZWiAAAAAAAABimQAAt4UAABjaWFlaIAAAAAAAACSgAAAPhAAAts9YWVogAAAAAAAA9tYAAQAAAADTLXBhcmEAAAAAAAQAAAACZmYAAPKnAAANWQAAE9AAAApbAAAAAAAAAABtbHVjAAAAAAAAAAEAAAAMZW5VUwAAACAAAAAcAEcAbwBvAGcAbABlACAASQBuAGMALgAgADIAMAAxADZWUDhMEQAAAC8DwAAAB1DVztSs/4GI6H8AAA=="

// What Persona is handed follows names.Avatar: an account's own picture is the one its main conversations share
// (agentsrc.status avatarUrl, club's addition), and each conversation's avatarUrl is its own picture, else the
// product's official icon, else nothing (Persona draws the title's first letter). Grok Bot gets its own app's artwork, never an icon of
// xAI's Grok, and a refused own picture is dropped.
func TestAvatarsHandedToPersona(t *testing.T) {
	cases := []struct {
		provider        string
		avatars         []string
		account, mainAv string
	}{
		{"grok_bot", []string{"https://images.example.com/bot_alpha.png"}, "https://images.example.com/bot_alpha.png", "https://images.example.com/bot_alpha.png"},
		{"grok_bot", []string{svgShape}, svgShape, svgShape},
		{"grok_bot", nil, "", names.Icon("grok_bot")},
		{"grok_bot", []string{"https://images.example.com/a.png", "https://images.example.com/b.png"}, "", "https://images.example.com/a.png"},
		{"grok_bot", []string{"http://images.example.com/plain.png"}, "", names.Icon("grok_bot")},
		{"claude_duet", nil, "", names.Icon("claude_duet")},
		{"cue", nil, "", "https://manus.im/apple-icon.png"},
		{"groq", nil, "", "https://groq.com/apple-touch-icon.png"},
		{"muse", nil, "", "https://muse.ai/images/pwa_icons/maskable/180.png?v=3"},
		{"dots", nil, "", names.Icon("dots")},
		// A Dot's own picture, as the page scaled it (a WebP data URL) or as a public address: it stands before the
		// dots icon, for the account and its main conversation. One past names' 64 KB is dropped for the icon.
		{"dots", []string{dotWebp}, dotWebp, dotWebp},
		{"dots", []string{"https://cdn.example.com/dot.png"}, "https://cdn.example.com/dot.png", "https://cdn.example.com/dot.png"},
		{"dots", []string{"data:image/webp;base64," + strings.Repeat("A", 64*1024)}, "", names.Icon("dots")},
		{"gemini_tasks", nil, "", "https://www.gstatic.com/images/branding/product/2x/gemini_96dp.png"},
		{"muse2api", nil, "", ""},
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
	if icon := names.Icon("grok_bot"); !strings.HasPrefix(icon, "https://is1-ssl.mzstatic.com/") {
		t.Errorf("Grok Bot's icon is not its own app's artwork: %s", icon)
	}
	if icon := names.Icon("claude_duet"); !strings.HasPrefix(icon, "https://is1-ssl.mzstatic.com/") {
		t.Errorf("Claude Duet's icon is not its own app's artwork: %s", icon)
	}
	if icon := names.Icon("dots"); !strings.HasPrefix(icon, "https://images.ctfassets.net/") {
		t.Errorf("ChatGPT dots' icon is not its own dot: %s", icon)
	}
}

// dotList is a ChatGPT Dots account whose one Dot lists the picture in *avatar, pending as *pending says.
type dotList struct {
	*fake
	avatar  *string
	pending *bool
}

func (d dotList) Name() string { return "dots" }
func (d dotList) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "dot", Title: "Dot", Kind: protocol.KindMain, CanSend: true,
		AvatarURL: *d.avatar, AvatarPending: *d.pending}}, nil
}

// A Dot's picture the page could not make this time (left out by the read's budget after a restart, or a failed
// fetch: AvatarPending) keeps the one the club has, for the account and on the Agents page; a Dot that no longer has
// one of its own (not pending) falls back to the dots icon.
func TestPendingDotAvatarIsKept(t *testing.T) {
	avatar, pending := dotWebp, false
	reg := provider.NewRegistry(dotList{fake: newFake(), avatar: &avatar, pending: &pending})
	club := New(Options{Registry: reg, FollowUps: []time.Duration{}, WatchLimit: -1})
	defer club.Close()
	ctx := context.Background()
	space, _ := club.Space(ctx, DefaultSpace)
	if _, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "dots", Credential: "fixture-credential"}); err != nil {
		t.Fatal(err)
	}
	check := func(step, want, wantAccount string) {
		t.Helper()
		space.SyncAll(ctx)
		st, _ := space.Status(ctx)
		list, _ := space.Sessions(ctx)
		if len(list.Sessions) != 1 || list.Sessions[0].AvatarURL != want || st.Accounts[0].AvatarURL != wantAccount {
			t.Errorf("%s: sessions %+v, account %.40q", step, list.Sessions, st.Accounts[0].AvatarURL)
		}
	}
	check("made", dotWebp, dotWebp)
	avatar, pending = "", true
	check("pending", dotWebp, dotWebp)
	avatar, pending = "", false
	check("gone", names.Icon("dots"), "")
}
