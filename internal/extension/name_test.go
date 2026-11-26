package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/cue"
	"github.com/nwverse/ensemble/internal/provider/echo"
)

// oldProductName is the product's former display name. Ids (personal-agent-club, pac, club) keep it; nothing a person
// or the assistant reads does.
const oldProductName = "personal agent club"

// manifestDisplay is every field of the manifest the hub shows a person.
func manifestDisplay(t *testing.T) (name string, shown []string) {
	t.Helper()
	var m struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Contributes struct {
			AgentSources []struct {
				Name string `json:"name"`
			} `json:"agent_sources"`
			Connectors []struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"connectors"`
			UI []wire.UIItem `json:"ui"`
		} `json:"contributes"`
		Configuration struct {
			Properties map[string]struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	shown = append(shown, m.Name, m.Description)
	for _, s := range m.Contributes.AgentSources {
		shown = append(shown, s.Name)
	}
	for _, c := range m.Contributes.Connectors {
		shown = append(shown, c.Name, c.Description)
	}
	for _, u := range m.Contributes.UI {
		shown = append(shown, u.Title)
	}
	for _, p := range m.Configuration.Properties {
		shown = append(shown, p.Title, p.Description)
	}
	return m.Name, shown
}

// The product is shown as Ensemble everywhere a person or the assistant reads its name: the manifest's display
// fields, every page in Chinese and English, and the assistant's tool texts. The pages' settings path names the
// product as the manifest does.
func TestProductIsShownAsEnsemble(t *testing.T) {
	name, shown := manifestDisplay(t)
	if name != "Ensemble" {
		t.Fatalf("manifest name %q", name)
	}
	if productName != name {
		t.Fatalf("the pages name the product %q, the manifest %q", productName, name)
	}
	for _, s := range shown {
		if strings.Contains(strings.ToLower(s), oldProductName) {
			t.Errorf("the manifest shows %q", s)
		}
	}

	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	// browser accounts and the main assistant off, so the pages send people to the settings by path
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())),
		json.RawMessage(`{"demo_echo":true,"browser_providers":false,"assistant_runtime":false}`))

	check := func(where, s string) {
		t.Helper()
		if strings.Contains(strings.ToLower(s), oldProductName) {
			t.Errorf("%s shows the old name: %s", where, s)
		}
	}
	pages := func(when string) []pageState {
		sts := []pageState{{}, {Page: pageGrok}, {Page: pageBrowserPair, Provider: "claude_duet"}}
		for _, p := range []string{"cue", "groq", "muse2api", echo.Name} {
			sts = append(sts, pageState{Page: pagePair, Provider: p})
		}
		if when != "" {
			space, _ := club.Space(context.Background(), core.DefaultSpace)
			status, _ := space.Status(context.Background())
			sts = append(sts, pageState{Page: pageAccount, Account: status.Accounts[0].AccountID})
		}
		return sts
	}
	renderAll := func(when string) (all string) {
		for _, locale := range []string{"zh-Hans", "en"} {
			for _, ui := range []struct{ id, placement string }{
				{HomeUI, wire.PlacementNavPage}, {AccountsUI, wire.PlacementPluginPage}, {HomeCardUI, wire.PlacementOverviewCard},
			} {
				for _, st := range pages(when) {
					if ui.id == HomeCardUI && st.Page != pageList {
						continue
					}
					var v wire.View
					if err := h.Call("ui.render", wire.UIParams{Contribution: ui.id, Placement: ui.placement, Size: "full",
						UIVersion: 11, Locale: locale, State: stateOf(st)}, &v); err != nil {
						t.Fatalf("ui.render %s %s %+v: %v", locale, ui.id, st, err)
					}
					check(locale+" "+ui.id+" "+st.Page+" "+st.Provider+when, text(&v))
					all += text(&v)
				}
			}
		}
		return all
	}

	// the assistant's texts before anything is paired
	var tl wire.ConnectorTools
	if err := h.Call("connector.tools.list", wire.ConnectorParams{Contribution: Connector}, &tl); err != nil {
		t.Fatal(err)
	}
	var al wire.ConnectorTools
	if err := h.Call("connector.actions.list", wire.ConnectorParams{Contribution: Connector}, &al); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(append(tl.Tools, al.Tools...))
	check("tools", string(raw))
	if !strings.Contains(string(raw), "Ensemble 里的账号") {
		t.Errorf("the list tool does not name Ensemble: %s", raw)
	}
	var cs wire.ConnectorStatus
	if err := h.Call("connector.status", wire.ConnectorParams{Contribution: Connector}, &cs); err != nil {
		t.Fatal(err)
	}
	if guide := strings.Join(cs.Guide, "\n"); !strings.Contains(guide, "设置 › 扩展 › Ensemble › 账号") {
		t.Errorf("not-configured guide: %q", guide)
	}
	listOut := func() string {
		var res wire.ConnectorCallResult
		if err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: Connector, Name: toolList, Args: json.RawMessage(`{}`)}, &res); err != nil {
			t.Fatal(err)
		}
		return res.Text
	}
	check("list (none paired)", listOut())

	all := renderAll("")
	for _, want := range []string{"设置 › 扩展 › Ensemble › 设置", "Settings › Extensions › Ensemble › Settings"} {
		if !strings.Contains(all, want) {
			t.Errorf("no page says %q", want)
		}
	}

	// paired, not handed to the assistant: the list tool says where to hand it over
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential, fieldName: "演示一号"})
	if out := listOut(); !strings.Contains(out, "设置 › 扩展 › Ensemble › 账号") {
		t.Errorf("list (paired, not handed over): %q", out)
	}
	renderAll(" (one account)")
}

// The hub lists every contribution by its name on the extension's settings page (and the connector under Settings ›
// Connectors), so the agent source and the connector each say what they are, keep the product's name, and never read
// alike. Their ids do not change.
func TestContributionNamesSayWhatEachIs(t *testing.T) {
	var m wire.Manifest
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Contributes.AgentSources) != 1 || len(m.Contributes.Connectors) != 1 {
		t.Fatalf("%+v", m.Contributes)
	}
	source, conn := m.Contributes.AgentSources[0], m.Contributes.Connectors[0]
	if source.ID != Contribution || conn.ID != Connector {
		t.Fatalf("ids changed: %q %q", source.ID, conn.ID)
	}
	if source.Name != productName+" 的账号和对话" || conn.Name != "让 Persona 助手使用 "+productName {
		t.Fatalf("names: %q, %q", source.Name, conn.Name)
	}
	if source.Name == conn.Name || source.Name == m.Name || conn.Name == m.Name {
		t.Fatalf("contributions read alike: %q %q %q", m.Name, source.Name, conn.Name)
	}
}

// Cue's add row and pairing page say which service it is (a Manus account's agents, through the Manus API, the only
// thing provider/cue talks to) and where its key comes from, in Chinese and English; its brand stays "Cue".
func TestCueSaysItIsManus(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(cue.New(cue.Options{Origin: "http://127.0.0.1:1"})), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{}`))
	for _, c := range []struct{ locale, row, intro, key, footer string }{
		{"zh-Hans", "你在 Manus 上的智能体：到 Manus 网站复制你的 API Key 粘贴进来。", "Cue 就是你在 Manus 上的智能体，这里通过 Manus 的官方接口收发消息。",
			"你 Manus 账号的 API Key：到 Manus 网站复制，整串粘贴到这里。", "只会发给 Manus（api.manus.ai）"},
		{"en", "Your agents on Manus: copy your API key from Manus and paste it.", "Cue means your agents on Manus, reached through Manus's official API.",
			"Your Manus account's API key: copy it from Manus and paste all of it.", "sent only to Manus (api.manus.ai)"},
	} {
		var row *wire.Component
		for _, s := range renderIn(t, h, c.locale, pageState{Page: pageAdd}).Sections {
			for i := range s.Items {
				if s.Items[i].Key == "pair-"+cue.Name {
					row = &s.Items[i]
				}
			}
		}
		if row == nil || row.Title != "Cue" || row.Subtitle != c.row {
			t.Errorf("%s add row: %+v", c.locale, row)
		}
		page := renderIn(t, h, c.locale, pageState{Page: pagePair, Provider: cue.Name})
		s := page.Sections[0]
		if page.Title != "添加 Cue" && page.Title != "Add Cue" || s.Items[0].Type != "text" || s.Items[0].Text != c.intro ||
			s.Items[1].Name != fieldCredential || s.Items[1].Detail != c.key || !strings.Contains(s.Footer, c.footer) {
			t.Errorf("%s pairing page: %s", c.locale, text(page))
		}
	}
}

// The hub lists the UI contributions by their titles too: the home and its Overview card must not read alike, and the
// card's title is the one its view draws as its header on Overview, where it must still read well.
func TestUIContributionTitlesNeverReadAlike(t *testing.T) {
	var m wire.Manifest
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	titles := map[string]string{}
	seen := map[string]string{}
	for _, u := range m.Contributes.UI {
		titles[u.ID] = u.Title
		if other, dup := seen[u.Title]; dup {
			t.Errorf("%s and %s are both titled %q", other, u.ID, u.Title)
		}
		seen[u.Title] = u.ID
	}
	if titles[HomeUI] != homeTitle || titles[HomeCardUI] != homeCardTitle || titles[AccountsUI] != accountsTitle {
		t.Fatalf("titles: %v", titles)
	}
	// the card names the home it opens, then what it is
	if !strings.HasPrefix(homeCardTitle, homeTitle+" · ") || len([]rune(homeCardTitle)) > 40 {
		t.Fatalf("card title %q", homeCardTitle)
	}
	h, _ := start(t)
	for _, c := range []struct{ locale, want string }{{"zh-Hans", homeCardTitle}, {"en", homeCardTitle + " (My AI assistants at a glance)"}} {
		var v wire.View
		if err := h.Call("ui.render", wire.UIParams{Contribution: HomeCardUI, Placement: wire.PlacementOverviewCard, Size: "medium",
			UIVersion: 11, Locale: c.locale}, &v); err != nil {
			t.Fatal(err)
		}
		if v.Title != c.want {
			t.Errorf("%s card header %q, want %q", c.locale, v.Title, c.want)
		}
	}
	// the manifest's description sends people to the card by the title it now has
	if !strings.Contains(m.Description, "「"+homeCardTitle+"」卡片") {
		t.Errorf("description: %s", m.Description)
	}
}

// The settings page's permission lines (network hosts, the agent runtime's socket) are the hub's words: the manifest's
// own text never names the plumbing, and its network entries are bare host names or config: forms with no label.
func TestManifestOwnWordsNameNoPlumbing(t *testing.T) {
	_, shown := manifestDisplay(t)
	for _, s := range shown {
		for _, w := range []string{"runtime.sock", "socket", "PERSONA_RUNTIME_SOCKET", "agent_runtimes"} {
			if strings.Contains(s, w) {
				t.Errorf("the manifest says %q: %s", w, s)
			}
		}
	}
	var m struct {
		Permissions struct {
			Network []string `json:"network"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	for _, n := range m.Permissions.Network {
		if strings.ContainsAny(n, " （(") {
			t.Errorf("network entry %q", n)
		}
	}
}
