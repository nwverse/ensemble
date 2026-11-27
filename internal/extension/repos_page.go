package extension

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// The account's 仓库 page (repos.go): what each channel has mounted, what the Claude GitHub App was granted, and a
// form to mount repositories on a channel. The page is the owner's own hands: each write asks first (the action's
// confirm), as the assistant's tools ask on the phone.

const pageRepos = "repos"

const (
	actRepoCheck     = "repo_check"
	actRepoBind      = "repo_bind"
	actRepoAuthorize = "repo_authorize"
	actRepoResume    = "repo_resume"
	actRepoRefresh   = "repo_refresh"
)

const (
	fieldRepoChannel = "repo_channel"
	fieldRepos       = "repos"
)

// repoChecked is a check's outcome on the page (nothing written).
const repoChecked repoState = "checked"

// repoOutcomeShown is how long the page shows the last outcome.
const repoOutcomeShown = 30 * time.Minute

// shortRepo is owner/name.
func shortRepo(u string) string {
	o, n := provider.RepoOwnerName(u)
	if n == "" {
		return u
	}
	return o + "/" + n
}

func shortRepos(us []string) []string {
	out := make([]string, 0, len(us))
	for _, u := range us {
		out = append(out, shortRepo(u))
	}
	return out
}

// reposEntry is the account page's 仓库 section, for an account whose agent mounts repositories.
func reposEntry(l lang, at string, acct protocol.Account) wire.Section {
	return wire.Section{Title: l.t("仓库", "Repositories"),
		Footer: l.t("看 Claude Duet 每个频道挂了哪些仓库、Claude GitHub App 授权了哪些，把仓库挂到频道；没授权的可以去 GitHub 增选。",
			"See each Claude Duet channel's repositories and what the Claude GitHub App was granted; mount repositories, and add missing ones on GitHub."),
		Items: []wire.Component{{Type: "button", Title: l.t("查看和挂载仓库", "View and mount repositories"),
			Action: &wire.Action{ID: "open", Push: &wire.Push{UI: at, Title: l.t("仓库", "Repositories"),
				State: ext.EncodeState(pageState{Page: pageRepos, Account: acct.AccountID})}}}}}
}

func (a *accounts) reposView(ctx context.Context, r *ext.UIRequest, l lang, st *pageState) *wire.View {
	v := &wire.View{Title: l.t("仓库", "Repositories")}
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
	v.Title = l.t("仓库 · ", "Repositories · ") + accountTitle(l, acct)
	var sections []wire.Section
	if o, ok := repoMemos.last(acct.AccountID); ok && time.Since(o.At) < repoOutcomeShown {
		sections = append(sections, outcomeSections(l, o)...)
	}
	inv, err := repoMemos.inventory(ctx, space, acct.AccountID, false)
	if err != nil {
		words := reason(l, err, "")
		if p, ok := problemOf(l, protocol.ReasonOf(err), placerFor(ctx, space, a.browser).of(ctx, acct, protocol.ReasonOf(err))); ok {
			words = p.Text(l)
		}
		sections = append(sections, wire.Section{Title: l.t("读不到仓库", "Could not read the repositories"), Items: []wire.Component{
			{Type: "text", Text: words},
			{Type: "button", Title: l.t("重试", "Try again"), Action: &wire.Action{ID: actRepoRefresh}}}})
		v.Sections = sections
		return v
	}
	sections = append(sections, mountForm(l, inv, st), channelsSection(l, inv), grantSection(l, inv))
	v.Sections = sections
	return v
}

// mountForm is 挂载到频道: the channel, the repositories, and the three ways on.
func mountForm(l lang, inv provider.RepoInventory, st *pageState) wire.Section {
	choices := make([]wire.Choice, 0, len(inv.Workspaces))
	value := st.Chan
	for _, w := range inv.Workspaces {
		title := "#" + w.Name
		if w.Main {
			title = w.Name
		}
		choices = append(choices, wire.Choice{Value: w.ID, Title: title})
		if value == "" && w.Main {
			value = w.ID
		}
	}
	return wire.Section{Title: l.t("挂载到频道", "Mount on a channel"),
		Footer: l.t("只往频道里添加，已挂的仓库都保留。「去 GitHub 授权并挂载」只在 GitHub 上增选你填的仓库，已选的不动，不会改成 All repositories；GitHub 要你登录、确认身份（密码/2FA/passkey）或组织审批时会停下，给你直达链接，不会替你输入密码。",
			"Only adds: the channel keeps its repositories. \"Authorize on GitHub and mount\" only adds the repositories you name on GitHub, never unticks one or switches to All repositories; it stops with a link when GitHub asks you to sign in, confirm access or get an owner's approval, and never types a password."),
		Items: []wire.Component{
			{Type: "field", Name: fieldRepoChannel, Label: l.t("频道", "Channel"), Kind: wire.FieldKindChoice, Choices: choices, Value: value},
			{Type: "field", Name: fieldRepos, Label: l.t("仓库", "Repositories"), Kind: wire.FieldKindMultiline, Value: st.Repos,
				Placeholder: "nwverse/openlinkedin, nwverse/openbilibili"},
			{Type: "button", Title: l.t("检查", "Check"), Action: &wire.Action{ID: actRepoCheck, Submit: true}},
			{Type: "button", Title: l.t("挂载（只挂已授权的）", "Mount (granted ones only)"), Action: &wire.Action{ID: actRepoBind, Submit: true,
				Confirm: l.t("把填的仓库里已授权的挂到这个频道（已挂的保留）。没授权的会给你 GitHub 授权页链接，这次不改 GitHub。继续吗？",
					"Mount the granted ones on this channel (keeping what it has). Ungranted ones get a GitHub link; GitHub is not changed. Continue?")}},
			{Type: "button", Title: l.t("去 GitHub 授权并挂载", "Authorize on GitHub and mount"), Action: &wire.Action{ID: actRepoAuthorize, Submit: true,
				Confirm: l.t("会在你电脑浏览器里的 GitHub 上，把没授权的仓库增选进 Claude GitHub App（已选的不动，不改成 All repositories），再挂到这个频道。GitHub 要确认身份时会停下给你链接。继续吗？",
					"In your computer's browser, adds the ungranted repositories to the Claude GitHub App on GitHub (keeping the others, never All repositories), then mounts them. Stops with a link if GitHub asks to confirm access. Continue?")}},
		}}
}

func channelsSection(l lang, inv provider.RepoInventory) wire.Section {
	sec := wire.Section{Title: l.t("各频道已挂载的仓库", "Repositories on each channel"),
		Footer: l.t("一个频道可以挂多个仓库。", "A channel can have several repositories.")}
	for _, w := range inv.Workspaces {
		title := "#" + w.Name
		if w.Main {
			title = w.Name
		}
		c := wire.Component{Type: "row", Title: title, Value: fmt.Sprintf(l.t("%d 个", "%d"), len(w.Repos))}
		if len(w.Repos) > 0 {
			c.Detail = strings.Join(shortRepos(w.Repos), l.t("、", ", "))
		} else {
			c.Detail = l.t("还没挂仓库", "None yet")
		}
		sec.Items = append(sec.Items, c)
	}
	if len(sec.Items) == 0 {
		sec.Items = []wire.Component{{Type: "text", Text: l.t("没有频道", "No channels")}}
	}
	return sec
}

func grantSection(l lang, inv provider.RepoInventory) wire.Section {
	names := make([]string, 0, len(inv.Authorized))
	for _, r := range inv.Authorized {
		names = append(names, shortRepo(r.URL))
	}
	text := strings.Join(names, l.t("、", ", "))
	if text == "" {
		text = l.t("还没有授权任何仓库", "No repositories granted yet")
	}
	items := []wire.Component{{Type: "text", Text: text}}
	if httpsURL(inv.GrantURL) {
		items = append(items, wire.Component{Type: "link", Title: l.t("在 GitHub 管理 Claude GitHub App 的授权", "Manage the Claude GitHub App on GitHub"), URL: inv.GrantURL})
	}
	return wire.Section{Title: fmt.Sprintf(l.t("Claude GitHub App 已授权的仓库（%d 个）", "Granted to the Claude GitHub App (%d)"), len(inv.Authorized)),
		Items: items}
}

// outcomeSections is the last outcome: what happened, each repository's status, and the guide when GitHub is left.
func outcomeSections(l lang, o repoOutcome) []wire.Section {
	head, tone := outcomeHead(l, o)
	sec := wire.Section{Title: l.t("上次结果 · ", "Last result · ") + o.Channel,
		Items: []wire.Component{{Type: "row", Title: head, Badge: &wire.Badge{Text: outcomeBadge(l, o.State), Tone: tone}}}}
	for _, s := range o.Repos {
		val, t := l.t("未授权", "Not granted"), wire.ToneCaution
		switch {
		case s.Mounted:
			val, t = l.t("已挂载", "Mounted"), wire.TonePositive
		case s.Authorized:
			val, t = l.t("已授权，未挂载", "Granted, not mounted"), wire.ToneNeutral
		}
		sec.Items = append(sec.Items, wire.Component{Type: "row", Title: shortRepo(s.URL), Value: val, Badge: &wire.Badge{Text: val, Tone: t}})
	}
	out := []wire.Section{sec}
	if g := o.Guide; g != nil {
		gs := wire.Section{Title: l.t("还要在 GitHub 上勾选", "Still to tick on GitHub"), Footer: g.Resume,
			Items: []wire.Component{{Type: "text", Text: g.Why}}}
		for i, s := range g.Steps {
			gs.Items = append(gs.Items, wire.Component{Type: "text", Text: fmt.Sprintf("%d. %s", i+1, s)})
		}
		gs.Items = append(gs.Items, wire.Component{Type: "row", Title: l.t("要勾的仓库", "To tick"), Value: strings.Join(shortRepos(g.Repos), l.t("、", ", "))})
		if httpsURL(g.URL) {
			gs.Items = append(gs.Items, wire.Component{Type: "link", Title: l.t("打开 GitHub 授权页", "Open the GitHub page"), URL: g.URL})
		}
		gs.Items = append(gs.Items, wire.Component{Type: "button", Title: l.t("重新检测并挂载", "Check again and mount"),
			Action: &wire.Action{ID: actRepoResume, Values: map[string]string{fieldRepoChannel: o.ChannelID,
				fieldRepos: strings.Join(shortRepos(resumeRepos(o)), ", ")}, Confirm: l.t("重新读 Claude 能看到的仓库，把已授权的挂到「"+o.Channel+"」（已挂的保留）。继续吗？",
				"Read the granted repositories again and mount them on \""+o.Channel+"\" (keeping what it has). Continue?")}})
		out = append(out, gs)
	}
	return out
}

func outcomeHead(l lang, o repoOutcome) (string, string) {
	switch o.State {
	case repoMounted:
		return l.t("已挂好并读回确认：", "Mounted and read back: ") + strings.Join(shortRepos(o.Added), l.t("、", ", ")), wire.TonePositive
	case repoAlready:
		return l.t("都已经挂在这个频道", "All already mounted here"), wire.TonePositive
	case repoNeedsGitHub:
		if len(o.Added) > 0 {
			return l.t("已挂好 ", "Mounted ") + strings.Join(shortRepos(o.Added), l.t("、", ", ")) + l.t("；其余还要在 GitHub 上授权", "; the rest need GitHub"), wire.ToneCaution
		}
		return l.t("还要在 GitHub 上授权", "Needs granting on GitHub"), wire.ToneCaution
	case repoMountRefused:
		return l.t("Claude 拒绝了这次修改，频道没有变（", "Claude refused the change; the channel is unchanged (") + o.Refused + ")", wire.ToneDanger
	case repoUnconfirmed:
		return l.t("已提交，但读回时没看到全部仓库", "Sent, but reading back did not show them all"), wire.ToneCaution
	case repoChecked:
		return l.t("检查结果（没有改动）", "Checked (nothing changed)"), wire.ToneNeutral
	}
	return string(o.State), wire.ToneNeutral
}

func outcomeBadge(l lang, s repoState) string {
	switch s {
	case repoMounted, repoAlready:
		return l.t("完成", "Done")
	case repoNeedsGitHub:
		return l.t("要你处理", "Needs you")
	case repoMountRefused:
		return l.t("没改", "Unchanged")
	case repoUnconfirmed:
		return l.t("待核对", "Check")
	}
	return l.t("检查", "Checked")
}

// repoAction handles the 仓库 page's actions.
func (a *accounts) repoAction(ctx context.Context, r *ext.UIRequest, l lang, st pageState) (*wire.ActionResult, error) {
	show := func() (*wire.ActionResult, error) { return &wire.ActionResult{View: a.view(ctx, r, st)}, nil }
	refuse := func(note string) (*wire.ActionResult, error) {
		st.Note, st.NoteTone = note, wire.ToneDanger
		return &wire.ActionResult{View: a.view(ctx, r, st), Toast: note}, nil
	}
	space, err := a.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return refuse(reason(l, err, ""))
	}
	if r.Action == actRepoRefresh {
		if _, err := repoMemos.inventory(ctx, space, st.Account, true); err != nil {
			return refuse(reason(l, err, ""))
		}
		return show()
	}
	// The form's fields, or what 重新检测并挂载 carries (the outcome's channel and repositories, whether the page or the
	// assistant made it); the page's own state when an older app sends no values.
	if c := strings.TrimSpace(r.Values[fieldRepoChannel]); c != "" || r.Action != actRepoResume {
		st.Chan = c
	}
	if v := strings.TrimSpace(r.Values[fieldRepos]); v != "" || r.Action != actRepoResume {
		st.Repos = v
	}
	repos, bad := parseRepos([]string{st.Repos})
	if len(bad) > 0 {
		return refuse(l.t("这些不是 GitHub 仓库：", "Not GitHub repositories: ") + strings.Join(bad, ", ") + l.t("（写成 owner/name）", " (write owner/name)"))
	}
	if len(repos) == 0 || st.Chan == "" {
		return refuse(l.t("先选频道，再填至少一个仓库", "Choose a channel and name at least one repository"))
	}
	if len(repos) > 20 {
		return refuse(l.t("一次最多 20 个仓库", "At most 20 repositories at a time"))
	}
	if r.Action == actRepoCheck {
		inv, err := repoMemos.inventory(ctx, space, st.Account, true)
		if err != nil {
			return refuse(reason(l, err, ""))
		}
		ws, ok := inv.Workspace(st.Chan)
		if !ok {
			return refuse(errNoChannel(st.Chan, inv).Error())
		}
		plan := planRepos(inv, ws, repos)
		o := repoOutcome{State: repoChecked, AccountID: st.Account, Channel: ws.Name, ChannelID: ws.ID, ChannelURL: ws.URL,
			Repos: plan.Statuses, Mounted: ws.Repos, At: time.Now()}
		if len(plan.Unauthorized) > 0 {
			o.Guide = guideFor(inv, plan.Unauthorized, nil, false)
		}
		repoMemos.remember(o)
		return show()
	}
	out, err := bindRepos(ctx, space, st.Account, st.Chan, repos, r.Action == actRepoAuthorize)
	if err != nil {
		return refuse(reason(l, err, ""))
	}
	repoMemos.remember(out)
	head, tone := outcomeHead(l, out)
	st.Note, st.NoteTone = head, tone
	return &wire.ActionResult{View: a.view(ctx, r, st), Toast: head}, nil
}

// resumeRepos are the repositories an outcome was about.
func resumeRepos(o repoOutcome) []string {
	out := make([]string, 0, len(o.Repos))
	for _, r := range o.Repos {
		out = append(out, r.URL)
	}
	return out
}
