package browser

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/nwverse/ensemble/internal/provider"
)

// Claude Duet's repositories (read from claude.ai with the page's own cookies, 2026-11):
//   - GET /api/organizations/<org>/code/repos: {repos: [{repo: {name, owner: {login}, private, permissions: {push}},
//     source_url}], is_complete}: every repository the Claude GitHub App was granted and the account can see.
//   - GET /v1/code/channels/<chan>/config: {config: {default_source_urls: [...]}}: the repositories mounted in a
//     channel (the main chat's too), several per channel.
//   - PATCH /v1/code/channels/<chan> {default_source_urls}: what the page sends to change them (updateChannel); a
//     repository the app was not granted is refused (github_repo_access_denied, github_app_not_connected).
// A repository the app was not granted is added on GitHub (github_app.go), not on claude.ai.

var _ provider.RepoMounter = (*Adapter)(nil)

// siteRepoMounter is a siteImpl that mounts repositories (Claude Duet).
type siteRepoMounter interface {
	repos(ctx context.Context, a *Adapter, credential string) (provider.RepoInventory, error)
	mountRepos(ctx context.Context, a *Adapter, credential, workspace string, repos []string) (provider.MountResult, error)
}

// MountsRepos says whether this site's agent has repositories (Claude Duet's does).
func (a *Adapter) MountsRepos() bool {
	_, ok := a.site.impl.(siteRepoMounter)
	return ok
}

func (a *Adapter) Repos(ctx context.Context, credential string) (provider.RepoInventory, error) {
	m, ok := a.site.impl.(siteRepoMounter)
	if !ok {
		return provider.RepoInventory{}, &provider.Error{Code: provider.CodeUnavailable}
	}
	return m.repos(ctx, a, credential)
}

func (a *Adapter) MountRepos(ctx context.Context, credential, workspace string, repos []string) (provider.MountResult, error) {
	m, ok := a.site.impl.(siteRepoMounter)
	if !ok {
		return provider.MountResult{}, &provider.Error{Code: provider.CodeUnavailable}
	}
	return m.mountRepos(ctx, a, credential, workspace, repos)
}

func (a *Adapter) AuthorizeRepos(ctx context.Context, credential, owner string, repos []string) (provider.AuthorizeResult, error) {
	if !a.MountsRepos() {
		return provider.AuthorizeResult{}, &provider.Error{Code: provider.CodeUnavailable}
	}
	return authorizeGitHubApp(ctx, a, credential, claudeGitHubApp, owner, repos)
}

// duetReposScript lists the authorized repositories and every channel's mounted ones.
const duetReposScript = `
await page.waitForFunction(() => !!document.querySelector('a[href^="/duet/chan_"]'), undefined, { timeout: 15000 }).catch(() => {});
await page.waitForFunction(() => performance.getEntriesByType("resource").some(e => /\/v1\/code\/aibo\/channels\/chan_/.test(e.name)), undefined, { timeout: 10000 }).catch(() => {});
return await page.evaluate(async () => {
%s
  const __org = async () => {
    const m = (document.cookie || "").match(/(?:^|;\s*)lastActiveOrg=([^;]+)/);
    if (m) return decodeURIComponent(m[1]);
    const r = await fetch("/api/organizations", { credentials: "include" });
    if (r.status === 401 || r.status === 403) throw new Error("auth");
    const list = r.ok ? await r.json() : [];
    const chat = (Array.isArray(list) ? list : []).find(o => o && o.uuid && Array.isArray(o.capabilities) && o.capabilities.includes("chat"));
    return chat ? chat.uuid : "";
  };
  try {
    const org = await __org();
    if (!org) throw new Error("auth");
    const rr = await fetch("/api/organizations/" + encodeURIComponent(org) + "/code/repos?skip_status=true", { credentials: "include" });
    if (rr.status === 401 || rr.status === 403) throw new Error("auth");
    if (!rr.ok) throw new Error("http " + rr.status);
    const rj = await rr.json();
    const authorized = (rj.repos || []).map(x => ({ url: String(x.source_url || ""), private: !!(x.repo && x.repo.private),
      can_push: !!(x.repo && x.repo.permissions && x.repo.permissions.push) })).filter(x => x.url);
    const main = await __duetMain();
    const seen = new Set();
    const chans = [];
    if (main) { seen.add(main); chans.push({ id: main, name: "", main: true }); }
    for (const el of document.querySelectorAll('a[href^="/duet/chan_"]')) {
      const m = (el.getAttribute('href') || '').match(/^\/duet\/(chan_[A-Za-z0-9]+)(?:$|[?#])/);
      if (!m || seen.has(m[1])) continue;
      seen.add(m[1]);
      chans.push({ id: m[1], name: (el.innerText || '').trim().split('\n')[0] || m[1], main: false });
      if (chans.length >= 60) break;
    }
    await Promise.all(chans.map(async c => {
      try {
        const cfg = await __api("/v1/code/channels/" + encodeURIComponent(c.id) + "/config");
        c.repos = ((cfg && cfg.config && cfg.config.default_source_urls) || []).map(String);
      } catch (e) {
        if (String(e.message) === "auth") throw e;
        c.repos = null;
      }
    }));
    return { authorized, complete: rj.is_complete === true, channels: chans };
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
});
`

type duetReposAnswer struct {
	Error      string           `json:"error"`
	Authorized []duetRepoAnswer `json:"authorized"`
	Complete   bool             `json:"complete"`
	Channels   []duetChanAnswer `json:"channels"`
}

type duetRepoAnswer struct {
	URL     string `json:"url"`
	Private bool   `json:"private"`
	CanPush bool   `json:"can_push"`
}

// duetChanAnswer is a channel and its repositories (Repos nil when its config did not answer).
type duetChanAnswer struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Main  bool      `json:"main"`
	Repos *[]string `json:"repos"`
}

func (duetSite) repos(ctx context.Context, a *Adapter, credential string) (provider.RepoInventory, error) {
	// The sidebar is drawn after the page loads: the page is loaded fresh, as listSessions does.
	script := siteScript(duetOrigin+"/duet", "", nil, `
if (/\/login\b/.test(await page.url())) return { error: "auth" };
`+fmt.Sprintf(duetReposScript, duetPrelude))
	var r duetReposAnswer
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RepoInventory{}, err
	}
	if err := dotsError(r.Error); err != nil {
		return provider.RepoInventory{}, err
	}
	return duetInventory(r), nil
}

// duetInventory is the script's answer as an inventory: canonical URLs, the main chat first, channels whose config
// did not answer left out (their repositories are not known, and are not shown as none).
func duetInventory(r duetReposAnswer) provider.RepoInventory {
	inv := provider.RepoInventory{Complete: r.Complete, GrantURL: claudeGitHubApp.installURL(),
		Authorized: []provider.Repo{}, Workspaces: []provider.Workspace{}}
	for _, x := range r.Authorized {
		if u, ok := provider.CanonicalRepo(x.URL); ok {
			inv.Authorized = append(inv.Authorized, provider.Repo{URL: u, Private: x.Private, CanPush: x.CanPush})
		}
	}
	for _, c := range r.Channels {
		if !strings.HasPrefix(c.ID, "chan_") || c.Repos == nil {
			continue
		}
		w := provider.Workspace{ID: c.ID, Name: c.Name, Main: c.Main, Repos: canonicalAll(*c.Repos),
			URL: duetOrigin + "/duet/" + c.ID}
		if c.Main {
			w.Name, w.URL = "主聊天", duetOrigin+"/duet"
		}
		inv.Workspaces = append(inv.Workspaces, w)
	}
	slices.SortStableFunc(inv.Workspaces, func(x, y provider.Workspace) int {
		switch {
		case x.Main == y.Main:
			return 0
		case x.Main:
			return -1
		}
		return 1
	})
	return inv
}

// canonicalAll is each of urls in canonical form, once (a URL that is not a GitHub repository is kept as it is).
func canonicalAll(urls []string) []string {
	out := []string{}
	for _, u := range urls {
		c, ok := provider.CanonicalRepo(u)
		if !ok {
			c = strings.TrimSpace(u)
		}
		if c != "" && !slices.ContainsFunc(out, func(o string) bool { return strings.EqualFold(o, c) }) {
			out = append(out, c)
		}
	}
	return out
}

// mergeRepos is before with each of add it does not have appended: the order of before is kept, nothing is taken out.
func mergeRepos(before, add []string) []string {
	out := slices.Clone(before)
	for _, r := range add {
		if !slices.ContainsFunc(out, func(o string) bool { return provider.SameRepo(o, r) || strings.EqualFold(o, r) }) {
			out = append(out, r)
		}
	}
	return out
}

// duetMountScript reads the channel's repositories, writes them back with the new ones appended (only when one is
// new), and reads them again. ARGS: {chan, add}. The merge is made in the page from what the channel has at that
// moment (not from the inventory the caller read earlier); a config it cannot read fails the run without writing.
const duetMountScript = `
    const path = "/v1/code/channels/" + encodeURIComponent(ARGS.chan);
    const read = async () => {
      const cfg = await __api(path + "/config");
      // Fail closed: a config whose repositories cannot be read is never written back (it would drop them).
      if (!cfg || !cfg.config || !Array.isArray(cfg.config.default_source_urls)) throw new Error("shape");
      return cfg.config.default_source_urls.map(String);
    };
    const before = await read();
    const key = u => String(u).toLowerCase().replace(/\.git$/, "").replace(/\/+$/, "");
    const have = new Set(before.map(key));
    const next = before.slice();
    for (const u of ARGS.add) if (!have.has(key(u))) { next.push(u); have.add(key(u)); }
    if (next.length === before.length) return { before, after: before };
    const r = await fetch(path, { method: "PATCH", credentials: "include",
      headers: { "content-type": "application/json", "anthropic-version": "2023-06-01" },
      body: JSON.stringify({ default_source_urls: next }) });
    if (r.status === 401) throw new Error("auth");
    if (!r.ok) {
      let reason = "";
      try { const j = await r.json(); reason = String((j && j.error && (j.error.reason || j.error.type)) || ""); } catch (e) {}
      return { before, after: before, refused: reason || ("http " + r.status) };
    }
    return { before, after: await read() };
`

func (duetSite) mountRepos(ctx context.Context, a *Adapter, credential, workspace string, repos []string) (provider.MountResult, error) {
	if !strings.HasPrefix(workspace, "chan_") {
		return provider.MountResult{}, &provider.Error{Code: provider.CodeNotFound}
	}
	add := canonicalAll(repos)
	for _, r := range add {
		if _, ok := provider.CanonicalRepo(r); !ok {
			return provider.MountResult{}, &provider.Error{Code: provider.CodeBadResponse}
		}
	}
	script := duetScript(duetOrigin+"/duet", map[string]any{"chan": workspace, "add": add}, duetMountScript)
	var r struct {
		Error   string   `json:"error"`
		Before  []string `json:"before"`
		After   []string `json:"after"`
		Refused string   `json:"refused"`
	}
	if err := a.runSend(ctx, credential, script, &r); err != nil {
		return provider.MountResult{}, err
	}
	if err := dotsError(r.Error); err != nil {
		return provider.MountResult{}, err
	}
	return provider.MountResult{Workspace: workspace, Before: canonicalAll(r.Before), After: canonicalAll(r.After),
		Refused: r.Refused}, nil
}
