package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

func TestSendAuthFailurePreservesExplicitPause(t *testing.T) {
	e := newEnv(t, "cue")
	ctx := context.Background()
	e.p.sessions = []provider.RemoteSession{{ExternalID: "main", CanSend: true}}
	acct := e.pair(t, "fixture-original")
	entered, release := make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	e.p.mu.Lock()
	e.p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
		close(entered)
		<-release
		return provider.SendResult{}, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401}
	}
	e.p.mu.Unlock()

	type outcome struct {
		msg protocol.Message
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		msg, err := e.space.Send(ctx, protocol.SendParams{
			SessionKey: SessionKey("cue", acct.AccountID, "main"), ClientID: uuid(9), Text: "hello",
		})
		done <- outcome{msg, err}
	}()
	select {
	case <-entered:
	case got := <-done:
		t.Fatalf("send returned before reaching provider: %v", got.err)
	case <-time.After(5 * time.Second):
		t.Fatal("send did not reach provider")
	}

	paused := true
	updated, err := e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: acct.AccountID, Paused: &paused})
	if err != nil || updated.Status != protocol.AccountPaused {
		t.Fatalf("explicit pause: status = %q, err = %v", updated.Status, err)
	}
	finish()
	select {
	case got := <-done:
		if got.err != nil || got.msg.Local == nil || got.msg.Local.Status != protocol.LocalFailed || got.msg.Local.Error != provider.CodeAuthFailed {
			t.Fatalf("send did not preserve auth failure: local = %+v, err = %v", got.msg.Local, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("send did not settle")
	}
	stored, err := e.space.Account(ctx, acct.AccountID)
	if err != nil || stored.Status != protocol.AccountPaused || stored.LastError != provider.CodeAuthFailed {
		t.Fatalf("stored account: status = %q, last error = %q, err = %v", stored.Status, stored.LastError, err)
	}
}
