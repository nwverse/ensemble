package core

import (
	"context"

	"github.com/nwverse/personal-agent-club/internal/provider"
)

// AccountMachine is the computer a browser-driven account runs on, and the browser backend there, so a page can
// tell the user which computer to go to. The credential of such an account names only the computer (never a
// cookie or a key); "" for any other account.
func (s *Space) AccountMachine(ctx context.Context, id string) (machineID, backend string) {
	a, ok, err := s.store.Account(ctx, id)
	if err != nil || !ok {
		return "", ""
	}
	adapter, ok := s.club.opts.Registry.Get(a.Provider)
	if !ok {
		return "", ""
	}
	m, ok := adapter.(provider.MachineBound)
	if !ok {
		return "", ""
	}
	cred, found, err := s.vault.Get(ctx, id)
	if err != nil || !found {
		return "", ""
	}
	return m.Machine(cred)
}
