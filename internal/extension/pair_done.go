package extension

import (
	"context"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// The step a pairing ends on (a key, a computer's browser or the Grok Bot sign-in): what was added and where its
// chats will show, then right there whether the Persona assistant may use it (助手指挥) and, where it can be one,
// whether it is a main assistant (主助手). It asks and does not decide: a new account stays off until the user picks
// a level (core.DefaultControl), and leaving without picking keeps it so, which the page says.
const (
	pageDone       = "done"
	actFinish      = "finish"
	actOpenAccount = "open_account"
)

func (a *accounts) doneView(ctx context.Context, l lang, st pageState) *wire.View {
	v := &wire.View{Title: l.t("已添加", "Added")}
	finish := wire.Section{Items: []wire.Component{{Type: "button", Title: l.t("好了", "Done"), Action: &wire.Action{ID: actFinish}}}}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text", Text: reason(l, err, "")}}}, finish}
		return v
	}
	acct, err := space.Account(ctx, st.Account)
	if err != nil {
		// Removed meanwhile (or never there): say so, and still offer the way back.
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "empty", Icon: "warning",
			Title: l.t("这个账号已不在", "This account is gone"), Detail: reason(l, err, "")}}}, finish}
		return v
	}
	control, err := space.AssistantControl(ctx, acct.AccountID)
	if err != nil {
		// Without the level the question would show a choice that is not the saved one: say so instead.
		v.Sections = []wire.Section{{Items: []wire.Component{{Type: "text",
			Text: l.t("已添加「", "Added \"") + accountTitle(l, acct) + l.t("」，但现在读不到 Persona 助手能不能用它：", "\", but whether the Persona assistant may use it cannot be read now: ") +
				reason(l, err, "") + l.t(" 稍后到这个账号的页面里看「助手指挥」。", " Look at Assistant control on its page later.")}}},
			{Items: []wire.Component{finish.Items[0],
				{Type: "button", Title: l.t("打开这个账号", "Open this account"), Action: &wire.Action{ID: actOpenAccount}}}}}
		return v
	}

	// Pairing synced the account before this step (core AddAccount), so its chats are in, unless that sync failed.
	synced := l.t("」。它的对话已经同步好，可以在 Persona 的「Agents」页看到；之后会自动更新。",
		"\". Its chats are synced and on Persona's Agents page; they keep updating on their own.")
	if acct.Status == protocol.AccountError || acct.LastError != "" {
		synced = l.t("」，但第一次同步没成功：", "\", but its first sync did not work: ") + problemWords(l, acct)
	}
	added := wire.Section{Items: []wire.Component{{Type: "text", Text: l.t("已添加「", "Added \"") + accountTitle(l, acct) + synced}}}

	now := l.t("现在是「", "Now: ") + controlChoiceTitle(l, control) + l.t("」。", ". ")
	if control == core.ControlOff {
		now += l.t("不选就一直保持关闭，Persona 助手看不到它；以后也能在这个账号的页面里改。",
			"Leave it and it stays off, so the Persona assistant does not see it; you can change it later on the account's page.")
	} else {
		now += l.t("以后也能在这个账号的页面里改。", "You can change it later on the account's page.")
	}
	askItems := []wire.Component{
		{Type: "text", Style: "secondary", Text: l.t("关闭：Persona 助手完全看不到这个账号。", "Off: the Persona assistant does not see this account at all.")},
		{Type: "text", Style: "secondary", Text: l.t("只读：Persona 助手能读它的对话，帮你总结、提醒，但不会替你发消息。",
			"Read: the Persona assistant can read its chats to sum up and remind you, and writes nothing.")},
		{Type: "text", Style: "secondary", Text: l.t("可代发（逐次确认）：还能替你给它发消息，每次发之前都先在手机上问你。",
			"Confirm: it can also write to it for you, asking on your phone before each message.")},
		{Type: "text", Style: "secondary", Text: l.t("持续跟进：按账号页开启的对话租约，在指定对话、限定轮数和有效期内连续跟进，其余写操作仍先在手机上问你。",
			"Follow: it continues turns in chats that have an active follow-up lease on the account page, and asks first elsewhere.")},
		{Type: "text", Style: "secondary", Text: l.t("直接放行：发消息不再逐次询问（需先二次确认，受额度、未确认发送、突发频率限制及仓库强制确认保护）。",
			"Auto: it writes without asking each time (requires a second confirmation and is guarded by allowance, unconfirmed-send, burst-limit and repository confirmation rules).")},
		{Type: "segmented", Name: fieldControl, Choices: controlChoices(l, control), Value: string(control),
			Action: &wire.Action{ID: actControl}},
	}
	if st.ConfirmAuto && control != core.ControlAuto {
		askItems = append(askItems, autoConfirmItems(l)...)
	}
	ask := wire.Section{Title: l.t("要让 Persona 助手用这个账号吗？", "Should the Persona assistant use this account?"),
		Footer: now,
		Items:  askItems}
	sections := []wire.Section{added, ask}

	if a.runtimeEnabled() && a.canChat(ctx, space, acct) {
		val, now := "off", l.t("现在没有当主助手，不选就保持这样。", "It is not a main assistant now, and stays so if you leave it.")
		if acct.AssistantBackend {
			val, now = "on", l.t("现在是主助手。", "It is a main assistant now.")
		}
		sections = append(sections, wire.Section{Title: l.t("要不要让它直接当你的主助手？（可以不选）", "Make it your main assistant too? (optional)"),
			Footer: now,
			Items: []wire.Component{
				{Type: "text", Style: "secondary", Text: l.t("和上面不一样：上面是让 Persona 助手去用这个账号；主助手是让这个账号自己来当你的助手，在 Persona 对话里选它，由它来回答你。",
					"Unlike the question above (the Persona assistant using this account), this makes the account itself your assistant: pick it in Persona's chats and it answers you.")},
				{Type: "segmented", Name: fieldAssistantBackend, Value: val, Action: &wire.Action{ID: actAssistantBackend},
					Choices: []wire.Choice{{Value: "off", Title: l.t("关闭", "Off")}, {Value: "on", Title: l.t("开启", "On")}}},
			}})
	}

	finish.Items = append(finish.Items, wire.Component{Type: "button", Title: l.t("打开这个账号", "Open this account"),
		Action: &wire.Action{ID: actOpenAccount}})
	sections = append(sections, finish)
	v.Sections = sections
	return v
}

// added is a pairing's success: the done step in place of the pairing page, so 好了 goes back to the list.
func (a *accounts) added(ctx context.Context, r *ext.UIRequest, acct protocol.Account) *wire.ActionResult {
	l := lang(r.Locale)
	return &wire.ActionResult{View: a.view(ctx, r, pageState{Page: pageDone, Account: acct.AccountID}),
		Toast: l.t("已添加：", "Added: ") + accountTitle(l, acct)}
}

// canChat says whether an account can answer as a main assistant: the runtime bridge talks to it through a chat it
// can write in, or one it starts.
func (a *accounts) canChat(ctx context.Context, space *core.Space, acct protocol.Account) bool {
	if adapter, ok := a.club.Registry().Get(acct.Provider); ok {
		if _, ok := adapter.(provider.SessionCreator); ok {
			return true
		}
	}
	sessions, err := space.Sessions(ctx)
	if err != nil {
		return false
	}
	for _, s := range sessions.Sessions {
		if s.AccountID == acct.AccountID && s.CanSend {
			return true
		}
	}
	return false
}

// finish leaves the done step for the page below (the list, or the account signed in again), saying when the
// Persona assistant still cannot use the account.
func (a *accounts) finish(ctx context.Context, l lang, st pageState) *wire.ActionResult {
	res := &wire.ActionResult{Pop: true}
	if st.Page != pageDone {
		return res
	}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return res
	}
	if control, err := space.AssistantControl(ctx, st.Account); err == nil && control == core.ControlOff {
		res.Toast = l.t("好了。Persona 助手还不能用这个账号，想让它用时，到这个账号的页面里改「助手指挥」",
			"Done. The Persona assistant cannot use this account yet; change Assistant control on its page when you want it to")
	}
	return res
}
