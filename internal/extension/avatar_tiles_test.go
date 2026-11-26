package extension

import (
	"context"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/personal-agent-club/internal/names"
)

// Each service's avatar names its own tile for an app of UI version 6 and later, which the page serves itself through
// ui.image: a PNG the hub takes, built into the program, so serving one needs no hub and no network (avatarTile is
// called below with none). The letters stay as the fallback; apps of UI version 3 to 5 get them alone, as before.
func TestAvatarTilesPerService(t *testing.T) {
	h, club := startServices(t)
	pairFive(t, club)
	for _, version := range []int{0, 6, 11} {
		for _, ui := range []string{HomeUI, AccountsUI} {
			for _, st := range []pageState{{}, {Page: pageAdd}} {
				refs := map[string]string{}
				for _, s := range renderVersion(t, h, ui, version, st).Sections {
					for _, c := range s.Items {
						if c.Avatar == nil {
							continue
						}
						p := strings.TrimPrefix(strings.TrimPrefix(c.Key, "pair-"), "acct-")
						if c.Avatar.Ref == "" || c.Avatar.Text == "" || c.Avatar.URL != "" {
							t.Errorf("%s %+v, UI version %d: row %s avatar %+v", ui, st, version, c.Key, c.Avatar)
							continue
						}
						if other, dup := refs[c.Avatar.Ref]; dup && other != p {
							t.Errorf("%s: %s and %s both draw %s", ui, other, p, c.Avatar.Ref)
						}
						refs[c.Avatar.Ref] = p
						var img wire.UIImage
						if err := h.Call("ui.image", wire.UIImageParams{Contribution: ui, Ref: c.Avatar.Ref, Width: 320}, &img); err != nil {
							t.Fatalf("ui.image %s: %v", c.Avatar.Ref, err)
						}
						checked, err := wire.CheckImage(&img)
						if err != nil || checked.Mime != "image/png" || checked.Width != 128 || *img.MaxAge != wire.ImageMaxAgeMost {
							t.Errorf("ui.image %s: %+v %v", c.Avatar.Ref, checked, err)
						}
					}
				}
				if len(refs) < 5 {
					t.Errorf("%s %+v, UI version %d: %d tiles", ui, st, version, len(refs))
				}
			}
		}
	}
	for _, version := range []int{3, 4, 5} {
		for _, s := range renderVersion(t, h, AccountsUI, version, pageState{Page: pageAdd}).Sections {
			for _, c := range s.Items {
				if c.Avatar != nil && (c.Avatar.Ref != "" || c.Avatar.Text == "") {
					t.Errorf("UI version %d: row %s avatar %+v", version, c.Key, c.Avatar)
				}
			}
		}
	}
	var img wire.UIImage
	if err := h.Call("ui.image", wire.UIImageParams{Contribution: HomeUI, Ref: "tile/../../persona-extension.json"}, &img); err == nil {
		t.Errorf("ui.image answered a ref it never drew")
	}
	offline := &ext.ImageRequest{UIImageParams: wire.UIImageParams{Contribution: AccountsUI, Ref: names.TileRef("zh-Hans", "dots")}}
	if got, err := avatarTile(context.Background(), offline); err != nil || got.Data == "" {
		t.Errorf("avatarTile without a hub: %v", err)
	}
}
