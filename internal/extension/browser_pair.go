package extension

import (
	"context"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/browser"
)

const (
	pageBrowserPair = "browser_pair"
	actBrowserPair  = "browser_pair"
	fieldMachine    = "machine_id"
)

var browserProviders = []browser.Site{
	browser.SiteMuse,
	browser.SiteClaudeDuet,
	browser.SiteGeminiTasks,
}

func (a *accounts) browserPairView(ctx context.Context, l lang, st pageState) *wire.View {
	name := providerName(l, st.Provider)
	v := &wire.View{Title: l.t("通过浏览器配对 ", "Pair via browser: ") + name}
	var choices []wire.Choice
	if a.browser != nil {
		machines, err := a.browser.Machines(ctx)
		if err != nil {
			v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
			return v
		}
		for _, m := range machines {
			label := m.Name
			if label == "" {
				label = m.MachineID
			}
			if m.Backend != "" {
				label += " (" + m.Backend + ")"
			}
			if !m.Online {
				label += l.t(" · 离线", " · offline")
			}
			choices = append(choices, wire.Choice{Value: m.MachineID + "|" + m.Backend, Title: label})
		}
	}
	if len(choices) == 0 {
		v.Sections = []wire.Section{{Items: []wire.Component{
			{Type: "empty", Icon: "link", Title: l.t("没有可用的浏览器节点", "No browser-enabled computer"),
				Detail: l.t("请先在某台已配对的 Mac 上开启 ego lite 插件（或接入支持 BrowserBackend 的浏览器），并保持登录目标网页。",
					"Enable the ego lite plugin (or another BrowserBackend) on a paired computer and sign in to the site first.")},
		}}}
		return v
	}
	v.Sections = []wire.Section{{
		Footer: l.t("不会导出或上传任何 Cookie：仅在所选电脑的隔离浏览器空间里访问已登录页面。",
			"No cookies are exported or uploaded: the site is accessed inside an isolated browser space on the selected computer."),
		Items: []wire.Component{
			{Type: "segmented", Name: fieldMachine, Choices: choices, Value: choices[0].Value},
			{Type: "field", Name: fieldName, Label: l.t("名称（可选）", "Name (optional)"), Kind: wire.FieldKindText},
			{Type: "button", Title: l.t("验证并配对", "Verify and pair"), Action: &wire.Action{ID: actBrowserPair, Submit: true}},
		},
	}}
	return v
}

func parseMachineChoice(raw string) browser.Credential {
	raw = strings.TrimSpace(raw)
	mid, backend, _ := strings.Cut(raw, "|")
	return browser.Credential{MachineID: strings.TrimSpace(mid), Backend: strings.TrimSpace(backend)}
}

func browserProviderLabel(p string) (string, bool) {
	for _, s := range browserProviders {
		if s.Provider == p {
			return s.Label, true
		}
	}
	return "", false
}

func (a *accounts) pairBrowserAccount(ctx context.Context, providerName, rawChoice, displayName string) (protocol.Account, error) {
	cred := parseMachineChoice(rawChoice)
	if cred.MachineID == "" {
		return protocol.Account{}, protocol.Invalid("choose a browser-enabled computer")
	}
	var acct protocol.Account
	p := protocol.AddAccountParams{
		Provider:    providerName,
		Credential:  browser.EncodeCredential(cred),
		DisplayName: strings.TrimSpace(displayName),
	}
	if err := call(ctx, a.club, protocol.MethodClubAccountsAdd, p, &acct); err != nil {
		return protocol.Account{}, err
	}
	return acct, nil
}
