package extension

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/browser"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

// AccountsUI is the id of the accounts page (contributes.ui): Settings › Extensions › Personal Agent Club › Accounts.
// It lists the paired accounts and pairs, pauses, resumes, syncs and unpairs them through the core's club.* methods,
// the same ones the serve shell's admin token reaches.
//
// Credentials go in and never come out: the API key field is a secret field (the apps never echo it and the
// extension cannot prefill it), the key goes straight to club.accounts.add and is kept nowhere in the page's state,
// and nothing the page draws or logs carries it.
const AccountsUI = "accounts"

// Pages of the accounts UI (pageState.Page).
const (
	pageList    = ""
	pageAccount = "account"
	pagePair    = "pair"
	pageGrok    = "grok"
)

// Actions of the accounts UI.
const (
	actSync         = "sync"
	actPause        = "pause"
	actResume       = "resume"
	actRemove       = "remove"
	actPurge        = "purge"
	actControl      = "control"
	actQuotaDisplay = "quota_display"
	actPair         = "pair"
	actGrokStart    = "grok_start"
	actGrokCheck    = "grok_check"
	actGrokStop     = "grok_cancel"
	actTakeover     = "takeover"
	actWake         = "wake"
)

// Field names on the pages.
const (
	fieldCredential   = "credential"
	fieldName         = "display_name"
	fieldControl      = "assistant_control"
	fieldQuotaDisplay = "quota_display"
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

// Providers paired by pasting a key on the accounts page. Grok Bot signs in through its own page instead.
var keyProviders = []string{"cue", "groq"}

const grokBot = "grok_bot"

// Reasons a Grok Bot sign-in poll fails for now and may still succeed: the sign-in is kept, and asked again.
var retryableLogin = []string{"NETWORK_ERROR", "UPSTREAM_ERROR", "RATE_LIMITED"}

// accountsPage is the accounts UI.
func accountsPage(club *core.Club, hub *atomic.Pointer[ext.Hub], drv browser.Driver) ext.Page {
	a := &accounts{club: club, hub: hub, browser: drv}
	return ext.Page{Render: a.render, Action: a.action}
}

type accounts struct {
	club    *core.Club
	hub     *atomic.Pointer[ext.Hub]
	browser browser.Driver
}

func (a *accounts) render(ctx context.Context, r *ext.UIRequest) (*wire.View, error) {
	var st pageState
	_ = r.DecodeState(&st)
	return a.view(ctx, r, st), nil
}

func (a *accounts) view(ctx context.Context, r *ext.UIRequest, st pageState) *wire.View {
	l := lang(r.Locale)
	var v *wire.View
	switch st.Page {
	case pageAccount:
		v = a.accountView(ctx, r, l, &st)
	case pagePair:
		v = a.pairView(l, st)
	case pageGrok:
		v = a.grokView(ctx, l, &st)
	case pageBrowserPair:
		v = a.browserPairView(ctx, l, st)
	default:
		v = a.listView(ctx, r, l)
	}
	if st.Note != "" {
		tone := st.NoteTone
		if tone == "" {
			tone = wire.ToneNeutral
		}
		v.Sections = append([]wire.Section{{Items: []wire.Component{{Type: "row", Title: st.Note,
			Badge: &wire.Badge{Text: l.t("结果", "Result"), Tone: tone}}}}}, v.Sections...)
	}
	v.State = ext.EncodeState(st)
	return v
}

func (a *accounts) listView(ctx context.Context, r *ext.UIRequest, l lang) *wire.View {
	v := &wire.View{Title: l.t("账号", "Accounts"), RefreshAfter: 60}
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
	paired := wire.Section{Title: l.t("已配对", "Paired")}
	for _, acct := range status.Accounts {
		name := accountName(acct)
		sub := providerName(l, acct.Provider) + " · " + l.t("上次同步 ", "synced ") + when(l, acct.LastSyncedAt, r.Location())
		if acct.LastError != "" {
			sub += " · " + l.t("错误 ", "error ") + acct.LastError
		}
		paired.Items = append(paired.Items, wire.Component{Type: "row", Key: "acct-" + acct.AccountID, Icon: "person",
			Title: name, Subtitle: sub, Badge: statusBadge(l, acct.Status),
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: AccountsUI, Title: name,
				State: ext.EncodeState(pageState{Page: pageAccount, Account: acct.AccountID})}}})
	}
	if len(paired.Items) == 0 {
		paired.Items = []wire.Component{{Type: "empty", Icon: "person", Title: l.t("还没有账号", "No accounts yet"),
			Detail: l.t("在下面配对一个账号，它的会话就会出现在 Agents 页。", "Pair an account below and its sessions appear on the Agents page.")}}
	}
	available := map[string]bool{}
	for _, p := range status.Providers {
		available[p.Provider] = p.Available
	}
	add := wire.Section{Title: l.t("配对新账号", "Pair an account"),
		Footer: l.t("API Key 只在配对时提交一次，之后不会在任何地方显示。", "An API key is sent once, when pairing, and never shown again.")}
	pairRow := func(p, subtitle string, st pageState) {
		row := wire.Component{Type: "row", Key: "pair-" + p, Icon: "link", Title: providerName(l, p), Subtitle: subtitle,
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: AccountsUI, Title: providerName(l, p), State: ext.EncodeState(st)}}}
		if !available[p] {
			row.Badge = &wire.Badge{Text: l.t("此版本未接入", "Not in this build"), Tone: wire.ToneCaution}
		}
		add.Items = append(add.Items, row)
	}
	for _, p := range keyProviders {
		pairRow(p, l.t("粘贴 API Key", "Paste an API key"), pageState{Page: pagePair, Provider: p})
	}
	pairRow(grokBot, l.t("在浏览器里登录", "Sign in in the browser"), pageState{Page: pageGrok})
	for _, s := range browserProviders {
		pairRow(s.Provider, l.t("通过已配对电脑的浏览器（ego lite）", "Via a paired computer's browser (ego lite)"), pageState{Page: pageBrowserPair, Provider: s.Provider})
	}
	if _, ok := a.club.Registry().Get(echo.Name); ok {
		pairRow(echo.Name, l.t("演示：任意以 echo- 开头的字符串", "Demo: any text starting with echo-"), pageState{Page: pagePair, Provider: echo.Name})
	}
	v.Sections = []wire.Section{paired, add}
	return v
}

func (a *accounts) accountView(ctx context.Context, r *ext.UIRequest, l lang, st *pageState) *wire.View {
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
	v.Title = accountName(acct)
	control, _ := space.AssistantControl(ctx, acct.AccountID)
	info := wire.Section{Items: []wire.Component{
		{Type: "row", Title: l.t("提供方", "Provider"), Value: providerName(l, acct.Provider)},
		{Type: "row", Title: l.t("状态", "Status"), Badge: statusBadge(l, acct.Status)},
		{Type: "row", Title: l.t("上次同步", "Last synced"), Value: when(l, acct.LastSyncedAt, r.Location())},
		{Type: "row", Title: l.t("会话", "Sessions"), Value: fmt.Sprint(acct.SessionCount)},
	}}
	if acct.LastError != "" {
		info.Items = append(info.Items, wire.Component{Type: "row", Title: l.t("错误", "Error"), Value: acct.LastError,
			Badge: &wire.Badge{Text: l.t("出错", "Error"), Tone: wire.ToneDanger}})
	}
	choices := []wire.Choice{{Value: string(core.ControlOff), Title: l.t("关闭", "Off")},
		{Value: string(core.ControlRead), Title: l.t("只读", "Read")},
		{Value: string(core.ControlConfirm), Title: l.t("逐次确认", "Confirm")}}
	if control == core.ControlAuto {
		choices = append(choices, wire.Choice{Value: string(core.ControlAuto), Title: l.t("自动", "Auto")})
	}
	assistant := wire.Section{Title: l.t("助手指挥", "Assistant control"),
		Footer: l.t("关闭：助手看不到这个账号。只读：能列出和读。逐次确认：发消息前每次在手机上确认。",
			"Off: the assistant does not see this account. Read: it can list and read. Confirm: each send is confirmed on your phone first."),
		Items: []wire.Component{{Type: "segmented", Name: fieldControl, Choices: choices, Value: string(control),
			Action: &wire.Action{ID: actControl}}}}
	quotaSection := quotaDisplaySection(l, acct)
	pause := wire.Component{Type: "button", Title: l.t("暂停同步", "Pause"), Action: &wire.Action{ID: actPause}}
	if acct.Status == protocol.AccountPaused {
		pause = wire.Component{Type: "button", Title: l.t("恢复同步", "Resume"), Action: &wire.Action{ID: actResume}}
	}
	actions := wire.Section{Items: []wire.Component{
		{Type: "button", Title: l.t("立即同步", "Sync now"), Action: &wire.Action{ID: actSync}},
		pause,
	}}
	if acct.Provider == grokBot {
		actions.Items = append(actions.Items, wire.Component{Type: "button", Title: l.t("重新登录", "Sign in again"),
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: AccountsUI, Title: providerName(l, grokBot),
				State: ext.EncodeState(pageState{Page: pageGrok, Account: acct.AccountID})}}})
	}
	actions.Items = append(actions.Items, []wire.Component{
		{Type: "button", Title: l.t("解除配对", "Unpair"), Role: "destructive", Action: &wire.Action{ID: actRemove,
			Confirm: l.t("解除配对后凭据会被删除，会话和消息保留。", "Unpairing deletes the credential and keeps the sessions and messages.")}},
		{Type: "button", Title: l.t("解除配对并删除数据", "Unpair and delete data"), Role: "destructive", Action: &wire.Action{ID: actPurge,
			Confirm: l.t("凭据、会话和消息都会被删除，不能恢复。", "The credential, sessions and messages are all deleted, for good.")}},
	}...)
	sections := []wire.Section{info, quotaSection}
	if takeoverSupported(acct.Provider) && a.hub != nil {
		sec, pending := a.machineSection(ctx, l, acct)
		sections = append(sections, sec)
		if pending {
			v.RefreshAfter = 10
		}
	}
	sections = append(sections, assistant, actions)
	v.Sections = sections
	return v
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
		{Value: protocol.QuotaDisplayAuto, Title: l.t("自动（最低）", "Auto")},
	}
	seen := map[string]bool{protocol.QuotaDisplayAuto: true, protocol.QuotaDisplayOff: true}
	var items []wire.Component
	if acct.Quota != nil {
		badgeText := acct.Quota.Label
		if acct.Quota.Percent != nil {
			badgeText = fmt.Sprintf("%d%%", *acct.Quota.Percent)
		}
		items = append(items, wire.Component{
			Type:  "row",
			Title: l.t("当前额度", "Current quota"),
			Value: acct.Quota.Label,
			Badge: &wire.Badge{Text: badgeText, Tone: quotaBadgeTone(acct.Quota.Tone)},
		})
		for _, b := range acct.Quota.Buckets {
			if b.ID == "" || seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			title := b.ID
			if b.Percent != nil {
				title = fmt.Sprintf("%s (%d%%)", b.ID, *b.Percent)
			}
			choices = append(choices, wire.Choice{Value: b.ID, Title: title})
		}
	}
	if !seen[val] {
		choices = append(choices, wire.Choice{Value: val, Title: val})
	}
	choices = append(choices, wire.Choice{Value: protocol.QuotaDisplayOff, Title: l.t("隐藏", "Off")})
	items = append(items, wire.Component{
		Type:    "segmented",
		Name:    fieldQuotaDisplay,
		Choices: choices,
		Value:   val,
		Action:  &wire.Action{ID: actQuotaDisplay},
	})
	return wire.Section{
		Title: l.t("头像额度条", "Avatar quota indicator"),
		Footer: l.t("选择在头像电量条上展示哪一项额度（百分比与颜色随剩余比例变化）。",
			"Choose which quota bucket is shown on the avatar battery ring (percentage and color follow remaining quota)."),
		Items: items,
	}
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

func (a *accounts) pairView(l lang, st pageState) *wire.View {
	name := providerName(l, st.Provider)
	return &wire.View{Title: l.t("配对 ", "Pair ") + name, Sections: []wire.Section{{
		Footer: l.t("提交后 API Key 加密保存在这台 hub 上，只发给 ", "Once sent, the key is kept encrypted on this hub and sent only to ") + name +
			l.t("，不会再显示。", "; it is never shown again."),
		Items: []wire.Component{
			{Type: "field", Name: fieldCredential, Label: "API Key", Kind: wire.FieldKindSecret},
			{Type: "field", Name: fieldName, Label: l.t("名称（可选）", "Name (optional)"), Kind: wire.FieldKindText},
			{Type: "button", Title: l.t("配对", "Pair"), Action: &wire.Action{ID: actPair, Submit: true}},
		}}}}
}

// grokView is the Grok Bot sign-in (club.logins.grok_bot.*): start gives a login id and the https page the user
// approves on, each render polls it (pending, or done with the account). A sign-in lasts 10 minutes and at most two
// wait at once; one that expired, was canceled or failed is not found any more. With st.Account it signs that
// account in again (its credential is replaced) instead of pairing a new one.
func (a *accounts) grokView(ctx context.Context, l lang, st *pageState) *wire.View {
	v := &wire.View{Title: providerName(l, grokBot)}
	start := wire.Component{Type: "button", Title: l.t("开始登录", "Start signing in"), Action: &wire.Action{ID: actGrokStart}}
	again := st.Account
	if st.Login == "" {
		intro := l.t("登录在你自己的浏览器里完成：点开始后打开授权页并批准，这里会自动检查结果。",
			"You sign in in your own browser: start, open the page and approve; this page checks for the result.")
		if again != "" {
			intro = l.t("重新登录这个 Grok Bot 账号：批准后它的凭据会被替换，会话和消息不变。",
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
			st.Note, st.NoteTone = l.t("暂时查不到登录结果（", "Could not check the sign-in just now (")+why+l.t("），稍后会再查。", "); it is checked again shortly."), wire.ToneCaution
			v.Sections = []wire.Section{{Items: a.grokPending(l, *st)}}
			v.RefreshAfter = 30
			return v
		default:
			*st = pageState{Page: pageGrok, Account: again, Note: l.t("登录没有完成：", "Sign-in did not finish: ") + reason(l, err, ""),
				NoteTone: wire.ToneDanger}
		}
		v.Sections = []wire.Section{{Items: []wire.Component{start}}}
		return v
	}
	if res.Status == protocol.LoginDone {
		name := providerName(l, grokBot)
		if res.Account != nil {
			name = accountName(*res.Account)
		}
		note := l.t("已登录并配对：", "Signed in and paired: ")
		if again != "" {
			note = l.t("已重新登录：", "Signed in again: ")
		}
		*st = pageState{Page: pageGrok, Account: again, Note: note + name, NoteTone: wire.TonePositive}
		v.Sections = []wire.Section{{Items: []wire.Component{start}}}
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
	failed := func(err error, secret string) (*wire.ActionResult, error) {
		st.Note, st.NoteTone = reason(l, err, secret), wire.ToneDanger
		return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
	}
	switch r.Action {
	case actSync:
		if err := call(ctx, a.club, protocol.MethodClubAccountsSync, protocol.AccountParams{AccountID: st.Account}, nil); err != nil {
			return failed(err, "")
		}
		return done(l.t("已同步", "Synced"))
	case actPause, actResume:
		paused := r.Action == actPause
		if err := call(ctx, a.club, protocol.MethodClubAccountsUpdate, map[string]any{"account_id": st.Account, "paused": paused}, nil); err != nil {
			return failed(err, "")
		}
		return done(l.t("已更新", "Updated"))
	case actControl:
		level, err := core.ParseControl(r.Values[fieldControl])
		if err != nil {
			return failed(protocol.Invalid(err.Error()), "")
		}
		if err := call(ctx, a.club, protocol.MethodClubAccountsUpdate, map[string]any{"account_id": st.Account, "assistant_control": level}, nil); err != nil {
			return failed(err, "")
		}
		return done(l.t("已更新", "Updated"))
	case actQuotaDisplay:
		disp := strings.TrimSpace(r.Values[fieldQuotaDisplay])
		if err := call(ctx, a.club, protocol.MethodClubAccountsUpdate, map[string]any{"account_id": st.Account, "quota_display": disp}, nil); err != nil {
			return failed(err, "")
		}
		return done(l.t("已更新", "Updated"))
	case actRemove, actPurge:
		p := protocol.RemoveAccountParams{AccountID: st.Account, DeleteData: r.Action == actPurge}
		if err := call(ctx, a.club, protocol.MethodClubAccountsRemove, p, nil); err != nil {
			return failed(err, "")
		}
		return &wire.ActionResult{Pop: true, Toast: l.t("已解除配对", "Unpaired")}, nil
	case actPair:
		cred := strings.TrimSpace(r.Values[fieldCredential])
		if cred == "" {
			return failed(protocol.Invalid(l.t("请填 API Key", "the API key is empty")), "")
		}
		var acct protocol.Account
		p := protocol.AddAccountParams{Provider: st.Provider, Credential: cred, DisplayName: strings.TrimSpace(r.Values[fieldName])}
		if err := call(ctx, a.club, protocol.MethodClubAccountsAdd, p, &acct); err != nil {
			return failed(err, cred)
		}
		return &wire.ActionResult{Pop: true, Toast: l.t("已配对：", "Paired: ") + accountName(acct)}, nil
	case actBrowserPair:
		acct, err := a.pairBrowserAccount(ctx, st.Provider, r.Values[fieldMachine], r.Values[fieldName])
		if err != nil {
			return failed(err, "")
		}
		return &wire.ActionResult{Pop: true, Toast: l.t("已配对：", "Paired: ") + accountName(acct)}, nil
	case actGrokStart:
		var res protocol.GrokBotLoginStart
		err := call(ctx, a.club, protocol.MethodClubLoginsGrokBotStart, protocol.GrokBotLoginStartParams{AccountID: st.Account}, &res)
		if protocol.ReasonOf(err) == protocol.ReasonLoginLimit {
			st.Note, st.NoteTone = l.t("已有两个登录在等待：先取消一个，或等它们 10 分钟后过期。",
				"Two sign-ins are already waiting: cancel one, or wait for them to expire after 10 minutes."), wire.ToneCaution
			return &wire.ActionResult{View: a.view(ctx, r, st), Toast: st.Note}, nil
		}
		if err != nil {
			return failed(err, "")
		}
		if res.LoginID == "" || !httpsURL(res.LoginURL) {
			return failed(fmt.Errorf("%s", l.t("登录没有给出可打开的授权页", "the sign-in gave no page to open")), "")
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
		return done(l.t("已取消", "Canceled"))
	case actTakeover:
		mid, err := a.runTakeover(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		return done(l.t("已发送夺舍指令（机器 "+mid+"），连接后请在「机器」页确认", "Bootstrap sent for "+mid+"; confirm on Machines once connected"))
	case actWake:
		mid, err := a.runWake(ctx, st.Account)
		if err != nil {
			return failed(err, "")
		}
		return done(l.t("已向机器 "+mid+" 发送唤醒指令", "Wake instructions sent for "+mid))
	}
	return nil, nil
}

func accountName(a protocol.Account) string {
	if a.DisplayName != "" {
		return a.DisplayName
	}
	return providerName("en", a.Provider)
}

func providerName(l lang, p string) string {
	switch p {
	case "cue":
		return "Cue"
	case grokBot:
		return "Grok Bot"
	case "groq":
		return "Groq"
	case echo.Name:
		return l.t("Echo（演示）", "Echo (demo)")
	}
	if label, ok := browserProviderLabel(p); ok {
		return label
	}
	return p
}

func statusBadge(l lang, status string) *wire.Badge {
	switch status {
	case protocol.AccountActive:
		return &wire.Badge{Text: l.t("正常", "Active"), Tone: wire.TonePositive}
	case protocol.AccountPaused:
		return &wire.Badge{Text: l.t("已暂停", "Paused"), Tone: wire.ToneNeutral}
	case protocol.AccountError:
		return &wire.Badge{Text: l.t("出错", "Error"), Tone: wire.ToneDanger}
	}
	return &wire.Badge{Text: status, Tone: wire.ToneNeutral}
}

func when(l lang, ms int64, loc *time.Location) string {
	if ms <= 0 {
		return l.t("从未", "never")
	}
	return time.UnixMilli(ms).In(loc).Format("2006-01-02 15:04")
}

func httpsURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme == "https" && u.Host != ""
}
