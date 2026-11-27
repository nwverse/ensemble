package core

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/store"
)

const (
	grokBotLoginTTLMs        = 10 * 60_000
	grokBotLoginsWaiting     = 2
	liveStateTTLMs           = 3_000
	grokBot                  = "grok_bot"
	settingIdentity          = "browserIdentity"
	settingAssistantBackends = "assistantBackendAccounts"
	settingInboxAccountsOff  = "inboxAccountsOff"
	settingInboxSince        = "inboxSince"
	settingAssistantSessions = "assistantActiveSessions"
)

// background runs f after the call that started it, as the gateway's "void this.syncAccount(...)".
func (s *Space) background(f func(ctx context.Context)) { s.club.after(0, func() { f(s.club.ctx) }) }

// UpdateAccount is club.accounts.update: pause or resume (which clears lastError and, resuming, syncs), rename,
// and set the assistant control level and the quota display switch.
func (s *Space) UpdateAccount(ctx context.Context, p protocol.UpdateAccountParams) (protocol.Account, error) {
	a, err := s.requireAccount(ctx, p.AccountID)
	if err != nil {
		return protocol.Account{}, err
	}
	var patch store.AccountPatch
	if p.DisplayName != nil {
		name := strings.TrimSpace(*p.DisplayName)
		if name == "" || len(utf16.Encode([]rune(name))) > maxDisplayName {
			return protocol.Account{}, badInput("display_name must be 1 to 80 characters")
		}
		owner := names.SourceOwner
		patch.DisplayName, patch.NameSource = &name, &owner
	}
	if p.AssistantControl != nil {
		c, err := ParseControl(*p.AssistantControl)
		if err != nil || *p.AssistantControl == "" {
			return protocol.Account{}, badInput("assistant_control must be off, read, confirm or auto")
		}
		level := string(c)
		patch.AssistantControl = &level
	}
	patch.ShowQuota = p.ShowQuota
	if p.QuotaDisplay != nil {
		d := strings.TrimSpace(*p.QuotaDisplay)
		if len(d) > 64 {
			return protocol.Account{}, badInput("quota_display must be at most 64 characters")
		}
		patch.QuotaDisplay = &d
		if d == protocol.QuotaDisplayOff && p.ShowQuota == nil {
			off := false
			patch.ShowQuota = &off
		} else if d != "" && d != protocol.QuotaDisplayOff && p.ShowQuota == nil {
			on := true
			patch.ShowQuota = &on
		}
	}
	if p.Paused != nil {
		st, empty := protocol.AccountActive, ""
		if *p.Paused {
			st = protocol.AccountPaused
		}
		patch.Status, patch.LastError = &st, &empty
	}
	if err := s.store.UpdateAccount(ctx, a.AccountID, patch, s.now()); err != nil {
		return protocol.Account{}, err
	}
	if p.AssistantBackend != nil {
		if err := s.setAssistantBackend(ctx, a.AccountID, *p.AssistantBackend); err != nil {
			return protocol.Account{}, err
		}
	}
	if p.Inbox != nil {
		if err := s.SetInboxAccount(ctx, a.AccountID, *p.Inbox); err != nil {
			return protocol.Account{}, err
		}
	}
	if p.Paused != nil && !*p.Paused {
		s.background(func(ctx context.Context) { s.sync(ctx, a.AccountID, "") })
	}
	s.notify(nil)
	return s.Account(ctx, a.AccountID)
}

// ReplaceCredential is club.accounts.replace_credential: a new credential for the same remote account (a credential
// of another remote account is refused). The account becomes active again unless it is paused.
func (s *Space) ReplaceCredential(ctx context.Context, p protocol.ReplaceCredentialParams) (protocol.Account, error) {
	a, err := s.requireAccount(ctx, p.AccountID)
	if err != nil {
		return protocol.Account{}, err
	}
	cred := strings.TrimSpace(p.Credential)
	if err := checkCredential(cred); err != nil {
		return protocol.Account{}, err
	}
	adapter, err := s.adapter(a.Provider)
	if err != nil {
		return protocol.Account{}, err
	}
	remote, err := adapter.Verify(ctx, cred)
	s.observe(a.AccountID, err)
	if err != nil {
		return protocol.Account{}, verifyError(err)
	}
	s.noteQuota(a.AccountID, remote.Quota)
	if a.ExternalAccountID != "" && remote.ExternalAccountID != a.ExternalAccountID {
		return protocol.Account{}, protocol.Conflict("the credential belongs to another account").Because(protocol.ReasonAccountMismatch)
	}
	if err := s.vault.Put(ctx, a.AccountID, cred); err != nil {
		return protocol.Account{}, err
	}
	st, empty := protocol.AccountActive, ""
	if a.Status == protocol.AccountPaused {
		st = protocol.AccountPaused
	}
	if err := s.store.UpdateAccount(ctx, a.AccountID, store.AccountPatch{Status: &st, LastError: &empty}, s.now()); err != nil {
		return protocol.Account{}, err
	}
	if st != protocol.AccountPaused {
		s.background(func(ctx context.Context) { s.sync(ctx, a.AccountID, "") })
	}
	s.notify(nil)
	return s.Account(ctx, a.AccountID)
}

type liveEntry struct {
	at    int64
	done  chan struct{}
	state protocol.LiveState
	err   error
}

// Live is club.live: whether the agent is working on the session, asked at most every 3 s per session; callers
// joining a lookup get its answer or its error.
func (s *Space) Live(ctx context.Context, p protocol.SessionParams) (protocol.LiveState, error) {
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return protocol.LiveState{}, err
	}
	adapter, _ := s.club.opts.Registry.Get(sess.Provider)
	ts, hasTS := adapter.(provider.TurnStater)
	ls, hasLS := adapter.(provider.LiveStater)
	if !hasTS && !hasLS {
		return protocol.LiveState{}, nil
	}
	now := s.now()
	s.mu.Lock()
	for k, e := range s.live {
		if now-e.at > liveStateTTLMs {
			delete(s.live, k)
		}
	}
	if e := s.live[sess.SessionKey]; e != nil {
		s.mu.Unlock()
		<-e.done
		return e.state, e.err
	}
	e := &liveEntry{at: now, done: make(chan struct{})}
	s.live[sess.SessionKey] = e
	s.mu.Unlock()
	e.state, e.err = s.lookupLive(ctx, sess, ts, ls)
	if e.err != nil {
		s.mu.Lock()
		delete(s.live, sess.SessionKey)
		s.mu.Unlock()
	}
	close(e.done)
	return e.state, e.err
}

func (s *Space) lookupLive(ctx context.Context, sess store.Session, ts provider.TurnStater, ls provider.LiveStater) (protocol.LiveState, error) {
	a, err := s.requireAccount(ctx, sess.AccountID)
	if err != nil {
		return protocol.LiveState{}, err
	}
	cred, found, err := s.vault.Get(ctx, sess.AccountID)
	if err != nil {
		return protocol.LiveState{}, err
	}
	if a.Status == protocol.AccountPaused || !found {
		return protocol.LiveState{}, nil
	}
	ref := provider.SessionRef{ExternalID: sess.ExternalID, Ref: sess.Ref}
	if ts != nil {
		st, err := ts.TurnState(ctx, cred, ref)
		s.observe(sess.AccountID, err)
		if err != nil {
			return protocol.LiveState{}, protocol.Failure(provider.CodeOf(err), "the provider did not answer: "+provider.CodeOf(err))
		}
		return protocol.LiveState{
			Supported:      true,
			Running:        st.Running,
			Composing:      st.Composing,
			WaitingForUser: st.WaitingForUser,
			LastTurnError:  st.LastTurnError,
		}, nil
	}
	st, err := ls.LiveState(ctx, cred, ref)
	s.observe(sess.AccountID, err)
	if err != nil {
		return protocol.LiveState{}, protocol.Failure(provider.CodeOf(err), "the provider did not answer: "+provider.CodeOf(err))
	}
	return protocol.LiveState{Supported: true, Running: st.Running, Composing: st.Composing}, nil
}

type login struct {
	flow      provider.LoginFlow
	createdAt int64
	accountID string
}

func newLoginID() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return "g" + hex.EncodeToString(b[:])
}

func (s *Space) deviceLogin() (provider.DeviceLogin, error) {
	adapter, err := s.adapter(grokBot)
	if err != nil {
		return nil, err
	}
	d, ok := adapter.(provider.DeviceLogin)
	if !ok {
		return nil, protocol.Unsupported("grok_bot has no sign-in in this build").Because(protocol.ReasonProviderUnavailable)
	}
	return d, nil
}

// StartGrokBotLogin is club.logins.grok_bot.start: the approval URL only, the verifier and the token stay in club.
// A sign-in lasts 10 minutes; at most two wait at once. account_id signs that Grok Bot account in again.
func (s *Space) StartGrokBotLogin(ctx context.Context, p protocol.GrokBotLoginStartParams) (protocol.GrokBotLoginStart, error) {
	d, err := s.deviceLogin()
	if err != nil {
		return protocol.GrokBotLoginStart{}, err
	}
	if p.AccountID != "" {
		a, err := s.requireAccount(ctx, p.AccountID)
		if err != nil {
			return protocol.GrokBotLoginStart{}, err
		}
		if a.Provider != grokBot {
			return protocol.GrokBotLoginStart{}, protocol.Conflict("the account is not a Grok Bot account").Because(protocol.ReasonProviderMismatch)
		}
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, l := range s.logins {
		if now-l.createdAt > grokBotLoginTTLMs {
			delete(s.logins, id)
		}
	}
	if len(s.logins) >= grokBotLoginsWaiting {
		return protocol.GrokBotLoginStart{}, protocol.Conflict("two sign-ins are already waiting").Because(protocol.ReasonLoginLimit)
	}
	flow, err := d.StartLogin()
	if err != nil {
		return protocol.GrokBotLoginStart{}, err
	}
	id := newLoginID()
	s.logins[id] = &login{flow: flow, createdAt: now, accountID: p.AccountID}
	return protocol.GrokBotLoginStart{LoginID: id, LoginURL: flow.URL}, nil
}

func noLogin() *protocol.Error {
	return protocol.NotFound("no such sign-in").Because(protocol.ReasonLoginNotFound)
}

// PollGrokBotLogin is club.logins.grok_bot.poll: pending until the user approved, then the account, paired anew or
// (for a sign-in again) with its credential replaced. The token is never answered.
func (s *Space) PollGrokBotLogin(ctx context.Context, p protocol.LoginParams) (protocol.GrokBotLoginPoll, error) {
	d, err := s.deviceLogin()
	if err != nil {
		return protocol.GrokBotLoginPoll{}, err
	}
	s.mu.Lock()
	l := s.logins[p.LoginID]
	if l == nil || s.now()-l.createdAt > grokBotLoginTTLMs {
		delete(s.logins, p.LoginID)
		s.mu.Unlock()
		return protocol.GrokBotLoginPoll{}, noLogin()
	}
	s.mu.Unlock()
	cred, err := d.PollLogin(ctx, l.flow)
	if err != nil {
		var pe *provider.Error
		if !errors.As(err, &pe) || !pe.Retryable {
			s.dropLogin(p.LoginID)
		}
		return protocol.GrokBotLoginPoll{}, protocol.Invalid("the sign-in failed: " + provider.CodeOf(err)).Because(provider.CodeOf(err))
	}
	if cred == "" {
		return protocol.GrokBotLoginPoll{Status: protocol.LoginPending}, nil
	}
	s.dropLogin(p.LoginID)
	var acct protocol.Account
	if l.accountID != "" {
		acct, err = s.ReplaceCredential(ctx, protocol.ReplaceCredentialParams{AccountID: l.accountID, Credential: cred})
	} else {
		acct, err = s.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: cred, DisplayName: p.DisplayName})
	}
	if err != nil {
		return protocol.GrokBotLoginPoll{}, err
	}
	return protocol.GrokBotLoginPoll{Status: protocol.LoginDone, Account: &acct}, nil
}

func (s *Space) dropLogin(id string) {
	s.mu.Lock()
	delete(s.logins, id)
	s.mu.Unlock()
}

// CancelGrokBotLogin is club.logins.grok_bot.cancel.
func (s *Space) CancelGrokBotLogin(_ context.Context, p protocol.LoginParams) error {
	s.dropLogin(p.LoginID)
	return nil
}

var (
	localeID   = regexp.MustCompile(`^([A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*)?$`)
	timezoneID = regexp.MustCompile(`^([A-Za-z_]+(/[A-Za-z0-9_+-]+)*)?$`)
	browserArg = regexp.MustCompile(`^--(window-size=\d{2,5},\d{2,5}|lang=[A-Za-z]{2,3}(-[A-Za-z0-9]{2,8})*|accept-lang=[A-Za-z0-9-]+(,[A-Za-z0-9\-;=.]+)*|disable-blink-features=AutomationControlled|force-device-scale-factor=\d+(\.\d+)?|force-color-profile=[A-Za-z0-9_-]+|hide-scrollbars|mute-audio)$`)
)

func len16(s string) int { return len(utf16.Encode([]rune(s))) }

// cleanIdentity checks a browser identity as the gateway's schema does, trimming its fields.
func cleanIdentity(in protocol.BrowserIdentity) (protocol.BrowserIdentity, error) {
	out := protocol.BrowserIdentity{UserAgent: strings.TrimSpace(in.UserAgent), Locale: strings.TrimSpace(in.Locale),
		Timezone: strings.TrimSpace(in.Timezone), ExtraArgs: []string{}}
	switch {
	case len16(out.UserAgent) > 512:
		return out, badInput("userAgent is longer than 512 characters")
	case !localeID.MatchString(out.Locale):
		return out, badInput("locale is not a language tag")
	case len16(out.Timezone) > 64 || !timezoneID.MatchString(out.Timezone):
		return out, badInput("timezone is not a time zone name")
	case len(in.ExtraArgs) > 32:
		return out, badInput("at most 32 extraArgs")
	}
	for _, a := range in.ExtraArgs {
		a = strings.TrimSpace(a)
		if len16(a) > 512 || !browserArg.MatchString(a) {
			return out, badInput("extraArgs may only hold the presentation switches the Lark gateway allows")
		}
		out.ExtraArgs = append(out.ExtraArgs, a)
	}
	return out, nil
}

// Settings is club.settings.get. Club only keeps the browser identity in this version.
func (s *Space) Settings(ctx context.Context) (protocol.Settings, error) {
	raw, ok, err := s.store.Setting(ctx, settingIdentity)
	if err != nil {
		return protocol.Settings{}, err
	}
	var id protocol.BrowserIdentity
	if ok {
		_ = json.Unmarshal([]byte(raw), &id)
	}
	clean, err := cleanIdentity(id)
	if err != nil {
		clean = protocol.BrowserIdentity{ExtraArgs: []string{}}
	}
	return protocol.Settings{BrowserIdentity: clean}, nil
}

// SetSettings is club.settings.set.
func (s *Space) SetSettings(ctx context.Context, p protocol.SettingsParams) (protocol.Settings, error) {
	if p.BrowserIdentity == nil {
		return protocol.Settings{}, badInput("browser_identity is required")
	}
	if err := s.putIdentity(ctx, *p.BrowserIdentity); err != nil {
		return protocol.Settings{}, err
	}
	s.notify(nil)
	return s.Settings(ctx)
}

func (s *Space) putIdentity(ctx context.Context, in protocol.BrowserIdentity) error {
	clean, err := cleanIdentity(in)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(clean)
	return s.store.PutSetting(ctx, settingIdentity, string(b))
}

func (s *Space) assistantBackends(ctx context.Context) map[string]bool {
	raw, ok, err := s.store.Setting(ctx, settingAssistantBackends)
	if err != nil || !ok {
		return map[string]bool{}
	}
	var m map[string]bool
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		return map[string]bool{}
	}
	return m
}

func (s *Space) setAssistantBackend(ctx context.Context, accountID string, enabled bool) error {
	m := s.assistantBackends(ctx)
	if enabled {
		m[accountID] = true
	} else {
		delete(m, accountID)
	}
	b, _ := json.Marshal(m)
	return s.store.PutSetting(ctx, settingAssistantBackends, string(b))
}

// AssistantSessionKeys returns the set of club session keys currently backing a Persona main-assistant session so
// extension tools do not expose them for self-messaging.
func (s *Space) AssistantSessionKeys(ctx context.Context) (map[string]bool, error) {
	raw, ok, err := s.store.Setting(ctx, settingAssistantSessions)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	if !ok {
		return out, nil
	}
	var keys []string
	if json.Unmarshal([]byte(raw), &keys) == nil {
		for _, k := range keys {
			if k = strings.TrimSpace(k); k != "" {
				out[k] = true
			}
		}
	}
	return out, nil
}

// SetAssistantSessionKeys persists the normalized set of active Persona main-assistant backing session keys.
func (s *Space) SetAssistantSessionKeys(ctx context.Context, keys []string) error {
	seen := map[string]bool{}
	clean := make([]string, 0, len(keys))
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if k == "" || seen[k] {
			continue
		}
		seen[k] = true
		clean = append(clean, k)
	}
	slices.Sort(clean)
	b, _ := json.Marshal(clean)
	if err := s.store.PutSetting(ctx, settingAssistantSessions, string(b)); err != nil {
		return err
	}
	s.notify(nil)
	return nil
}

func callWithRefresh[T any](ctx context.Context, s *Space, accountID string, adapter provider.Adapter, cred string, fn func(string) (T, error)) (T, error) {
	res, err := fn(cred)
	if provider.CodeOf(err) == provider.CodeAuthFailed {
		if fresh := s.refresh(ctx, accountID, adapter, cred); fresh != "" {
			res, err = fn(fresh)
		}
	}
	s.observe(accountID, err)
	if err != nil && provider.CodeOf(err) == provider.CodeAuthFailed {
		st := protocol.AccountError
		code := provider.CodeAuthFailed
		_ = s.store.UpdateAccount(ctx, accountID, store.AccountPatch{Status: &st, LastError: &code}, s.now())
	}
	return res, err
}

// StartSession opens a new session with its first message on a provider implementing provider.SessionCreator.
func (s *Space) StartSession(ctx context.Context, p protocol.StartSessionParams) (protocol.Session, error) {
	text := strings.TrimSpace(p.Text)
	title := strings.TrimSpace(p.Title)
	switch {
	case p.AccountID == "":
		return protocol.Session{}, badInput("account_id is required")
	case text == "":
		return protocol.Session{}, badInput("text is required")
	case len16(text) > protocol.MaxText:
		return protocol.Session{}, badInput("text is longer than 20000 characters")
	}
	acct, err := s.requireAccount(ctx, p.AccountID)
	if err != nil {
		return protocol.Session{}, err
	}
	if acct.Status == protocol.AccountPaused {
		return protocol.Session{}, protocol.Conflict("the account is paused").Because(protocol.ReasonAccountPaused)
	}
	adapter, err := s.adapter(acct.Provider)
	if err != nil {
		return protocol.Session{}, err
	}
	sc, ok := adapter.(provider.SessionCreator)
	if !ok {
		return protocol.Session{}, protocol.Unsupported("club.sessions.start: provider does not support starting sessions").
			Because(protocol.ReasonProviderUnavailable)
	}
	cred, found, err := s.vault.Get(ctx, acct.AccountID)
	if err != nil {
		return protocol.Session{}, err
	}
	if !found {
		return protocol.Session{}, protocol.Failure("NOT_PAIRED", "account has no credential")
	}
	type createOut struct {
		sess provider.RemoteSession
		res  provider.SendResult
	}
	out, err := callWithRefresh(ctx, s, acct.AccountID, adapter, cred, func(c string) (createOut, error) {
		rs, sr, e := sc.CreateSession(ctx, c, title, text)
		return createOut{rs, sr}, e
	})
	if err != nil {
		return protocol.Session{}, protocol.Failure(provider.CodeOf(err), "start session failed: "+provider.CodeOf(err))
	}
	s.noteQuota(acct.AccountID, out.res.Quota)
	rows := sessionRows(acct, []provider.RemoteSession{out.sess})
	if _, err := s.store.UpsertSessions(ctx, rows, s.now()); err != nil {
		return protocol.Session{}, err
	}
	sessKey := rows[0].SessionKey
	if len(out.res.Messages) > 0 {
		if _, err := s.store.UpsertMessages(ctx, sessKey, storeMessages(sessKey, out.res.Messages, out.sess.LastActivityMs), s.now()); err != nil {
			return protocol.Session{}, err
		}
	}
	sess, ok, err := s.store.Session(ctx, sessKey)
	if err != nil {
		return protocol.Session{}, err
	}
	if !ok {
		return protocol.Session{}, noSession()
	}
	if !wantsHistory(adapter) {
		s.followUp(sess)
		s.watchReply(sess)
	}
	s.notify(nil)
	v := sessionView(sess)
	v.Quota = s.quotaView(acct, s.now())
	return v, nil
}

// InterruptSession stops the active turn of a session on a provider implementing provider.Interrupter.
func (s *Space) InterruptSession(ctx context.Context, p protocol.SessionParams) error {
	if strings.TrimSpace(p.SessionKey) == "" {
		return protocol.Unsupported("club.sessions.interrupt: no provider in this build can interrupt a session").
			Because(protocol.ReasonProviderUnavailable)
	}
	sess, err := s.requireSession(ctx, p.SessionKey)
	if err != nil {
		return err
	}
	adapter, err := s.adapter(sess.Provider)
	if err != nil {
		return err
	}
	intr, ok := adapter.(provider.Interrupter)
	if !ok {
		return protocol.Unsupported("club.sessions.interrupt: provider does not support interrupting sessions").
			Because(protocol.ReasonProviderUnavailable)
	}
	cred, found, err := s.vault.Get(ctx, sess.AccountID)
	if err != nil {
		return err
	}
	if !found {
		return protocol.Failure("NOT_PAIRED", "account has no credential")
	}
	ref := provider.SessionRef{ExternalID: sess.ExternalID, Ref: sess.Ref}
	_, err = callWithRefresh(ctx, s, sess.AccountID, adapter, cred, func(c string) (struct{}, error) {
		return struct{}{}, intr.Interrupt(ctx, c, ref)
	})
	if err != nil {
		return protocol.Failure(provider.CodeOf(err), "interrupt failed: "+provider.CodeOf(err))
	}
	s.mu.Lock()
	delete(s.live, sess.SessionKey)
	s.mu.Unlock()
	s.notify([]string{sess.SessionKey})
	return nil
}

// SupportsTools reports whether the account's provider implements native provider.ToolCaller.
func (s *Space) SupportsTools(ctx context.Context, accountID string) bool {
	acct, err := s.requireAccount(ctx, accountID)
	if err != nil {
		return false
	}
	adapter, err := s.adapter(acct.Provider)
	if err != nil {
		return false
	}
	_, ok := adapter.(provider.ToolCaller)
	return ok
}

// CallTools executes one native function-calling step against a ToolCaller provider (such as Groq). Returns
// supported=false when the account's provider does not implement provider.ToolCaller.
func (s *Space) CallTools(ctx context.Context, accountID string, turns []provider.ToolTurn, tools []provider.ToolDef) (provider.ToolStepResult, bool, error) {
	acct, err := s.requireAccount(ctx, accountID)
	if err != nil {
		return provider.ToolStepResult{}, true, err
	}
	if acct.Status == protocol.AccountPaused {
		return provider.ToolStepResult{}, true, protocol.Conflict("the account is paused").Because(protocol.ReasonAccountPaused)
	}
	adapter, err := s.adapter(acct.Provider)
	if err != nil {
		return provider.ToolStepResult{}, false, err
	}
	tc, ok := adapter.(provider.ToolCaller)
	if !ok {
		return provider.ToolStepResult{}, false, nil
	}
	cred, found, err := s.vault.Get(ctx, acct.AccountID)
	if err != nil {
		return provider.ToolStepResult{}, true, err
	}
	if !found {
		return provider.ToolStepResult{}, true, protocol.Failure("NOT_PAIRED", "account has no credential")
	}
	res, err := callWithRefresh(ctx, s, acct.AccountID, adapter, cred, func(c string) (provider.ToolStepResult, error) {
		return tc.CallTools(ctx, c, turns, tools)
	})
	if err != nil {
		return provider.ToolStepResult{}, true, err
	}
	s.noteQuota(acct.AccountID, res.Quota)
	return res, true, nil
}

// InboxAccountsOff are the accounts whose chats the owner kept out of Persona's Inbox. Every account is in until
// turned off, once the extension's switch is on.
func (s *Space) InboxAccountsOff(ctx context.Context) map[string]bool {
	raw, ok, err := s.store.Setting(ctx, settingInboxAccountsOff)
	if err != nil || !ok {
		return map[string]bool{}
	}
	var m map[string]bool
	if json.Unmarshal([]byte(raw), &m) != nil || m == nil {
		return map[string]bool{}
	}
	return m
}

// SetInboxAccount puts one account's chats into Persona's Inbox (on) or keeps them out (off).
func (s *Space) SetInboxAccount(ctx context.Context, accountID string, on bool) error {
	m := s.InboxAccountsOff(ctx)
	if on {
		delete(m, accountID)
	} else {
		m[accountID] = true
	}
	b, _ := json.Marshal(m)
	if err := s.store.PutSetting(ctx, settingInboxAccountsOff, string(b)); err != nil {
		return err
	}
	s.notify(nil)
	return nil
}

// InboxSince is when the extension's Inbox switch was first seen on (unix ms); 0 while it is off.
func (s *Space) InboxSince(ctx context.Context) int64 {
	raw, ok, err := s.store.Setting(ctx, settingInboxSince)
	if err != nil || !ok {
		return 0
	}
	n, _ := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	return n
}

// MarkInboxSince records when the Inbox switch was first seen on (kept while it stays on) or forgets it (off), so
// that turning it on does not bring a day's old chats into the Inbox as new.
func (s *Space) MarkInboxSince(ctx context.Context, on bool) (int64, error) {
	cur := s.InboxSince(ctx)
	switch {
	case on && cur == 0:
		cur = s.now()
		if err := s.store.PutSetting(ctx, settingInboxSince, strconv.FormatInt(cur, 10)); err != nil {
			return 0, err
		}
	case !on && cur != 0:
		if err := s.store.PutSetting(ctx, settingInboxSince, ""); err != nil {
			return 0, err
		}
		cur = 0
	}
	return cur, nil
}
