package extension

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// fakeDuet is a Claude Duet with repositories in memory: what the GitHub App was granted, each channel's mounted
// ones, and how GitHub answers an authorization (stop: a stop state, else it grants).
type fakeDuet struct {
	named
	mu         sync.Mutex
	authorized []string
	channels   map[string][]string
	order      []string
	names      map[string]string
	stop       provider.AuthorizeState
	refuse     string
	mounts     [][]string
	authorizes [][]string
}

func newFakeDuet() *fakeDuet {
	return &fakeDuet{named: named{"claude_duet"},
		authorized: []string{"https://github.com/nwverse/persona", "https://github.com/nwverse/ensemble"},
		channels: map[string][]string{"chan_main1": {"https://github.com/nwverse/persona"}, "chan_01CFwJZJtCoUJweZ64fQSYzu": {},
			"chan_01DCDvtmZrq32yBT59hH7XwF": {}},
		order: []string{"chan_main1", "chan_01CFwJZJtCoUJweZ64fQSYzu", "chan_01DCDvtmZrq32yBT59hH7XwF"},
		names: map[string]string{"chan_01CFwJZJtCoUJweZ64fQSYzu": "openlinkedin", "chan_01DCDvtmZrq32yBT59hH7XwF": "openbilibili"}}
}

func (f *fakeDuet) MountsRepos() bool { return true }

func (f *fakeDuet) Repos(context.Context, string) (provider.RepoInventory, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inv := provider.RepoInventory{Complete: true, GrantURL: "https://github.com/apps/claude/installations/new"}
	for _, r := range f.authorized {
		inv.Authorized = append(inv.Authorized, provider.Repo{URL: r, CanPush: true})
	}
	for _, id := range f.order {
		w := provider.Workspace{ID: id, Name: f.names[id], Repos: slices.Clone(f.channels[id]), URL: "https://claude.ai/duet/" + id}
		if id == "chan_main1" {
			w.Name, w.Main, w.URL = "主聊天", true, "https://claude.ai/duet"
		}
		inv.Workspaces = append(inv.Workspaces, w)
	}
	return inv, nil
}

func (f *fakeDuet) MountRepos(_ context.Context, _, ws string, repos []string) (provider.MountResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mounts = append(f.mounts, slices.Clone(repos))
	before := slices.Clone(f.channels[ws])
	if f.refuse != "" {
		return provider.MountResult{Workspace: ws, Before: before, After: before, Refused: f.refuse}, nil
	}
	for _, r := range repos {
		if !slices.Contains(f.authorized, r) {
			return provider.MountResult{Workspace: ws, Before: before, After: before, Refused: "github_repo_access_denied"}, nil
		}
	}
	f.channels[ws] = append(f.channels[ws], repos...)
	return provider.MountResult{Workspace: ws, Before: before, After: slices.Clone(f.channels[ws])}, nil
}

func (f *fakeDuet) AuthorizeRepos(_ context.Context, _, owner string, repos []string) (provider.AuthorizeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.authorizes = append(f.authorizes, slices.Clone(repos))
	res := provider.AuthorizeResult{Owner: owner, Repos: repos, URL: "https://github.com/apps/claude/installations/10000002"}
	if f.stop != "" {
		res.State = f.stop
		return res, nil
	}
	f.authorized = append(f.authorized, repos...)
	res.State, res.Added = provider.AuthorizeDone, repos
	return res, nil
}

var _ provider.RepoMounter = (*fakeDuet)(nil)

const (
	linkedin = "https://github.com/nwverse/openlinkedin"
	bilibili = "https://github.com/nwverse/openbilibili"
	chanLI   = "chan_01CFwJZJtCoUJweZ64fQSYzu"
)

func startDuet(t *testing.T) (*exttest.Host, *core.Club, *fakeDuet, string) {
	t.Helper()
	repoRetry.Times, repoRetry.Wait = 2, time.Millisecond
	repoMemos = &repoMemo{inv: map[string]memoInv{}, outcomes: map[string]repoOutcome{}}
	fd := newFakeDuet()
	club := core.New(core.Options{Registry: provider.NewRegistry(fd), FollowUps: []time.Duration{}, WatchLimit: -1})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{}`))
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	a, err := space.AddAccount(context.Background(), protocol.AddAccountParams{Provider: "claude_duet", Credential: "fixture-credential-duet"})
	if err != nil {
		t.Fatal(err)
	}
	return h, club, fd, a.AccountID
}

func TestBindReposFlow(t *testing.T) {
	ctx := context.Background()

	// All granted: only the one not mounted is mounted, the channel keeps what it had.
	_, club, fd, id := startDuet(t)
	space, _ := club.Space(ctx, core.DefaultSpace)
	out, err := bindRepos(ctx, space, id, "main", []string{"https://github.com/nwverse/persona", "https://github.com/nwverse/ensemble"}, false)
	if err != nil || out.State != repoMounted || !slices.Equal(out.Added, []string{"https://github.com/nwverse/ensemble"}) ||
		!slices.Equal(fd.channels["chan_main1"], []string{"https://github.com/nwverse/persona", "https://github.com/nwverse/ensemble"}) {
		t.Fatalf("granted: %+v %v %v", out, err, fd.channels)
	}
	if out, _ := bindRepos(ctx, space, id, "main", []string{"nwverse/ensemble"}, false); out.State != repoAlready || len(fd.mounts) != 1 {
		t.Errorf("again: %+v, %d mounts", out, len(fd.mounts))
	}

	// Not granted, bind only: GitHub is not touched; the granted one is mounted and the rest guided.
	out, err = bindRepos(ctx, space, id, "openlinkedin", []string{linkedin, "nwverse/ensemble"}, false)
	if err != nil || out.State != repoNeedsGitHub || len(fd.authorizes) != 0 || !slices.Equal(out.Added, []string{"https://github.com/nwverse/ensemble"}) {
		t.Fatalf("bind only: %+v %v, %d authorizes", out, err, len(fd.authorizes))
	}
	if g := out.Guide; g == nil || g.URL != "https://github.com/apps/claude/installations/new" || !slices.Equal(g.Repos, []string{linkedin}) ||
		g.Stop != "" || !strings.Contains(strings.Join(g.Steps, " "), "已选的仓库一个都不要动") || !strings.Contains(g.Resume, "重新检测并挂载") {
		t.Errorf("guide: %+v", out.Guide)
	}

	// Authorize, and GitHub asks to confirm access: nothing pending is mounted, the guide goes to the installation.
	fd.stop = provider.AuthorizeSudo
	out, err = bindRepos(ctx, space, id, chanLI, []string{linkedin, bilibili}, true)
	if err != nil || out.State != repoNeedsGitHub || len(fd.authorizes) != 1 || !slices.Equal(fd.authorizes[0], []string{linkedin, bilibili}) {
		t.Fatalf("sudo: %+v %v %v", out, err, fd.authorizes)
	}
	if g := out.Guide; g.Stop != provider.AuthorizeSudo || g.URL != "https://github.com/apps/claude/installations/10000002" ||
		!strings.Contains(g.Why, "不会替你确认") || !strings.Contains(g.Why, "不会问你密码") || len(g.Repos) != 2 {
		t.Errorf("sudo guide: %+v", g)
	}
	if slices.Contains(fd.channels[chanLI], linkedin) {
		t.Error("an ungranted repository was mounted")
	}
	for _, st := range []provider.AuthorizeState{provider.AuthorizeSignIn, provider.AuthorizeApproval, provider.AuthorizeHumanCheck,
		provider.AuthorizeNotInstalled, provider.AuthorizeUnrecognized} {
		fd.stop = st
		if out, _ := bindRepos(ctx, space, id, chanLI, []string{linkedin}, true); out.State != repoNeedsGitHub || out.Guide.Stop != st || out.Guide.Why == "" ||
			!httpsURL(out.Guide.URL) {
			t.Errorf("%s: %+v", st, out.Guide)
		}
	}

	// Authorize, and GitHub grants: read again, then mounted.
	fd.stop = ""
	out, err = bindRepos(ctx, space, id, chanLI, []string{linkedin, bilibili}, true)
	if err != nil || out.State != repoMounted || out.Guide != nil || !slices.Equal(fd.channels[chanLI], []string{"https://github.com/nwverse/ensemble", linkedin, bilibili}) {
		t.Fatalf("granted on GitHub: %+v %v %v", out, err, fd.channels[chanLI])
	}

	// The agent refuses the channel change.
	fd.refuse = "github_repo_access_denied"
	if out, _ := bindRepos(ctx, space, id, "openbilibili", []string{"nwverse/persona"}, false); out.State != repoMountRefused || out.Refused == "" {
		t.Errorf("refused: %+v", out)
	}
	if _, err := bindRepos(ctx, space, id, "nope", []string{"nwverse/persona"}, false); err == nil || !strings.Contains(err.Error(), "openlinkedin") {
		t.Errorf("unknown channel: %v", err)
	}
	// Two channels of one name: a write by name is refused, by id it goes.
	fd.refuse = ""
	fd.mu.Lock()
	fd.names["chan_01DCDvtmZrq32yBT59hH7XwF"] = "openlinkedin"
	fd.mu.Unlock()
	if _, err := bindRepos(ctx, space, id, "openlinkedin", []string{"nwverse/persona"}, false); err == nil || !strings.Contains(err.Error(), "chan_") {
		t.Errorf("ambiguous name: %v", err)
	}
	if out, err := bindRepos(ctx, space, id, "chan_01DCDvtmZrq32yBT59hH7XwF", []string{"nwverse/persona"}, false); err != nil || out.State != repoMounted {
		t.Errorf("by id: %+v %v", out, err)
	}

	// Several owners: the guide's page is one owner's, and names that owner's repositories; the others are said apart.
	fd.stop = provider.AuthorizeSudo
	out, _ = bindRepos(ctx, space, id, "main", []string{"octo-user/one", "nwverse/two"}, true)
	if g := out.Guide; g == nil || !slices.Equal(g.Repos, []string{"https://github.com/octo-user/one"}) ||
		!strings.Contains(strings.Join(g.Steps, " "), "https://github.com/nwverse/two") {
		t.Errorf("owners: %+v", out.Guide)
	}
	fd.stop = provider.AuthorizeUnconfirmed
	if out, _ := bindRepos(ctx, space, id, "main", []string{"octo-user/one"}, true); !strings.Contains(out.Guide.Why, "可能已经") || strings.Contains(out.Guide.Why, "没有动它") {
		t.Errorf("unconfirmed is not \"nothing changed\": %q", out.Guide.Why)
	}
}

func TestRepoToolsAreGated(t *testing.T) {
	h, club, fd, id := startDuet(t)
	if !strings.Contains(string(pac.Manifest), `"club_agent_*"`) {
		t.Fatal("the manifest no longer confirms club_agent_* on the phone")
	}
	var reads, writes wire.ConnectorTools
	_ = h.Call("connector.tools.list", wire.ConnectorParams{Contribution: Connector}, &reads)
	_ = h.Call("connector.actions.list", wire.ConnectorParams{Contribution: Connector}, &writes)
	has := func(ts wire.ConnectorTools, n string) bool {
		return slices.ContainsFunc(ts.Tools, func(x wire.ConnectorTool) bool { return x.Name == n })
	}
	if !has(reads, toolReposList) || has(reads, toolRepoBind) || !has(writes, toolRepoBind) || !has(writes, toolRepoAuthorize) || has(writes, toolReposList) {
		t.Fatalf("reads %v writes %v", reads, writes)
	}
	for _, w := range writes.Tools {
		if !strings.HasPrefix(w.Name, "club_agent_") {
			t.Errorf("write %s is not under confirm_always", w.Name)
		}
	}
	call := func(name string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorCallResult
		err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: Connector, Name: name, Args: raw}, &res)
		return res.Text, err
	}
	describe := func(name string, args any) (wire.ConnectorDescribeResult, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorDescribeResult
		err := h.Call("connector.actions.describe", wire.ConnectorCallParams{Contribution: Connector, Name: name, Args: raw}, &res)
		return res, err
	}
	do := func(name string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorCallResult
		err := h.Call("connector.actions.do", wire.ConnectorDoParams{Contribution: Connector, Name: name, Args: raw, RequestID: "r1"}, &res)
		return res.Text, err
	}
	args := map[string]any{"account_id": id, "channel": "openlinkedin", "repos": []string{"nwverse/openlinkedin", "nwverse/ensemble"}}
	// Writes are not tools: tools.call never runs them, at any level.
	setControl(t, club, id, core.ControlConfirm)
	for _, n := range []string{toolRepoBind, toolRepoAuthorize} {
		if _, err := call(n, args); err == nil {
			t.Errorf("tools.call ran %s", n)
		}
	}
	setControl(t, club, id, core.ControlOff)

	// off: the account does not exist for the assistant.
	if out, err := call(toolReposList, map[string]any{}); err != nil || strings.Contains(out, id) {
		t.Errorf("list at off: %q %v", out, err)
	}
	if _, err := describe(toolRepoBind, args); err == nil {
		t.Error("describe at off")
	}
	// read: list, never write (neither describe nor do).
	setControl(t, club, id, core.ControlRead)
	out, err := call(toolReposList, map[string]any{"channel": "openlinkedin", "repos": []string{"nwverse/openlinkedin"}})
	if err != nil || !strings.Contains(out, "chan_01CFwJZJtCoUJweZ64fQSYzu") || !strings.Contains(out, `"authorized":false`) ||
		!strings.Contains(out, "https://github.com/nwverse/ensemble") {
		t.Errorf("list at read: %q %v", out, err)
	}
	// Without channel, a repository mounted only in a side channel is mounted, and mounted_in names that channel.
	fd.mu.Lock()
	fd.channels["chan_01DCDvtmZrq32yBT59hH7XwF"] = []string{"https://github.com/nwverse/ensemble"}
	fd.mu.Unlock()
	out, err = call(toolReposList, map[string]any{"account_id": id, "repos": []string{"nwverse/ensemble", "nwverse/openlinkedin"}})
	var listed struct {
		Accounts []struct {
			Check   []repoStatus `json:"check"`
			CheckIn string       `json:"check_channel"`
		} `json:"accounts"`
	}
	if err == nil {
		err = json.Unmarshal([]byte(out[:strings.Index(out, "\n")]), &listed)
	}
	if err != nil || len(listed.Accounts) != 1 || len(listed.Accounts[0].Check) != 2 || listed.Accounts[0].CheckIn != "" {
		t.Fatalf("list without channel: %q %v", out, err)
	}
	if c := listed.Accounts[0].Check; !c[0].Mounted || !slices.Equal(c[0].MountedIn, []string{"openbilibili（chan_01DCDvtmZrq32yBT59hH7XwF）"}) ||
		c[1].Mounted || len(c[1].MountedIn) != 0 {
		t.Errorf("mounted anywhere: %+v", c)
	}
	// With a channel, only that channel counts.
	if out, _ := call(toolReposList, map[string]any{"account_id": id, "channel": "openlinkedin", "repos": []string{"nwverse/ensemble"}}); !strings.Contains(out, `"mounted":false`) || strings.Contains(out, "mounted_in") {
		t.Errorf("list in a channel: %s", out)
	}
	fd.mu.Lock()
	fd.channels["chan_01DCDvtmZrq32yBT59hH7XwF"] = []string{}
	fd.mu.Unlock()
	for _, n := range []string{toolRepoBind, toolRepoAuthorize} {
		if _, err := describe(n, args); err == nil {
			t.Errorf("%s described at read", n)
		}
		if _, err := do(n, args); err == nil {
			t.Errorf("%s done at read", n)
		}
	}
	if len(fd.mounts)+len(fd.authorizes) != 0 {
		t.Fatal("wrote at read")
	}
	// confirm: the card names the account, the channel, each repository and the bounds; do writes.
	setControl(t, club, id, core.ControlConfirm)
	d, err := describe(toolRepoAuthorize, args)
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"openlinkedin", chanLI, "nwverse/openlinkedin：未授权：在 GitHub 上增选", "nwverse/ensemble：已授权，挂到这个频道",
		"已挂的仓库都保留", "已选的仓库不动", "不会改成「All repositories」", "不会替你输入或索要密码"} {
		if !strings.Contains(d.Action.Name+d.Summary, must) {
			t.Errorf("authorize card lacks %q:\n%s\n%s", must, d.Action.Name, d.Summary)
		}
	}
	if d, _ := describe(toolRepoBind, args); !strings.Contains(d.Summary, "这次不改 GitHub") || strings.Contains(d.Summary, "增选") {
		t.Errorf("bind card: %s", d.Summary)
	}
	if _, err := describe(toolRepoBind, map[string]any{"account_id": id, "channel": "openlinkedin", "repos": []string{"not a repo!"}}); err == nil {
		t.Error("a bad repository name was described")
	}
	fd.stop = provider.AuthorizeSudo
	out, err = do(toolRepoBind, args)
	if err != nil || !strings.Contains(out, "needs_github") || !strings.Contains(out, "https://github.com/apps/claude/installations/new") || len(fd.authorizes) != 0 {
		t.Errorf("bind: %q %v, %d authorizes", out, err, len(fd.authorizes))
	}
	out, err = do(toolRepoAuthorize, args)
	if err != nil || !strings.Contains(out, "不要替用户输入密码") || !strings.Contains(out, "installations/10000002") || len(fd.authorizes) != 1 {
		t.Errorf("authorize at sudo: %q %v", out, err)
	}
	fd.stop = ""
	if out, err = do(toolRepoAuthorize, args); err != nil || !strings.Contains(out, "已挂好并读回确认") {
		t.Errorf("authorize granted: %q %v", out, err)
	}
}

func TestReposPage(t *testing.T) {
	h, _, fd, id := startDuet(t)
	acct := render(t, h, pageState{Page: pageAccount, Account: id})
	if i := sectionIndex(acct, "仓库"); i < 0 || acct.Sections[i].Items[0].Action.Push == nil {
		t.Fatalf("account page has no 仓库: %s", text(acct))
	}
	st := pageState{Page: pageRepos, Account: id}
	v := render(t, h, st)
	for _, must := range []string{"挂载到频道", "#openlinkedin", "主聊天", "Claude GitHub App 已授权的仓库（2 个）", "nwverse/persona", "不会改成 All repositories"} {
		if !strings.Contains(text(v)+string(mustJSON(v)), must) {
			t.Errorf("repos page lacks %q", must)
		}
	}
	// Every write on the page asks first.
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Action != nil && (c.Action.ID == actRepoBind || c.Action.ID == actRepoAuthorize || c.Action.ID == actRepoResume) && c.Action.Confirm == "" {
				t.Errorf("%s has no confirm", c.Action.ID)
			}
		}
	}
	values := map[string]string{fieldRepoChannel: chanLI, fieldRepos: "nwverse/openlinkedin, nwverse/openbilibili"}
	res := act(t, h, st, actRepoCheck, values)
	if len(fd.mounts)+len(fd.authorizes) != 0 || !strings.Contains(string(mustJSON(res.View)), "检查结果（没有改动）") {
		t.Errorf("check wrote or said nothing: %s", mustJSON(res.View))
	}
	fd.stop = provider.AuthorizeSudo
	res = act(t, h, st, actRepoAuthorize, values)
	page := string(mustJSON(res.View))
	for _, must := range []string{"还要在 GitHub 上勾选", "打开 GitHub 授权页", "https://github.com/apps/claude/installations/10000002", "重新检测并挂载",
		"nwverse/openlinkedin、nwverse/openbilibili"} {
		if !strings.Contains(page, must) {
			t.Errorf("after sudo the page lacks %q", must)
		}
	}
	// The owner ticks them on GitHub; 重新检测并挂载 mounts without touching GitHub again.
	fd.mu.Lock()
	fd.authorized = append(fd.authorized, linkedin, bilibili)
	fd.mu.Unlock()
	var next pageState
	_ = json.Unmarshal([]byte(res.View.State), &next)
	if next.Chan != chanLI || next.Repos == "" {
		t.Fatalf("state for resume: %+v", next)
	}
	// 重新检测并挂载 carries its channel and repositories: it works from a page opened afresh (no state), as after an
	// outcome the assistant made.
	var resume *wire.Action
	for _, s := range res.View.Sections {
		for _, c := range s.Items {
			if c.Action != nil && c.Action.ID == actRepoResume {
				resume = c.Action
			}
		}
	}
	if resume == nil || resume.Confirm == "" || resume.Values[fieldRepoChannel] != chanLI || !strings.Contains(resume.Values[fieldRepos], "nwverse/openbilibili") {
		t.Fatalf("resume action: %+v", resume)
	}
	res = act(t, h, pageState{Page: pageRepos, Account: id}, actRepoResume, resume.Values)
	if len(fd.authorizes) != 1 || !slices.Equal(fd.channels[chanLI], []string{linkedin, bilibili}) || !strings.Contains(res.Toast, "已挂好并读回确认") {
		t.Errorf("resume: %q %v %v", res.Toast, fd.authorizes, fd.channels[chanLI])
	}
	if res := act(t, h, st, actRepoBind, map[string]string{fieldRepoChannel: chanLI, fieldRepos: "bad name!"}); !strings.Contains(res.Toast, "不是 GitHub 仓库") {
		t.Errorf("bad name: %q", res.Toast)
	}
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
