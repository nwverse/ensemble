package core

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

const settingTerminalSSHTargets = "terminalSSHTargets"

const (
	TransportSSH       = "ssh"
	TransportTailscale = "tailscale"
)

// SSHTarget is a per-account non-secret SSH destination for remote status checks and owner-confirmed commands.
// Passwords and private keys are never accepted or stored.
type SSHTarget struct {
	Target    string `json:"target"`
	Transport string `json:"transport,omitempty"` // "ssh" (default) or "tailscale"
}

// Strict allowlist for SSH targets: host alias, user@host, user@100.x.y.z, optional :port.
// Never allows a leading '-', whitespace, quotes, or shell metacharacters.
var sshTargetPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}(@[A-Za-z0-9][A-Za-z0-9._-]{0,252})?(:[0-9]{1,5})?$`)

// ValidateSSHTarget validates a non-empty SSH destination and rejects any option/shell injection or secret-like text.
func ValidateSSHTarget(raw string) error {
	_, _, err := SplitSSHTarget(raw)
	return err
}

// SplitSSHTarget validates raw and splits an optional :port suffix so callers can pass `-p <port>` safely.
func SplitSSHTarget(raw string) (dest string, port string, err error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", errors.New("empty ssh target")
	}
	if len(s) > 256 || !sshTargetPattern.MatchString(s) {
		return "", "", errors.New("invalid ssh target: use host alias or user@host[:port] without spaces or options")
	}
	if idx := strings.LastIndex(s, ":"); idx >= 0 {
		pStr := s[idx+1:]
		n, perr := strconv.Atoi(pStr)
		if perr != nil || n < 1 || n > 65535 {
			return "", "", errors.New("invalid ssh port: must be between 1 and 65535")
		}
		dest = s[:idx]
		port = pStr
	} else {
		dest = s
	}
	if strings.HasPrefix(dest, "-") || strings.Contains(dest, "..") {
		return "", "", errors.New("invalid ssh target")
	}
	if user, host, ok := strings.Cut(dest, "@"); ok {
		if user == "" || host == "" || strings.HasPrefix(host, "-") {
			return "", "", errors.New("invalid ssh user@host")
		}
	}
	return dest, port, nil
}

// NormalizeTransport returns TransportTailscale for "tailscale" and TransportSSH otherwise.
func NormalizeTransport(t string) string {
	if strings.EqualFold(strings.TrimSpace(t), TransportTailscale) {
		return TransportTailscale
	}
	return TransportSSH
}

func (s *Space) readSSHTargets(ctx context.Context) (map[string]SSHTarget, error) {
	raw, ok, err := s.store.Setting(ctx, settingTerminalSSHTargets)
	if err != nil {
		return nil, err
	}
	if !ok || raw == "" {
		return map[string]SSHTarget{}, nil
	}
	var m map[string]SSHTarget
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	if m == nil {
		return map[string]SSHTarget{}, nil
	}
	return m, nil
}

func (s *Space) writeSSHTargets(ctx context.Context, m map[string]SSHTarget) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.store.PutSetting(ctx, settingTerminalSSHTargets, string(b))
}

// AccountSSHTarget returns the configured non-secret SSH target for an account, if any.
func (s *Space) AccountSSHTarget(ctx context.Context, accountID string) (SSHTarget, bool, error) {
	if _, err := s.requireAccount(ctx, accountID); err != nil {
		return SSHTarget{}, false, err
	}
	s.sshMu.Lock()
	m, err := s.readSSHTargets(ctx)
	s.sshMu.Unlock()
	if err != nil {
		return SSHTarget{}, false, err
	}
	t, ok := m[accountID]
	if !ok || strings.TrimSpace(t.Target) == "" {
		return SSHTarget{}, false, nil
	}
	t.Transport = NormalizeTransport(t.Transport)
	return t, true, nil
}

// SetAccountSSHTarget saves or clears (when Target is empty) the non-secret SSH target for an account.
func (s *Space) SetAccountSSHTarget(ctx context.Context, accountID string, cfg SSHTarget) error {
	if _, err := s.requireAccount(ctx, accountID); err != nil {
		return err
	}
	target := strings.TrimSpace(cfg.Target)
	if target == "" {
		return s.clearSSHTarget(ctx, accountID)
	}
	_, port, err := SplitSSHTarget(target)
	if err != nil {
		return badInput(err.Error())
	}
	transport := NormalizeTransport(cfg.Transport)
	if transport == TransportTailscale && port != "" {
		return badInput("tailscale ssh does not support a custom :port")
	}
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	m, err := s.readSSHTargets(ctx)
	if err != nil {
		return err
	}
	m[accountID] = SSHTarget{
		Target:    target,
		Transport: transport,
	}
	return s.writeSSHTargets(ctx, m)
}

func (s *Space) clearSSHTarget(ctx context.Context, accountID string) error {
	s.sshMu.Lock()
	defer s.sshMu.Unlock()
	m, err := s.readSSHTargets(ctx)
	if err != nil {
		return err
	}
	if _, ok := m[accountID]; !ok {
		return nil
	}
	delete(m, accountID)
	return s.writeSSHTargets(ctx, m)
}

// AccountBoxState queries the account provider's read-only cloud sandbox state when supported.
// Returns empty state without making a live RPC when the account is paused.
func (s *Space) AccountBoxState(ctx context.Context, accountID string) (provider.BoxState, bool, error) {
	acct, err := s.requireAccount(ctx, accountID)
	if err != nil {
		return provider.BoxState{}, false, err
	}
	adapter, err := s.adapter(acct.Provider)
	if err != nil {
		return provider.BoxState{}, false, nil
	}
	bs, ok := adapter.(provider.BoxStater)
	if !ok {
		return provider.BoxState{}, false, nil
	}
	if acct.Status == protocol.AccountPaused {
		return provider.BoxState{}, true, nil
	}
	cred, found, err := s.vault.Get(ctx, acct.AccountID)
	if err != nil || !found {
		return provider.BoxState{}, true, err
	}
	st, err := callWithRefresh(ctx, s, acct.AccountID, adapter, cred, func(c string) (provider.BoxState, error) {
		return bs.BoxState(ctx, c)
	})
	if err != nil {
		return provider.BoxState{}, true, protocol.Failure(provider.CodeOf(err), "box state check failed: "+provider.CodeOf(err))
	}
	return st, true, nil
}
