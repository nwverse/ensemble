// Package echo is an in-memory provider for tests and demos only: it answers every message with "echo: <text>".
// It reaches no service and is never registered unless asked for.
//
// Credentials are any string starting with "echo-" (8 characters at least). Each credential is its own account with
// two sessions: "main", which takes messages, and "notes", which is read-only. Text containing "#unconfirmed" is
// kept but answered as unconfirmed; text containing "#fail" is refused.
package echo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// Name is the provider's name.
const Name = "echo"

// Markers in a message's text that steer the demo.
const (
	MarkUnconfirmed = "#unconfirmed"
	MarkFail        = "#fail"
)

// Provider is the echo provider.
type Provider struct {
	Now func() time.Time

	mu       sync.Mutex
	accounts map[string]*account // by credential
	sends    map[string]int      // by client id
	seq      int
}

type account struct {
	sessions []*session
}

type session struct {
	remote   provider.RemoteSession
	messages []provider.RemoteMessage
}

// New is an empty echo provider.
func New() *Provider { return &Provider{accounts: map[string]*account{}, sends: map[string]int{}} }

var (
	_ provider.Adapter        = (*Provider)(nil)
	_ provider.SendConfirmer  = (*Provider)(nil)
	_ provider.LiveStater     = (*Provider)(nil)
	_ provider.SessionCreator = (*Provider)(nil)
)

func (p *Provider) Name() string       { return Name }
func (p *Provider) Connection() string { return protocol.ConnectionServer }

func (p *Provider) now() int64 {
	if p.Now != nil {
		return p.Now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

func valid(credential string) bool {
	return strings.HasPrefix(credential, "echo-") && len(credential) >= 8
}

func externalID(credential string) string {
	sum := sha256.Sum256([]byte(credential))
	return "echo:" + hex.EncodeToString(sum[:4])
}

// account is the credential's account, made on first use. p.mu is held.
func (p *Provider) account(credential string) (*account, error) {
	if !valid(credential) {
		return nil, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401}
	}
	a := p.accounts[credential]
	if a == nil {
		now := p.now()
		a = &account{sessions: []*session{
			{remote: provider.RemoteSession{ExternalID: "main", Title: "Echo", Kind: protocol.KindMain, CanSend: true,
				LastActivityMs: now, Ref: map[string]string{}}},
			{remote: provider.RemoteSession{ExternalID: "notes", Title: "Echo notes", Kind: protocol.KindSide,
				ParentExternalID: "main", LastActivityMs: now, Ref: map[string]string{}},
				messages: []provider.RemoteMessage{{ExternalID: "notes-1", Role: protocol.RoleAssistant,
					Blocks: []protocol.Block{protocol.TextBlock("This session is read-only.")}, CreateTimeMs: now}}},
		}}
		p.accounts[credential] = a
	}
	return a, nil
}

func (a *account) find(id string) *session {
	for _, s := range a.sessions {
		if s.remote.ExternalID == id {
			return s
		}
	}
	return nil
}

func (p *Provider) Verify(_ context.Context, credential string) (provider.RemoteAccount, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.account(credential); err != nil {
		return provider.RemoteAccount{}, err
	}
	return provider.RemoteAccount{ExternalAccountID: externalID(credential)}, nil
}

func (p *Provider) ListSessions(_ context.Context, credential string) ([]provider.RemoteSession, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.account(credential)
	if err != nil {
		return nil, err
	}
	out := make([]provider.RemoteSession, len(a.sessions))
	for i, s := range a.sessions {
		out[i] = s.remote
	}
	return out, nil
}

func (p *Provider) ListMessages(_ context.Context, credential string, ref provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.account(credential)
	if err != nil {
		return provider.RemotePage{}, err
	}
	s := a.find(ref.ExternalID)
	if s == nil {
		return provider.RemotePage{}, &provider.Error{Code: provider.CodeNotFound, HTTPStatus: 404}
	}
	msgs := s.messages
	if opts.Limit > 0 && len(msgs) > opts.Limit {
		msgs = msgs[len(msgs)-opts.Limit:]
	}
	return provider.RemotePage{Messages: append([]provider.RemoteMessage(nil), msgs...)}, nil
}

func (p *Provider) Send(_ context.Context, credential string, ref provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, err := p.account(credential)
	if err != nil {
		return provider.SendResult{}, err
	}
	s := a.find(ref.ExternalID)
	if s == nil {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeNotFound, HTTPStatus: 404}
	}
	if !s.remote.CanSend {
		return provider.SendResult{}, &provider.Error{Code: "READ_ONLY", HTTPStatus: 403}
	}
	if strings.Contains(msg.Text, MarkFail) {
		return provider.SendResult{}, &provider.Error{Code: "ECHO_REFUSED", HTTPStatus: 400}
	}
	p.sends[msg.ClientID]++
	id := p.append(s, protocol.RoleUser, msg.Text)
	if strings.Contains(msg.Text, MarkUnconfirmed) {
		return provider.SendResult{Status: provider.Unconfirmed, Error: "ECHO_UNCONFIRMED"}, nil
	}
	p.append(s, protocol.RoleAssistant, "echo: "+msg.Text)
	// Like a provider that answers in the same call: the exchange comes back with the send.
	answered := append([]provider.RemoteMessage(nil), s.messages[len(s.messages)-2:]...)
	return provider.SendResult{Status: provider.Sent, ExternalID: id, Messages: answered}, nil
}

// append adds a message to s. p.mu is held.
func (p *Provider) append(s *session, role, text string) string {
	p.seq++
	now := p.now()
	id := fmt.Sprintf("m%d", p.seq)
	s.messages = append(s.messages, provider.RemoteMessage{ExternalID: id, Role: role,
		Blocks: []protocol.Block{protocol.TextBlock(text)}, CreateTimeMs: now, Position: int64(p.seq)})
	s.remote.LastActivityMs = now
	return id
}

// ConfirmSend never knows more than the send itself said.
func (p *Provider) ConfirmSend(context.Context, string, provider.SessionRef, string) (provider.Confirmation, error) {
	return provider.Confirmation{Status: provider.ConfirmUnknown}, nil
}

// LiveState: echo answers at once, so it is never working.
func (p *Provider) LiveState(context.Context, string, provider.SessionRef) (provider.LiveState, error) {
	return provider.LiveState{}, nil
}

// CreateSession opens a new session and sends text into it.
func (p *Provider) CreateSession(ctx context.Context, credential, title, text string) (provider.RemoteSession, provider.SendResult, error) {
	p.mu.Lock()
	a, err := p.account(credential)
	if err != nil {
		p.mu.Unlock()
		return provider.RemoteSession{}, provider.SendResult{}, err
	}
	if title == "" {
		title = "Echo"
	}
	p.seq++
	s := &session{remote: provider.RemoteSession{ExternalID: fmt.Sprintf("s%d", p.seq), Title: title,
		Kind: protocol.KindMain, CanSend: true, LastActivityMs: p.now(), Ref: map[string]string{}}}
	a.sessions = append(a.sessions, s)
	p.mu.Unlock()
	res, err := p.Send(ctx, credential, s.remote.SessionRef(), provider.Outgoing{Text: text}, provider.SendContext{})
	return s.remote, res, err
}

// Sends is how many times a client id reached the provider (for tests).
func (p *Provider) Sends(clientID string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sends[clientID]
}
