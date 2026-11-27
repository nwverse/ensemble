package extension

import (
	"context"
	"strings"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider/browser"
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
	browser.SiteDots,
}

// browserPairView adds an account through a computer's browser: the computer to use and what to do on it first.
// When the browser accounts are switched off in the settings it says so, and where, instead of offering computers.
func (a *accounts) browserPairView(ctx context.Context, l lang, st pageState) *wire.View {
	name := providerName(l, st.Provider)
	title := l.t("添加 ", "Add ") + name
	site, known := browserSite(st.Provider)
	if !known || !a.available(st.Provider) {
		return unavailableView(l, title, l.t("这类账号要借用电脑上的浏览器，这个功能还没打开。先到 ", "These accounts use a computer's browser, which is switched off. First switch on ")+
			settingsPath(l)+l.t(" 里打开「"+settingBrowser+"」，保存后再回来添加。", " → \""+settingBrowser+"\", save, then come back."))
	}
	v := &wire.View{Title: title}
	var choices []wire.Choice
	if a.browser != nil {
		machines, err := a.browser.Machines(ctx)
		if err != nil {
			v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: l.t("查不到你的电脑：", "Could not list your computers: ") + reason(l, err, "")}}}}
			return v
		}
		for _, m := range machines {
			label := m.Name
			if label == "" {
				label = m.MachineID
			}
			if m.Online {
				label += l.t(" · 在线", " · online")
			} else {
				label += l.t(" · 离线", " · offline")
			}
			choices = append(choices, wire.Choice{Value: m.MachineID + "|" + m.Backend, Title: label})
		}
	}
	host := siteHost(site.AppURL)
	if len(choices) == 0 {
		v.Sections = []wire.Section{{Items: []wire.Component{
			{Type: "empty", Icon: "link", Title: l.t("还没有能用的电脑", "No computer to use yet"),
				Detail: l.t("需要一台已经连上 Persona、并且装好了 Persona 能控制的浏览器（"+browserName+"）的电脑（比如家里的 Mac），在这个浏览器里登录好 ", "You need a computer connected to Persona (your Mac at home, say) with the browser Persona controls ("+browserName+") installed, and ") +
					host + l.t("。弄好后回到这里刷新。", " signed in in it. Then come back and refresh.")},
		}}}
		return v
	}
	v.Sections = []wire.Section{
		{Title: l.t("先做这一步", "First"), Items: []wire.Component{{Type: "text",
			Text: l.t("在你要用的那台电脑上，打开 Persona 控制的那个浏览器（"+browserName+"，不是你平时用的其他浏览器），在里面登录 ", "On the computer you will use, open the browser Persona controls ("+browserName+", not another browser you use) and sign in to ") + host +
				l.t("。登录好了再回来，选这台电脑，点「检查登录并添加」。", " in it. Then come back, choose the computer and tap Check sign-in and add.")}}},
		{Footer: l.t("Persona 会在这个浏览器里另开一个专用的空间来收发消息，用的就是你在这个浏览器里的登录；密码和登录信息不会离开那台电脑。那台电脑关机或断网时，这个账号会暂时用不了。",
			"Persona reads and writes in a space of its own inside that browser, using your sign-in there; passwords and sign-ins never leave that computer. While it is off or offline, this account cannot be used."),
			Items: []wire.Component{
				{Type: "field", Name: fieldMachine, Label: l.t("用哪台电脑", "Which computer"), Kind: wire.FieldKindChoice, Choices: choices,
					Value: choices[0].Value},
				{Type: "field", Name: fieldName, Label: l.t("名字（可不填）", "Name (optional)"), Kind: wire.FieldKindText,
					Detail: l.t("给它起个好认的名字，比如「工作用 ", "A name you will recognize, like \"Work ") + name + l.t("」，以后也能改。", "\"; you can change it later.")},
				{Type: "button", Title: l.t("检查登录并添加", "Check sign-in and add"), Action: &wire.Action{ID: actBrowserPair, Submit: true}},
			}},
	}
	return v
}

func parseMachineChoice(raw string) browser.Credential {
	raw = strings.TrimSpace(raw)
	mid, backend, _ := strings.Cut(raw, "|")
	return browser.Credential{MachineID: strings.TrimSpace(mid), Backend: strings.TrimSpace(backend)}
}

func browserProviderLabel(p string) (string, bool) {
	s, ok := browserSite(p)
	return s.Label, ok
}

func browserSite(p string) (browser.Site, bool) {
	for _, s := range browserProviders {
		if s.Provider == p {
			return s, true
		}
	}
	return browser.Site{}, false
}

func (a *accounts) pairBrowserAccount(ctx context.Context, providerName, rawChoice, displayName string) (protocol.Account, error) {
	cred := parseMachineChoice(rawChoice)
	if cred.MachineID == "" {
		return protocol.Account{}, protocol.Invalid("choose a computer")
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
