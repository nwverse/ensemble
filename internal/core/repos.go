package core

import (
	"context"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// An account's repositories, for an agent that mounts them (provider.RepoMounter: Claude Duet's channels). The
// credential stays here: callers name the account.

// MountsRepos says whether the account's agent has repositories to show and mount.
func (s *Space) MountsRepos(ctx context.Context, id string) bool {
	m, _, err := s.repoMounter(ctx, id)
	return err == nil && m != nil
}

func (s *Space) repoMounter(ctx context.Context, id string) (provider.RepoMounter, string, error) {
	a, err := s.requireAccount(ctx, id)
	if err != nil {
		return nil, "", err
	}
	adapter, err := s.adapter(a.Provider)
	if err != nil {
		return nil, "", err
	}
	m, ok := adapter.(provider.RepoMounter)
	if !ok || !m.MountsRepos() {
		return nil, "", protocol.Unsupported("这个账号的 Agent 没有可挂载的仓库")
	}
	cred, found, err := s.vault.Get(ctx, id)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "", protocol.Conflict("这个账号没有保存的登录").Because(provider.CodeAuthFailed)
	}
	return m, cred, nil
}

// repoError is a provider's refusal as a protocol error carrying its code (problems.go puts it in words).
func repoError(err error) error {
	if provider.IsRefusal(err) {
		return protocol.Failure(provider.CodeOf(err), "provider: "+provider.CodeOf(err))
	}
	return err
}

// AccountRepos is what the account's agent sees of repositories now.
func (s *Space) AccountRepos(ctx context.Context, id string) (provider.RepoInventory, error) {
	m, cred, err := s.repoMounter(ctx, id)
	if err != nil {
		return provider.RepoInventory{}, err
	}
	inv, err := m.Repos(ctx, cred)
	if err != nil {
		s.observe(id, err)
		return provider.RepoInventory{}, repoError(err)
	}
	return inv, nil
}

// MountRepos adds repos to a workspace of the account's agent (never taking one out).
func (s *Space) MountRepos(ctx context.Context, id, workspace string, repos []string) (provider.MountResult, error) {
	m, cred, err := s.repoMounter(ctx, id)
	if err != nil {
		return provider.MountResult{}, err
	}
	res, err := m.MountRepos(ctx, cred, workspace, repos)
	if err != nil {
		s.observe(id, err)
		return provider.MountResult{}, repoError(err)
	}
	s.log().Debug("repos mounted", "account", id, "workspace", workspace, "asked", len(repos), "before", len(res.Before),
		"after", len(res.After), "refused", res.Refused)
	return res, nil
}

// AuthorizeRepos adds repos of owner to the code host app's grant, in the account's computer's browser.
func (s *Space) AuthorizeRepos(ctx context.Context, id, owner string, repos []string) (provider.AuthorizeResult, error) {
	m, cred, err := s.repoMounter(ctx, id)
	if err != nil {
		return provider.AuthorizeResult{}, err
	}
	res, err := m.AuthorizeRepos(ctx, cred, owner, repos)
	if err != nil {
		return res, repoError(err)
	}
	s.log().Debug("repos authorized", "account", id, "owner", owner, "asked", len(repos), "state", string(res.State),
		"added", len(res.Added))
	return res, nil
}
