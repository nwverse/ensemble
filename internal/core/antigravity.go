package core

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/antigravity"
)

const antigravityLoginsWaiting = 2

// antigravityLogin is one sign-in waiting for the authorization code the user pastes.
type antigravityLogin struct {
	flow      provider.LoginFlow
	createdAt int64
	accountID string
}

func (s *Space) codeLogin() (provider.CodeLogin, error) {
	adapter, err := s.adapter(antigravity.Name)
	if err != nil {
		return nil, err
	}
	d, ok := adapter.(provider.CodeLogin)
	if !ok {
		return nil, protocol.Unsupported("antigravity has no sign-in in this build").Because(protocol.ReasonProviderUnavailable)
	}
	return d, nil
}

func (s *Space) antigravityLogins() map[string]*antigravityLogin {
	if s.agyLogins == nil {
		s.agyLogins = map[string]*antigravityLogin{}
	}
	return s.agyLogins
}

// StartAntigravityLogin is club.logins.antigravity.start: the approval URL only. The PKCE verifier stays in club
// until the user pastes the authorization code. A sign-in lasts 10 minutes; at most two wait at once. account_id
// signs that Antigravity account in again.
func (s *Space) StartAntigravityLogin(ctx context.Context, p protocol.AntigravityLoginStartParams) (protocol.AntigravityLoginStart, error) {
	d, err := s.codeLogin()
	if err != nil {
		return protocol.AntigravityLoginStart{}, err
	}
	if p.AccountID != "" {
		a, err := s.requireAccount(ctx, p.AccountID)
		if err != nil {
			return protocol.AntigravityLoginStart{}, err
		}
		if a.Provider != antigravity.Name {
			return protocol.AntigravityLoginStart{}, protocol.Conflict("the account is not an Antigravity account").Because(protocol.ReasonProviderMismatch)
		}
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	logins := s.antigravityLogins()
	for id, l := range logins {
		if now-l.createdAt > grokBotLoginTTLMs {
			delete(logins, id)
		}
	}
	if len(logins) >= antigravityLoginsWaiting {
		return protocol.AntigravityLoginStart{}, protocol.Conflict("two sign-ins are already waiting").Because(protocol.ReasonLoginLimit)
	}
	flow, err := d.StartLogin()
	if err != nil {
		return protocol.AntigravityLoginStart{}, err
	}
	id := newLoginID()
	logins[id] = &antigravityLogin{flow: flow, createdAt: now, accountID: p.AccountID}
	return protocol.AntigravityLoginStart{LoginID: id, LoginURL: flow.URL}, nil
}

// CompleteAntigravityLogin is club.logins.antigravity.complete: the pasted authorization code is exchanged and the
// account paired, or (for a sign-in again) its credential replaced. The tokens are never answered.
func (s *Space) CompleteAntigravityLogin(ctx context.Context, p protocol.AntigravityLoginCompleteParams) (protocol.GrokBotLoginPoll, error) {
	d, err := s.codeLogin()
	if err != nil {
		return protocol.GrokBotLoginPoll{}, err
	}
	code := strings.TrimSpace(p.Code)
	if code == "" {
		return protocol.GrokBotLoginPoll{}, protocol.Invalid("the authorization code is empty").Because(protocol.ReasonInvalidInput)
	}
	s.mu.Lock()
	l := s.antigravityLogins()[p.LoginID]
	if l == nil || s.now()-l.createdAt > grokBotLoginTTLMs {
		delete(s.antigravityLogins(), p.LoginID)
		s.mu.Unlock()
		return protocol.GrokBotLoginPoll{}, noLogin()
	}
	s.mu.Unlock()
	cred, err := d.CompleteLogin(ctx, l.flow, code)
	if err != nil {
		if !errorsAsRetryable(err) {
			s.dropAntigravityLogin(p.LoginID)
		}
		return protocol.GrokBotLoginPoll{}, protocol.Invalid("the sign-in failed: " + provider.CodeOf(err)).Because(provider.CodeOf(err))
	}
	s.dropAntigravityLogin(p.LoginID)
	var acct protocol.Account
	if l.accountID != "" {
		acct, err = s.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{AccountID: l.accountID, Credential: cred})
	} else {
		acct, err = s.AddAccount(ctx, protocol.AddAccountParams{Provider: antigravity.Name, Credential: cred, DisplayName: p.DisplayName})
	}
	if err != nil {
		return protocol.GrokBotLoginPoll{}, err
	}
	return protocol.GrokBotLoginPoll{Status: protocol.LoginDone, Account: &acct}, nil
}

func errorsAsRetryable(err error) bool {
	var pe *provider.Error
	return errors.As(err, &pe) && pe.Retryable
}

// CancelAntigravityLogin is club.logins.antigravity.cancel.
func (s *Space) CancelAntigravityLogin(_ context.Context, p protocol.LoginParams) error {
	s.dropAntigravityLogin(p.LoginID)
	return nil
}

func (s *Space) dropAntigravityLogin(id string) {
	s.mu.Lock()
	delete(s.antigravityLogins(), id)
	s.mu.Unlock()
}

// agyCredentialFile is the file agy 1.5.12 reads a sign-in from, under a home directory.
func agyCredentialFile(home string) string {
	return filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
}

// agyFile is the credential as agy stores it. The access token, the refresh token and the email are the whole of
// it; nothing else from the stored credential is written.
type agyFile struct {
	Token struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		RefreshToken string `json:"refresh_token"`
		Expiry       string `json:"expiry"`
		ExpiresIn    int64  `json:"expires_in"`
	} `json:"token"`
	AuthMethod   string `json:"auth_method"`
	IDTokenEmail string `json:"id_token_email,omitempty"`
}

// InstallAntigravity is club.antigravity.install: write one paired account's sign-in where an agy CLI on this
// machine reads it (home "" is the process's own home). A file that is already there is left untouched, so a CLI the
// user signed in by hand is never overwritten; the answer says where it wrote, never what.
func (s *Space) InstallAntigravity(ctx context.Context, p protocol.AntigravityInstallParams) (protocol.AntigravityInstall, error) {
	a, err := s.requireAccount(ctx, p.AccountID)
	if err != nil {
		return protocol.AntigravityInstall{}, err
	}
	if a.Provider != antigravity.Name {
		return protocol.AntigravityInstall{}, protocol.Conflict("the account is not an Antigravity account").Because(protocol.ReasonProviderMismatch)
	}
	cred, found, err := s.vault.Get(ctx, p.AccountID)
	if err != nil {
		return protocol.AntigravityInstall{}, err
	}
	if !found {
		return protocol.AntigravityInstall{}, protocol.NotFound("the account has no credential").Because(protocol.ReasonNotSignedIn)
	}
	home := strings.TrimSpace(p.Home)
	if home == "" {
		home, err = os.UserHomeDir()
		if err != nil || home == "" {
			return protocol.AntigravityInstall{}, protocol.Invalid("no home directory").Because(protocol.ReasonInvalidInput)
		}
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return protocol.AntigravityInstall{}, protocol.Invalid("the home directory is not a path").Because(protocol.ReasonInvalidInput)
	}
	target := agyCredentialFile(home)
	if _, err := os.Stat(target); err == nil {
		return protocol.AntigravityInstall{}, protocol.Conflict("an agy sign-in is already there and was left untouched").Because(protocol.ReasonAlreadyPaired)
	} else if !os.IsNotExist(err) {
		return protocol.AntigravityInstall{}, err
	}
	file, email, err := agyCredential(cred, time.UnixMilli(s.now()))
	if err != nil {
		return protocol.AntigravityInstall{}, protocol.Invalid("the stored sign-in cannot be written for agy").Because(protocol.ReasonInvalidInput)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return protocol.AntigravityInstall{}, err
	}
	if err := os.WriteFile(target, file, 0o600); err != nil {
		return protocol.AntigravityInstall{}, err
	}
	return protocol.AntigravityInstall{Path: target, Email: email}, nil
}

// agyCredential renders a stored credential as agy's file. The bytes carry the tokens; callers write them and do
// not log them.
func agyCredential(credential string, now time.Time) ([]byte, string, error) {
	var stored struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		Expiry       string `json:"expiry"`
		Email        string `json:"email"`
	}
	if json.Unmarshal([]byte(credential), &stored) != nil || stored.AccessToken == "" || stored.RefreshToken == "" {
		return nil, "", os.ErrInvalid
	}
	expiry, err := time.Parse(time.RFC3339, stored.Expiry)
	if err != nil {
		expiry = now.Add(time.Hour)
	}
	expiresIn := int64(expiry.Sub(now).Seconds())
	if expiresIn < 0 {
		expiresIn = 0
	}
	var out agyFile
	out.Token.AccessToken = stored.AccessToken
	out.Token.TokenType = "Bearer"
	out.Token.RefreshToken = stored.RefreshToken
	out.Token.Expiry = expiry.UTC().Format(time.RFC3339Nano)
	out.Token.ExpiresIn = expiresIn
	out.AuthMethod = "consumer"
	out.IDTokenEmail = stored.Email
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return nil, "", err
	}
	return append(b, '\n'), stored.Email, nil
}
