package extension

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// withPicture is a provider whose one conversation carries avatar, as a roster would list it.
type withPicture struct {
	named
	avatar string
}

func (w withPicture) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "main", Title: "Main", Kind: protocol.KindMain, CanSend: true, AvatarURL: w.avatar}}, nil
}

func pngDataURL(t *testing.T) string {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 4, 4))); err != nil {
		t.Fatal(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
}

const ownPicture = "https://images.example.com/bot_alpha.png"

// startPictured pairs Grok Bot with a bot picture at an https address, Muse with one given as PNG data, Gemini with
// an SVG shape (no https address, and the hub takes no SVG from ui.image), and Claude Duet and ChatGPT Dots with none.
func startPictured(t *testing.T) (*exttest.Host, map[string]protocol.Account) {
	t.Helper()
	pngData := pngDataURL(t)
	reg := provider.NewRegistry(withPicture{named{grokBot}, ownPicture}, withPicture{named{"muse"}, pngData},
		withPicture{named{"gemini_tasks"}, "data:image/svg+xml;base64,PHN2Zy8+"}, named{"claude_duet"}, named{"dots"},
		named{"cue"}, named{"groq"})
	club := core.New(core.Options{Registry: reg, FollowUps: []time.Duration{}, WatchLimit: -1})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)
	for _, p := range []string{grokBot, "muse", "gemini_tasks", "claude_duet", "dots"} {
		if _, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: p, Credential: "fixture-credential-" + p}); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := space.Status(ctx)
	byID := map[string]protocol.Account{}
	for _, a := range st.Accounts {
		byID[a.AccountID] = a
	}
	return h, byID
}

// wantAvatar is the avatar names.Avatar's order gives an app of UI version: own picture, product icon, tile, letters.
func wantAvatar(version int, provider string, acct *protocol.Account) *wire.Avatar {
	if version > 0 && version < 3 {
		return nil
	}
	own := ""
	if acct != nil {
		own = acct.AvatarURL
	}
	p := names.Avatar("zh-Hans", provider, own)
	refs := version == 0 || version >= 6
	av := &wire.Avatar{Text: p.Text}
	switch {
	case strings.HasPrefix(p.Own, "https://"):
		av.URL = p.Own
	case refs && strings.HasPrefix(p.Own, "data:image/png"):
		av.Ref = "own/" + acct.AccountID
	case p.Icon != "":
		av.URL = p.Icon
	case refs:
		av.Ref = p.Ref
	}
	return av
}

func sameAvatar(a, b *wire.Avatar) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// Every picture the pages draw takes the resolver's order (TestAvatarOrderPerService): the account's own picture,
// else its product's official icon, else its tile, and the letters alone before UI version 6; the letters ride along
// with every picture, for while it loads and when it fails. That holds for the account rows of the home and of 账号,
// the add rows, and an account's page from both entries (which on 账号 still starts with the 服务 row, no header).
func TestAvatarsFollowTheResolver(t *testing.T) {
	h, accts := startPictured(t)
	check := func(where string, version int, c wire.Component, provider string, acct *protocol.Account) {
		t.Helper()
		want := wantAvatar(version, provider, acct)
		if !sameAvatar(c.Avatar, want) {
			t.Errorf("%s, UI version %d: %s avatar %+v, want %+v", where, version, provider, c.Avatar, want)
		}
		if c.Avatar != nil && c.Avatar.Text == "" {
			t.Errorf("%s: %s avatar without letters", where, provider)
		}
		if c.Avatar != nil && (strings.Contains(c.Avatar.URL, "x.ai") || strings.Contains(c.Avatar.URL, "grok.com")) {
			t.Errorf("%s: %s draws xAI's Grok: %s", where, provider, c.Avatar.URL)
		}
	}
	for _, version := range []int{0, 2, 3, 5, 6, 11} {
		for _, at := range []string{HomeUI, AccountsUI} {
			seen := 0
			for _, s := range renderVersion(t, h, at, version, pageState{}).Sections {
				for _, c := range s.Items {
					if id, ok := strings.CutPrefix(c.Key, "acct-"); ok {
						a := accts[id]
						check(at, version, c, a.Provider, &a)
						seen++
					}
				}
			}
			if seen != len(accts) {
				t.Errorf("%s, UI version %d: %d account rows", at, version, seen)
			}
			adds := 0
			for _, s := range renderVersion(t, h, at, version, pageState{Page: pageAdd}).Sections {
				for _, c := range s.Items {
					if p, ok := strings.CutPrefix(c.Key, "pair-"); ok {
						check(at+" add", version, c, p, nil)
						adds++
					}
				}
			}
			if adds < 6 {
				t.Errorf("%s, UI version %d: %d add rows", at, version, adds)
			}
		}
		for id, a := range accts {
			home := renderVersion(t, h, HomeUI, version, pageState{Page: pageAccount, Account: id})
			head := home.Sections[0].Items[0]
			check("account page (home)", version, head, a.Provider, &a)
			page := renderVersion(t, h, AccountsUI, version, pageState{Page: pageAccount, Account: id})
			first := page.Sections[0].Items[0]
			if first.Type != "row" || first.Title != "服务" {
				t.Errorf("账号, UI version %d: the account page starts with %+v", version, first)
			}
			check("account page (账号)", version, first, a.Provider, &a)
			if version >= 4 || version == 0 {
				if head.Type != "header" || !sameAvatar(head.Avatar, first.Avatar) {
					t.Errorf("UI version %d: the home's header %+v, 账号's row %+v", version, head, first)
				}
			}
		}
	}
}

// What the pages name by ref is served without any network: an own picture given as PNG data (decoded from the
// last sync) and the tiles; a ref never drawn, or an own picture that is no PNG, JPEG, WebP or GIF, is refused.
func TestPicturesServedByRef(t *testing.T) {
	h, accts := startPictured(t)
	refs := map[string]bool{}
	for _, st := range []pageState{{}, {Page: pageAdd}} {
		for _, s := range renderVersion(t, h, AccountsUI, 11, st).Sections {
			for _, c := range s.Items {
				if c.Avatar != nil && c.Avatar.Ref != "" {
					refs[c.Avatar.Ref] = true
				}
			}
		}
	}
	own := 0
	for ref := range refs {
		var img wire.UIImage
		if err := h.Call("ui.image", wire.UIImageParams{Contribution: AccountsUI, Ref: ref, Width: 320}, &img); err != nil {
			t.Fatalf("ui.image %s: %v", ref, err)
		}
		checked, err := wire.CheckImage(&img)
		if err != nil || checked.Mime != "image/png" {
			t.Errorf("ui.image %s: %+v %v", ref, checked, err)
		}
		if strings.HasPrefix(ref, ownRefPrefix) {
			own++
			if *img.MaxAge > 60 {
				t.Errorf("an own picture is kept %d s", *img.MaxAge)
			}
		}
	}
	// One own picture and at least one tile (the demo account's; every product with an icon is drawn by url).
	if own != 1 || len(refs) < 2 {
		t.Errorf("refs drawn: %v", refs)
	}
	for id, a := range accts {
		if a.Provider == "gemini_tasks" {
			var img wire.UIImage
			if err := h.Call("ui.image", wire.UIImageParams{Contribution: AccountsUI, Ref: ownRef(id)}, &img); err == nil {
				t.Errorf("an SVG own picture was served")
			}
		}
	}
	var img wire.UIImage
	for _, ref := range []string{"tile/../../persona-extension.json", "own/nobody"} {
		if err := h.Call("ui.image", wire.UIImageParams{Contribution: HomeUI, Ref: ref}, &img); err == nil {
			t.Errorf("ui.image answered %s", ref)
		}
	}
	offline := &ext.ImageRequest{UIImageParams: wire.UIImageParams{Contribution: AccountsUI, Ref: names.TileRef("zh-Hans", "dots")}}
	if got, err := avatarTile(context.Background(), offline); err != nil || got.Data == "" {
		t.Errorf("avatarTile without a hub: %v", err)
	}
}

// dotWebp is a Dot's own picture as the page's canvas encodes it (browser.dotsAvatarJS): a 4x4 WebP data URL.
const dotWebp = "data:image/webp;base64,UklGRgACAABXRUJQVlA4WAoAAAAgAAAAAwAAAwAASUNDUMgBAAAAAAHIAAAAAAQwAABtbnRyUkdCIFhZWiAH4AABAAEAAAAAAABhY3NwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAQAA9tYAAQAAAADTLQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAlkZXNjAAAA8AAAACRyWFlaAAABFAAAABRnWFlaAAABKAAAABRiWFlaAAABPAAAABR3dHB0AAABUAAAABRyVFJDAAABZAAAAChnVFJDAAABZAAAAChiVFJDAAABZAAAAChjcHJ0AAABjAAAADxtbHVjAAAAAAAAAAEAAAAMZW5VUwAAAAgAAAAcAHMAUgBHAEJYWVogAAAAAAAAb6IAADj1AAADkFhZWiAAAAAAAABimQAAt4UAABjaWFlaIAAAAAAAACSgAAAPhAAAts9YWVogAAAAAAAA9tYAAQAAAADTLXBhcmEAAAAAAAQAAAACZmYAAPKnAAANWQAAE9AAAApbAAAAAAAAAABtbHVjAAAAAAAAAAEAAAAMZW5VUwAAACAAAAAcAEcAbwBvAGcAbABlACAASQBuAGMALgAgADIAMAAxADZWUDhMEQAAAC8DwAAAB1DVztSs/4GI6H8AAA=="

// A ChatGPT Dot's own picture is drawn on the pages in the dots icon's place: given as WebP data (the page scaled it,
// its address being signed and short-lived), the list and the account's page name it by ref and ui.image serves the
// WebP; given at a public address, they draw the address. A Dot without one, or with one past 64 KB, keeps the dots
// icon. Every view passes wire.CheckView (renderVersion).
func TestDotOwnPictureOnThePages(t *testing.T) {
	for _, tc := range []struct {
		name, own string
		ref       bool   // drawn by ref (own/<id>), served as WebP
		url       string // else drawn from this address
	}{
		{"scaled", dotWebp, true, ""},
		{"public", "https://cdn.example.com/dot.png", false, "https://cdn.example.com/dot.png"},
		{"none", "", false, names.Icon("dots")},
		{"oversized", "data:image/webp;base64," + strings.Repeat("A", 64*1024), false, names.Icon("dots")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			club := core.New(core.Options{Registry: provider.NewRegistry(withPicture{named{"dots"}, tc.own}), FollowUps: []time.Duration{}, WatchLimit: -1})
			defer club.Close()
			h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
			ctx := context.Background()
			space, _ := club.Space(ctx, core.DefaultSpace)
			acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "dots", Credential: "fixture-credential-dots"})
			if err != nil {
				t.Fatal(err)
			}
			id := acct.AccountID
			check := func(where string, av *wire.Avatar) {
				t.Helper()
				if av == nil || av.Text == "" {
					t.Fatalf("%s: avatar %+v", where, av)
				}
				if tc.ref && (av.Ref != ownRef(id) || av.URL != "") || !tc.ref && (av.URL != tc.url || av.Ref != "") {
					t.Errorf("%s: avatar %+v", where, av)
				}
			}
			rows := 0
			for _, s := range renderVersion(t, h, AccountsUI, 11, pageState{}).Sections {
				for _, c := range s.Items {
					if c.Key == "acct-"+id {
						check("list", c.Avatar)
						rows++
					}
				}
			}
			if rows != 1 {
				t.Fatalf("%d rows for the Dot's account", rows)
			}
			check("account page", renderVersion(t, h, AccountsUI, 11, pageState{Page: pageAccount, Account: id}).Sections[0].Items[0].Avatar)
			var img wire.UIImage
			err = h.Call("ui.image", wire.UIImageParams{Contribution: AccountsUI, Ref: ownRef(id)}, &img)
			if !tc.ref {
				if err == nil {
					t.Errorf("ui.image served an own picture the Dot has not")
				}
				return
			}
			if err != nil {
				t.Fatalf("ui.image: %v", err)
			}
			checked, err := wire.CheckImage(&img)
			if err != nil || checked.Mime != "image/webp" {
				t.Errorf("ui.image: %+v %v", checked, err)
			}
		})
	}
}
