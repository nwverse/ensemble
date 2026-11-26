package extension

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf16"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/browser"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
	"github.com/nwverse/personal-agent-club/internal/provider/muse2api"
)

// productName is the product as the pages name it. It must read as the manifest's "name" does, since the pages send
// people to Settings › Extensions › <name>; a rename of the product is this one line and the manifest's.
const productName = "Personal Agent Club"

// The extension's pages (contributes.ui). They all draw the same home, the page a first-time user lands on: what this
// is, where it stands, the one thing to do next, then the accounts and the ways to add one; an account's own page
// and the pairing pages open from there.
//
//   - HomeUI is the entry in the main navigation (nav_page).
//   - HomeCardUI is the card on Overview, and in Settings › Extensions on the phones (overview_card): the state and
//     the next step in two lines, opening HomeUI.
//   - AccountsUI is Settings › Extensions › <name> › 账号 (plugin_page), which the assistant's guide points at.
//
// They list the paired accounts and pair, pause, resume, sync, rename and unpair them through the core's club.*
// methods, the same ones the serve shell's admin token reaches.
//
// Credentials go in and never come out: the API key field is a secret field (the apps never echo it and the
// extension cannot prefill it), the key goes straight to club.accounts.add and is kept nowhere in the page's state,
// and nothing the page draws or logs carries it.
const (
	HomeUI     = "home"
	HomeCardUI = "home-card"
	AccountsUI = "accounts"
)

// settingsPath is where the extension's settings are, for the pages to send people there (a page cannot link to
// the host's settings page).
func settingsPath(l lang) string {
	return l.t("设置 › 扩展 › "+productName+" › 设置", "Settings › Extensions › "+productName+" › Settings")
}

// The settings' titles as the manifest has them, for the pages that send people to one. The manifest has one
// language (Chinese), so the English pages quote these titles too: they are what the settings page shows.
const (
	settingBrowser  = "用电脑上的浏览器连接网页版助手"
	settingRuntime  = "让这些账号可以当主助手"
	settingMuse2API = "自建 Muse 转接服务的地址"
)

// homeTitle is the home's title, as the manifest's navigation entry has it (one language there too).
const homeTitle = "我的 AI 助手"

// browserName is the browser Persona drives on a computer (browser.run): the one a browser account must be signed
// in to. Pairing runs in a task space of its own inside it, with that browser's sign-ins.
const browserName = "ego lite"

// Pages of the accounts UI (pageState.Page).
const (
	pageList    = ""
	pageAccount = "account"
	pagePair    = "pair"
	pageGrok    = "grok"
)

// Actions of the accounts UI.
const (
	actSync             = "sync"
	actPause            = "pause"
	actResume           = "resume"
	actRemove           = "remove"
	actPurge            = "purge"
	actControl          = "control"
	actAssistantBackend = "assistant_backend"
	actQuotaDisplay     = "quota_display"
	actRename           = "rename"
	actPair             = "pair"
	actGrokStart        = "grok_start"
	actGrokCheck        = "grok_check"
	actGrokStop         = "grok_cancel"
	actTakeover         = "takeover"
	actWake             = "wake"
	actRecheck          = "recheck"
)

// Field names on the pages.
const (
	fieldCredential       = "credential"
	fieldName             = "display_name"
	fieldControl          = "assistant_control"
	fieldAssistantBackend = "assistant_backend"
	fieldQuotaDisplay     = "quota_display"
)

// What the core takes (core/space.go): a credential of 8 to 8192 characters, a name of at most 80 UTF-16 units. The
// pages check them first so the refusal comes in words.
const (
	minCredential  = 8
	maxCredential  = 8192
	maxDisplayName = 80
)

// pageState is the page's state as the apps send it back: which page, about what, and the last action's outcome.
// Never a credential.
type pageState struct {
	Page     string `json:"p,omitempty"`
	Account  string `json:"a,omitempty"`
	Provider string `json:"v,omitempty"`
	Login    string `json:"l,omitempty"`
	LoginURL string `json:"u,omitempty"`
	Note     string `json:"n,omitempty"`
	NoteTone string `json:"t,omitempty"`
}

// Providers paired by pasting a key on the accounts page. Grok Bot signs in through its own page instead, and
// muse2api (a key too) has its own row, since it needs its server's address first.
var keyProviders = []string{"cue", "groq"}

const grokBot = "grok_bot"

// Reasons a Grok Bot sign-in poll fails for now and may still succeed: the sign-in is kept, and asked again.
var retryableLogin = []string{"NETWORK_ERROR", "UPSTREAM_ERROR", "RATE_LIMITED"}

// accountsPage is the accounts UI. runtimeOn says whether the main assistant bridge runs (the setting
// assistant_runtime), which decides whether an account's 主助手 switch does anything.
func accountsPage(club *core.Club, hub *atomic.Pointer[ext.Hub], drv browser.Driver, runtimeOn *atomic.Bool) ext.Page {
	a := &accounts{club: club, hub: hub, browser: drv, runtimeOn: runtimeOn}
	return ext.Page{Render: a.render, Action: a.action}
}

type accounts struct {
	club      *core.Club
	hub       *atomic.Pointer[ext.Hub]
	browser   browser.Driver
	runtimeOn *atomic.Bool
}

func (a *accounts) runtimeEnabled() bool { return a.runtimeOn == nil || a.runtimeOn.Load() }

func (a *accounts) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	var st pageState
	_ = r.DecodeState(&st)
	return a.view(ctx, r, st), nil
}

// ui is the page a request is for, so the pages it opens stay in it.
func ui(r *ext.UIRequest) string {
	switch r.Contribution {
	case HomeUI, AccountsUI:
		return r.Contribution
	}
	return AccountsUI
}

func (a *accounts) view(ctx context.Context, r *ext.UIRequest, st pageState) *wire.View {
	l := lang(r.Locale)
	var v *wire.View
	switch {
	case r.Contribution == HomeCardUI || r.Placement == wire.PlacementOverviewCard:
		v = a.cardView(ctx, l)
	case st.Page == pageAccount:
		v = a.accountView(ctx, r, l, &st)
	case st.Page == pagePair:
		v = a.pairView(l, st)
	case st.Page == pageGrok:
		v = a.grokView(ctx, l, &st)
	case st.Page == pageBrowserPair:
		v = a.browserPairView(ctx, l, st)
	case st.Page == pageDone:
		v = a.doneView(ctx, l, st)
	default:
		v = a.listView(ctx, r, l)
	}
	if st.Note != "" {
		tone := st.NoteTone
		if tone == "" {
			tone = wire.ToneNeutral
		}
		v.Sections = append([]wire.Section{{Items: []wire.Component{{Type: "row", Title: st.Note,
			Badge: noteBadge(l, tone)}}}}, v.Sections...)
	}
	v.State = ext.EncodeState(st)
	return v
}

// noteBadge says what kind of outcome a note is.
func noteBadge(l lang, tone string) *wire.Badge {
	switch tone {
	case wire.TonePositive:
		return &wire.Badge{Text: l.t("完成", "Done"), Tone: tone}
	case wire.ToneDanger:
		return &wire.Badge{Text: l.t("没成功", "Failed"), Tone: tone}
	case wire.ToneCaution:
		return &wire.Badge{Text: l.t("请注意", "Note"), Tone: tone}
	}
	return &wire.Badge{Text: l.t("提示", "Note"), Tone: tone}
}

// overview is where the accounts stand, for the home and its card.
type overview struct {
	accounts []protocol.Account
	broken   []protocol.Account // need the user: signed out, or failing
	paused   int
	read     int // the assistant may read
	send     int // ... and send, asking each time (or not)
	backend  int // main assistant
}

func overviewOf(accts []protocol.Account) overview {
	o := overview{accounts: accts}
	for _, acct := range accts {
		switch {
		case acct.Status == protocol.AccountError:
			o.broken = append(o.broken, acct)
		case acct.Status == protocol.AccountPaused:
			o.paused++
		}
		switch core.Control(acct.AssistantControl) {
		case core.ControlRead:
			o.read++
		case core.ControlConfirm, core.ControlAuto:
			o.send++
		}
		if acct.AssistantBackend {
			o.backend++
		}
	}
	return o
}

// stateLine is the state in one line: how many accounts, how many need the user, how many the assistant may use.
func (o overview) stateLine(l lang) string {
	if len(o.accounts) == 0 {
		return l.t("还没有接入任何账号。", "No accounts yet.")
	}
	s := l.t(fmt.Sprintf("已接入 %d 个账号", len(o.accounts)), plural(len(o.accounts), "account"))
	if n := len(o.broken); n > 0 {
		s += l.t(fmt.Sprintf("，其中 %d 个要你处理", n), fmt.Sprintf(", %d need you", n))
	}
	if o.paused > 0 {
		s += l.t(fmt.Sprintf("，%d 个已暂停", o.paused), fmt.Sprintf(", %d paused", o.paused))
	}
	if o.read+o.send == 0 {
		return s + l.t("；Persona 助手现在一个都用不了。", "; the Persona assistant can use none of them yet.")
	}
	return s + l.t(fmt.Sprintf("；Persona 助手能用其中 %d 个。", o.read+o.send),
		fmt.Sprintf("; the Persona assistant can use %d of them.", o.read+o.send))
}

// plural is "1 account", "2 accounts".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// nextStep is the one thing most worth doing now: add the first account, fix a broken one, or let the assistant
// use one. Nothing when all is well. push says whether the step opens a page (an account's); otherwise it points
// at the rows below.
func (o overview) nextStep(l lang, at string) (wire.Component, bool) {
	switch {
	case len(o.accounts) == 0:
		return wire.Component{Type: "row", Icon: "plus", Title: l.t("连接你的第一个账号", "Connect your first account"),
			Subtitle: l.t("在下面「添加账号」里选你常用的那个服务，点进去照提示做，一两分钟就好。",
				"Pick the service you use under Add an account below and follow the steps; it takes a minute or two.")}, true
	case len(o.broken) > 0:
		acct := o.broken[0]
		return wire.Component{Type: "row", Key: "next-" + acct.AccountID, Icon: "warning",
			Title:    l.t("先修好「", "First fix ") + accountTitle(l, acct) + l.t("」", ""),
			Subtitle: problemWords(l, acct) + l.t(" 点这里打开它。", " Tap to open it."),
			Badge:    &wire.Badge{Text: l.t("要处理", "Needs you"), Tone: wire.ToneCaution},
			Action:   openAccount(l, at, acct)}, true
	case o.read+o.send == 0:
		acct := o.accounts[0]
		return wire.Component{Type: "row", Key: "next-" + acct.AccountID, Icon: "robot",
			Title: l.t("让 Persona 助手能帮你看这些账号", "Let the Persona assistant use these accounts"),
			Subtitle: l.t("为了安全，新账号默认不让 Persona 助手碰。点开一个账号，在「助手指挥」里选「只读」（它能帮你看对话、做总结）或「可代发」（还能替你发消息，每次先问你）。",
				"New accounts are off limits to the Persona assistant. Open one and set Assistant control to Read (it can read and sum up) or Confirm (it can also send, asking you each time)."),
			Action: openAccount(l, at, acct)}, true
	}
	return wire.Component{}, false
}

func openAccount(l lang, at string, acct protocol.Account) *wire.Action {
	return &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: accountTitle(l, acct),
		State: ext.EncodeState(pageState{Page: pageAccount, Account: acct.AccountID})}}
}

// cardView is the home in two lines, for Overview.
func (a *accounts) cardView(ctx context.Context, l lang) *wire.View {
	v := &wire.View{Title: l.t(homeTitle, homeTitle+" (My AI assistants)"), RefreshAfter: 300}
	accts, err := a.accounts(ctx)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	o := overviewOf(accts)
	items := []wire.Component{{Type: "text", Text: o.stateLine(l)}}
	if step, ok := o.nextStep(l, HomeUI); ok {
		items = append(items, wire.Component{Type: "text", Style: "secondary", Text: l.t("下一步：", "Next: ") + step.Title})
	}
	v.Sections = []wire.Section{{Items: items}}
	return v
}

func (a *accounts) account(ctx context.Context, id string) (protocol.Account, error) {
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return protocol.Account{}, err
	}
	return space.Account(ctx, id)
}

func (a *accounts) accounts(ctx context.Context) ([]protocol.Account, error) {
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return nil, err
	}
	status, err := space.Status(ctx)
	if err != nil {
		return nil, err
	}
	return status.Accounts, nil
}

func (a *accounts) listView(ctx context.Context, r *ext.UIRequest, l lang) *wire.View {
	at := ui(r)
	v := &wire.View{Title: l.t(homeTitle, homeTitle+" (My AI assistants)"), RefreshAfter: 60}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	status, err := space.Status(ctx)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	o := overviewOf(status.Accounts)
	about := wire.Section{Items: []wire.Component{{Type: "text",
		Text: l.t("这里把你在 Grok、Muse、Claude、Gemini、ChatGPT 等 AI 服务上的账号接进 Persona。接好后，它们的对话会出现在 Persona 的「Agents」页，你在 Persona 里就能看、能直接给它们发消息。你还可以允许 Persona 助手替你读这些对话、替你发消息，每次发之前都会先在手机上问你。",
			"This brings your accounts at AI services such as Grok, Muse, Claude, Gemini and ChatGPT into Persona. Their chats then show on Persona's Agents page, where you read them and write to them. You can also let the Persona assistant read them and write for you, asking you on your phone before each message.")}}}
	state := wire.Section{Title: l.t("现在的状态", "Where things stand"),
		Items: []wire.Component{{Type: "text", Text: o.stateLine(l)}}}
	if step, ok := o.nextStep(l, at); ok {
		state.Items = append(state.Items, wire.Component{Type: "text", Style: "secondary", Text: l.t("下一步", "Next")}, step)
	}
	paired := wire.Section{Title: l.t("已接入的账号", "Your accounts")}
	for _, acct := range status.Accounts {
		name := accountTitle(l, acct)
		paired.Items = append(paired.Items, wire.Component{Type: "row", Key: "acct-" + acct.AccountID, Icon: "person",
			Title: name, Subtitle: accountLine(l, acct, r.Location()), Badge: statusBadge(l, acct),
			Action: openAccount(l, at, acct)})
	}
	if len(paired.Items) == 0 {
		paired.Items = []wire.Component{{Type: "empty", Icon: "person", Title: l.t("还没有接入任何账号", "No accounts yet"),
			Detail: l.t("从下面选一个你常用的服务添加。", "Add one of the services you use below.")}}
	} else {
		paired.Footer = l.t("点开一个账号可以改名、决定 Persona 助手能不能用它（助手指挥）、设为主助手，或者暂停、移除。",
			"Open an account to rename it, choose what the Persona assistant may do with it (Assistant control), make it the main assistant, pause or remove it.")
	}
	v.Sections = append([]wire.Section{about, state, paired}, a.addSections(l, at, status.Providers)...)
	return v
}

// accountLine is an account's row subtitle: the brand when the name does not carry it, the state in words and what
// the Persona assistant may do with it.
func accountLine(l lang, acct protocol.Account, loc *time.Location) string {
	var parts []string
	if brand := providerName(l, acct.Provider); !strings.Contains(accountTitle(l, acct), brand) {
		parts = append(parts, brand)
	}
	switch {
	case acct.Status == protocol.AccountError:
		parts = append(parts, problemWords(l, acct))
	case acct.Status == protocol.AccountPaused:
		parts = append(parts, l.t("已暂停同步", "paused"))
	case acct.LastError != "":
		parts = append(parts, problemWords(l, acct))
	default:
		parts = append(parts, l.t("上次同步 ", "synced ")+when(l, acct.LastSyncedAt, loc))
	}
	parts = append(parts, controlWords(l, core.Control(acct.AssistantControl)))
	if acct.AssistantBackend {
		parts = append(parts, l.t("是主助手", "main assistant"))
	}
	return strings.Join(parts, " · ")
}

// controlWords is what the Persona assistant may do with an account, short.
func controlWords(l lang, c core.Control) string {
	switch c {
	case core.ControlRead:
		return l.t("Persona 助手：只读", "assistant: read")
	case core.ControlConfirm:
		return l.t("Persona 助手：可代发（每次先问你）", "assistant: may send, asking first")
	case core.ControlAuto:
		return l.t("Persona 助手：可代发（每次先问你）", "assistant: may send, asking first")
	}
	return l.t("Persona 助手：不能用", "assistant: off")
}

// browserConnection says whether a provider works through a computer's browser.
func browserConnection(p string) bool {
	_, ok := browserProviderLabel(p)
	return ok
}

// problemWords is what is wrong with an account and what to do, from its last error (problems.go says it), without
// looking its computer up (the account page names it).
func problemWords(l lang, acct protocol.Account) string {
	if acct.LastError == "" {
		if acct.Status == protocol.AccountError {
			return l.t("出了问题：点开它，点「重新检查」。", "Something is wrong: open it and check again.")
		}
		return ""
	}
	p, _ := problemOf(l, acct.LastError, basicPlace(acct))
	if p.NeedsUser {
		// The fix names the computer, which the account page looks up: the row only says where to find it.
		return p.Short + l.t("：点开它看怎么处理。", ": open it to see what to do.")
	}
	return p.Short + l.t("：", ": ") + p.Do
}

// addSections are the ways to add an account, grouped by how one connects, each row saying in one line what it takes
// and, when it cannot be added in this configuration, why and what to switch on.
func (a *accounts) addSections(l lang, at string, providers []protocol.Provider) []wire.Section {
	available := map[string]bool{}
	for _, p := range providers {
		available[p.Provider] = p.Available
	}
	row := func(p, subtitle string, st pageState) wire.Component {
		return wire.Component{Type: "row", Key: "pair-" + p, Icon: "plus", Title: providerName(l, p), Subtitle: subtitle,
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: l.t("添加 ", "Add ") + providerName(l, p), State: ext.EncodeState(st)}}}
	}
	off := func(c wire.Component, why string) wire.Component {
		c.Subtitle = why
		c.Badge = &wire.Badge{Text: l.t("还没打开", "Not switched on"), Tone: wire.ToneCaution}
		return c
	}
	signIn := wire.Section{Title: l.t("添加账号：在浏览器里登录", "Add an account: sign in in your browser"),
		Items: []wire.Component{row(grokBot, l.t("在你自己的浏览器里点「同意」就行。", "Approve it in your own browser."), pageState{Page: pageGrok})}}
	if !available[grokBot] {
		signIn.Items[0] = off(signIn.Items[0], l.t("这个版本没有带上 Grok Bot。", "This version has no Grok Bot."))
	}

	web := wire.Section{Title: l.t("添加账号：借用电脑上已登录的网页", "Add an account: use a website signed in on your computer"),
		Footer: l.t("「借用电脑上的浏览器」是指：一台已经连上 Persona 的电脑（比如家里的 Mac）上有一个 Persona 能控制的浏览器（"+browserName+"），你在这个浏览器里登录好这些网站，Persona 就通过它收发消息。密码和登录信息不会离开那台电脑，那台电脑要开着。",
			"Using a computer's browser means: a computer connected to Persona (your Mac at home, say) has a browser Persona controls ("+browserName+"); you sign in to these sites in that browser and Persona reads and writes through it. Passwords and sign-ins never leave that computer, which has to be on.")}
	for _, s := range browserProviders {
		c := row(s.Provider, l.t("用那个浏览器里登录好的 ", "Through ")+siteHost(s.AppURL)+l.t("。", " signed in in that browser."),
			pageState{Page: pageBrowserPair, Provider: s.Provider})
		if !available[s.Provider] {
			c = off(c, l.t("要先在 ", "Switch on ")+settingsPath(l)+l.t(" 里打开「"+settingBrowser+"」。", " → \""+settingBrowser+"\"."))
		}
		web.Items = append(web.Items, c)
	}

	keys := wire.Section{Title: l.t("添加账号：粘贴密钥", "Add an account: paste a key"),
		Footer: l.t("密钥（API Key）是对方网站给你的一串字符，凭它就能代表你使用服务。它只在添加时提交一次，加密保存，之后不会再显示出来。",
			"A key (API key) is a string the service gives you that acts on your behalf. It is sent once when adding, kept encrypted, and never shown again.")}
	for _, p := range keyProviders {
		c := row(p, l.t("到 ", "Copy a key from ")+providerName(l, p)+l.t(" 网站复制一串密钥粘贴进来。", " and paste it."), pageState{Page: pagePair, Provider: p})
		if !available[p] {
			c = off(c, l.t("这个版本没有带上它。", "This version does not include it."))
		}
		keys.Items = append(keys.Items, c)
	}
	m := row(muse2api.Name, l.t("只有你自己搭了 Muse 转接服务才用得上，粘贴那个服务的密钥。", "Only if you run your own Muse relay: paste its key."),
		pageState{Page: pagePair, Provider: muse2api.Name})
	if !available[muse2api.Name] {
		// Not missing from the build: it waits for its server's address in the extension's settings.
		m = off(m, l.t("自己搭了 Muse 转接服务才需要：先在 ", "Only for your own Muse relay: first write its address in ")+settingsPath(l)+
			l.t(" 里填「"+settingMuse2API+"」。", " → \""+settingMuse2API+"\"."))
	}
	keys.Items = append(keys.Items, m)
	sections := []wire.Section{signIn, web, keys}
	if available[echo.Name] {
		sections = append(sections, wire.Section{Title: l.t("先试试", "Try it first"), Items: []wire.Component{
			row(echo.Name, l.t("不用真账号：你发什么它就回什么，不连任何网站。", "No real account: it answers with what you send and reaches nothing."),
				pageState{Page: pagePair, Provider: echo.Name})}})
	}
	return sections
}

// siteHost is a site's address as people write it ("claude.ai").
func siteHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	return strings.TrimPrefix(u.Host, "www.")
}

func (a *accounts) accountView(ctx context.Context, r *ext.UIRequest, l lang, st *pageState) *wire.View {
	at := ui(r)
	v := &wire.View{RefreshAfter: 60}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}}
		return v
	}
	acct, err := space.Account(ctx, st.Account)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "empty", Icon: "warning",
			Title: l.t("这个账号已不在", "This account is gone"), Detail: reason(l, err, "")}}}}
		return v
	}
	v.Title = accountTitle(l, acct)
	control, _ := space.AssistantControl(ctx, acct.AccountID)

	// What it is and how it is doing, with the fix when it needs one.
	info := wire.Section{Items: []wire.Component{
		{Type: "row", Title: l.t("服务", "Service"), Value: providerName(l, acct.Provider)},
		{Type: "row", Title: l.t("现在", "Now"), Value: stateWords(l, acct), Badge: statusBadge(l, acct)},
		{Type: "row", Title: l.t("上次同步", "Last synced"), Value: when(l, acct.LastSyncedAt, r.Location())},
		{Type: "row", Title: l.t("对话", "Chats"), Value: fmt.Sprint(acct.SessionCount)},
	}}
	if words := problemWords(l, acct); words != "" {
		if p, ok := problemOf(l, acct.LastError, placerFor(ctx, space, a.browser).of(ctx, acct, acct.LastError)); ok {
			words = p.Text(l)
		}
		info.Items = append(info.Items, wire.Component{Type: "text", Text: words},
			wire.Component{Type: "button", Title: l.t("重新检查", "Check again"), Action: &wire.Action{ID: actRecheck}})
	}
	if acct.Provider == grokBot {
		info.Items = append(info.Items, wire.Component{Type: "button", Title: l.t("重新登录", "Sign in again"),
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: providerName(l, grokBot),
				State: ext.EncodeState(pageState{Page: pageGrok, Account: acct.AccountID})}}})
	}

	name := acct.DisplayName
	if name == acct.Provider {
		name = ""
	}
	rename := wire.Section{Title: l.t("名字", "Name"),
		Footer: l.t("给它起个好认的名字，比如「工作用 Claude」。只在 Persona 里显示，不会改对方网站上的名字。",
			"A name you will recognize, like \"Work Claude\". Shown only in Persona; the service's own name is not changed."),
		Items: []wire.Component{
			{Type: "field", Name: fieldName, Label: l.t("名字", "Name"), Kind: wire.FieldKindText, Value: name,
				Placeholder: accountTitle(l, acct)},
			{Type: "button", Title: l.t("改名", "Rename"), Action: &wire.Action{ID: actRename, Submit: true}},
		}}

	sections := []wire.Section{info, rename, assistantSection(l, acct, control), a.mainAssistantSection(l, acct)}

	pause := wire.Component{Type: "button", Title: l.t("暂停同步", "Pause"), Action: &wire.Action{ID: actPause,
		Confirm: l.t("暂停后不再拉取它的新对话，也不能给它发消息，随时可以恢复。", "While paused, no new chats are fetched and nothing can be sent; resume any time.")}}
	if acct.Status == protocol.AccountPaused {
		pause = wire.Component{Type: "button", Title: l.t("恢复同步", "Resume"), Action: &wire.Action{ID: actResume}}
	}
	sections = append(sections, wire.Section{Title: l.t("同步", "Syncing"),
		Footer: l.t("平时每隔几分钟会自动拉取它的新对话。", "New chats are fetched on their own every few minutes."),
		Items: []wire.Component{
			{Type: "button", Title: l.t("立即同步", "Sync now"), Action: &wire.Action{ID: actSync}},
			pause,
		}})
	sections = append(sections, quotaDisplaySection(l, acct))
	if takeoverSupported(acct.Provider) && a.hub != nil {
		sec, pending := a.machineSection(ctx, l, acct)
		sections = append(sections, sec)
		if pending {
			v.RefreshAfter = 10
		}
	}
	sections = append(sections, wire.Section{Title: l.t("移除", "Remove"), Items: []wire.Component{
		{Type: "button", Title: l.t("移除这个账号（保留聊天记录）", "Remove (keep the chats)"), Role: "destructive", Action: &wire.Action{ID: actRemove,
			Confirm: l.t("移除后，保存的密钥或登录会被删除，以前的对话和消息还留着。", "Removing deletes the saved key or sign-in and keeps the chats and messages.")}},
		{Type: "button", Title: l.t("移除并删除所有聊天记录", "Remove and delete the chats"), Role: "destructive", Action: &wire.Action{ID: actPurge,
			Confirm: l.t("密钥、对话和消息都会被删除，不能恢复。", "The key, chats and messages are all deleted, for good.")}},
	}})
	v.Sections = sections
	return v
}

// stateWords is an account's state in a few words.
func stateWords(l lang, acct protocol.Account) string {
	switch {
	case acct.Status == protocol.AccountError:
		return l.t("要你处理", "Needs you")
	case acct.Status == protocol.AccountPaused:
		return l.t("已暂停同步", "Paused")
	case acct.LastError != "":
		return l.t("能用，最近有点小问题", "Working, with a recent hiccup")
	}
	return l.t("正常，在自动同步", "Working, syncing on its own")
}

// assistantSection is 助手指挥: how far the Persona assistant may use the account. The name is the one the assistant's
// guide (tools.go) sends the user to.
func assistantSection(l lang, acct protocol.Account, control core.Control) wire.Section {
	return wire.Section{Title: l.t("助手指挥：Persona 助手能不能用这个账号", "Assistant control: may the Persona assistant use this account"),
		Footer: l.t("关闭：Persona 助手完全看不到它（新账号默认这样）。只读：Persona 助手能读它的对话，帮你总结、提醒。可代发：还能替你给它发消息，每次发之前都先在手机上问你。",
			"Off: the Persona assistant does not see it (new accounts start so). Read: it can read the chats to sum up and remind you. Confirm: it can also write for you, asking on your phone before each message."),
		Items: []wire.Component{{Type: "segmented", Name: fieldControl, Choices: controlChoices(l, control), Value: string(control),
			Action: &wire.Action{ID: actControl}}}}
}

// controlChoices are the 助手指挥 levels to choose from; the automatic one only for an account that already has it.
func controlChoices(l lang, control core.Control) []wire.Choice {
	choices := []wire.Choice{{Value: string(core.ControlOff), Title: l.t("关闭", "Off")},
		{Value: string(core.ControlRead), Title: l.t("只读", "Read")},
		{Value: string(core.ControlConfirm), Title: l.t("可代发（逐次确认）", "Confirm")}}
	if control == core.ControlAuto {
		choices = append(choices, wire.Choice{Value: string(core.ControlAuto), Title: l.t("可代发（自动档）", "Auto")})
	}
	return choices
}

// controlChoiceTitle is a level's name as the choices have it.
func controlChoiceTitle(l lang, control core.Control) string {
	for _, c := range controlChoices(l, control) {
		if c.Value == string(control) {
			return c.Title
		}
	}
	return l.t("关闭", "Off")
}

// mainAssistantSection is 主助手: the account itself answering as the main assistant. With the bridge switched off in
// the settings the switch would do nothing, so the page says what to switch on instead (and still lets an account
// that has it on turn it off).
func (a *accounts) mainAssistantSection(l lang, acct protocol.Account) wire.Section {
	sec := wire.Section{Title: l.t("主助手：让它直接当你的助手", "Main assistant: let it be your assistant"),
		Footer: l.t("和上面的「助手指挥」不一样：助手指挥是让 Persona 助手去用这个账号；主助手是反过来，让这个账号自己来当你的助手——开启后，在 Persona 对话里可以选它来聊天、督办任务，由它来回答你。",
			"Unlike Assistant control above (the Persona assistant using this account), this makes the account itself your assistant: once on, you can pick it in Persona's chats and it answers you and supervises tasks.")}
	if !a.runtimeEnabled() {
		sec.Items = []wire.Component{{Type: "text", Text: l.t("现在用不了：先到 ", "Not available now: first switch on ") + settingsPath(l) +
			l.t(" 里打开「"+settingRuntime+"」。", " → \""+settingRuntime+"\".")}}
		if !acct.AssistantBackend {
			return sec
		}
	}
	val := "off"
	if acct.AssistantBackend {
		val = "on"
	}
	sec.Items = append(sec.Items, wire.Component{Type: "segmented", Name: fieldAssistantBackend,
		Choices: []wire.Choice{{Value: "off", Title: l.t("关闭", "Off")}, {Value: "on", Title: l.t("开启", "On")}},
		Value:   val, Action: &wire.Action{ID: actAssistantBackend}})
	return sec
}

func quotaDisplaySection(l lang, acct protocol.Account) wire.Section {
	val := acct.QuotaDisplay
	if val == "" {
		if !acct.ShowQuota {
			val = protocol.QuotaDisplayOff
		} else {
			val = protocol.QuotaDisplayAuto
		}
	}
	choices := []wire.Choice{
		{Value: protocol.QuotaDisplayAuto, Title: l.t("自动（剩得最少的那项）", "Auto (the lowest)")},
	}
	seen := map[string]bool{protocol.QuotaDisplayAuto: true, protocol.QuotaDisplayOff: true}
	var items []wire.Component
	if acct.Quota == nil {
		items = append(items, wire.Component{Type: "text", Style: "secondary",
			Text: l.t("这个服务还没告诉我们额度用了多少。", "The service has not reported any allowance yet.")})
	} else {
		value := quotaWords(l, acct.Quota.Kind, "", acct.Quota.Percent)
		for _, b := range acct.Quota.Buckets {
			if b.ID == acct.Quota.Selected {
				value = quotaWords(l, b.Kind, b.Window, b.Percent)
			}
		}
		if acct.Quota.State == protocol.QuotaExhausted {
			value = l.t("用完了", "Used up")
			if acct.Quota.ResetsAt > 0 {
				value += l.t("，", ", back at ") + time.UnixMilli(acct.Quota.ResetsAt).Format("01-02 15:04") + l.t(" 恢复", "")
			}
		}
		badgeText := l.t("剩余", "left")
		if acct.Quota.Percent != nil {
			badgeText = fmt.Sprintf("%d%%", *acct.Quota.Percent)
		}
		items = append(items, wire.Component{Type: "row", Title: l.t("现在还剩", "Left now"), Value: value,
			Badge: &wire.Badge{Text: badgeText, Tone: quotaBadgeTone(acct.Quota.Tone)}})
		for _, b := range acct.Quota.Buckets {
			if b.ID == "" || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			choices = append(choices, wire.Choice{Value: b.ID, Title: quotaWords(l, b.Kind, b.Window, b.Percent)})
		}
	}
	if !seen[val] {
		choices = append(choices, wire.Choice{Value: val, Title: l.t("之前选的那项", "The one chosen before")})
	}
	choices = append(choices, wire.Choice{Value: protocol.QuotaDisplayOff, Title: l.t("不显示", "Off")})
	items = append(items, wire.Component{
		Type:    "segmented",
		Name:    fieldQuotaDisplay,
		Choices: choices,
		Value:   val,
		Action:  &wire.Action{ID: actQuotaDisplay},
	})
	return wire.Section{
		Title: l.t("头像上显示哪项额度", "Which allowance the avatar shows"),
		Footer: l.t("有的服务限制你每天、每分钟能用多少。Persona 会在这个账号的头像外画一圈「电量」，表示还剩多少；这里选画哪一项。",
			"Some services limit how much you use per day or minute. Persona draws a battery ring around the avatar for what is left; choose which one."),
		Items: items,
	}
}

// quotaWords names an allowance the way people say it: "每天的次数 62%".
func quotaWords(l lang, kind, window string, pct *int) string {
	var per string
	switch window {
	case "minute":
		per = l.t("每分钟的", "per minute ")
	case "hour":
		per = l.t("每小时的", "per hour ")
	case "day":
		per = l.t("每天的", "per day ")
	case "week":
		per = l.t("每周的", "per week ")
	case "month":
		per = l.t("每月的", "per month ")
	case "":
	default:
		per = l.t("每 "+window+" 的", "per "+window+" ")
	}
	var what string
	switch kind {
	case protocol.QuotaKindRequests:
		what = l.t("次数", "requests")
	case protocol.QuotaKindTokens:
		what = l.t("用量（字数）", "usage (tokens)")
	case protocol.QuotaKindCredits:
		what = l.t("点数", "credits")
	case protocol.QuotaKindWindow:
		what = l.t("时段额度", "window")
	default:
		what = l.t("额度", "allowance")
	}
	s := per + what
	if pct != nil {
		s += fmt.Sprintf(" %d%%", *pct)
	}
	return s
}

func quotaBadgeTone(tone string) string {
	switch tone {
	case protocol.QuotaToneExhausted:
		return wire.ToneDanger
	case protocol.QuotaToneLow:
		return wire.ToneCaution
	case protocol.QuotaToneOK:
		return wire.TonePositive
	default:
		return wire.ToneNeutral
	}
}

// unavailableView is a pairing page for a provider that cannot be added in this configuration: why, and what to
// switch on.
func unavailableView(l lang, title, why string) *wire.View {
	return &wire.View{Title: title, Sections: []wire.Section{{Items: []wire.Component{
		{Type: "empty", Icon: "settings", Title: l.t("现在还加不了", "Cannot be added yet"), Detail: why},
	}}}}
}

func (a *accounts) available(p string) bool {
	_, ok := a.club.Registry().Get(p)
	return ok
}

func (a *accounts) pairView(l lang, st pageState) *wire.View {
	name := providerName(l, st.Provider)
	title := l.t("添加 ", "Add ") + name
	if !a.available(st.Provider) {
		why := l.t("这个版本没有带上 ", "This version does not include ") + name + l.t("。", ".")
		if st.Provider == muse2api.Name {
			why = l.t("这一项只给自己搭了 Muse 转接服务的人用。先到 ", "This is only for your own Muse relay. First write its address in ") +
				settingsPath(l) + l.t(" 里填「"+settingMuse2API+"」，保存后再回来。普通的 Muse 账号请用「借用电脑上已登录的网页」那一组里的 Muse（网页版）。",
				" → \""+settingMuse2API+"\" and save. For an ordinary Muse account use Muse (web) instead.")
		}
		return unavailableView(l, title, why)
	}
	keyLabel := l.t("密钥（API Key）", "Key (API key)")
	keyHelp := l.t("到 ", "Copy the API key from ") + name + l.t(" 网站的设置里找到 API Key，复制整串粘贴到这里。", "'s settings and paste all of it.")
	switch st.Provider {
	case muse2api.Name:
		keyHelp = l.t("你自己搭的 Muse 转接服务启动时设的那串密钥。", "The key your own Muse relay was started with.")
	case echo.Name:
		keyLabel = l.t("随便填一串字", "Any text")
		keyHelp = l.t("演示账号不连任何网站：填一串以 echo- 开头的字就行，比如 echo-test-001。", "The demo reaches nothing: any text starting with echo-, like echo-test-001.")
	}
	footer := l.t("密钥加密保存在你的 Persona 上，只会发给 ", "The key is kept encrypted on your Persona and sent only to ") + name +
		l.t("，之后不会再显示出来。", "; it is never shown again.")
	if st.Provider == echo.Name {
		footer = l.t("演示账号不会把任何东西发到外面。", "The demo account sends nothing anywhere.")
	}
	return &wire.View{Title: title, Sections: []wire.Section{{
		Footer: footer,
		Items: []wire.Component{
			{Type: "field", Name: fieldCredential, Label: keyLabel, Kind: wire.FieldKindSecret, Detail: keyHelp},
			{Type: "field", Name: fieldName, Label: l.t("名字（可不填）", "Name (optional)"), Kind: wire.FieldKindText,
				Detail: l.t("给它起个好认的名字，比如「工作用 ", "A name you will recognize, like \"Work ") + name + l.t("」，以后也能改。", "\"; you can change it later.")},
			{Type: "button", Title: l.t("添加", "Add"), Action: &wire.Action{ID: actPair, Submit: true}},
		}}}}
}

// grokView is the Grok Bot sign-in (club.logins.grok_bot.*): start gives a login id and the https page the user
// approves on, each render polls it (pending, or done with the account). A sign-in lasts 10 minutes and at most two
// wait at once; one that expired, was canceled or failed is not found any more. With st.Account it signs that
// account in again (its credential is replaced) instead of pairing a new one.
func (a *accounts) grokView(ctx context.Context, l lang, st *pageState) *wire.View {
	v := &wire.View{Title: providerName(l, grokBot)}
	if st.Login == "" && st.Account == "" && !a.available(grokBot) {
		return unavailableView(l, l.t("添加 ", "Add ")+providerName(l, grokBot), l.t("这个版本没有带上 Grok Bot。", "This version has no Grok Bot."))
	}
	start := wire.Component{Type: "button", Title: l.t("开始登录", "Start signing in"), Action: &wire.Action{ID: actGrokStart}}
	again := st.Account
	if st.Login == "" {
		intro := l.t("在你自己的浏览器里登录 Grok Bot：点「开始登录」会打开 Grok 的授权页，在那里点同意，这里会自动看到结果。",
			"You sign in in your own browser: start, open the page and approve; this page checks for the result.")
		if again != "" {
			intro = l.t("重新登录这个 Grok Bot 账号：同意后换成新的登录，以前的对话和消息都还在。",
				"Signing this Grok Bot account in again: once you approve, its credential is replaced; its sessions and messages stay.")
		}
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: intro}, start}}}
		return v
	}
	var res protocol.GrokBotLoginPoll
	if err := call(ctx, a.club, protocol.MethodClubLoginsGrokBotPoll, protocol.LoginParams{LoginID: st.Login}, &res); err != nil {
		why := protocol.ReasonOf(err)
		switch {
		case why == protocol.ReasonLoginNotFound:
			*st = pageState{Page: pageGrok, Account: again, NoteTone: wire.ToneCaution,
				Note: l.t("这次登录已过期（10 分钟）或已取消，请重新开始。", "This sign-in expired (after 10 minutes) or was canceled; start again.")}
		case slices.Contains(retryableLogin, why):
			st.Note, st.NoteTone = l.t("暂时查不到登录结果，稍后会自动再查。", "Could not check the sign-in just now; it is checked again shortly."), wire.ToneCaution
			v.Sections = []wire.Section{{Items: a.grokPending(l, *st)}}
			v.RefreshAfter = 30
			return v
		default:
			words := reasonWords(l, why)
			if words == "" && protocol.CodeOf(err) == protocol.CodeInvalid {
				words = l.t("Grok 没有同意这次登录（可能点了拒绝，或者这个 Grok 账号用不了），请重新开始。",
					"Grok did not approve this sign-in (refused, or the account cannot be used); start again.")
			}
			if words == "" {
				words = reason(l, err, "")
			}
			*st = pageState{Page: pageGrok, Account: again, Note: l.t("登录没有完成：", "Sign-in did not finish: ") + words,
				NoteTone: wire.ToneDanger}
		}
		v.Sections = []wire.Section{{Items: []wire.Component{start}}}
		return v
	}
	if res.Status == protocol.LoginDone {
		name := providerName(l, grokBot)
		if res.Account != nil {
			name = accountTitle(l, *res.Account)
		}
		if again == "" && res.Account != nil {
			// A new account: the done step, which asks about the assistant (and offers no second sign-in).
			*st = pageState{Page: pageDone, Account: res.Account.AccountID}
			return a.doneView(ctx, l, *st)
		}
		note := l.t("已登录并配对：", "Signed in and paired: ")
		if again != "" {
			note = l.t("已重新登录：", "Signed in again: ")
		}
		*st = pageState{Page: pageGrok, Account: again, Note: note + name, NoteTone: wire.TonePositive}
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "button", Title: l.t("好了", "Done"), Action: &wire.Action{ID: actFinish}}}}}
		return v
	}
	v.Sections = []wire.Section{{Items: a.grokPending(l, *st)}}
	v.RefreshAfter = 30
	return v
}

func (a *accounts) grokPending(l lang, st pageState) []wire.Component {
	items := []wire.Component{}
	if httpsURL(st.LoginURL) {
		items = append(items, wire.Component{Type: "link", Title: l.t("打开授权页", "Open the sign-in page"), URL: st.LoginURL})
	}
	return append(items,
		wire.Component{Type: "text", Style: "secondary", Text: l.t("批准后这页每 30 秒检查一次，也可以点下面立即检查。授权页 10 分钟内有效。",
			"After you approve, this page checks every 30 seconds, or check now below. The page is valid for 10 minutes.")},
		wire.Component{Type: "button", Title: l.t("我已批准，立即检查", "I approved; check now"), Action: &wire.Action{ID: actGrokCheck}},
		wire.Component{Type: "button", Title: l.t("取消", "Cancel"), Role: "destructive", Action: &wire.Action{ID: actGrokStop}})
}

func (a *accounts) action(ctx context.Context, r *ext.UIRequest) (*wire.ActionResult, error) {
	l := lang(r.Locale)
	var st pageState
	_ = r.DecodeState(&st)
	st.Note, st.NoteTone = "", ""
	done := func(note string) (*wire.ActionResult, error) {
		st.Note, st.NoteTone = note, wire.TonePositive
		return &wire.ActionResult{View: a.view(ctx, r, st), Toast: note}, nil
	}
	// refuse is a refusal of the page's own, in words with the next step, before anything is asked of the core.
	refuse := func(note string) (*wire.ActionResult, error) {
		st.Note, st.NoteTone = note, wire.ToneDanger
		return &wire.ActionResult{View: a.view(ctx, r, st), Toast: note}, nil
	}
	failed := func(err error, secret string) (*wire.ActionResult, error) {
		return refuse(reason(l, err, secret))
	}
	update := func(patch map[string]any) error {
		patch["account_id"] = st.Account
		return call(ctx, a.club, protocol.MethodClubAccountsUpdate, patch, nil)
	}
	switch r.Action {
	case actSync:
		// club.accounts.sync answers ok whether or not the sync worked (a failure is kept on the account), so the
		// outcome is read back from the account.
		before, _ := a.account(ctx, st.Account)
		if err := call(ctx, a.club, protocol.MethodClubAccountsSync, protocol.AccountParams{AccountID: st.Account}, nil); err != nil {
			return failed(err, "")
		}
		after, err := a.account(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		if after.Status == protocol.AccountError || after.LastError != "" {
			st.Note, st.NoteTone = l.t("检查过了，还没好。", "Checked; still not working. ")+problemWords(l, after), wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if before.Status == protocol.AccountError || before.LastError != "" {
			return done(l.t("检查过了，已经恢复正常，最新的对话也拉下来了", "Checked: working again, and the latest chats are in"))
		}
		return done(l.t("已同步，最新的对话已经拉下来了", "Synced; the latest chats are in"))
	case actRecheck:
		return a.recheck(ctx, r, l, st)
	case actPause, actResume:
		paused := r.Action == actPause
		if err := update(map[string]any{"paused": paused}); err != nil {
			return failed(err, "")
		}
		if paused {
			return done(l.t("已暂停同步：不再拉取它的新对话，也不能给它发消息", "Paused: no new chats are fetched and nothing can be sent"))
		}
		return done(l.t("已恢复同步：会继续自动拉取它的新对话", "Resumed: new chats are fetched again"))
	case actControl:
		level, err := core.ParseControl(r.Values[fieldControl])
		if err != nil || r.Values[fieldControl] == "" {
			return refuse(l.t("没认出选的是哪一项，请重新选一次", "That choice was not recognized; choose again"))
		}
		if err := update(map[string]any{"assistant_control": level}); err != nil {
			return failed(err, "")
		}
		switch level {
		case core.ControlRead:
			return done(l.t("已设为只读：Persona 助手现在能读这个账号的对话，但不会替你发消息", "Set to Read: the Persona assistant can now read this account's chats, not write"))
		case core.ControlConfirm:
			return done(l.t("已设为可代发：Persona 助手能读，也能替你发消息，每次发之前先在手机上问你", "Set to Confirm: the Persona assistant can read and write, asking you each time"))
		case core.ControlAuto:
			return done(l.t("已设为可代发（自动档）：Persona 助手能读也能替你发消息；这个扩展规定每次发送仍要先在手机上问你", "Set to Auto: the Persona assistant can read and write; this extension still has every send confirmed on your phone"))
		}
		return done(l.t("已关闭：Persona 助手现在看不到这个账号", "Off: the Persona assistant no longer sees this account"))
	case actAssistantBackend:
		on := strings.TrimSpace(r.Values[fieldAssistantBackend]) == "on"
		if on && !a.runtimeEnabled() {
			return refuse(l.t("现在开不了：先到 ", "Cannot be switched on: first switch on ") + settingsPath(l) +
				l.t(" 里打开「"+settingRuntime+"」", " → \""+settingRuntime+"\""))
		}
		if err := update(map[string]any{"assistant_backend": on}); err != nil {
			return failed(err, "")
		}
		if on {
			return done(l.t("已设为主助手：在 Persona 对话里选它，就能直接跟它聊", "Now a main assistant: pick it in Persona's chats to talk to it"))
		}
		return done(l.t("已不再当主助手：Persona 对话里不会再列出它", "No longer a main assistant: Persona's chats stop offering it"))
	case actQuotaDisplay:
		disp := strings.TrimSpace(r.Values[fieldQuotaDisplay])
		if err := update(map[string]any{"quota_display": disp}); err != nil {
			return failed(err, "")
		}
		if disp == protocol.QuotaDisplayOff {
			return done(l.t("头像上不再显示额度", "The avatar no longer shows an allowance"))
		}
		return done(l.t("已改好头像上显示的额度", "The avatar's allowance is changed"))
	case actRename:
		name := strings.TrimSpace(r.Values[fieldName])
		switch {
		case name == "":
			return refuse(l.t("名字不能是空的：输入一个名字再点「改名」", "The name is empty: type one, then Rename"))
		case len(utf16.Encode([]rune(name))) > maxDisplayName:
			return refuse(l.t(fmt.Sprintf("名字太长了：最多 %d 个字，请删短一点再点「改名」", maxDisplayName), fmt.Sprintf("The name is too long: at most %d characters", maxDisplayName)))
		}
		if err := update(map[string]any{"display_name": name}); err != nil {
			return failed(err, "")
		}
		return done(l.t("已改名为「", "Renamed to \"") + name + l.t("」", "\""))
	case actRemove, actPurge:
		p := protocol.RemoveAccountParams{AccountID: st.Account, DeleteData: r.Action == actPurge}
		if err := call(ctx, a.club, protocol.MethodClubAccountsRemove, p, nil); err != nil {
			return failed(err, "")
		}
		if p.DeleteData {
			return &wire.ActionResult{Pop: true, Toast: l.t("已移除这个账号，并删除了它的聊天记录", "Removed, with its chats deleted")}, nil
		}
		return &wire.ActionResult{Pop: true, Toast: l.t("已移除这个账号，以前的聊天记录还留着", "Removed; its chats are kept")}, nil
	case actPair:
		cred := strings.TrimSpace(r.Values[fieldCredential])
		name := strings.TrimSpace(r.Values[fieldName])
		switch {
		case cred == "":
			return refuse(l.t("还没填密钥：把对方网站给你的那串密钥粘贴进来，再点「添加」", "No key yet: paste the key from the service, then Add"))
		case len(cred) < minCredential:
			return refuse(l.t("这串密钥太短了，可能没复制完整：回对方网站重新复制整串再试", "The key is too short; it may be cut off. Copy all of it again"))
		case len(cred) > maxCredential:
			return refuse(l.t("这串密钥太长了，可能多复制了别的内容：只复制密钥本身再试", "The key is too long; copy only the key"))
		case len(utf16.Encode([]rune(name))) > maxDisplayName:
			return refuse(l.t(fmt.Sprintf("名字太长了：最多 %d 个字，请删短一点", maxDisplayName), fmt.Sprintf("The name is too long: at most %d characters", maxDisplayName)))
		}
		var acct protocol.Account
		p := protocol.AddAccountParams{Provider: st.Provider, Credential: cred, DisplayName: name}
		if err := call(ctx, a.club, protocol.MethodClubAccountsAdd, p, &acct); err != nil {
			return failed(explain(l, err, place{Provider: st.Provider, Pairing: pairKey}), cred)
		}
		return a.added(ctx, r, acct), nil
	case actBrowserPair:
		if !a.available(st.Provider) {
			return refuse(l.t("这类账号还没打开：先到 ", "Not switched on: first switch on ") + settingsPath(l) +
				l.t(" 里打开「"+settingBrowser+"」", " → \""+settingBrowser+"\""))
		}
		if parseMachineChoice(r.Values[fieldMachine]).MachineID == "" {
			return refuse(l.t("还没选电脑：在「用哪台电脑」里选一台，再点「检查登录并添加」", "No computer chosen: choose one, then try again"))
		}
		if len(utf16.Encode([]rune(strings.TrimSpace(r.Values[fieldName])))) > maxDisplayName {
			return refuse(l.t(fmt.Sprintf("名字太长了：最多 %d 个字，请删短一点", maxDisplayName), fmt.Sprintf("The name is too long: at most %d characters", maxDisplayName)))
		}
		acct, err := a.pairBrowserAccount(ctx, st.Provider, r.Values[fieldMachine], r.Values[fieldName])
		if why := protocol.ReasonOf(err); why == provider.CodeAuthFailed || why == protocol.ReasonNotSignedIn {
			site, _ := browserSite(st.Provider)
			return refuse(l.t("那台电脑上 Persona 控制的浏览器里还没登录好 ", "The browser Persona controls on that computer is not signed in to ") + siteHost(site.AppURL) +
				l.t("：先在那个浏览器里登录，再回来点「检查登录并添加」", ": sign in in that browser first, then come back and try again"))
		}
		if err != nil {
			return failed(explain(l, err, a.pairPlace(ctx, st.Provider, r.Values[fieldMachine])), "")
		}
		return a.added(ctx, r, acct), nil
	case actFinish:
		return a.finish(ctx, l, st), nil
	case actOpenAccount:
		return &wire.ActionResult{View: a.view(ctx, r, pageState{Page: pageAccount, Account: st.Account})}, nil
	case actGrokStart:
		var res protocol.GrokBotLoginStart
		err := call(ctx, a.club, protocol.MethodClubLoginsGrokBotStart, protocol.GrokBotLoginStartParams{AccountID: st.Account}, &res)
		if protocol.ReasonOf(err) == protocol.ReasonLoginLimit {
			st.Note, st.NoteTone = reasonWords(l, protocol.ReasonLoginLimit), wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if err != nil {
			return failed(err, "")
		}
		if res.LoginID == "" || !httpsURL(res.LoginURL) {
			return refuse(l.t("登录没有给出可以打开的授权页，请稍后重新开始", "The sign-in gave no page to open; start again later"))
		}
		st = pageState{Page: pageGrok, Account: st.Account, Login: res.LoginID, LoginURL: res.LoginURL}
		return &wire.ActionResult{View: a.view(ctx, r, st), OpenURL: res.LoginURL}, nil
	case actGrokCheck:
		return &wire.ActionResult{View: a.view(ctx, r, st)}, nil
	case actGrokStop:
		if st.Login != "" {
			if err := call(ctx, a.club, protocol.MethodClubLoginsGrokBotCancel, protocol.LoginParams{LoginID: st.Login}, nil); err != nil {
				st.Login, st.LoginURL = "", ""
				return failed(err, "")
			}
		}
		st = pageState{Page: pageGrok, Account: st.Account}
		return done(l.t("已取消这次登录", "Sign-in canceled"))
	case actTakeover:
		mid, err := a.runTakeover(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		return done(l.t("已把接入说明发给它（电脑编号 "+mid+"）。连上后，请到 Persona 的「机器」页点「这是我的电脑」确认",
			"Instructions sent ("+mid+"); once it connects, confirm it on the Machines page"))
	case actWake:
		mid, err := a.runWake(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		return done(l.t("已让它把电脑重新连上（电脑编号 "+mid+"）", "Asked it to reconnect its computer ("+mid+")"))
	}
	return nil, nil
}

// accountName is an account's name for the assistant's tools and the machine it pairs: the name it was given or
// found, else its service.
func accountName(a protocol.Account) string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return providerName("en", a.Provider)
}

// echoAccountName is what the demo provider names every account it verifies (echo.go); the pages say 演示账号.
const echoAccountName = "Echo"

// accountTitle is what an account is called on the pages: the name it was given or found, else its service in the
// page's words (never the raw provider id the core falls back to).
func accountTitle(l lang, a protocol.Account) string {
	if a.DisplayName != "" && a.DisplayName != a.Provider && !(a.Provider == echo.Name && a.DisplayName == echoAccountName) {
		return a.DisplayName
	}
	return providerName(l, a.Provider)
}

// providerName is a service's name as people know it.
func providerName(l lang, p string) string {
	switch p {
	case "cue":
		return "Cue"
	case grokBot:
		return "Grok Bot"
	case "groq":
		return "Groq"
	case muse2api.Name:
		return l.t("Muse（自建转接）", "Muse (own relay)")
	case echo.Name:
		return l.t("演示账号", "Demo account")
	case browser.SiteMuse.Provider:
		return l.t("Muse（网页版）", "Muse (web)")
	case browser.SiteGeminiTasks.Provider:
		return "Gemini"
	case browser.SiteDots.Provider:
		return "ChatGPT Dots"
	}
	if label, ok := browserProviderLabel(p); ok {
		return label
	}
	return p
}

func statusBadge(l lang, acct protocol.Account) *wire.Badge {
	switch acct.Status {
	case protocol.AccountActive:
		if acct.LastError != "" {
			return &wire.Badge{Text: l.t("有点小问题", "Hiccup"), Tone: wire.ToneCaution}
		}
		return &wire.Badge{Text: l.t("正常", "Working"), Tone: wire.TonePositive}
	case protocol.AccountPaused:
		return &wire.Badge{Text: l.t("已暂停", "Paused"), Tone: wire.ToneNeutral}
	case protocol.AccountError:
		return &wire.Badge{Text: l.t("要处理", "Needs you"), Tone: wire.ToneDanger}
	}
	return &wire.Badge{Text: l.t("状态不明", "Unknown"), Tone: wire.ToneNeutral}
}

func when(l lang, ms int64, loc *time.Location) string {
	if ms <= 0 {
		return l.t("还没同步过", "never")
	}
	return time.UnixMilli(ms).In(loc).Format("2006-01-02 15:04")
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
