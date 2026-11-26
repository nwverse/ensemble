package core

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/names"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
)

// named is a provider fake under a real provider's id. A credential "<alias>|<n>" verifies as account n with that
// alias; Grok Bot's "bot:<name>|<n>" verifies with GetMe's email and gives the bot's name as its pairing alias.
type named struct {
	*fake
	id string
}

func (n named) Name() string { return n.id }
func (n named) Verify(_ context.Context, cred string) (provider.RemoteAccount, error) {
	alias, num, _ := strings.Cut(cred, "|")
	if strings.HasPrefix(alias, "bot:") {
		alias = "ada@example.com"
	}
	return provider.RemoteAccount{ExternalAccountID: n.id + "-" + num, Alias: strings.TrimSpace(alias)}, nil
}

type namedGrok struct{ named }

func (n namedGrok) PairingAlias(_ context.Context, cred string) string {
	if bot, ok := strings.CutPrefix(cred, "bot:"); ok {
		name, _, _ := strings.Cut(bot, "|")
		return name
	}
	return ""
}

// What each provider's Verify reports as the alias, as its adapter reads it (see the provider tests).
var verifiedAlias = map[string]string{
	"grok_bot": "bot:Grok 秘书", "muse": "Glint", "claude_duet": "Ada", "gemini_tasks": "Ada", "dots": "Ada",
	"cue": "小助手", "groq": "", "muse2api": "", "echo": "",
}

// What versions before this one stored for an account of each provider: Muse, Claude Duet, Gemini and Dots rows from
// before 0.5 hold the tab's title.
var legacyStored = map[string]string{
	"grok_bot": "Grok Bot", "muse": "Muse · Glint", "claude_duet": "Claude Duet - Claude", "gemini_tasks": "gemini_tasks",
	"dots": "Dots · Ada", "cue": "Cue · 小助手", "groq": "Groq", "muse2api": "Muse (muse2api)", "echo": "Echo",
}

var namedProviders = []string{"grok_bot", "muse", "claude_duet", "gemini_tasks", "dots", "cue", "groq", "muse2api", "echo"}

type nameRow struct {
	provider, scenario, stored, source, listZh, listEn, agentsrc, launcher, sessionFallback string
}

func namingSpace(t *testing.T) *Space {
	t.Helper()
	var adapters []provider.Adapter
	for _, p := range namedProviders {
		n := named{fake: newFake(), id: p}
		if p == "grok_bot" {
			adapters = append(adapters, namedGrok{n})
		} else {
			adapters = append(adapters, n)
		}
	}
	clk := &clock{t: time.UnixMilli(1_800_000_000_000)}
	club := New(Options{Registry: provider.NewRegistry(adapters...), Now: clk.Now, FollowUps: []time.Duration{}})
	t.Cleanup(club.Close)
	space, err := club.Space(context.Background(), DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	return space
}

var namingScenarios = []string{"1 account", "1, no alias", "2, no alias", "2, same alias", "custom name",
	"custom, brand-shaped", "legacy row", "legacy row, synced"}

// namingRows pairs (or, for legacy, stores) the scenario's accounts and reads every place their name shows: the
// accounts page title in Chinese and English (extension.accountTitle: DisplayName / DisplayNameEn), the displayName
// agentsrc.status hands Persona, the launcher and assistant session title (runtime's accountTitle: names.Fallback),
// and that title for an account with no name.
func namingRows(t *testing.T, p, scenario string) []nameRow {
	t.Helper()
	ctx := context.Background()
	s := namingSpace(t)
	pair := func(cred, custom string) {
		t.Helper()
		if _, err := s.AddAccount(ctx, protocol.AddAccountParams{Provider: p, Credential: cred, DisplayName: custom}); err != nil {
			t.Fatal(err)
		}
	}
	alias := verifiedAlias[p]
	switch scenario {
	case "1 account":
		pair(alias+"|account-1", "")
	case "1, no alias":
		pair("|account-1", "")
	case "2, no alias":
		pair("|account-1", "")
		pair("|account-2", "")
	case "2, same alias":
		pair(alias+"|account-1", "")
		pair(alias+"|account-2", "")
	case "custom name":
		pair(alias+"|account-1", "工作号")
	case "custom, brand-shaped":
		// what the owner typed: the bare brand twice, and the brand with a suffix of their own
		pair(alias+"|account-1", names.Brand("en", p))
		pair(alias+"|account-2", names.Brand("en", p))
		pair(alias+"|account-3", names.Brand("en", p)+"-Pro")
	case "legacy row", "legacy row, synced":
		for i := 1; i <= 2; i++ {
			id := fmt.Sprintf("a00000000000%d", i)
			if err := s.store.InsertAccount(ctx, store.Account{AccountID: id, Provider: p,
				DisplayName: legacyStored[p], ExternalAccountID: fmt.Sprintf("old-%d", i), Status: protocol.AccountActive,
				AssistantControl: string(DefaultControl), CreatedAt: int64(i), UpdatedAt: int64(i)}); err != nil {
				t.Fatal(err)
			}
			if err := s.vault.Put(ctx, id, fmt.Sprintf("%s|account-%d", alias, i)); err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy row, synced" {
				if err := s.SyncAccount(ctx, id); err != nil {
					t.Fatal(err)
				}
			}
			if p != "grok_bot" && p != "muse" {
				break // one legacy row; Grok Bot and Muse show two that read alike
			}
		}
	}
	rows, err := s.store.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	status, err := s.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []nameRow
	for i, a := range status.Accounts {
		one, err := s.Account(ctx, a.AccountID)
		if err != nil || one.DisplayName != a.DisplayName || one.DisplayNameEn != a.DisplayNameEn {
			t.Fatalf("Account(%s) = %q, Status says %q (%v)", a.AccountID, one.DisplayName, a.DisplayName, err)
		}
		src := rows[i].NameSource
		if src == "" {
			src = "(legacy)"
		}
		out = append(out, nameRow{provider: p, scenario: scenario, stored: rows[i].DisplayName, source: src,
			listZh: a.DisplayName, listEn: a.DisplayNameEn, agentsrc: a.DisplayName,
			launcher: names.Fallback(names.Default, p, a.DisplayName), sessionFallback: names.Fallback(names.Default, p, "")})
	}
	return out
}

// TestNamingTable is the naming of every provider's accounts, end to end through pairing, a legacy row's first sync
// and agentsrc.status. With NAMING_TABLE=<path> it also writes the table as markdown.
func TestNamingTable(t *testing.T) {
	var table []nameRow
	for _, p := range namedProviders {
		for _, sc := range namingScenarios {
			rows := namingRows(t, p, sc)
			table = append(table, rows...)
			seen := map[string]bool{}
			for _, r := range rows {
				owner := strings.HasPrefix(sc, "custom")
				for _, v := range []string{r.agentsrc, r.listEn, r.launcher, r.sessionFallback} {
					if v == "" || v == p && strings.Contains(p, "_") {
						t.Errorf("%s %s: bad name %q", p, sc, v)
					}
					if !owner && (strings.Count(v, names.Sep) > 1 || strings.Contains(strings.ToLower(v), "claude duet · claude")) {
						t.Errorf("%s %s: brand repeated in %q", p, sc, v)
					}
				}
				if r.sessionFallback != names.Brand(names.Default, p) {
					t.Errorf("%s: session title fallback %q", p, r.sessionFallback)
				}
				if !owner && seen[strings.ToLower(r.agentsrc)] {
					t.Errorf("%s %s: two accounts both read %q", p, sc, r.agentsrc)
				}
				seen[strings.ToLower(r.agentsrc)] = true
				if owner && (r.agentsrc != r.stored || r.listEn != r.stored || r.source != names.SourceOwner) {
					t.Errorf("%s %s: the owner's name changed: %+v", p, sc, r)
				}
			}
		}
	}
	if path := os.Getenv("NAMING_TABLE"); path != "" {
		var b strings.Builder
		b.WriteString("| provider | accounts | stored displayName | source | page title (zh) | page title (en) | agentsrc displayName | launcher / session | fallback |\n|---|---|---|---|---|---|---|---|---|\n")
		for _, r := range table {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s | %s |\n", r.provider, r.scenario, r.stored, r.source,
				r.listZh, r.listEn, r.agentsrc, r.launcher, r.sessionFallback)
		}
		if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPairingNamesGrokBotAfterItsSoleBot(t *testing.T) {
	ctx := context.Background()
	s := namingSpace(t)
	one, _ := s.AddAccount(ctx, protocol.AddAccountParams{Provider: "grok_bot", Credential: "bot:Grok 秘书|account-1"})
	several, _ := s.AddAccount(ctx, protocol.AddAccountParams{Provider: "grok_bot", Credential: "bot:|account-2"})
	if one.DisplayName != "Grok Bot · Grok 秘书" || several.DisplayName != "Grok Bot · ada" || several.Email != "ada@example.com" {
		t.Fatalf("%q %q", one.DisplayName, several.DisplayName)
	}
}

// A row an earlier version stored reads as brand and alias before it is synced, without re-pairing; its first
// successful sync settles it: a name club composed (or a saved tab title) is renamed from Verify, the owner's is
// kept and marked theirs.
func TestLegacyAccountNamesRenderAndSettleOnSync(t *testing.T) {
	before := map[string][]string{
		"grok_bot":     {"Grok Bot (1)", "Grok Bot (2)"},
		"muse":         {"Muse（网页版） · Glint (1)", "Muse（网页版） · Glint (2)"},
		"claude_duet":  {"Claude Duet"},
		"gemini_tasks": {"Gemini"},
		"muse2api":     {"Muse（自建转接）"},
		"echo":         {"演示账号"},
		"cue":          {"Cue · 小助手"},
	}
	after := map[string][]string{
		"grok_bot":    {"Grok Bot · Grok 秘书 (1)", "Grok Bot · Grok 秘书 (2)"},
		"claude_duet": {"Claude Duet · Ada"},
		"dots":        {"ChatGPT Dots · Ada"},
	}
	for p, w := range before {
		rows := namingRows(t, p, "legacy row")
		if len(rows) != len(w) {
			t.Fatalf("%s: %+v", p, rows)
		}
		for i, r := range rows {
			if r.agentsrc != w[i] || r.stored != legacyStored[p] {
				t.Errorf("%s before sync: %+v, want %q", p, r, w[i])
			}
		}
	}
	for p, w := range after {
		rows := namingRows(t, p, "legacy row, synced")
		for i, r := range rows {
			if r.agentsrc != w[i] || r.source != names.SourceAuto {
				t.Errorf("%s after sync: %+v, want %q", p, r, w[i])
			}
		}
	}
	// An owner's old name is kept as typed, marked theirs, and never re-verified.
	ctx := context.Background()
	s := namingSpace(t)
	id := "a000000000009"
	if err := s.store.InsertAccount(ctx, store.Account{AccountID: id, Provider: "cue", DisplayName: "Cue-Pro",
		Status: protocol.AccountActive, AssistantControl: string(DefaultControl)}); err != nil {
		t.Fatal(err)
	}
	_ = s.vault.Put(ctx, id, "小助手|account-9")
	_ = s.SyncAccount(ctx, id)
	a, _, _ := s.store.Account(ctx, id)
	if a.DisplayName != "Cue-Pro" || a.NameSource != names.SourceOwner {
		t.Fatalf("%+v", a)
	}
}

func TestRenameIsTheOwnersName(t *testing.T) {
	ctx := context.Background()
	s := namingSpace(t)
	a, _ := s.AddAccount(ctx, protocol.AddAccountParams{Provider: "groq", Credential: "|account-1"})
	b, _ := s.AddAccount(ctx, protocol.AddAccountParams{Provider: "groq", Credential: "|account-2"})
	name := "Groq: fast"
	if _, err := s.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	brand := "Groq"
	if _, err := s.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: b.AccountID, DisplayName: &brand}); err != nil {
		t.Fatal(err)
	}
	st, _ := s.Status(ctx)
	var got []string
	for _, x := range st.Accounts {
		got = append(got, x.DisplayName+" / "+x.DisplayNameEn)
	}
	slices.Sort(got)
	if strings.Join(got, ", ") != "Groq / Groq, Groq: fast / Groq: fast" {
		t.Fatal(got)
	}
}

// wholeName is an adapter that still reports a whole DisplayName (Muse's verify until it sets Alias).
type wholeName struct{ named }

func (w wholeName) Verify(_ context.Context, cred string) (provider.RemoteAccount, error) {
	name, num, _ := strings.Cut(cred, "|")
	return provider.RemoteAccount{ExternalAccountID: "muse-" + num, DisplayName: name}, nil
}

func TestAWholeVerifiedNameIsReadForItsAlias(t *testing.T) {
	ctx := context.Background()
	club := New(Options{Registry: provider.NewRegistry(wholeName{named{fake: newFake(), id: "muse"}}), FollowUps: []time.Duration{}})
	t.Cleanup(club.Close)
	s, _ := club.Space(ctx, DefaultSpace)
	for i, verified := range []string{"Muse · Glint", "Muse", "Muse · Muse"} {
		if _, err := s.AddAccount(ctx, protocol.AddAccountParams{Provider: "muse", Credential: fmt.Sprintf("%s|account-%d", verified, i)}); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Status(ctx)
	var got []string
	for _, a := range st.Accounts {
		got = append(got, a.DisplayNameEn)
	}
	slices.Sort(got)
	if strings.Join(got, ", ") != "Muse (web) (1), Muse (web) (2), Muse (web) · Glint" {
		t.Fatal(got)
	}
}

// slowVerify is a Claude Duet fake whose Verify waits until released, telling it has started.
type slowVerify struct {
	named
	started, release chan struct{}
}

func (v slowVerify) Verify(ctx context.Context, cred string) (provider.RemoteAccount, error) {
	v.started <- struct{}{}
	<-v.release
	return v.named.Verify(ctx, cred)
}

// A rename made while a legacy row's settling Verify runs wins: the settle writes only onto the row as it read it.
func TestSettlingNeverOverwritesARenameMadeMeanwhile(t *testing.T) {
	ctx := context.Background()
	v := slowVerify{named{fake: newFake(), id: "claude_duet"}, make(chan struct{}, 1), make(chan struct{})}
	club := New(Options{Registry: provider.NewRegistry(v), FollowUps: []time.Duration{}})
	t.Cleanup(club.Close)
	s, _ := club.Space(ctx, DefaultSpace)
	id := "a000000000001"
	if err := s.store.InsertAccount(ctx, store.Account{AccountID: id, Provider: "claude_duet", DisplayName: "Claude Duet - Claude",
		Status: protocol.AccountActive, AssistantControl: string(DefaultControl)}); err != nil {
		t.Fatal(err)
	}
	_ = s.vault.Put(ctx, id, "Ada|account-1")
	done := make(chan struct{})
	go func() { _ = s.SyncAccount(ctx, id); close(done) }()
	<-v.started
	name := "工作用 Claude"
	if _, err := s.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: id, DisplayName: &name}); err != nil {
		t.Fatal(err)
	}
	close(v.release)
	<-done
	a, _, _ := s.store.Account(ctx, id)
	if a.DisplayName != name || a.NameSource != names.SourceOwner {
		t.Fatalf("the rename was lost: %+v", a)
	}
	// Without a rename in between the same settle does rename it.
	id2 := "a000000000002"
	_ = s.store.InsertAccount(ctx, store.Account{AccountID: id2, Provider: "claude_duet", DisplayName: "Claude Duet - Claude",
		Status: protocol.AccountActive, AssistantControl: string(DefaultControl)})
	_ = s.vault.Put(ctx, id2, "Ada|account-2")
	go func() { <-v.started }()
	_ = s.SyncAccount(ctx, id2)
	if a, _, _ := s.store.Account(ctx, id2); a.DisplayName != "Claude Duet · Ada" || a.NameSource != names.SourceAuto {
		t.Fatalf("%+v", a)
	}
}

// An owner's "<Brand> · x" from 0.6.0 (whose rename field was prefilled with the composed name) survives the
// settle and later syncs exactly; a "<Brand> · x" club wrote, whose x is still the alias Verify reports, is club's.
func TestLegacyBrandDotXIsKeptUnlessItIsTheVerifiedAlias(t *testing.T) {
	ctx := context.Background()
	s := namingSpace(t)
	rows := []struct {
		id, provider, stored, cred, wantStored, wantSource, wantShown string
	}{
		// providers whose 0.6.0 Verify only gave the bare brand: any "· x" is the owner's, Verify or not
		{"a000000000001", "grok_bot", "Grok Bot · Work", "bot:Grok 秘书|account-1", "Grok Bot · Work", names.SourceOwner, "Grok Bot · Work"},
		{"a000000000002", "groq", "Groq · fast", "|account-2", "Groq · fast", names.SourceOwner, "Groq · fast"},
		{"a000000000003", "echo", "Echo · test", "|account-3", "Echo · test", names.SourceOwner, "Echo · test"},
		// x differs from the alias Verify reports now ("Shengtao"): the owner edited it
		{"a000000000004", "claude_duet", "Claude Duet · Shengtao 工作", "Shengtao|account-4", "Claude Duet · Shengtao 工作", names.SourceOwner, "Claude Duet · Shengtao 工作"},
		// x is the verified alias: club's own, rewritten in today's form
		{"a000000000005", "claude_duet", "Claude Duet · Shengtao", "Shengtao|account-5", "Claude Duet · Shengtao", names.SourceAuto, "Claude Duet · Shengtao"},
		{"a000000000006", "dots", "Dots · Ada", "Ada|account-6", "ChatGPT Dots · Ada", names.SourceAuto, "ChatGPT Dots · Ada"},
		{"a000000000007", "cue", "Cue · 小助手 Pro", "小助手|account-7", "Cue · 小助手 Pro", names.SourceOwner, "Cue · 小助手 Pro"},
		// the page-title rule is unchanged
		{"a000000000008", "claude_duet", "Claude Duet - Claude", "Ada|account-8", "Claude Duet · Ada", names.SourceAuto, "Claude Duet · Ada"},
	}
	for i, r := range rows {
		if err := s.store.InsertAccount(ctx, store.Account{AccountID: r.id, Provider: r.provider, DisplayName: r.stored,
			Status: protocol.AccountActive, AssistantControl: string(DefaultControl), CreatedAt: int64(i + 1)}); err != nil {
			t.Fatal(err)
		}
		_ = s.vault.Put(ctx, r.id, r.cred)
	}
	for round := 1; round <= 2; round++ {
		for _, r := range rows {
			_ = s.SyncAccount(ctx, r.id)
		}
		st, _ := s.Status(ctx)
		shown := map[string]string{}
		for _, a := range st.Accounts {
			shown[a.AccountID] = a.DisplayName
		}
		for _, r := range rows {
			a, _, _ := s.store.Account(ctx, r.id)
			if a.DisplayName != r.wantStored || a.NameSource != r.wantSource || shown[r.id] != r.wantShown {
				t.Errorf("sync %d, %s %q: stored %q (%s), shown %q; want %q (%s), %q", round, r.provider, r.stored,
					a.DisplayName, a.NameSource, shown[r.id], r.wantStored, r.wantSource, r.wantShown)
			}
		}
	}
}

// An e-mail alias is shown before its @ wherever the account is named, in both languages, while the stored name and
// the account's Email keep the whole address: for an account paired now, for a row 0.6.1 stored (also after it
// syncs), and with two e-mails sharing that part numbered. A name the owner typed, an e-mail or not, is kept as typed.
func TestEmailAliasesAreShownBeforeTheirAt(t *testing.T) {
	ctx := context.Background()
	s := namingSpace(t)
	pair := func(p, cred, name string) protocol.Account {
		t.Helper()
		a, err := s.AddAccount(ctx, protocol.AddAccountParams{Provider: p, Credential: cred, DisplayName: name})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	grok := pair("grok_bot", "bot:|account-1", "") // no single bot: GetMe's e-mail, ada@example.com
	gemini := pair("gemini_tasks", "ada@example.com|account-2", "")
	dotsA := pair("dots", "ada@example.com|account-3", "")
	dotsB := pair("dots", "ada@example.org|account-4", "")
	owner := pair("gemini_tasks", "bob@example.com|account-5", "bob@example.com")
	// as 0.6.1 stored a Grok Bot account without a single bot
	old := "a000000000009"
	if err := s.store.InsertAccount(ctx, store.Account{AccountID: old, Provider: "grok_bot", DisplayName: "Grok Bot · someone@example.com",
		NameSource: names.SourceAuto, ExternalAccountID: "grok_bot-old", Status: protocol.AccountActive,
		AssistantControl: string(DefaultControl), CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	_ = s.vault.Put(ctx, old, "bot:|account-9")
	want := map[string]struct{ zh, en, email, stored string }{
		grok.AccountID:   {"Grok Bot · ada", "Grok Bot · ada", "ada@example.com", "Grok Bot · ada@example.com"},
		gemini.AccountID: {"Gemini · ada", "Gemini · ada", "ada@example.com", "Gemini · ada@example.com"},
		dotsA.AccountID:  {"ChatGPT Dots · ada (1)", "ChatGPT Dots · ada (1)", "ada@example.com", "ChatGPT Dots · ada@example.com"},
		dotsB.AccountID:  {"ChatGPT Dots · ada (2)", "ChatGPT Dots · ada (2)", "ada@example.org", "ChatGPT Dots · ada@example.org"},
		owner.AccountID:  {"bob@example.com", "bob@example.com", "", "bob@example.com"},
		old:              {"Grok Bot · someone", "Grok Bot · someone", "someone@example.com", "Grok Bot · someone@example.com"},
	}
	check := func(when string) {
		t.Helper()
		st, err := s.Status(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range st.Accounts {
			w := want[a.AccountID]
			one, _ := s.Account(ctx, a.AccountID)
			row, _, _ := s.store.Account(ctx, a.AccountID)
			if a.DisplayName != w.zh || a.DisplayNameEn != w.en || one.DisplayName != w.zh || one.Email != w.email || row.DisplayName != w.stored {
				t.Errorf("%s %s: shown %q / %q, email %q, stored %q (%s); want %+v", when, a.AccountID, a.DisplayName, a.DisplayNameEn,
					one.Email, row.DisplayName, row.NameSource, w)
			}
			if wantOwner := a.AccountID == owner.AccountID; (one.OwnerName != "") != wantOwner || (row.NameSource == names.SourceOwner) != wantOwner {
				t.Errorf("%s %s: owner name %q, source %q", when, a.AccountID, one.OwnerName, row.NameSource)
			}
		}
	}
	check("before sync")
	for id := range want {
		_ = s.SyncAccount(ctx, id)
	}
	check("after sync")
}
