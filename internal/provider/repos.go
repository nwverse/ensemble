package provider

import (
	"context"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// Repositories a personal agent works on. Some agents (Claude Duet first) let the owner mount source
// repositories into a workspace (a Duet channel), and only see repositories a code host app was granted (the Claude
// GitHub App's installation, "Only select repositories"). RepoMounter is that capability: list what the agent can
// see and what each workspace has, add repositories to a workspace, and add repositories to the code host's grant.
//
// Every write only adds: MountRepos never takes a repository out of a workspace, AuthorizeRepos never unticks a
// repository nor switches a grant to all repositories. Each is verified by reading it back.

// Repo is a repository the agent can see (one the code host app was granted).
type Repo struct {
	URL     string `json:"url"` // canonical: https://github.com/owner/name
	Private bool   `json:"private,omitempty"`
	CanPush bool   `json:"can_push,omitempty"`
}

// Workspace is where an agent's work happens and repositories are mounted (a Duet channel or its main chat).
type Workspace struct {
	ID    string   `json:"id"`
	Name  string   `json:"name"`
	Main  bool     `json:"main,omitempty"`
	Repos []string `json:"repos"` // canonical URLs mounted now
	URL   string   `json:"url,omitempty"`
}

// RepoInventory is what an account's agent sees of repositories.
type RepoInventory struct {
	Authorized []Repo      `json:"authorized"`
	Complete   bool        `json:"complete"` // the agent said its list is whole (not cut short)
	Workspaces []Workspace `json:"workspaces"`
	// GrantURL is where the owner adds repositories to the code host app's grant (the GitHub App's install page).
	GrantURL string `json:"grant_url"`
}

// IsAuthorized says whether the agent can see repo (a canonical URL).
func (inv RepoInventory) IsAuthorized(repo string) bool {
	return slices.ContainsFunc(inv.Authorized, func(r Repo) bool { return SameRepo(r.URL, repo) })
}

// Workspace is the workspace with this id, or named so (case-insensitive), or the main one for "main".
func (inv RepoInventory) Workspace(ref string) (Workspace, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Workspace{}, false
	}
	for _, w := range inv.Workspaces {
		if w.ID == ref {
			return w, true
		}
	}
	for _, w := range inv.Workspaces {
		if strings.EqualFold(w.Name, ref) || strings.EqualFold(strings.TrimPrefix(ref, "#"), w.Name) {
			return w, true
		}
	}
	if strings.EqualFold(ref, "main") || ref == "主聊天" {
		for _, w := range inv.Workspaces {
			if w.Main {
				return w, true
			}
		}
	}
	return Workspace{}, false
}

// MountResult is a workspace's repositories before and after a mount, as read back from the agent.
type MountResult struct {
	Workspace string   `json:"workspace"`
	Before    []string `json:"before"`
	After     []string `json:"after"`
	// Refused is the agent's reason when it refused the change (github_repo_access_denied, …): nothing changed.
	Refused string `json:"refused,omitempty"`
}

// Verified says whether After holds everything Before had and every repository in want.
func (m MountResult) Verified(want []string) bool {
	for _, r := range append(slices.Clone(m.Before), want...) {
		if !slices.ContainsFunc(m.After, func(a string) bool { return SameRepo(a, r) }) {
			return false
		}
	}
	return true
}

// AuthorizeState is how an AuthorizeRepos ended.
type AuthorizeState string

const (
	// AuthorizeDone: the repositories were added to the grant and read back there.
	AuthorizeDone AuthorizeState = "done"
	// AuthorizeAlready: nothing to add (already selected, or the grant covers all repositories).
	AuthorizeAlready AuthorizeState = "already"
	// The rest are stops: nothing was changed, the owner has to act in the browser (URL says where).
	AuthorizeSignIn       AuthorizeState = "sign_in"       // not signed in to the code host
	AuthorizeSudo         AuthorizeState = "sudo"          // the code host asks to confirm access (password, 2FA, passkey)
	AuthorizeApproval     AuthorizeState = "approval"      // an organization owner has to approve the request
	AuthorizeHumanCheck   AuthorizeState = "human_check"   // a captcha or other human check
	AuthorizeNotInstalled AuthorizeState = "not_installed" // the app is not installed on that owner at all
	AuthorizeNotFound     AuthorizeState = "not_found"     // a repository does not exist or the owner cannot see it
	AuthorizeUnrecognized AuthorizeState = "unrecognized"  // the page is not what was expected: nothing was touched
	AuthorizeUnconfirmed  AuthorizeState = "unconfirmed"   // saved, but reading it back did not show every repository
)

// Stopped says the code host needs the owner, nothing was changed.
func (s AuthorizeState) Stopped() bool {
	switch s {
	case AuthorizeDone, AuthorizeAlready, AuthorizeUnconfirmed:
		return false
	}
	return true
}

// AuthorizeResult is the end of an AuthorizeRepos for one owner.
type AuthorizeResult struct {
	State AuthorizeState `json:"state"`
	Owner string         `json:"owner"`
	// URL is the page the owner goes to: the installation's own settings page when it was found, else the app's
	// install page.
	URL      string   `json:"url"`
	Repos    []string `json:"repos"`               // the repositories asked for (canonical URLs)
	Added    []string `json:"added,omitempty"`     // added by this call
	Missing  []string `json:"missing,omitempty"`   // not found at the code host
	AllRepos bool     `json:"all_repos,omitempty"` // the grant already covers all the owner's repositories
}

// RepoMounter is a provider that mounts repositories into workspaces.
type RepoMounter interface {
	// MountsRepos says whether this provider's agent has repositories at all (the Adapter of a browser site may not).
	MountsRepos() bool
	Repos(ctx context.Context, credential string) (RepoInventory, error)
	// MountRepos adds repos to the workspace's own, keeping every one it had.
	MountRepos(ctx context.Context, credential, workspace string, repos []string) (MountResult, error)
	// AuthorizeRepos adds repos (all of one owner) to the code host app's grant on that owner, only those: every
	// repository already selected stays, and a grant of selected repositories is never switched to all.
	AuthorizeRepos(ctx context.Context, credential, owner string, repos []string) (AuthorizeResult, error)
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]{0,99})$`)

// CanonicalRepo is a GitHub repository's canonical URL (https://github.com/owner/name) from what a person or an
// agent wrote: owner/name, a github.com web or clone address, git@github.com:owner/name.git. ok is false for
// anything else.
func CanonicalRepo(s string) (string, bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	var path string
	switch {
	case strings.HasPrefix(s, "git@github.com:"):
		path = strings.TrimPrefix(s, "git@github.com:")
	case strings.Contains(s, "://"):
		u, err := url.Parse(s)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "ssh") {
			return "", false
		}
		host := strings.ToLower(u.Hostname())
		if host != "github.com" && host != "www.github.com" {
			return "", false
		}
		path = strings.Trim(u.Path, "/")
	case strings.HasPrefix(strings.ToLower(s), "github.com/"):
		path = s[len("github.com/"):]
	default:
		path = s
	}
	path = strings.TrimSuffix(path, ".git")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || strings.Contains(name, "/") || !repoPart.MatchString(owner) || !repoPart.MatchString(name) ||
		name == "." || name == ".." {
		return "", false
	}
	return "https://github.com/" + owner + "/" + name, true
}

// SameRepo says whether a and b name the same repository (GitHub's names are case-insensitive).
func SameRepo(a, b string) bool {
	ca, oka := CanonicalRepo(a)
	cb, okb := CanonicalRepo(b)
	return oka && okb && strings.EqualFold(ca, cb)
}

// RepoOwnerName splits a canonical URL into owner and name.
func RepoOwnerName(canonical string) (owner, name string) {
	owner, name, _ = strings.Cut(strings.TrimPrefix(canonical, "https://github.com/"), "/")
	return owner, name
}
