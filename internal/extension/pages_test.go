package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/names"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
)

// named is a provider that pairs any credential under its name, for pages with accounts of several services.
type named struct{ name string }

func (n named) Name() string       { return n.name }
func (n named) Connection() string { return protocol.ConnectionServer }
func (n named) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: n.name + "-remote"}, nil
}
func (n named) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "main", Title: "Main", Kind: protocol.KindMain, CanSend: true}}, nil
}
func (n named) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}
func (n named) Send(context.Context, string, provider.SessionRef, provider.Outgoing, provider.SendContext) (provider.SendResult, error) {
	return provider.SendResult{Status: provider.Sent}, nil
}

var fiveServices = []string{grokBot, "claude_duet", "gemini_tasks", "dots", "cue"}

// startServices is a host whose club has every service the add-account page offers, with nothing paired.
func startServices(t *testing.T) (*exttest.Host, *core.Club) {
	t.Helper()
	reg := provider.NewRegistry(named{grokBot}, named{"muse"}, named{"claude_duet"}, named{"gemini_tasks"}, named{"dots"},
		named{"cue"}, named{"groq"})
	club := core.New(core.Options{Registry: reg, FollowUps: []time.Duration{}, WatchLimit: -1})
	return startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`)), club
}

// pairFive pairs one account of each of fiveServices, the third one signed out; it answers their ids in that order.
func pairFive(t *testing.T, club *core.Club) []string {
	t.Helper()
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)
	var ids []string
	for _, p := range fiveServices {
		a, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: p, Credential: "fixture-credential-" + p})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.AccountID)
	}
	st, code := protocol.AccountError, "AUTH_FAILED"
	if err := space.Store().UpdateAccount(ctx, ids[2], store.AccountPatch{Status: &st, LastError: &code}, 1); err != nil {
		t.Fatal(err)
	}
	return ids
}

func renderVersion(t *testing.T, h *exttest.Host, ui string, version int, st pageState) *wire.View {
	t.Helper()
	placement := wire.PlacementPluginPage
	if ui == HomeUI {
		placement = wire.PlacementNavPage
	}
	var v wire.View
	if err := h.Call("ui.render", wire.UIParams{Contribution: ui, Placement: placement, Size: "full", UIVersion: version,
		Locale: "zh-Hans", State: stateOf(st)}, &v); err != nil {
		t.Fatalf("ui.render %s %+v: %v", ui, st, err)
	}
	checkView(t, &v)
	return &v
}

// sectionJSON is each section as drawn.
func sectionJSON(v *wire.View) []string {
	var out []string
	for _, s := range v.Sections {
		raw, _ := json.Marshal(s)
		out = append(out, string(raw))
	}
	return out
}

// addGroups are the sections of the ways to add an account in a view.
func addGroups(v *wire.View) int {
	n := 0
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if strings.HasPrefix(c.Key, "pair-") {
				n++
				break
			}
		}
	}
	return n
}

// The home and 账号 never draw the same section, with no account and with five; the ways to add one are drawn in
// full on the home only before the first account, and never on 账号; once there are accounts both have one row that
// opens the add-account page, and the home lists the account that needs the user first.
func TestHomeAndAccountsPageDoDifferentJobs(t *testing.T) {
	h, club := startServices(t)
	check := func(when string, accounts int) (*wire.View, *wire.View) {
		home := renderVersion(t, h, HomeUI, 11, pageState{})
		list := renderVersion(t, h, AccountsUI, 11, pageState{})
		for _, a := range sectionJSON(home) {
			for _, b := range sectionJSON(list) {
				if a == b {
					t.Errorf("%s: the home and 账号 both draw %s", when, a)
				}
			}
		}
		for _, hs := range home.Sections {
			for _, ls := range list.Sections {
				if hs.Title != "" && hs.Title == ls.Title {
					t.Errorf("%s: both have a section titled %q", when, hs.Title)
				}
			}
		}
		if n := addGroups(list); n != 0 {
			t.Errorf("%s: 账号 draws %d groups of ways to add one", when, n)
		}
		wantGroups := 0
		if accounts == 0 {
			wantGroups = 4 // sign in, a computer's web page, a key, the demo
		}
		if n := addGroups(home); n != wantGroups {
			t.Errorf("%s: the home draws %d groups of ways to add one, want %d", when, n, wantGroups)
		}
		if accounts > 0 && !strings.Contains(text(home), `"key":"add"`) || !strings.Contains(text(list), `"key":"add"`) {
			t.Errorf("%s: no 添加账号 row: home %v, 账号 %v", when, strings.Contains(text(home), `"key":"add"`), strings.Contains(text(list), `"key":"add"`))
		}
		return home, list
	}
	check("no account", 0)
	ids := pairFive(t, club)
	home, list := check("five accounts", 5)

	// the home: the signed-out one first, each row in a few words; 账号 in the space's order, with the last sync
	rows := func(v *wire.View, title string) []wire.Component {
		i := sectionIndex(v, title)
		if i < 0 {
			t.Fatalf("no section %q: %s", title, text(v))
		}
		var out []wire.Component
		for _, c := range v.Sections[i].Items {
			if strings.HasPrefix(c.Key, "acct-") {
				out = append(out, c)
			}
		}
		return out
	}
	glance, all := rows(home, "账号一览"), rows(list, "已接入的账号")
	if len(glance) != 5 || len(all) != 5 || glance[0].Key != "acct-"+ids[2] || !strings.Contains(glance[0].Subtitle, "点开看看") {
		t.Fatalf("home list %+v\n账号 list %+v", glance, all)
	}
	for _, c := range glance {
		if strings.Contains(c.Subtitle, "上次同步") {
			t.Errorf("the home's row says the last sync: %q", c.Subtitle)
		}
	}
	for _, c := range all {
		if !strings.Contains(c.Subtitle, "上次同步") && !strings.Contains(c.Subtitle, "点开它") {
			t.Errorf("账号's row lacks its details: %q", c.Subtitle)
		}
	}
	if strings.Contains(text(home), "点开一个账号可以改名") || !strings.Contains(text(list), "点开一个账号可以改名") {
		t.Error("what an account's page offers is said on the home, or not on 账号")
	}
}

// pushes are the pages a view's rows and buttons open.
func pushes(v *wire.View) []*wire.Push {
	var out []*wire.Push
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Action != nil && c.Action.Push != nil {
				out = append(out, c.Action.Push)
			}
		}
	}
	return out
}

// reach is every page state reachable from a page in at most depth taps, with the fewest taps to it.
func reach(t *testing.T, h *exttest.Host, ui string, from pageState, depth int) map[pageState]int {
	t.Helper()
	seen := map[pageState]int{from: 0}
	frontier := []pageState{from}
	for d := 1; d <= depth; d++ {
		var next []pageState
		for _, st := range frontier {
			for _, p := range pushes(renderVersion(t, h, ui, 11, st)) {
				if p.UI != ui {
					t.Errorf("%s %+v pushes into %s", ui, st, p.UI)
				}
				var to pageState
				_ = json.Unmarshal([]byte(p.State), &to)
				if _, ok := seen[to]; !ok {
					seen[to] = d
					next = append(next, to)
				}
			}
		}
		frontier = next
	}
	return seen
}

// Every page an earlier version opened in one tap from the home and from 账号 (a way to add each service, each
// account's page) opens in at most two now, and in one where it did before: the accounts and, before the first
// account, the ways to add one on the home. Old saved states and the pages' ids still draw.
func TestEveryActionStaysReachable(t *testing.T) {
	h, club := startServices(t)
	h.Answer("browser.machines", func(json.RawMessage) (any, error) {
		return wire.BrowserMachinesResult{Machines: []wire.BrowserMachine{{MachineID: "build-box", Name: "build-box", Backend: "ego", Online: true, Ready: true}}}, nil
	})
	adds := []pageState{{Page: pageGrok}, {Page: pagePair, Provider: "cue"}, {Page: pagePair, Provider: "groq"},
		{Page: pagePair, Provider: "muse2api"}, {Page: pagePair, Provider: "echo"}}
	for _, s := range browserProviders {
		adds = append(adds, pageState{Page: pageBrowserPair, Provider: s.Provider})
	}
	home := reach(t, h, HomeUI, pageState{}, 2)
	for _, st := range adds {
		if home[st] != 1 {
			t.Errorf("first visit: %+v is %d taps from the home, want 1", st, home[st])
		}
	}
	ids := pairFive(t, club)
	for _, ui := range []string{HomeUI, AccountsUI} {
		got := reach(t, h, ui, pageState{}, 2)
		if got[pageState{Page: pageAdd}] != 1 {
			t.Errorf("%s: the add-account page is not one tap away", ui)
		}
		for _, st := range adds {
			if d, ok := got[st]; !ok || d > 2 {
				t.Errorf("%s: %+v is not within two taps (%d, %v)", ui, st, d, ok)
			}
		}
		for _, id := range ids {
			if got[pageState{Page: pageAccount, Account: id}] != 1 {
				t.Errorf("%s: account %s is not one tap away", ui, id)
			}
		}
	}
	// the pages' ids and old saved states draw as before
	for _, ui := range []string{HomeUI, AccountsUI} {
		for _, st := range append(adds, pageState{}, pageState{Page: pageAccount, Account: ids[0]}, pageState{Page: pageDone, Account: ids[0]}) {
			if v := renderVersion(t, h, ui, 11, st); len(v.Sections) == 0 {
				t.Errorf("%s %+v draws nothing", ui, st)
			}
		}
	}
}

// One line above the ways to add an account says which service goes which way, naming every one; each way's own
// explanation is one sentence, and the fuller one is on the page its rows open.
func TestAddPageSaysHowToChooseInOneLine(t *testing.T) {
	h, _ := startServices(t)
	for _, v := range []*wire.View{renderVersion(t, h, AccountsUI, 11, pageState{Page: pageAdd}), renderVersion(t, h, HomeUI, 11, pageState{})} {
		i := sectionIndex(v, "在你自己的浏览器里登录")
		if i < 1 {
			t.Fatalf("no ways to add one: %s", text(v))
		}
		above := v.Sections[i-1]
		if len(above.Items) != 1 || above.Items[0].Type != "text" || !strings.HasPrefix(above.Items[0].Text, "怎么选：") {
			t.Fatalf("no one line above the ways: %+v", above)
		}
		for _, p := range append([]string{grokBot, "muse", "claude_duet", "gemini_tasks", "dots"}, keyProviders...) {
			if !strings.Contains(above.Items[0].Text, providerName("zh-Hans", p)) {
				t.Errorf("the line does not name %s: %q", p, above.Items[0].Text)
			}
		}
		for _, s := range v.Sections[i:] {
			if strings.Count(s.Footer, "。") > 1 || len([]rune(s.Footer)) > 50 {
				t.Errorf("group %q explains in more than one line: %q", s.Title, s.Footer)
			}
		}
	}
	if pv := render(t, h, pageState{Page: pagePair, Provider: "groq"}); !strings.Contains(pv.Sections[0].Footer, "凭它就能代表你使用服务") {
		t.Errorf("the key page lost the explanation: %s", text(pv))
	}
	if pv := render(t, h, pageState{Page: pageAdd}); pv.Title != addTitle {
		t.Errorf("add page title %q", pv.Title)
	}
}

// Each service's rows draw its own letter avatar: the accounts on both pages, the ways to add one, the head of an
// account's page. Apps before UI version 3 keep the icons they drew.
func TestAvatarsDifferPerService(t *testing.T) {
	h, club := startServices(t)
	add := renderVersion(t, h, AccountsUI, 11, pageState{Page: pageAdd})
	seen := map[string]string{}
	for _, s := range add.Sections {
		for _, c := range s.Items {
			if !strings.HasPrefix(c.Key, "pair-") {
				continue
			}
			p := strings.TrimPrefix(c.Key, "pair-")
			if c.Avatar == nil || c.Avatar.Text != names.Letters("zh-Hans", p) || c.Icon != "" {
				t.Errorf("add row %s: %+v", p, c)
				continue
			}
			if other, dup := seen[c.Avatar.Text]; dup {
				t.Errorf("%s and %s both draw %q", other, p, c.Avatar.Text)
			}
			seen[c.Avatar.Text] = p
		}
	}
	ids := pairFive(t, club)
	for _, ui := range []string{HomeUI, AccountsUI} {
		letters := map[string]bool{}
		for _, s := range renderVersion(t, h, ui, 11, pageState{}).Sections {
			for _, c := range s.Items {
				if strings.HasPrefix(c.Key, "acct-") {
					if c.Avatar == nil || c.Avatar.Text == "" {
						t.Errorf("%s: row %s has no avatar", ui, c.Key)
						continue
					}
					letters[c.Avatar.Text] = true
				}
			}
		}
		if len(letters) != len(fiveServices) {
			t.Errorf("%s: %d different avatars for %d services", ui, len(letters), len(fiveServices))
		}
		for _, s := range renderVersion(t, h, ui, 2, pageState{}).Sections {
			for _, c := range s.Items {
				if c.Avatar != nil || strings.HasPrefix(c.Key, "acct-") && c.Icon != "person" {
					t.Errorf("%s at UI version 2: %+v", ui, c)
				}
			}
		}
	}
	page := renderVersion(t, h, HomeUI, 11, pageState{Page: pageAccount, Account: ids[1]})
	if head := page.Sections[0].Items[0]; head.Type != "header" || head.Avatar == nil || head.Avatar.Text != names.Letters("zh-Hans", "claude_duet") ||
		head.Subtitle != "Claude Duet" {
		t.Errorf("account page head: %+v", head)
	}
	old := renderVersion(t, h, HomeUI, 3, pageState{Page: pageAccount, Account: ids[1]})
	if row := old.Sections[0].Items[0]; row.Type != "row" || row.Title != "服务" || row.Value != "Claude Duet" || row.Avatar == nil {
		t.Errorf("account page head at UI version 3: %+v", row)
	}
}

// An account's page starts with a header (title, service, avatar) on the home only. Opened from the settings page's
// 账号 it starts with the 服务 row, avatar on it, as in 0.6.3: the web app draws 账号 inside its Settings screen, and
// there an account page pushed with a header atop shifted the whole screen, navigation included, up by about 300 px,
// for good (a host layout bug, reported to the app's maintainers). Drop this once the apps in use are fixed.
func TestAccountPageHeadPerEntry(t *testing.T) {
	h, club := startServices(t)
	ids := pairFive(t, club)
	for _, version := range []int{0, 4, 11} {
		home := renderVersion(t, h, HomeUI, version, pageState{Page: pageAccount, Account: ids[1]})
		if head := home.Sections[0].Items[0]; head.Type != "header" || head.Title != home.Title || head.Avatar == nil {
			t.Errorf("home, UI version %d: the page starts with %+v, not the header", version, head)
		}
		page := renderVersion(t, h, AccountsUI, version, pageState{Page: pageAccount, Account: ids[1]})
		if page.Title != home.Title {
			t.Errorf("账号, UI version %d: title %q, the home's %q", version, page.Title, home.Title)
		}
		first := page.Sections[0].Items[0]
		if first.Type != "row" || first.Title != "服务" || first.Value != "Claude Duet" || first.Avatar == nil ||
			first.Avatar.Text != names.Letters("zh-Hans", "claude_duet") {
			t.Errorf("账号, UI version %d: the page starts with %+v, not the 服务 row with its avatar", version, first)
		}
		for _, s := range page.Sections {
			for _, c := range s.Items {
				if c.Type == "header" {
					t.Errorf("账号, UI version %d: a header on the account page: %+v", version, c)
				}
			}
		}
		// the rest of the page is the home's
		if len(page.Sections) != len(home.Sections) || len(page.Sections[0].Items) != len(home.Sections[0].Items) {
			t.Errorf("UI version %d: 账号 draws %d sections, the home %d", version, len(page.Sections), len(home.Sections))
		}
	}
}

// The advanced section of an account with a cloud computer says what one gets and when one wants it, in plain words.
func TestCloudComputerSectionSaysWhatYouGet(t *testing.T) {
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: "echo"}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	v := render(t, h, pageState{Page: pageAccount, Account: status.Accounts[0].AccountID})
	i := sectionIndex(v, "高级：")
	if i < 0 {
		t.Fatalf("no advanced section: %s", text(v))
	}
	s := v.Sections[i]
	for _, want := range []string{"云端电脑", "「机器」页", "运行程序、处理文件", "才需要"} {
		if !strings.Contains(s.Title+s.Footer, want) {
			t.Errorf("lacks %q: %q / %q", want, s.Title, s.Footer)
		}
	}
	if strings.Contains(s.Title, "接进 Persona") || strings.Count(s.Footer, "。") > 1 {
		t.Errorf("still the old wording, or more than one line: %q / %q", s.Title, s.Footer)
	}
}
