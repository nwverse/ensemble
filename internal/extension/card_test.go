package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

// renderCard draws the Overview card the way the apps ask for it (compact) and holds it to a card's limits.
func renderCard(t *testing.T, h *exttest.Host) *wire.View {
	t.Helper()
	var raw json.RawMessage
	if err := h.Call("ui.render", wire.UIParams{Contribution: HomeCardUI, Placement: wire.PlacementOverviewCard,
		Size: wire.SizeCompact, UIVersion: 11, Locale: "zh-Hans"}, &raw); err != nil {
		t.Fatalf("ui.render card: %v", err)
	}
	if err := exttest.CheckView(raw, true); err != nil {
		t.Fatalf("card: %v", err)
	}
	var v wire.View
	_ = json.Unmarshal(raw, &v)
	noJargon(t, "card", &v)
	// nothing that needs scrolling: one section, the counts and one step
	if len(v.Sections) != 1 || len(v.Sections[0].Items) > 2 {
		t.Errorf("card is more than the counts and a step: %s", text(&v))
	}
	return &v
}

// cardParts are the card's counts by label and its step row.
func cardParts(t *testing.T, v *wire.View) (map[string]wire.Stat, wire.Component) {
	t.Helper()
	stats := map[string]wire.Stat{}
	var step wire.Component
	for _, c := range v.Sections[0].Items {
		for _, s := range c.Stats {
			stats[s.Label] = s
			for _, w := range jargon {
				if strings.Contains(s.Label+s.Value+s.Detail, w) {
					t.Errorf("card stat %+v shows %q", s, w)
				}
			}
		}
		if c.Type == "row" {
			step = c
		}
	}
	if step.Title == "" {
		t.Fatalf("card has no next step: %s", text(v))
	}
	return stats, step
}

// The Overview card says where things stand at a glance (connected, needing the user with the first one named, usable
// by the assistant) and the one next step, for no account, one needing the user, none handed over and all well.
func TestOverviewCardAtAGlance(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))

	// no account: it says so, with no counts of nothing, and the step opens the home
	v := renderCard(t, h)
	stats, step := cardParts(t, v)
	if len(stats) != 0 || !strings.Contains(text(v), "还没有接入任何账号") || step.Title != "连接你的第一个账号" ||
		step.Action == nil || step.Action.Push == nil || step.Action.Push.UI != HomeUI || strings.Contains(step.Subtitle, "下面") {
		t.Errorf("card (no account): %s", text(v))
	}

	for i, n := range []string{"写作助手", "资料助手", "翻译助手"} {
		act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair,
			map[string]string{fieldCredential: fmt.Sprintf("echo-synthetic-%04d", i+1), fieldName: n})
	}
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())

	// none handed to the assistant: the step is 助手指挥
	v = renderCard(t, h)
	stats, step = cardParts(t, v)
	if stats["已接入"].Value != "3 个" || stats["要你处理"].Value != "0 个" || stats["Persona 助手能用"].Value != "0 个" ||
		stats["Persona 助手能用"].Detail != "还没交给它" || step.Title != "让 Persona 助手能帮你看这些账号" ||
		!strings.Contains(step.Subtitle, "助手指挥") {
		t.Errorf("card (none handed over): %s", text(v))
	}

	// one needs the user: counted, named, and the step opens that account
	for _, a := range status.Accounts {
		setControl(t, club, a.AccountID, core.ControlRead)
	}
	broken := status.Accounts[1]
	_ = space.Store().UpdateAccount(context.Background(), broken.AccountID, storeStatus(protocol.AccountError, "AUTH_FAILED"), 1)
	v = renderCard(t, h)
	stats, step = cardParts(t, v)
	name := accountTitle("zh", broken)
	if need := stats["要你处理"]; need.Value != "1 个" || need.Tone != wire.ToneCaution || need.Detail != "「"+name+"」" {
		t.Errorf("card (one needs you) counts: %+v", stats)
	}
	if stats["Persona 助手能用"].Value != "3 个" || stats["Persona 助手能用"].Detail != "只读" {
		t.Errorf("card (one needs you) assistant: %+v", stats)
	}
	if step.Title != "先修好「"+name+"」" || step.Action == nil || step.Action.Push == nil ||
		step.Action.Push.UI != HomeUI || !strings.Contains(step.Action.Push.State, broken.AccountID) {
		t.Errorf("card (one needs you) step: %+v", step)
	}

	// all well: nothing needs the user, and the step says where the chats are
	_ = space.Store().UpdateAccount(context.Background(), broken.AccountID, storeStatus(protocol.AccountActive, ""), 1)
	setControl(t, club, broken.AccountID, core.ControlConfirm)
	v = renderCard(t, h)
	stats, step = cardParts(t, v)
	if need := stats["要你处理"]; need.Value != "0 个" || need.Tone != wire.TonePositive {
		t.Errorf("card (all well) counts: %+v", stats)
	}
	if stats["Persona 助手能用"].Detail != "1 个可代发" || step.Title != "没有要你处理的事" ||
		!strings.Contains(step.Subtitle, "Agents") || step.Action == nil || step.Action.Push.UI != HomeUI {
		t.Errorf("card (all well): %s", text(v))
	}
}

// viewWithoutState is a view as drawn, without the state it carries back.
func viewWithoutState(v *wire.View) string {
	c := *v
	c.State = ""
	return text(&c)
}

// The settings page's two links, 我的 AI 助手 and 账号, open different views: the home, and the accounts list titled
// 账号 without the home's introduction and state; an account's page still opens from either (old links keep working).
func TestSettingsLinksOpenDifferentViews(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
	check := func(when string) {
		home := renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{})
		list := renderUI(t, h, AccountsUI, wire.PlacementPluginPage, pageState{})
		noJargon(t, "accounts ("+when+")", list)
		if viewWithoutState(home) == viewWithoutState(list) {
			t.Fatalf("%s: the two links draw the same view", when)
		}
		if home.Title != homeTitle || list.Title != accountsTitle {
			t.Errorf("%s: titles %q and %q", when, home.Title, list.Title)
		}
		if sectionIndex(list, "现在的状态") >= 0 || strings.Contains(text(list), "接进 Persona") || strings.Contains(text(list), "下一步") {
			t.Errorf("%s: 账号 repeats the home's introduction or state: %s", when, text(list))
		}
		if sectionIndex(home, "现在的状态") < 0 || sectionIndex(list, "已接入的账号") != 1 || !strings.Contains(text(list), `"key":"add"`) {
			t.Errorf("%s: home has no state, or 账号 does not start with the accounts and offer to add one: %s", when, text(list))
		}
		if top := list.Sections[0]; top.Title != "" || len(top.Items) != 1 || top.Items[0].Text != accountsAbout("zh-Hans") {
			t.Errorf("%s: 账号 does not say first what it is for: %+v", when, top)
		}
	}
	check("no account")
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential, fieldName: "演示一号"})
	check("one account")

	list := renderUI(t, h, AccountsUI, wire.PlacementPluginPage, pageState{})
	row := list.Sections[1].Items[0]
	if row.Title != "演示一号" || row.Action == nil || row.Action.Push == nil || row.Action.Push.UI != AccountsUI {
		t.Fatalf("账号 does not list the account opening in itself: %+v", row)
	}
	var st pageState
	_ = json.Unmarshal([]byte(row.Action.Push.State), &st)
	if acct := renderUI(t, h, AccountsUI, wire.PlacementPluginPage, st); !strings.Contains(text(acct), "助手指挥") {
		t.Errorf("an account's page does not open from 账号: %s", text(acct))
	}
}

// Every place that tells people where the home is stays true when the navigation keeps it under 更多: the manifest's
// description, the assistant's guides and the install guide.
func TestWhereTheHomeIsSaysMore(t *testing.T) {
	var m struct{ Description string }
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile("../../docs/persona-install.md")
	if err != nil {
		t.Fatal(err)
	}
	var cs wire.ConnectorStatus
	h, _ := start(t)
	if err := h.Call("connector.status", wire.ConnectorParams{Contribution: Connector}, &cs); err != nil {
		t.Fatal(err)
	}
	for where, s := range map[string]string{
		"description":        m.Description,
		"guide (no account)": grantGuide(nil),
		"guide (hidden)":     grantGuide([]string{"演示账号"}),
		"connector status":   strings.Join(cs.Guide, "\n"),
	} {
		for _, want := range []string{homeTitle, "导航", "「更多」", "卡片"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s does not say %q: %s", where, want, s)
			}
		}
		if strings.Contains(s, "左侧") || strings.Contains(s, "下面的") {
			t.Errorf("%s says where the home is in words not true on every app: %s", where, s)
		}
	}
	if !strings.Contains(string(doc), "**更多**") || strings.Contains(string(doc), "All three open the same home") {
		t.Errorf("docs/persona-install.md does not say the entry may be under 更多")
	}
}
