package provider

import "testing"

func TestCanonicalRepo(t *testing.T) {
	for in, want := range map[string]string{
		"nwverse/openlinkedin":                        "https://github.com/nwverse/openlinkedin",
		" https://github.com/nwverse/openbilibili ":   "https://github.com/nwverse/openbilibili",
		"https://github.com/nwverse/openbilibili.git": "https://github.com/nwverse/openbilibili",
		"https://github.com/nwverse/ensemble/":        "https://github.com/nwverse/ensemble",
		"git@github.com:nwverse/persona.git":          "https://github.com/nwverse/persona",
		"github.com/a-b/c.d_e":                        "https://github.com/a-b/c.d_e",
		"ssh://git@github.com/o/r":                    "https://github.com/o/r",
	} {
		if got, ok := CanonicalRepo(in); !ok || got != want {
			t.Errorf("CanonicalRepo(%q) = %q %v, want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "openlinkedin", "https://gitlab.com/o/r", "https://github.com/o", "https://github.com/o/r/tree/main",
		"o/r/x", "../r", "o/..", "javascript:alert(1)", "https://github.com.evil.test/o/r", "o/r?x=1"} {
		if got, ok := CanonicalRepo(in); ok {
			t.Errorf("CanonicalRepo(%q) = %q, want refused", in, got)
		}
	}
	if !SameRepo("NWVerse/OpenLinkedIn", "https://github.com/nwverse/openlinkedin.git") || SameRepo("o/a", "o/b") {
		t.Error("SameRepo")
	}
}

func TestMountVerifiedNeedsEverythingBeforeAndAsked(t *testing.T) {
	m := MountResult{Before: []string{"https://github.com/o/a"}, After: []string{"https://github.com/o/a", "https://github.com/o/b"}}
	if !m.Verified([]string{"o/b"}) {
		t.Error("before kept and b added: verified")
	}
	if m.Verified([]string{"o/c"}) {
		t.Error("c missing: not verified")
	}
	dropped := MountResult{Before: []string{"https://github.com/o/a"}, After: []string{"https://github.com/o/b"}}
	if dropped.Verified([]string{"o/b"}) {
		t.Error("a repository taken out must not verify")
	}
}

func TestInventoryWorkspaceByIDNameOrMain(t *testing.T) {
	inv := RepoInventory{Workspaces: []Workspace{{ID: "chan_main", Name: "主聊天", Main: true}, {ID: "chan_1", Name: "openlinkedin"}}}
	for ref, want := range map[string]string{"chan_1": "chan_1", "openlinkedin": "chan_1", "#OpenLinkedIn": "chan_1", "main": "chan_main", "主聊天": "chan_main"} {
		if w, ok := inv.Workspace(ref); !ok || w.ID != want {
			t.Errorf("Workspace(%q) = %+v %v", ref, w, ok)
		}
	}
	if _, ok := inv.Workspace("nope"); ok {
		t.Error("unknown channel found")
	}
	if !(AuthorizeState("sudo")).Stopped() || AuthorizeDone.Stopped() || AuthorizeAlready.Stopped() {
		t.Error("Stopped")
	}
}
