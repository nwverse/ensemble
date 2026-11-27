// Package conformance is one protocol test suite that each shell runs against itself (serve over a unix socket, the
// Persona extension over the extension protocol), so both answer agentsrc.* the same way. Tests only.
package conformance

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/echo"
	"github.com/nwverse/ensemble/internal/store"
)

// Shell is a shell under test, as its client sees it.
type Shell interface {
	// Call calls a full method name (agentsrc.status) and decodes the result into out. A refusal with a code comes
	// back as a *protocol.Error.
	Call(method string, params, out any) error
	// Event waits up to d for the next agentsrc.updated.
	Event(d time.Duration) (protocol.UpdatedEvent, bool)
}

// Start starts a shell around club.
type Start func(t *testing.T, club *core.Club) Shell

// NewClub is the club the suite runs against: the echo provider and no follow-up timers.
func NewClub(t *testing.T) (*core.Club, *echo.Provider) {
	t.Helper()
	e := echo.New()
	club := core.New(core.Options{Version: "test", Registry: provider.NewRegistry(e), FollowUps: []time.Duration{}})
	t.Cleanup(club.Close)
	return club, e
}

const (
	clientA = "11111111-1111-4111-8111-111111111111"
	clientB = "22222222-2222-4222-8222-222222222222"
	clientC = "33333333-3333-4333-8333-333333333333"
	missing = "agent:echo:a000000000000:nowhere"
)

func code(err error) string {
	var e *protocol.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

// Run runs the suite against shells that start.
func Run(t *testing.T, start Start) {
	club, e := NewClub(t)
	ctx := context.Background()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	sh := start(t, club)

	var empty protocol.StatusResult
	must(t, sh.Call("agentsrc.status", nil, &empty))
	if !empty.Enabled || empty.Setup != protocol.SetupNoAccounts || len(empty.Accounts) != 0 {
		t.Fatalf("status before pairing: %+v", empty)
	}

	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: "echo-synthetic-1"})
	if err != nil {
		t.Fatal(err)
	}
	main := core.SessionKey(echo.Name, acct.AccountID, "main")
	notes := core.SessionKey(echo.Name, acct.AccountID, "notes")
	// A send queued before the program last stopped and never claimed: the one delete_message can still cancel.
	if _, _, err := space.Store().EnqueueOutbox(ctx, store.Outbox{ClientID: clientC, SessionKey: main,
		AccountID: acct.AccountID, Text: "queued", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	drain(sh)

	t.Run("status", func(t *testing.T) {
		var st protocol.StatusResult
		must(t, sh.Call("agentsrc.status", map[string]any{}, &st))
		if !st.Enabled || st.Setup != "" || len(st.Accounts) != 1 || st.Accounts[0].AccountID != acct.AccountID ||
			st.Accounts[0].SessionCount != 2 || st.Accounts[0].Status != protocol.AccountActive {
			t.Fatalf("status %+v", st)
		}
		avail := map[string]bool{}
		for _, p := range st.Providers {
			avail[p.Provider] = p.Available
		}
		if !avail[echo.Name] || avail["cue"] || avail["muse"] {
			t.Fatalf("providers %+v", st.Providers)
		}
		if _, ok := avail["claude_duet"]; !ok {
			t.Fatalf("known providers missing: %+v", st.Providers)
		}
	})

	t.Run("sessions", func(t *testing.T) {
		var res protocol.SessionsResult
		must(t, sh.Call("agentsrc.sessions", nil, &res))
		keys := []string{}
		for _, s := range res.Sessions {
			keys = append(keys, s.SessionKey)
			if s.AccountID != acct.AccountID || s.Provider != echo.Name {
				t.Fatalf("session %+v", s)
			}
		}
		if !slices.Contains(keys, main) || !slices.Contains(keys, notes) {
			t.Fatalf("sessions %v", keys)
		}
	})

	t.Run("messages", func(t *testing.T) {
		var page protocol.MessagePage
		must(t, sh.Call("agentsrc.messages", protocol.MessagesParams{SessionKey: notes, Limit: 20}, &page))
		if page.Session == nil || page.Session.SessionKey != notes || page.Session.CanSend || len(page.Messages) != 1 ||
			page.Messages[0].Role != protocol.RoleAssistant || page.Messages[0].Blocks[0].Text == "" || page.HasMore {
			t.Fatalf("notes page %+v", page)
		}
		wantCode(t, "missing session", sh.Call("agentsrc.messages", protocol.MessagesParams{SessionKey: missing, Limit: 20}, nil), protocol.CodeNotFound)
		wantCode(t, "no session_key", sh.Call("agentsrc.messages", map[string]any{"limit": 20}, nil), protocol.CodeInvalid)
		wantCode(t, "bad cursor", sh.Call("agentsrc.messages", protocol.MessagesParams{SessionKey: notes, Cursor: "%%", Limit: 20}, nil), protocol.CodeInvalid)
		wantCode(t, "params not an object", sh.Call("agentsrc.messages", []int{1}, nil), protocol.CodeInvalid)
	})

	t.Run("send", func(t *testing.T) {
		drain(sh)
		var m protocol.Message
		must(t, sh.Call("agentsrc.send", protocol.SendParams{SessionKey: main, ClientID: clientA, Text: "hello club"}, &m))
		if m.Local == nil || m.Local.ClientID != clientA || m.Local.Status != protocol.LocalSent {
			t.Fatalf("send answered %+v", m)
		}
		waitEvent(t, sh, main)
		var again protocol.Message
		must(t, sh.Call("agentsrc.send", protocol.SendParams{SessionKey: main, ClientID: clientA, Text: "hello club"}, &again))
		if again.Local == nil || again.Local.Status != protocol.LocalSent || e.Sends(clientA) != 1 {
			t.Fatalf("the same client_id again: %+v, %d sends", again.Local, e.Sends(clientA))
		}

		must(t, sh.Call("agentsrc.send", protocol.SendParams{SessionKey: main, ClientID: clientB, Text: "maybe " + echo.MarkUnconfirmed}, &m))
		if m.Local.Status != protocol.LocalUnconfirmed {
			t.Fatalf("unconfirmed send answered %+v", m.Local)
		}
		must(t, sh.Call("agentsrc.send", protocol.SendParams{SessionKey: main, ClientID: clientB, Text: "maybe " + echo.MarkUnconfirmed}, &m))
		if m.Local.Status != protocol.LocalUnconfirmed || e.Sends(clientB) != 1 {
			t.Fatalf("unconfirmed send was sent again: %+v, %d sends", m.Local, e.Sends(clientB))
		}

		wantCode(t, "read-only session", sh.Call("agentsrc.send", protocol.SendParams{SessionKey: notes, ClientID: "44444444-4444-4444-8444-444444444444", Text: "x"}, nil), protocol.CodeConflict)
		wantCode(t, "client_id not a UUID", sh.Call("agentsrc.send", protocol.SendParams{SessionKey: main, ClientID: "1", Text: "x"}, nil), protocol.CodeInvalid)
		wantCode(t, "empty text", sh.Call("agentsrc.send", protocol.SendParams{SessionKey: main, ClientID: "55555555-5555-4555-8555-555555555555", Text: ""}, nil), protocol.CodeInvalid)
		wantCode(t, "missing session", sh.Call("agentsrc.send", protocol.SendParams{SessionKey: missing, ClientID: "66666666-6666-4666-8666-666666666666", Text: "x"}, nil), protocol.CodeNotFound)

		var page protocol.MessagePage
		must(t, sh.Call("agentsrc.messages", protocol.MessagesParams{SessionKey: main, Limit: 50}, &page))
		texts := []string{}
		for _, msg := range page.Messages {
			for _, b := range msg.Blocks {
				texts = append(texts, b.Text)
			}
		}
		if !slices.Contains(texts, "echo: hello club") {
			t.Fatalf("messages after send: %v", texts)
		}
	})

	t.Run("mark_read", func(t *testing.T) {
		drain(sh)
		var res map[string]any
		must(t, sh.Call("agentsrc.mark_read", protocol.MarkReadParams{SessionKey: main}, &res))
		if len(res) != 0 {
			t.Fatalf("mark_read answered %v", res)
		}
		waitEvent(t, sh, main)
		var list protocol.SessionsResult
		must(t, sh.Call("agentsrc.sessions", nil, &list))
		for _, s := range list.Sessions {
			if s.SessionKey == main && s.UnreadCount != 0 {
				t.Fatalf("unread after mark_read: %+v", s)
			}
		}
		wantCode(t, "missing session", sh.Call("agentsrc.mark_read", protocol.MarkReadParams{SessionKey: missing}, nil), protocol.CodeNotFound)
	})

	t.Run("delete_message", func(t *testing.T) {
		drain(sh)
		var res map[string]any
		must(t, sh.Call("agentsrc.delete_message", protocol.DeleteMessageParams{SessionKey: main, ClientID: clientC}, &res))
		if len(res) != 0 {
			t.Fatalf("delete_message answered %v", res)
		}
		waitEvent(t, sh, main)
		wantCode(t, "already cancelled", sh.Call("agentsrc.delete_message", protocol.DeleteMessageParams{SessionKey: main, ClientID: clientC}, nil), protocol.CodeConflict)
		wantCode(t, "already sent", sh.Call("agentsrc.delete_message", protocol.DeleteMessageParams{SessionKey: main, ClientID: clientA}, nil), protocol.CodeConflict)
		wantCode(t, "missing session", sh.Call("agentsrc.delete_message", protocol.DeleteMessageParams{SessionKey: missing, ClientID: clientA}, nil), protocol.CodeNotFound)
		if e.Sends(clientC) != 0 {
			t.Fatal("the cancelled send went out")
		}
	})

	t.Run("search", func(t *testing.T) {
		var res protocol.SearchResult
		must(t, sh.Call("agentsrc.search", protocol.SearchParams{Q: "HELLO CLUB"}, &res))
		if len(res.Hits) < 2 {
			t.Fatalf("hits %+v", res.Hits)
		}
		for _, h := range res.Hits {
			if h.SessionKey != main || h.Title != "Echo" || h.Provider != echo.Name {
				t.Fatalf("hit %+v", h)
			}
		}
		must(t, sh.Call("agentsrc.search", protocol.SearchParams{Q: "no such words anywhere"}, &res))
		if res.Hits == nil || len(res.Hits) != 0 {
			t.Fatalf("no hits should be [] %+v", res)
		}
	})

	t.Run("unknown method", func(t *testing.T) {
		wantCode(t, "agentsrc.nope", sh.Call("agentsrc.nope", nil, nil), protocol.CodeUnsupported)
	})

	t.Run("updated lists changes", func(t *testing.T) {
		drain(sh)
		if _, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: "echo-synthetic-2"}); err != nil {
			t.Fatal(err)
		}
		for {
			ev, ok := sh.Event(5 * time.Second)
			if !ok {
				t.Fatal("no agentsrc.updated for the new account's sessions")
			}
			if ev.SessionKeys == nil {
				return
			}
		}
	})
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, what string, err error, want string) {
	t.Helper()
	if code(err) != want {
		t.Errorf("%s: got %v, want code %s", what, err, want)
	}
}

func drain(sh Shell) {
	for {
		if _, ok := sh.Event(20 * time.Millisecond); !ok {
			return
		}
	}
}

func waitEvent(t *testing.T, sh Shell, key string) {
	t.Helper()
	for {
		ev, ok := sh.Event(5 * time.Second)
		if !ok {
			t.Fatalf("no agentsrc.updated for %s", key)
		}
		if slices.Contains(ev.SessionKeys, key) {
			return
		}
	}
}
