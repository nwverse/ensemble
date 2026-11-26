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
		{"en", "muse", "Wren", "Muse (web) · Wren"},
		{"zh", "muse", "Wren", "Muse（网页版） · Wren"},
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
		{"gemini_tasks", "Gemini Tasks · ada@example.com", "Gemini · ada", "Gemini · ada"}, // an e-mail shows before its @
		{"gemini_tasks", "gemini_tasks", "Gemini", "Gemini"},
		{"gemini_tasks", "Google Gemini", "Gemini", "Gemini"},
		{"grok_bot", "Grok Bot", "Grok Bot", "Grok Bot"},
		{"muse", "Muse · Wren", "Muse（网页版） · Wren", "Muse (web) · Wren"},
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
				if src == SourceLegacy && alias != "" && bareBefore[p] {
					// club never wrote an alias for these before: a legacy "<Brand> · x" is the owner's
					if !owner || got != Stored(p, alias) {
						t.Errorf("Split(%q, Stored(%q), legacy) = %q %v, want the owner's", p, alias, got, owner)
					}
					continue
				}
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
		{ID: "b1", Provider: "muse", Stored: "Muse · Wren", CreatedAt: 1},
		{ID: "b2", Provider: "muse", Stored: "Muse · Wren", CreatedAt: 2},
		{ID: "c1", Provider: "claude_duet", Stored: "Claude Duet", Source: SourceAuto, CreatedAt: 1},
		{ID: "d1", Provider: "groq", Stored: "工作号", Source: SourceOwner, CreatedAt: 1},
		{ID: "d2", Provider: "cue", Stored: "工作号", Source: SourceOwner, CreatedAt: 2},
		{ID: "e1", Provider: "dots", Stored: "Dots", CreatedAt: 1},
		{ID: "e2", Provider: "dots", Stored: "ChatGPT Dots", Source: SourceAuto, CreatedAt: 2},
	})
	want := map[string]string{
		"a1": "Grok Bot (1)", "a2": "Grok Bot (2)", "a3": "Grok Bot · Grok 秘书",
		"b1": "Muse（网页版） · Wren (1)", "b2": "Muse（网页版） · Wren (2)",
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

// 0.6.0 prefilled the rename field with the composed name, so an owner's edit often reads "<Brand> · x".
func TestLegacyBrandDotXOfAProviderThatNeverWroteAnAliasIsTheOwners(t *testing.T) {
	for _, c := range []struct{ provider, stored string }{
		{"grok_bot", "Grok Bot · Work"}, {"groq", "Groq · fast"}, {"muse2api", "Muse (muse2api) · home"}, {"echo", "Echo · test"},
	} {
		if !OwnerChose(c.provider, c.stored) || Title("en", c.provider, c.stored, SourceLegacy) != c.stored {
			t.Errorf("%+v: %v %q", c, OwnerChose(c.provider, c.stored), Title("en", c.provider, c.stored, SourceLegacy))
		}
		if _, ok := LegacyAlias(c.provider, c.stored); ok {
			t.Errorf("%+v: offered for re-verification", c)
		}
	}
	got := Distinct("zh", []Account{{ID: "a", Provider: "grok_bot", Stored: "Grok Bot · Work"}, {ID: "b", Provider: "grok_bot", Stored: "Grok Bot · Work"}})
	if got["a"] != "Grok Bot · Work" || got["b"] != "Grok Bot · Work" {
		t.Fatal(got)
	}
	// Their bare brand and raw id are still club's.
	for _, c := range []struct{ provider, stored string }{{"grok_bot", "Grok Bot"}, {"groq", "groq"}, {"echo", "Echo"}} {
		if OwnerChose(c.provider, c.stored) {
			t.Errorf("%+v is club's", c)
		}
	}
}

func TestLegacyAliasIsOfferedOnlyWhereClubWroteThatShape(t *testing.T) {
	for _, c := range []struct {
		provider, stored, x string
		ok                  bool
	}{
		{"claude_duet", "Claude Duet · Rowan 工作", "Rowan 工作", true},
		{"cue", "Cue · 小助手", "小助手", true},
		{"dots", "Dots · Ada", "Ada", true},
		{"gemini_tasks", "Gemini Tasks · ada@example.com", "ada@example.com", true},
		{"muse", "Muse · Wren", "Wren", true},
		{"claude_duet", "Claude Duet · Claude", "", false}, // only the brand's words: club's, re-verified as before
		{"claude_duet", "Claude Duet - Claude", "", false}, // a tab title
		{"claude_duet", "Claude Duet", "", false},
		{"grok_bot", "Grok Bot · Work", "", false},
	} {
		if x, ok := LegacyAlias(c.provider, c.stored); x != c.x || ok != c.ok {
			t.Errorf("LegacyAlias(%s, %q) = %q %v", c.provider, c.stored, x, ok)
		}
	}
}

// An e-mail alias (Grok Bot's, and Gemini's or ChatGPT Dots' fallback) is titled by the part before @; the stored name
// keeps the whole e-mail, which Email gives back for the account's page. An owner's name is never shortened.
func TestEmailAliasesAreTitledBeforeTheirAt(t *testing.T) {
	for _, c := range []struct {
		provider, stored, source, zh, en, email string
	}{
		// paired now: Stored keeps the whole e-mail
		{"grok_bot", Stored("grok_bot", "someone@example.com"), SourceAuto, "Grok Bot · someone", "Grok Bot · someone", "someone@example.com"},
		{"gemini_tasks", Stored("gemini_tasks", "ada@example.com"), SourceAuto, "Gemini · ada", "Gemini · ada", "ada@example.com"},
		{"dots", Stored("dots", "ada.lovelace@example.com"), SourceAuto, "ChatGPT Dots · ada.lovelace", "ChatGPT Dots · ada.lovelace", "ada.lovelace@example.com"},
		// stored by 0.6.1, read as it is, without re-pairing
		{"grok_bot", "Grok Bot · someone@example.com", SourceAuto, "Grok Bot · someone", "Grok Bot · someone", "someone@example.com"},
		{"muse", "Muse (web) · wren@example.com", SourceAuto, "Muse（网页版） · wren", "Muse (web) · wren", "wren@example.com"},
		// a legacy Gemini row club wrote
		{"gemini_tasks", "Gemini Tasks · ada@example.com", SourceLegacy, "Gemini · ada", "Gemini · ada", "ada@example.com"},
		// the owner's: as typed, even when it is an e-mail or reads like club's
		{"grok_bot", "someone@example.com", SourceOwner, "someone@example.com", "someone@example.com", ""},
		{"grok_bot", "Grok Bot · someone@example.com", SourceOwner, "Grok Bot · someone@example.com", "Grok Bot · someone@example.com", ""},
		{"gemini_tasks", "ada@example.com", SourceLegacy, "ada@example.com", "ada@example.com", ""}, // not a shape club wrote
		{"grok_bot", "Grok Bot · someone@example.com", SourceLegacy, "Grok Bot · someone@example.com", "Grok Bot · someone@example.com", ""},
		// not e-mails, or a part before @ that is only the brand's words: as they are
		{"grok_bot", "Grok Bot · Grok 秘书", SourceAuto, "Grok Bot · Grok 秘书", "Grok Bot · Grok 秘书", ""},
		{"grok_bot", "Grok Bot · ada@localhost", SourceAuto, "Grok Bot · ada@localhost", "Grok Bot · ada@localhost", ""},
		{"grok_bot", "Grok Bot · @example.com", SourceAuto, "Grok Bot · @example.com", "Grok Bot · @example.com", ""},
		{"gemini_tasks", "Gemini · gemini@example.com", SourceAuto, "Gemini · gemini@example.com", "Gemini · gemini@example.com", ""},
	} {
		if got := Title("zh", c.provider, c.stored, c.source); got != c.zh {
			t.Errorf("Title(zh, %s, %q, %q) = %q, want %q", c.provider, c.stored, c.source, got, c.zh)
		}
		if got := Title("en", c.provider, c.stored, c.source); got != c.en {
			t.Errorf("Title(en, %s, %q, %q) = %q, want %q", c.provider, c.stored, c.source, got, c.en)
		}
		if got := Email(c.provider, c.stored, c.source); got != c.email {
			t.Errorf("Email(%s, %q, %q) = %q, want %q", c.provider, c.stored, c.source, got, c.email)
		}
		// shortening is for titles only: a name club stored is never read as the owner's
		if _, owner := Split(c.provider, c.stored, c.source); owner && (c.source == SourceAuto || c.email != "") {
			t.Errorf("Split(%s, %q, %q) reads it as the owner's", c.provider, c.stored, c.source)
		}
	}
	// The stored alias stays the whole e-mail, so a legacy row's settle still compares it with what Verify reports.
	if x, ok := LegacyAlias("gemini_tasks", "Gemini Tasks · ada@example.com"); !ok || x != "ada@example.com" {
		t.Fatal(x, ok)
	}
}

// Two accounts whose e-mails share the part before @ are numbered like any two composed titles that read alike, and
// never take an owner's title.
func TestEmailAliasesSharingTheirLocalPartAreNumbered(t *testing.T) {
	for _, l := range []string{"zh", "en"} {
		got := Distinct(l, []Account{
			{ID: "a", Provider: "grok_bot", Stored: "Grok Bot · ada@example.com", Source: SourceAuto, CreatedAt: 1},
			{ID: "b", Provider: "grok_bot", Stored: "Grok Bot · ada@example.org", Source: SourceAuto, CreatedAt: 2},
			{ID: "c", Provider: "grok_bot", Stored: "Grok Bot · bob@example.com", Source: SourceAuto, CreatedAt: 3},
			{ID: "d", Provider: "gemini_tasks", Stored: "Gemini · ada@example.com", Source: SourceAuto, CreatedAt: 4},
			{ID: "e", Provider: "gemini_tasks", Stored: "Gemini · bob", Source: SourceOwner, CreatedAt: 5},
			{ID: "f", Provider: "gemini_tasks", Stored: "Gemini · bob@example.com", Source: SourceAuto, CreatedAt: 6},
		})
		want := map[string]string{"a": "Grok Bot · ada (1)", "b": "Grok Bot · ada (2)", "c": "Grok Bot · bob",
			"d": "Gemini · ada", "e": "Gemini · bob", "f": "Gemini · bob (1)"}
		for id, w := range want {
			if got[id] != w {
				t.Errorf("%s %s: %q, want %q", l, id, got[id], w)
			}
		}
	}
}
