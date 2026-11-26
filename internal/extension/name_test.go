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
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
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
