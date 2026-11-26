package names

import (
	"strings"
	"testing"
)

var providers = []string{"grok_bot", "muse", "claude_duet", "gemini_tasks", "dots", "cue", "groq", "muse2api", "echo"}

func TestBrandsAreOneTable(t *testing.T) {
	for _, c := range []struct{ provider, zh, en string }{
		{"grok_bot", "Grok Bot", "Grok Bot"}, {"muse", "Muse（网页版）", "Muse (web)"}, {"claude_duet", "Claude Duet", "Claude Duet"},
		{"gemini_tasks", "Gemini", "Gemini"}, {"dots", "ChatGPT Dots", "ChatGPT Dots"}, {"cue", "Cue", "Cue"}, {"groq", "Groq", "Groq"},
		{"muse2api", "Muse（自建转接）", "Muse (own relay)"}, {"echo", "演示账号", "Demo account"}, {"my_site", "My Site", "My Site"},
	} {
		if Brand("zh-Hans", c.provider) != c.zh || Brand("en", c.provider) != c.en || Brand("", c.provider) != c.zh {
			t.Errorf("%s: %q / %q", c.provider, Brand("zh", c.provider), Brand("en", c.provider))
		}
	}
	if Brand("zh", "") != "账号" || Brand("en", "") != "Account" {
		t.Fatal(Brand("zh", ""), Brand("en", ""))
	}
}

func TestComposeAddsTheBrandOnceAndDropsItsOwnWords(t *testing.T) {
	for _, c := range []struct{ lang, provider, alias, want string }{
		{"zh", "claude_duet", "Ada", "Claude Duet · Ada"},
		{"zh", "claude_duet", "", "Claude Duet"},
		{"zh", "claude_duet", "Claude", "Claude Duet"},
		{"zh", "claude_duet", "Duet - Claude", "Claude Duet"},
		{"zh", "gemini_tasks", "ada@example.com", "Gemini · ada@example.com"},
		{"zh", "gemini_tasks", "Google Gemini", "Gemini"},
		{"en", "muse", "Glint", "Muse (web) · Glint"},
		{"zh", "muse", "Glint", "Muse（网页版） · Glint"},
		{"zh", "dots", "ChatGPT", "ChatGPT Dots"},
		{"zh", "grok_bot", "Grok 秘书", "Grok Bot · Grok 秘书"},
		{"zh", "grok_bot", "Grok", "Grok Bot"},
		{"zh", "muse2api", "", "Muse（自建转接）"},
		{"en", "echo", "Echo", "Demo account"},
	} {
		if got := Compose(c.lang, c.provider, c.alias); got != c.want {
			t.Errorf("Compose(%q, %q, %q) = %q, want %q", c.lang, c.provider, c.alias, got, c.want)
		}
	}
}

// A name the owner set is never rewritten, whatever its shape, and never numbered.
func TestOwnerNamesAreShownAsTyped(t *testing.T) {
	for _, c := range []struct{ provider, stored string }{
		{"cue", "Cue-Pro"}, {"groq", "Groq: fast"}, {"grok_bot", "Grok Bot"}, {"claude_duet", "Claude Duet - Claude"},
		{"claude_duet", "Claude Duet · Work"}, {"grok_bot", "工作号"}, {"gemini_tasks", "Gemini"},
	} {
		for _, l := range []string{"zh", "en"} {
			if got := Title(l, c.provider, c.stored, SourceOwner); got != c.stored {
				t.Errorf("Title(%s, %s, %q, owner) = %q", l, c.provider, c.stored, got)
			}
		}
	}
	got := Distinct("zh", []Account{{ID: "a", Provider: "grok_bot", Stored: "Grok Bot", Source: SourceOwner},
		{ID: "b", Provider: "grok_bot", Stored: "Grok Bot", Source: SourceOwner}})
	if got["a"] != "Grok Bot" || got["b"] != "Grok Bot" {
		t.Fatal(got)
	}
}

// Rows from before club kept who named them: only the exact forms club wrote (bare brand, "<Brand> · x", the raw
// provider id) and a browser site's tab title count as club's; anything else is the owner's.
func TestLegacyRowsReadOnlyExactGeneratedFormsAsClubs(t *testing.T) {
	for _, c := range []struct{ provider, stored, zh, en string }{
		{"claude_duet", "Claude Duet · Ada", "Claude Duet · Ada", "Claude Duet · Ada"},
		{"claude_duet", "Claude Duet - Claude", "Claude Duet", "Claude Duet"}, // a tab title saved before 0.5
		{"claude_duet", "Claude Duet · Claude", "Claude Duet", "Claude Duet"},
		{"claude_duet", "claude_duet", "Claude Duet", "Claude Duet"},
		{"gemini_tasks", "Gemini Tasks · ada@example.com", "Gemini · ada@example.com", "Gemini · ada@example.com"},
		{"gemini_tasks", "gemini_tasks", "Gemini", "Gemini"},
		{"gemini_tasks", "Google Gemini", "Gemini", "Gemini"},
		{"grok_bot", "Grok Bot", "Grok Bot", "Grok Bot"},
		{"muse", "Muse · Glint", "Muse（网页版） · Glint", "Muse (web) · Glint"},
		{"muse", "Chat — Muse", "Muse（网页版）", "Muse (web)"},
		{"dots", "Dots · Ada", "ChatGPT Dots · Ada", "ChatGPT Dots · Ada"},
		{"dots", "ChatGPT", "ChatGPT Dots", "ChatGPT Dots"},
		{"cue", "Cue · 小助手", "Cue · 小助手", "Cue · 小助手"},
		{"muse2api", "Muse (muse2api)", "Muse（自建转接）", "Muse (own relay)"},
		{"echo", "Echo", "演示账号", "Demo account"},
		{"echo", "", "演示账号", "Demo account"},
		// the owner's
		{"cue", "Cue-Pro", "Cue-Pro", "Cue-Pro"},
		{"groq", "Groq: fast", "Groq: fast", "Groq: fast"},
		{"groq", "Groq Chief", "Groq Chief", "Groq Chief"},
		{"claude_duet", "Claude Duet - 工作", "Claude Duet - 工作", "Claude Duet - 工作"},
		{"grok_bot", "Grok Bot - Claude", "Grok Bot - Claude", "Grok Bot - Claude"}, // not a browser site: no tab title
	} {
		if got := Title("zh", c.provider, c.stored, SourceLegacy); got != c.zh {
			t.Errorf("Title(zh, %q, %q) = %q, want %q", c.provider, c.stored, got, c.zh)
		}
		if got := Title("en", c.provider, c.stored, SourceLegacy); got != c.en {
			t.Errorf("Title(en, %q, %q) = %q, want %q", c.provider, c.stored, got, c.en)
		}
		ownerRows := map[string]bool{"Cue-Pro": true, "Groq: fast": true, "Groq Chief": true, "Claude Duet - 工作": true, "Grok Bot - Claude": true}
		if owner := OwnerChose(c.provider, c.stored); owner != ownerRows[c.stored] {
			t.Errorf("OwnerChose(%q, %q) = %v", c.provider, c.stored, owner)
		}
	}
}

func TestStoredIsReadBackAsComposed(t *testing.T) {
	for _, p := range providers {
		for _, alias := range []string{"", "Ada", "ada@example.com", "Grok 秘书"} {
			for _, src := range []string{SourceAuto, SourceLegacy} {
				got, owner := Split(p, Stored(p, alias), src)
				if owner || got != alias {
					t.Errorf("Split(%q, Stored(%q), %q) = %q %v", p, alias, src, got, owner)
				}
			}
		}
	}
}

func TestDistinctNumbersCollidingComposedNamesOnly(t *testing.T) {
	got := Distinct("zh", []Account{
		{ID: "a2", Provider: "grok_bot", Stored: "Grok Bot", Source: SourceAuto, CreatedAt: 2},
		{ID: "a1", Provider: "grok_bot", Stored: "Grok Bot", Source: SourceAuto, CreatedAt: 1},
		{ID: "a3", Provider: "grok_bot", Stored: "Grok Bot · Grok 秘书", Source: SourceAuto, CreatedAt: 3},
		{ID: "b1", Provider: "muse", Stored: "Muse · Glint", CreatedAt: 1},
		{ID: "b2", Provider: "muse", Stored: "Muse · Glint", CreatedAt: 2},
		{ID: "c1", Provider: "claude_duet", Stored: "Claude Duet", Source: SourceAuto, CreatedAt: 1},
		{ID: "d1", Provider: "groq", Stored: "工作号", Source: SourceOwner, CreatedAt: 1},
		{ID: "d2", Provider: "cue", Stored: "工作号", Source: SourceOwner, CreatedAt: 2},
		{ID: "e1", Provider: "dots", Stored: "Dots", CreatedAt: 1},
		{ID: "e2", Provider: "dots", Stored: "ChatGPT Dots", Source: SourceAuto, CreatedAt: 2},
	})
	want := map[string]string{
		"a1": "Grok Bot (1)", "a2": "Grok Bot (2)", "a3": "Grok Bot · Grok 秘书",
		"b1": "Muse（网页版） · Glint (1)", "b2": "Muse（网页版） · Glint (2)",
		"c1": "Claude Duet",
		"d1": "工作号", "d2": "工作号",
		"e1": "ChatGPT Dots (1)", "e2": "ChatGPT Dots (2)",
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s: %q, want %q", id, got[id], w)
		}
	}
}

// An owner's "Grok Bot (1)" next to two composed "Grok Bot": every title still reads differently.
func TestDistinctNeverRepeatsATitle(t *testing.T) {
	got := Distinct("zh", []Account{
		{ID: "o", Provider: "grok_bot", Stored: "Grok Bot (1)", Source: SourceOwner, CreatedAt: 1},
		{ID: "x", Provider: "grok_bot", Stored: "Grok Bot", Source: SourceAuto, CreatedAt: 2},
		{ID: "y", Provider: "grok_bot", Stored: "Grok Bot", Source: SourceAuto, CreatedAt: 3},
		{ID: "p", Provider: "groq", Stored: "Groq", Source: SourceOwner, CreatedAt: 4},
		{ID: "q", Provider: "groq", Stored: "Groq", Source: SourceAuto, CreatedAt: 5},
	})
	if got["o"] != "Grok Bot (1)" || got["x"] != "Grok Bot (2)" || got["y"] != "Grok Bot (3)" || got["p"] != "Groq" || got["q"] != "Groq (1)" {
		t.Fatal(got)
	}
	seen := map[string]string{}
	for id, title := range got {
		if other, dup := seen[strings.ToLower(title)]; dup {
			t.Fatalf("%s and %s both read %q", id, other, title)
		}
		seen[strings.ToLower(title)] = id
	}
}

func TestFallbackIsNeverARawProviderID(t *testing.T) {
	for _, c := range []struct{ lang, provider, name, want string }{
		{"zh", "gemini_tasks", "", "Gemini"}, {"zh", "claude_duet", "claude_duet", "Claude Duet"}, {"en", "echo", " ", "Demo account"},
		{"zh", "grok_bot", "Grok Bot (2)", "Grok Bot (2)"}, {"zh", "my_site", "", "My Site"},
	} {
		if got := Fallback(c.lang, c.provider, c.name); got != c.want {
			t.Errorf("Fallback(%s, %s, %q) = %q, want %q", c.lang, c.provider, c.name, got, c.want)
		}
	}
}
