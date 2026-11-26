package core

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/store"
)

// fake is a scripted provider: sendFn decides each send; calls counts sends per client id.
type fake struct {
	mu        sync.Mutex
	calls     map[string]int
	sendFn    func(msg provider.Outgoing) (provider.SendResult, error)
	confirmFn func(clientID string) provider.Confirmation
	messages  []provider.RemoteMessage
	confirms  int
}

func newFake() *fake {
	return &fake{calls: map[string]int{}, sendFn: func(provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{Status: provider.Sent}, nil
	}}
}

func (f *fake) Name() string       { return "fake" }
func (f *fake) Connection() string { return protocol.ConnectionServer }
func (f *fake) Verify(_ context.Context, cred string) (provider.RemoteAccount, error) {
	if cred == "bad-credential" {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401}
	}
	return provider.RemoteAccount{ExternalAccountID: "ext-" + cred, Alias: "Fake"}, nil
}
func (f *fake) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{
		{ExternalID: "main", Title: "Main", Kind: protocol.KindMain, CanSend: true, LastActivityMs: 1000},
		{ExternalID: "task:1", Title: "Sub task", Kind: protocol.KindTask, ParentExternalID: "main", LastActivityMs: 500},
	}, nil
}
func (f *fake) ListMessages(_ context.Context, _ string, ref provider.SessionRef, _ provider.ListOptions) (provider.RemotePage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if ref.ExternalID != "main" {
		return provider.RemotePage{}, nil
	}
	return provider.RemotePage{Messages: append([]provider.RemoteMessage(nil), f.messages...)}, nil
}
func (f *fake) Send(_ context.Context, _ string, _ provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	f.mu.Lock()
	f.calls[msg.ClientID]++
	fn := f.sendFn
	f.mu.Unlock()
	return fn(msg)
}
func (f *fake) ConfirmSend(_ context.Context, _ string, _ provider.SessionRef, clientID string) (provider.Confirmation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.confirms++
	if f.confirmFn == nil {
		return provider.Confirmation{Status: provider.ConfirmUnknown}, nil
	}
	return f.confirmFn(clientID), nil
}
func (f *fake) count(clientID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[clientID]
}

type fixture struct {
	club  *Club
	space *Space
	fake  *fake
	acct  protocol.Account
	main  string
	task  string
	clock *clock
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Millisecond)
	return c.t
}

func setup(t *testing.T, followUps ...time.Duration) *fixture {
	t.Helper()
	f := newFake()
	clk := &clock{t: time.UnixMilli(1_800_000_000_000)}
	if followUps == nil {
		followUps = []time.Duration{}
	}
	club := New(Options{Version: "test", Registry: provider.NewRegistry(f), Now: clk.Now, FollowUps: followUps})
	t.Cleanup(club.Close)
	ctx := context.Background()
	space, err := club.Space(ctx, DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "fake", Credential: "good-credential"})
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{club: club, space: space, fake: f, acct: acct, clock: clk,
		main: SessionKey("fake", acct.AccountID, "main"), task: SessionKey("fake", acct.AccountID, "task:1")}
}

func uuid(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

func (fx *fixture) outbox(t *testing.T, clientID string) store.Outbox {
	t.Helper()
	o, ok, err := fx.space.Store().Outbox(context.Background(), clientID)
	if err != nil || !ok {
		t.Fatalf("outbox %s: %v %v", clientID, ok, err)
	}
	return o
}

func TestAddAccount(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	if fx.acct.Status != protocol.AccountActive || fx.acct.SessionCount != 2 || !ValidAccountID(fx.acct.AccountID) {
		t.Fatalf("account %+v", fx.acct)
	}
	if c, _ := fx.space.AssistantControl(ctx, fx.acct.AccountID); c != ControlOff {
		t.Fatalf("new account control %q, want off", c)
	}
	_, err := fx.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "fake", Credential: "good-credential"})
	if protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("duplicate pairing: %v", err)
	}
	_, err = fx.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "fake", Credential: "bad-credential"})
	if protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("refused credential: %v", err)
	}
	_, err = fx.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "cue", Credential: "some-credential"})
	if protocol.CodeOf(err) != protocol.CodeUnsupported {
		t.Fatalf("known provider without adapter: %v", err)
	}
	_, err = fx.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "nope", Credential: "some-credential"})
	if protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("unknown provider: %v", err)
	}
	st, _ := fx.space.Status(ctx)
	if len(st.Accounts) != 1 || st.Setup != "" {
		t.Fatalf("status %+v", st)
	}
	var cue *protocol.Provider
	for i := range st.Providers {
		if st.Providers[i].Provider == "cue" {
			cue = &st.Providers[i]
		}
	}
	if cue == nil || cue.Available {
		t.Fatalf("cue should be listed unavailable: %+v", st.Providers)
	}
	if err := fx.space.RemoveAccount(ctx, protocol.RemoveAccountParams{AccountID: fx.acct.AccountID, DeleteData: true}); err != nil {
		t.Fatal(err)
	}
	st, _ = fx.space.Status(ctx)
	if st.Setup != protocol.SetupNoAccounts {
		t.Fatalf("after remove: %+v", st)
	}
	if _, ok, _ := fx.space.vault.Get(ctx, fx.acct.AccountID); ok {
		t.Fatal("credential kept after unpairing")
	}
}

func TestSendSameClientIDSendsOnce(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	p := protocol.SendParams{SessionKey: fx.main, ClientID: uuid(1), Text: "hello"}
	first, err := fx.space.Send(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fx.space.Send(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if fx.fake.count(uuid(1)) != 1 {
		t.Fatalf("provider saw %d sends", fx.fake.count(uuid(1)))
	}
	if first.Local.Status != protocol.LocalSent || second.Local.Status != protocol.LocalSent || second.ExternalID != "local:"+uuid(1) {
		t.Fatalf("first %+v second %+v", first.Local, second.Local)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			_, _ = fx.space.Send(ctx, protocol.SendParams{SessionKey: fx.main, ClientID: uuid(2), Text: "at once"})
		})
	}
	wg.Wait()
	if n := fx.fake.count(uuid(2)); n != 1 {
		t.Fatalf("concurrent sends with one client id reached the provider %d times", n)
	}

	_, err = fx.space.Send(ctx, protocol.SendParams{SessionKey: fx.task, ClientID: uuid(1), Text: "hello"})
	if protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("client id reused elsewhere: %v", err)
	}
}

func TestUnconfirmedIsNeverResent(t *testing.T) {
	for name, outcome := range map[string]func(provider.Outgoing) (provider.SendResult, error){
		"lost answer": func(provider.Outgoing) (provider.SendResult, error) {
			return provider.SendResult{}, errors.New("connection reset")
		},
		"provider unsure": func(provider.Outgoing) (provider.SendResult, error) {
			return provider.SendResult{Status: provider.Unconfirmed, Error: "TIMEOUT"}, nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx := setup(t, time.Millisecond, 2*time.Millisecond)
			fx.fake.sendFn = outcome
			ctx := context.Background()
			p := protocol.SendParams{SessionKey: fx.main, ClientID: uuid(3), Text: "maybe"}
			m, err := fx.space.Send(ctx, p)
			if err != nil {
				t.Fatal(err)
			}
			if m.Local.Status != protocol.LocalUnconfirmed || m.Local.Error == "" {
				t.Fatalf("local %+v", m.Local)
			}
			if again, _ := fx.space.Send(ctx, p); again.Local.Status != protocol.LocalUnconfirmed {
				t.Fatalf("again %+v", again.Local)
			}
			if err := fx.space.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(2 * time.Second)
			for {
				fx.fake.mu.Lock()
				asked := fx.fake.confirms
				fx.fake.mu.Unlock()
				if asked >= 2 || time.Now().After(deadline) {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if n := fx.fake.count(uuid(3)); n != 1 {
				t.Fatalf("provider saw %d sends", n)
			}
			if o := fx.outbox(t, uuid(3)); o.Status != protocol.LocalUnconfirmed {
				t.Fatalf("outbox %+v", o)
			}
			page, _ := fx.space.Messages(ctx, protocol.MessagesParams{SessionKey: fx.main})
			last := page.Messages[len(page.Messages)-1]
			if last.Local == nil || last.Local.Status != protocol.LocalUnconfirmed {
				t.Fatalf("messages do not show the unconfirmed send: %+v", page.Messages)
			}
		})
	}
}

func TestConfirmLaterSettlesWithoutResending(t *testing.T) {
	fx := setup(t, time.Millisecond)
	fx.fake.sendFn = func(provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{}, errors.New("timeout")
	}
	fx.fake.confirmFn = func(string) provider.Confirmation { return provider.Confirmation{Status: provider.ConfirmSent} }
	ctx := context.Background()
	if _, err := fx.space.Send(ctx, protocol.SendParams{SessionKey: fx.main, ClientID: uuid(4), Text: "x"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for fx.outbox(t, uuid(4)).Status != protocol.LocalSent {
		if time.Now().After(deadline) {
			t.Fatalf("still %+v", fx.outbox(t, uuid(4)))
		}
		time.Sleep(time.Millisecond)
	}
	if n := fx.fake.count(uuid(4)); n != 1 {
		t.Fatalf("provider saw %d sends", n)
	}
}

func TestRefusedSendFails(t *testing.T) {
	fx := setup(t)
	fx.fake.sendFn = func(provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeRateLimited, HTTPStatus: 429, Retryable: true}
	}
	m, err := fx.space.Send(context.Background(), protocol.SendParams{SessionKey: fx.main, ClientID: uuid(5), Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Local.Status != protocol.LocalFailed || m.Local.Error != provider.CodeRateLimited {
		t.Fatalf("local %+v", m.Local)
	}
}

func TestRecoverAfterRestart(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	st := fx.space.Store()
	for i, text := range []string{"claimed before the restart", "never claimed"} {
		if _, _, err := st.EnqueueOutbox(ctx, store.Outbox{ClientID: uuid(10 + i), SessionKey: fx.main,
			AccountID: fx.acct.AccountID, Text: text, CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok, _ := st.ClaimOutbox(ctx, uuid(10), 2); !ok {
		t.Fatal("claim")
	}
	if err := fx.space.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if o := fx.outbox(t, uuid(10)); o.Status != protocol.LocalUnconfirmed || o.LastError != "SEND_UNCONFIRMED" {
		t.Fatalf("claimed row after recovery: %+v", o)
	}
	if n := fx.fake.count(uuid(10)); n != 0 {
		t.Fatalf("claimed row was sent %d times", n)
	}
	if o := fx.outbox(t, uuid(11)); o.Status != protocol.LocalSent || fx.fake.count(uuid(11)) != 1 {
		t.Fatalf("pending row after recovery: %+v (%d sends)", o, fx.fake.count(uuid(11)))
	}
}

func TestCancel(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	if _, _, err := fx.space.Store().EnqueueOutbox(ctx, store.Outbox{ClientID: uuid(20), SessionKey: fx.main,
		AccountID: fx.acct.AccountID, Text: "not yet", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := fx.space.DeleteMessage(ctx, protocol.DeleteMessageParams{SessionKey: fx.main, ClientID: uuid(20)}); err != nil {
		t.Fatal(err)
	}
	if o := fx.outbox(t, uuid(20)); o.Status != protocol.LocalFailed || o.LastError != "CANCELLED" {
		t.Fatalf("cancelled row %+v", o)
	}
	_ = fx.space.Recover(ctx)
	if fx.fake.count(uuid(20)) != 0 {
		t.Fatal("a cancelled send went out")
	}
	err := fx.space.DeleteMessage(ctx, protocol.DeleteMessageParams{SessionKey: fx.main, ClientID: uuid(20)})
	if protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("cancel twice: %v", err)
	}
	if _, err := fx.space.Send(ctx, protocol.SendParams{SessionKey: fx.main, ClientID: uuid(21), Text: "gone"}); err != nil {
		t.Fatal(err)
	}
	err = fx.space.DeleteMessage(ctx, protocol.DeleteMessageParams{SessionKey: fx.main, ClientID: uuid(21)})
	if protocol.CodeOf(err) != protocol.CodeConflict {
		t.Fatalf("cancel a sent message: %v", err)
	}
	err = fx.space.DeleteMessage(ctx, protocol.DeleteMessageParams{SessionKey: "agent:fake:a000000000000:x", ClientID: uuid(21)})
	if protocol.CodeOf(err) != protocol.CodeNotFound {
		t.Fatalf("cancel in a missing session: %v", err)
	}
}

func TestSendRefusals(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	cases := map[string]struct {
		p    protocol.SendParams
		code string
	}{
		"no session key":   {protocol.SendParams{ClientID: uuid(30), Text: "x"}, protocol.CodeInvalid},
		"client id":        {protocol.SendParams{SessionKey: fx.main, ClientID: "not-a-uuid", Text: "x"}, protocol.CodeInvalid},
		"empty text":       {protocol.SendParams{SessionKey: fx.main, ClientID: uuid(30), Text: "  "}, protocol.CodeInvalid},
		"too long":         {protocol.SendParams{SessionKey: fx.main, ClientID: uuid(30), Text: longText(protocol.MaxText + 1)}, protocol.CodeInvalid},
		"missing session":  {protocol.SendParams{SessionKey: "agent:fake:a000000000000:x", ClientID: uuid(30), Text: "x"}, protocol.CodeNotFound},
		"read-only":        {protocol.SendParams{SessionKey: fx.task, ClientID: uuid(30), Text: "x"}, protocol.CodeConflict},
		"emoji at the cap": {protocol.SendParams{SessionKey: fx.main, ClientID: uuid(30), Text: longEmoji(protocol.MaxText/2 + 1)}, protocol.CodeInvalid},
	}
	for name, c := range cases {
		if _, err := fx.space.Send(ctx, c.p); protocol.CodeOf(err) != c.code {
			t.Errorf("%s: %v, want %s", name, err, c.code)
		}
	}
	paused := protocol.AccountPaused
	_ = fx.space.Store().UpdateAccount(ctx, fx.acct.AccountID, store.AccountPatch{Status: &paused}, 1)
	if _, err := fx.space.Send(ctx, protocol.SendParams{SessionKey: fx.main, ClientID: uuid(31), Text: "x"}); protocol.CodeOf(err) != protocol.CodeConflict {
		t.Errorf("paused account: %v", err)
	}
	if fx.fake.count(uuid(30))+fx.fake.count(uuid(31)) != 0 {
		t.Error("a refused send reached the provider")
	}
}

func longText(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = 'x'
	}
	return string(b)
}

func longEmoji(n int) string {
	s := ""
	for range n {
		s += "\U0001F600"
	}
	return s
}

func TestMessagesReadAndSearch(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	for i := range 7 {
		role := protocol.RoleAssistant
		if i == 0 {
			role = protocol.RoleUser
		}
		fx.fake.messages = append(fx.fake.messages, provider.RemoteMessage{ExternalID: fmt.Sprintf("r%d", i), Role: role,
			Blocks: []protocol.Block{protocol.TextBlock(fmt.Sprintf("Reply number %d", i))}, CreateTimeMs: 2_000_000_000_000 + int64(i), Position: int64(i)})
	}
	sub := fx.club.Subscribe(DefaultSpace)
	defer sub.Close()
	if err := fx.space.SyncAccount(ctx, fx.acct.AccountID); err != nil {
		t.Fatal(err)
	}
	<-sub.Ready()
	if evs := sub.Take(); len(evs) != 1 || len(evs[0].SessionKeys) != 1 || evs[0].SessionKeys[0] != fx.main {
		t.Fatalf("events %+v", evs)
	}
	sessions, _ := fx.space.Sessions(ctx)
	if sessions.Sessions[0].SessionKey != fx.main || sessions.Sessions[0].UnreadCount != 6 ||
		sessions.Sessions[0].PreviewText != "Reply number 6" || sessions.Sessions[0].PreviewIsMine {
		t.Fatalf("sessions %+v", sessions.Sessions)
	}
	if sessions.Sessions[1].ParentSessionKey != fx.main || sessions.Sessions[1].CanSend {
		t.Fatalf("task session %+v", sessions.Sessions[1])
	}

	page, err := fx.space.Messages(ctx, protocol.MessagesParams{SessionKey: fx.main, Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Messages) != 3 || !page.HasMore || page.NextCursor == "" || page.Messages[2].ExternalID != "r6" {
		t.Fatalf("first page %+v", page)
	}
	older, err := fx.space.Messages(ctx, protocol.MessagesParams{SessionKey: fx.main, Limit: 3, Cursor: page.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(older.Messages) != 3 || older.Messages[2].ExternalID != "r3" {
		t.Fatalf("older page %+v", older.Messages)
	}
	if _, err := fx.space.Messages(ctx, protocol.MessagesParams{SessionKey: fx.main, Cursor: "!!"}); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("bad cursor: %v", err)
	}

	if err := fx.space.MarkRead(ctx, protocol.MarkReadParams{SessionKey: fx.main}); err != nil {
		t.Fatal(err)
	}
	sessions, _ = fx.space.Sessions(ctx)
	if sessions.Sessions[0].UnreadCount != 0 {
		t.Fatalf("after mark_read %+v", sessions.Sessions[0])
	}
	<-sub.Ready()
	if evs := sub.Take(); len(evs) != 1 || evs[0].SessionKeys[0] != fx.main {
		t.Fatalf("mark_read events %+v", evs)
	}

	res, _ := fx.space.Search(ctx, protocol.SearchParams{Q: "NUMBER 4"})
	if len(res.Hits) != 1 || res.Hits[0].ExternalID != "r4" || res.Hits[0].Title != "Main" || res.Hits[0].Provider != "fake" {
		t.Fatalf("search %+v", res.Hits)
	}
	if res, _ := fx.space.Search(ctx, protocol.SearchParams{Q: " "}); res.Hits == nil || len(res.Hits) != 0 {
		t.Fatalf("empty query %+v", res)
	}
}
