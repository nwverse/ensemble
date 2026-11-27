package extension

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// Repositories of the agents that mount them (Claude Duet's channels): what each channel has, what the agent may
// see, and adding to both. One flow serves the account page's 仓库 page and the assistant's tools:
//
//  1. read what the agent sees now (the authorized repositories, each channel's mounted ones);
//  2. a repository it cannot see yet is only added to the code host app's grant when that was asked for (the
//     authorize tool, the page's 「去 GitHub 授权并挂载」, each confirmed first), and only that repository: others stay
//     selected, a grant of selected repositories is never switched to all. Anything GitHub puts in the way
//     (signed out, confirm access, an owner's approval, a human check) stops it with the page's address;
//  3. what the agent can see is mounted on the channel, keeping what it has, and read back.
//
// What is left (a repository still to tick on GitHub) comes back as a guide: the page to open, the repositories to
// tick there, and how to go on afterwards (the same call again; 「重新检测并挂载」 on the page).

// repoState is how a bind ended.
type repoState string

const (
	repoMounted      repoState = "mounted"         // every repository asked for is mounted now
	repoAlready      repoState = "already_mounted" // they all were already
	repoNeedsGitHub  repoState = "needs_github"    // some still have to be granted on GitHub (guide)
	repoMountRefused repoState = "mount_refused"   // the agent refused the channel change: nothing changed
	repoUnconfirmed  repoState = "unconfirmed"     // written, but reading it back did not show every repository
)

// repoStatus is one repository asked about.
type repoStatus struct {
	URL        string `json:"url"`
	Authorized bool   `json:"authorized"`
	Mounted    bool   `json:"mounted"`
	// MountedIn names the channels it is mounted in, when the check was not about one channel (repos_list without
	// channel): Mounted then says it is mounted in any of them.
	MountedIn []string `json:"mounted_in,omitempty"`
}

// repoGuide is what the owner does on GitHub for what is left.
type repoGuide struct {
	URL    string   `json:"url"`    // the page to open: the installation's settings page when known
	Owner  string   `json:"owner"`  // the account the app is installed on
	Repos  []string `json:"repos"`  // to tick there
	Why    string   `json:"why"`    // why it stopped, in words
	Steps  []string `json:"steps"`  // what to do there
	Resume string   `json:"resume"` // how to go on once done
	// Stop is the code host's stop (sudo, sign_in, approval, …), "" when GitHub was not tried.
	Stop provider.AuthorizeState `json:"stop,omitempty"`
}

// repoOutcome is the end of a bind, for the page and the tools.
type repoOutcome struct {
	State        repoState                  `json:"state"`
	AccountID    string                     `json:"account_id"`
	Channel      string                     `json:"channel"`
	ChannelID    string                     `json:"channel_id"`
	ChannelURL   string                     `json:"channel_url,omitempty"`
	Repos        []repoStatus               `json:"repos"`
	Mounted      []string                   `json:"mounted"` // the channel's repositories afterwards
	Added        []string                   `json:"added,omitempty"`
	GitHub       []provider.AuthorizeResult `json:"github,omitempty"`
	Guide        *repoGuide                 `json:"guide,omitempty"`
	Refused      string                     `json:"refused,omitempty"`
	At           time.Time                  `json:"at"`
	AuthorizeRan bool                       `json:"-"`
}

// repoSpace is what the flow needs of core.Space (a fake in tests).
type repoSpace interface {
	AccountRepos(ctx context.Context, id string) (provider.RepoInventory, error)
	MountRepos(ctx context.Context, id, workspace string, repos []string) (provider.MountResult, error)
	AuthorizeRepos(ctx context.Context, id, owner string, repos []string) (provider.AuthorizeResult, error)
}

var _ repoSpace = (*core.Space)(nil)

// repoRetry is how often and how long apart the agent's list is read again after GitHub saved a grant: Claude's
// list follows GitHub's within seconds.
var repoRetry = struct {
	Times int
	Wait  time.Duration
}{3, 3 * time.Second}

// parseRepos is each repository named in s (commas, spaces or new lines between), canonical, once; bad are the
// words that name none.
func parseRepos(in []string) (repos, bad []string) {
	for _, s := range in {
		for _, w := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == '，' || r == '、' || r == ' ' || r == '\n' || r == '\t' }) {
			c, ok := provider.CanonicalRepo(w)
			if !ok {
				bad = append(bad, w)
				continue
			}
			if !slices.ContainsFunc(repos, func(x string) bool { return provider.SameRepo(x, c) }) {
				repos = append(repos, c)
			}
		}
	}
	return repos, bad
}

// repoPlan is what a bind would do, from an inventory: mount the authorized ones not mounted yet, and grant (or
// guide to) the rest.
type repoPlan struct {
	Workspace    provider.Workspace
	Statuses     []repoStatus
	ToMount      []string
	Unauthorized []string
}

func planRepos(inv provider.RepoInventory, ws provider.Workspace, repos []string) repoPlan {
	p := repoPlan{Workspace: ws}
	for _, r := range repos {
		st := repoStatus{URL: r, Authorized: inv.IsAuthorized(r),
			Mounted: slices.ContainsFunc(ws.Repos, func(m string) bool { return provider.SameRepo(m, r) })}
		p.Statuses = append(p.Statuses, st)
		switch {
		case st.Mounted:
		case st.Authorized:
			p.ToMount = append(p.ToMount, r)
		default:
			p.Unauthorized = append(p.Unauthorized, r)
		}
	}
	return p
}

// checkEverywhere is each repository's status across every channel (main chat included): mounted when any channel
// has it, with the channels that do.
func checkEverywhere(inv provider.RepoInventory, repos []string) []repoStatus {
	out := make([]repoStatus, 0, len(repos))
	for _, r := range repos {
		st := repoStatus{URL: r, Authorized: inv.IsAuthorized(r)}
		for _, w := range inv.Workspaces {
			if slices.ContainsFunc(w.Repos, func(m string) bool { return provider.SameRepo(m, r) }) {
				st.Mounted = true
				st.MountedIn = append(st.MountedIn, w.Name+"（"+w.ID+"）")
			}
		}
		out = append(out, st)
	}
	return out
}

// byOwner groups canonical repositories by their owner, in first-seen order.
func byOwner(repos []string) (owners []string, of map[string][]string) {
	of = map[string][]string{}
	for _, r := range repos {
		o, _ := provider.RepoOwnerName(r)
		k := strings.ToLower(o)
		if _, ok := of[k]; !ok {
			owners = append(owners, o)
		}
		of[k] = append(of[k], r)
	}
	return owners, of
}

// bindRepos runs the flow on account id's channel (an id, a name, or main). authorize says whether repositories
// the agent cannot see may be added to the GitHub App's grant (always after the user's yes).
func bindRepos(ctx context.Context, space repoSpace, id, channel string, repos []string, authorize bool) (repoOutcome, error) {
	repos, bad := parseRepos(repos)
	if len(bad) > 0 || len(repos) == 0 {
		return repoOutcome{}, protocol.Invalid("这些不是 GitHub 仓库：" + strings.Join(bad, "、"))
	}
	inv, err := space.AccountRepos(ctx, id)
	if err != nil {
		return repoOutcome{}, err
	}
	ws, err := resolveChannel(inv, channel)
	if err != nil {
		return repoOutcome{}, err
	}
	out := repoOutcome{AccountID: id, Channel: ws.Name, ChannelID: ws.ID, ChannelURL: ws.URL, At: time.Now()}
	plan := planRepos(inv, ws, repos)
	var stops []provider.AuthorizeResult
	if authorize && len(plan.Unauthorized) > 0 {
		out.AuthorizeRan = true
		owners, of := byOwner(plan.Unauthorized)
		granted := false
		for _, o := range owners {
			res, err := space.AuthorizeRepos(ctx, id, o, of[strings.ToLower(o)])
			if err != nil {
				// The run may have ended after GitHub saved: say so rather than "nothing changed".
				res = provider.AuthorizeResult{State: provider.AuthorizeUnconfirmed, Owner: o, URL: inv.GrantURL,
					Repos: of[strings.ToLower(o)]}
			}
			out.GitHub = append(out.GitHub, res)
			switch {
			case res.State == provider.AuthorizeDone || res.State == provider.AuthorizeAlready:
				granted = true
			default:
				stops = append(stops, res)
			}
		}
		if granted {
			// Claude's list follows GitHub's: read it again until the new ones show (or give up and guide).
			for i := 0; i < repoRetry.Times; i++ {
				if fresh, err := space.AccountRepos(ctx, id); err == nil {
					inv = fresh
					if w, ok := inv.Workspace(ws.ID); ok {
						ws = w
					}
				}
				plan = planRepos(inv, ws, repos)
				if len(plan.Unauthorized) == 0 || i == repoRetry.Times-1 {
					break
				}
				select {
				case <-ctx.Done():
					return repoOutcome{}, ctx.Err()
				case <-time.After(repoRetry.Wait):
				}
			}
		}
	}
	out.Mounted = ws.Repos
	if len(plan.ToMount) > 0 {
		m, err := space.MountRepos(ctx, id, ws.ID, plan.ToMount)
		if err != nil {
			return repoOutcome{}, err
		}
		out.Mounted = m.After
		switch {
		case m.Refused != "":
			out.State, out.Refused = repoMountRefused, m.Refused
		case !m.Verified(plan.ToMount):
			out.State = repoUnconfirmed
		default:
			out.Added = slices.Clone(plan.ToMount)
		}
		ws.Repos = m.After
	}
	out.Repos = planRepos(inv, ws, repos).Statuses
	if out.State == "" {
		switch {
		case len(plan.Unauthorized) > 0:
			out.State = repoNeedsGitHub
		case len(out.Added) > 0:
			out.State = repoMounted
		default:
			out.State = repoAlready
		}
	}
	if len(plan.Unauthorized) > 0 {
		out.Guide = guideFor(inv, plan.Unauthorized, stops, out.AuthorizeRan)
	}
	return out, nil
}

// guideFor is the guide for repositories still to grant: the stop GitHub gave (the first owner's), else the app's
// install page.
func guideFor(inv provider.RepoInventory, pending []string, stops []provider.AuthorizeResult, tried bool) *repoGuide {
	g := &repoGuide{URL: inv.GrantURL, Repos: pending,
		Resume: "在 GitHub 页面保存后，回到 Ensemble 账号页的「仓库」点「重新检测并挂载」，或让 Persona 助手用同样的参数再调用一次 club_agent_repo_bind。"}
	owners, _ := byOwner(pending)
	if len(owners) > 0 {
		g.Owner = owners[0]
	}
	if len(stops) > 0 {
		s := stops[0]
		g.Stop, g.Owner = s.State, s.Owner
		if httpsURL(s.URL) {
			g.URL = s.URL
		}
	}
	// One page is one owner's installation: the guide names that owner's repositories, and the others after.
	var others []string
	mine := []string{}
	for _, r := range pending {
		if o, _ := provider.RepoOwnerName(r); strings.EqualFold(o, g.Owner) {
			mine = append(mine, r)
		} else {
			others = append(others, r)
		}
	}
	g.Repos = mine
	names := make([]string, 0, len(mine))
	for _, r := range mine {
		o, n := provider.RepoOwnerName(r)
		names = append(names, o+"/"+n)
	}
	tick := "在「Repository access」里保持「Only select repositories」，用「Select repositories」只增选 " +
		strings.Join(names, "、") + "（已选的仓库一个都不要动，不要改成「All repositories」），然后点 Save。"
	switch g.Stop {
	case provider.AuthorizeSudo:
		g.Why = "GitHub 要你先确认身份（passkey、GitHub Mobile、验证器或邮件验证码），Ensemble 不会替你确认，也不会问你密码。"
		g.Steps = []string{"在电脑的浏览器里打开下面的链接，按 GitHub 的提示自己确认身份", tick}
	case provider.AuthorizeSignIn:
		g.Why = "电脑上的浏览器（" + browserName + "）没有登录 GitHub。Ensemble 不会替你登录，也不会问你密码。"
		g.Steps = []string{"在电脑的 " + browserName + " 里登录 GitHub，再打开下面的链接", tick}
	case provider.AuthorizeApproval:
		g.Why = "这个 GitHub App 装在组织 " + g.Owner + " 上，你不是它的 owner：改它要组织 owner 批准。"
		g.Steps = []string{"打开下面的链接提交申请，或请 " + g.Owner + " 的 owner 照下面做", tick}
	case provider.AuthorizeHumanCheck:
		g.Why = "GitHub 要求人机验证，Ensemble 不会绕过。"
		g.Steps = []string{"在电脑的浏览器里打开下面的链接，完成验证", tick}
	case provider.AuthorizeNotInstalled:
		g.Why = "Claude GitHub App 还没装在 " + g.Owner + " 上。安装是 GitHub 上的一次授权，要你自己做。"
		g.Steps = []string{"打开下面的链接，选 " + g.Owner + "，选「Only select repositories」并只勾 " + strings.Join(names, "、"), "安装后回来"}
	case provider.AuthorizeNotFound:
		g.Why = "GitHub 上找不到其中的仓库，或这个 GitHub 账号看不到它：先确认名字。"
		g.Steps = []string{"确认仓库名字和你的权限", tick}
	case provider.AuthorizeUnrecognized:
		g.Why = "GitHub 的授权页不是预期的样子，Ensemble 没有动它。"
		g.Steps = []string{"打开下面的链接自己核对", tick}
	case provider.AuthorizeUnconfirmed:
		g.Why = "可能已经在 GitHub 上保存了，但没能读回确认（或中途出错）：请打开页面核对，不要重复操作。"
		g.Steps = []string{"打开下面的链接，看这些仓库是否已勾上", "没勾上的再照这样做：" + tick}
	default:
		if tried {
			g.Why = "GitHub 上已经保存，但 Claude 那边还没看到这些仓库：稍等一会儿再重新检测。"
			g.Steps = []string{"等一两分钟", "点「重新检测并挂载」"}
		} else {
			g.Why = "这些仓库还没授权给 Claude GitHub App，Claude Duet 看不到它们。"
			g.Steps = []string{"打开下面的链接，选 " + g.Owner + " 的「Configure」", tick}
		}
	}
	if len(others) > 0 {
		g.Steps = append(g.Steps, "其他账号下的仓库（"+strings.Join(others, "、")+"）在各自 owner 的 Claude GitHub App 设置里照样勾选："+inv.GrantURL)
	}
	return g
}

// resolveChannel is the channel ref names, refusing a name two channels share (a write must not pick one).
func resolveChannel(inv provider.RepoInventory, ref string) (provider.Workspace, error) {
	ws, ok := inv.Workspace(ref)
	if !ok {
		return provider.Workspace{}, errNoChannel(ref, inv)
	}
	if ws.ID != strings.TrimSpace(ref) {
		n := 0
		for _, w := range inv.Workspaces {
			if strings.EqualFold(w.Name, ws.Name) {
				n++
			}
		}
		if n > 1 {
			return provider.Workspace{}, protocol.Invalid("有 " + strconv.Itoa(n) + " 个频道叫「" + ws.Name + "」：请用频道 id（chan_…）")
		}
	}
	return ws, nil
}

// errNoChannel names the channels there are, so the caller can choose one.
func errNoChannel(asked string, inv provider.RepoInventory) error {
	var names []string
	for _, w := range inv.Workspaces {
		names = append(names, w.Name+"（"+w.ID+"）")
	}
	return protocol.NotFound("没有频道「" + asked + "」。现有的：" + strings.Join(names, "、"))
}

// repoMemo keeps each account's last inventory (briefly) and last outcome, shared by the page and the tools: the
// page reads the agent once a minute at most, and shows what the assistant last did.
type repoMemo struct {
	mu       sync.Mutex
	inv      map[string]memoInv
	outcomes map[string]repoOutcome
}

type memoInv struct {
	at  time.Time
	inv provider.RepoInventory
}

const repoInvTTL = time.Minute

var repoMemos = &repoMemo{inv: map[string]memoInv{}, outcomes: map[string]repoOutcome{}}

func (m *repoMemo) inventory(ctx context.Context, space repoSpace, id string, fresh bool) (provider.RepoInventory, error) {
	m.mu.Lock()
	c, ok := m.inv[id]
	m.mu.Unlock()
	if ok && !fresh && time.Since(c.at) < repoInvTTL {
		return c.inv, nil
	}
	inv, err := space.AccountRepos(ctx, id)
	if err != nil {
		return provider.RepoInventory{}, err
	}
	m.mu.Lock()
	m.inv[id] = memoInv{at: time.Now(), inv: inv}
	m.mu.Unlock()
	return inv, nil
}

func (m *repoMemo) remember(o repoOutcome) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outcomes[o.AccountID] = o
	delete(m.inv, o.AccountID) // read again next time
}

func (m *repoMemo) last(id string) (repoOutcome, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.outcomes[id]
	return o, ok
}
