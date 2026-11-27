package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// The assistant's repository tools (repos.go). club_agent_repos_list reads; club_agent_repo_bind and
// club_agent_repo_authorize write, so the manifest's confirm_always (club_agent_*) has the user confirm each on the
// phone, with describe's card naming the account, the channel, every repository and what will change. Writes need
// the account at 逐次确认 (the level that lets the assistant send), as a message would.
const (
	toolReposList     = "club_agent_repos_list"
	toolRepoBind      = "club_agent_repo_bind"
	toolRepoAuthorize = "club_agent_repo_authorize"
)

// describeInventoryWait bounds the inventory read the confirmation card shows statuses from.
const describeInventoryWait = 40 * time.Second

var repoReadTool = wire.ConnectorTool{Name: toolReposList,
	Description: "列出个人 Agent（目前是 Claude Duet）能看到的仓库：Claude GitHub App 已授权的仓库、每个频道（含主聊天）已挂载的仓库；带 repos 时逐个说明是否已授权、是否已挂到 channel（不传 channel 时说明挂在哪些频道：mounted 为任一频道已挂，mounted_in 列出频道）。用户要给 Agent 挂仓库、问某个仓库它能不能用时先调它。",
	InputSchema: schema(`{"type":"object","properties":{"account_id":{"type":"string","description":"只看这个账号；省略时列出所有能挂仓库的账号"},"channel":{"type":"string","description":"频道 id（chan_…）或名字，主聊天写 main"},"repos":{"type":"array","items":{"type":"string"},"description":"要检查的仓库，如 nwverse/openlinkedin 或 https://github.com/nwverse/openlinkedin"}}}`)}

var repoWriteTools = []wire.ConnectorTool{
	{Name: toolRepoBind,
		Description: "把仓库挂到个人 Agent（Claude Duet）的一个频道：只添加，频道已挂的仓库都保留，挂完读回验证。只挂已授权给 Claude GitHub App 的仓库，不改 GitHub；没授权的会返回 GitHub 授权页链接和要勾的仓库（state=needs_github），请把链接和步骤转告用户，用户在 GitHub 勾好后用同样参数再调一次。每次都要用户在手机上确认。",
		InputSchema: schema(`{"type":"object","properties":{"account_id":{"type":"string"},"channel":{"type":"string","description":"频道 id（chan_…）或名字，主聊天写 main"},"repos":{"type":"array","items":{"type":"string"}}},"required":["account_id","channel","repos"]}`)},
	{Name: toolRepoAuthorize,
		Description: "先在用户电脑浏览器里的 GitHub 上把还没授权的仓库加进 Claude GitHub App 的授权（只增选这些，已选的不动，绝不改成 All repositories），再挂到频道并验证。GitHub 要登录、确认身份（密码/2FA/passkey）、组织 owner 审批或人机验证时会停下，不绕过、不问密码，返回直达链接和步骤（state=needs_github），请转告用户。每次都要用户在手机上确认。",
		InputSchema: schema(`{"type":"object","properties":{"account_id":{"type":"string"},"channel":{"type":"string","description":"频道 id（chan_…）或名字，主聊天写 main"},"repos":{"type":"array","items":{"type":"string"}}},"required":["account_id","channel","repos"]}`)},
}

func init() {
	readTools.Tools = append(readTools.Tools, repoReadTool)
	writeTools.Tools = append(writeTools.Tools, repoWriteTools...)
}

type repoArgs struct {
	AccountID string   `json:"account_id"`
	Channel   string   `json:"channel"`
	Repos     []string `json:"repos"`
}

// repoAccount is an account the assistant may do op on whose agent mounts repositories.
func (t *tools) repoAccount(ctx context.Context, id string, op core.Operation) (*core.Space, protocol.Account, error) {
	acct, err := t.account(ctx, id, op)
	if err != nil {
		return nil, protocol.Account{}, err
	}
	space, err := t.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return nil, protocol.Account{}, err
	}
	if !space.MountsRepos(ctx, acct.AccountID) {
		return nil, protocol.Account{}, protocol.Unsupported(providerName("zh", acct.Provider) + " 没有可挂载的仓库（目前只有 Claude Duet 支持）")
	}
	return space, acct, nil
}

// repoWriteArgs checks a write's arguments: the account, the channel named, the repositories well formed.
func (t *tools) repoWriteArgs(ctx context.Context, raw json.RawMessage) (*core.Space, protocol.Account, repoArgs, []string, error) {
	var a repoArgs
	if err := args(raw, &a); err != nil {
		return nil, protocol.Account{}, a, nil, err
	}
	if strings.TrimSpace(a.Channel) == "" {
		return nil, protocol.Account{}, a, nil, protocol.Invalid("channel is required")
	}
	repos, bad := parseRepos(a.Repos)
	if len(bad) > 0 {
		return nil, protocol.Account{}, a, nil, protocol.Invalid("这些不是 GitHub 仓库：" + strings.Join(bad, "、") + "（写成 owner/name 或 https://github.com/owner/name）")
	}
	if len(repos) == 0 {
		return nil, protocol.Account{}, a, nil, protocol.Invalid("repos is required")
	}
	if len(repos) > 20 {
		return nil, protocol.Account{}, a, nil, protocol.Invalid("一次最多 20 个仓库")
	}
	space, acct, err := t.repoAccount(ctx, a.AccountID, core.OpSend)
	if err != nil {
		return nil, protocol.Account{}, a, nil, err
	}
	return space, acct, a, repos, nil
}

func (t *tools) repoList(ctx context.Context, raw json.RawMessage) (wire.ConnectorCallResult, error) {
	var a repoArgs
	if err := args(raw, &a); err != nil {
		return wire.ConnectorCallResult{}, err
	}
	repos, bad := parseRepos(a.Repos)
	if len(bad) > 0 {
		return wire.ConnectorCallResult{}, protocol.Invalid("这些不是 GitHub 仓库：" + strings.Join(bad, "、"))
	}
	space, accts, _, hidden, err := t.accounts(ctx)
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	type listed struct {
		AccountID  string               `json:"account_id"`
		Name       string               `json:"name"`
		Provider   string               `json:"provider"`
		Authorized []string             `json:"authorized"`
		Complete   bool                 `json:"authorized_complete"`
		Channels   []provider.Workspace `json:"channels"`
		GrantURL   string               `json:"grant_url"`
		Check      []repoStatus         `json:"check,omitempty"`
		CheckIn    string               `json:"check_channel,omitempty"`
		Error      string               `json:"error,omitempty"`
	}
	out := []listed{}
	for _, acct := range accts {
		if a.AccountID != "" && acct.AccountID != a.AccountID {
			continue
		}
		if !space.MountsRepos(ctx, acct.AccountID) {
			continue
		}
		l := listed{AccountID: acct.AccountID, Name: accountName(acct), Provider: acct.Provider, Authorized: []string{},
			Channels: []provider.Workspace{}}
		inv, err := repoMemos.inventory(ctx, space, acct.AccountID, true)
		if err != nil {
			l.Error = t.problemWords(ctx, space, acct.AccountID, protocol.ReasonOf(err))
			out = append(out, l)
			continue
		}
		for _, r := range inv.Authorized {
			l.Authorized = append(l.Authorized, r.URL)
		}
		l.Complete, l.Channels, l.GrantURL = inv.Complete, inv.Workspaces, inv.GrantURL
		if len(repos) > 0 {
			if strings.TrimSpace(a.Channel) == "" {
				// No channel named: mounted means mounted in any channel, which mounted_in names.
				l.Check = checkEverywhere(inv, repos)
			} else if ws, ok := inv.Workspace(a.Channel); ok {
				l.CheckIn = ws.Name + "（" + ws.ID + "）"
				l.Check = planRepos(inv, ws, repos).Statuses
			} else {
				l.Error = errNoChannel(a.Channel, inv).Error()
				l.Check = checkEverywhere(inv, repos)
			}
		}
		out = append(out, l)
	}
	if a.AccountID != "" && len(out) == 0 {
		if _, _, err := t.repoAccount(ctx, a.AccountID, core.OpList); err != nil {
			return wire.ConnectorCallResult{}, err
		}
	}
	b, _ := json.Marshal(map[string]any{"accounts": out})
	text := string(b) + "\n挂仓库用 club_agent_repo_bind（只挂已授权的）或 club_agent_repo_authorize（先在 GitHub 增选再挂），都要用户在手机上确认。"
	if len(out) == 0 {
		text = "没有能挂仓库的账号（目前只有 Claude Duet 支持）。" + text
		if len(hidden) > 0 {
			text += "\n另外，" + grantGuide(hidden)
		}
	}
	return wire.ConnectorCallResult{Text: text}, nil
}

func (t *tools) repoDescribe(ctx context.Context, p wire.ConnectorCallParams) (wire.ConnectorDescribeResult, error) {
	space, acct, a, repos, err := t.repoWriteArgs(ctx, p.Args)
	if err != nil {
		return wire.ConnectorDescribeResult{}, err
	}
	authorize := p.Name == toolRepoAuthorize
	chName, chID := a.Channel, ""
	var plan *repoPlan
	rctx, cancel := context.WithTimeout(ctx, describeInventoryWait)
	inv, invErr := repoMemos.inventory(rctx, space, acct.AccountID, false)
	cancel()
	if invErr == nil {
		ws, err := resolveChannel(inv, a.Channel)
		if err != nil {
			return wire.ConnectorDescribeResult{}, err
		}
		chName, chID = ws.Name, ws.ID
		pl := planRepos(inv, ws, repos)
		plan = &pl
	}
	return wire.ConnectorDescribeResult{
		Action:  wire.ConnectorAction{Name: repoActionName(acct, chName, authorize), Visible: providerName("zh", acct.Provider)},
		Summary: repoSummary(acct, chName, chID, repos, plan, authorize),
	}, nil
}

func repoActionName(acct protocol.Account, channel string, authorize bool) string {
	ch := "「" + strings.TrimPrefix(channel, "#") + "」"
	if authorize {
		return "在 GitHub 给 Claude GitHub App 增选仓库，并挂到 " + accountName(acct) + " 的频道" + ch
	}
	return "给 " + accountName(acct) + " 的频道" + ch + "挂载仓库"
}

// repoSummary is the confirmation card's text: who, where, each repository and what happens to it, and the bounds.
func repoSummary(acct protocol.Account, channel, channelID string, repos []string, plan *repoPlan, authorize bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "账号：%s（%s）\n", accountName(acct), providerName("zh", acct.Provider))
	if channelID != "" {
		fmt.Fprintf(&b, "频道：%s（%s）\n", channel, channelID)
	} else {
		fmt.Fprintf(&b, "频道：%s\n", channel)
	}
	b.WriteString("仓库：\n")
	for i, r := range repos {
		o, n := provider.RepoOwnerName(r)
		what := ""
		if plan != nil {
			st := plan.Statuses[i]
			switch {
			case st.Mounted:
				what = "已挂在这个频道，不变"
			case st.Authorized:
				what = "已授权，挂到这个频道"
			case authorize:
				what = "未授权：在 GitHub 上增选进 Claude GitHub App，再挂到这个频道"
			default:
				what = "未授权：这次不改 GitHub，会给你授权页链接"
			}
		} else if authorize {
			what = "未授权的会在 GitHub 上增选，已授权的直接挂"
		} else {
			what = "已授权的挂到这个频道；未授权的给你授权页链接"
		}
		fmt.Fprintf(&b, "· %s/%s：%s\n", o, n, what)
	}
	b.WriteString("影响：只往频道里添加，已挂的仓库都保留。")
	if authorize {
		b.WriteString("GitHub 上只增选上面列出的仓库，已选的仓库不动，不会改成「All repositories」；" +
			"GitHub 要你登录、确认身份（密码/2FA/passkey）、组织 owner 审批或人机验证时会停下并给你链接，不会替你输入或索要密码。")
	}
	return b.String()
}

func (t *tools) repoDo(ctx context.Context, p wire.ConnectorDoParams) (wire.ConnectorCallResult, error) {
	// The checks again: the level may have changed between the card and the user's yes.
	space, acct, a, repos, err := t.repoWriteArgs(ctx, p.Args)
	if err != nil {
		return wire.ConnectorCallResult{}, err
	}
	out, err := bindRepos(ctx, space, acct.AccountID, a.Channel, repos, p.Name == toolRepoAuthorize)
	if err != nil {
		return wire.ConnectorCallResult{}, t.explained(ctx, acct.AccountID, err)
	}
	repoMemos.remember(out)
	b, _ := json.Marshal(out)
	return wire.ConnectorCallResult{Text: repoOutcomeWords(out) + "\n" + string(b)}, nil
}

// repoOutcomeWords is a bind's end in a few words for the assistant, with what to tell the user.
func repoOutcomeWords(o repoOutcome) string {
	switch o.State {
	case repoMounted:
		return "已挂好并读回确认：" + strings.Join(o.Added, "、") + " 现在挂在频道「" + o.Channel + "」。"
	case repoAlready:
		return "这些仓库已经挂在频道「" + o.Channel + "」，没有改动。"
	case repoMountRefused:
		return "Claude 拒绝了这次频道修改（" + o.Refused + "），频道没有变：请用户在 " + o.ChannelURL + " 手动挂。"
	case repoUnconfirmed:
		return "已提交，但读回时没看到全部仓库：先用 club_agent_repos_list 核对，不要直接重试。"
	case repoNeedsGitHub:
		g := o.Guide
		w := "还有仓库没授权给 Claude GitHub App，需要用户在 GitHub 上操作（不要替用户输入密码，不要绕过）：" + g.Why +
			"\n请把这个链接发给用户：" + g.URL + "\n要勾的仓库：" + strings.Join(g.Repos, "、") +
			"\n步骤：" + strings.Join(g.Steps, "；") + "\n之后：" + g.Resume
		if len(o.Added) > 0 {
			w = "已挂好：" + strings.Join(o.Added, "、") + "。" + w
		}
		return w
	}
	return string(o.State)
}
