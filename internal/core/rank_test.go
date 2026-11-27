package core

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/store"
)

// timeless is a browser site as Muse is: its main chat's row has no date (LastActivityMs 0), its side chat's row only
// a coarse relative one read as of now ("今天": ActivityApprox), and no message carries a time of its own.
type timeless struct {
	*fake
	clk  *clock
	mu   sync.Mutex
	msgs map[string][]provider.RemoteMessage
}

func (p *timeless) Name() string { return "muse" }
func (p *timeless) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	p.clk.mu.Lock()
	now := p.clk.t.UnixMilli()
	p.clk.mu.Unlock()
	return []provider.RemoteSession{
		{ExternalID: "main", Title: "Muse", Kind: protocol.KindMain, CanSend: true},
		{ExternalID: "side", Title: "旁聊", Kind: protocol.KindSide, ParentExternalID: "main", CanSend: true,
			LastActivityMs: now, ActivityApprox: true},
	}, nil
}
func (p *timeless) ListMessages(_ context.Context, _ string, ref provider.SessionRef, _ provider.ListOptions) (provider.RemotePage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return provider.RemotePage{Messages: append([]provider.RemoteMessage(nil), p.msgs[ref.ExternalID]...)}, nil
}
func (p *timeless) add(session string, m provider.RemoteMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.msgs[session] = append(p.msgs[session], m)
}

// say is a message as a site's page gives it: in page order (Position), with no time (at 0) or its own.
func say(id, role, text string, at int64) provider.RemoteMessage {
	pos++
	return provider.RemoteMessage{ExternalID: id, Role: role, Blocks: []protocol.Block{protocol.TextBlock(text)}, CreateTimeMs: at, Position: pos}
}

var pos int64

// Syncing a site that gives no times, round after round with nothing new, leaves every conversation where it was in
// Persona's Messages page: its rankTimeMs, preview and unread count do not move (the main chat was stamped with the
// time of the read, and its messages with it, so it rose to the top at every sync). A new message, first seen or with
// its own time, moves its conversation to that time, with its preview and whose it is.
func TestIdleSyncsKeepRankAndNewMessagesMoveIt(t *testing.T) {
	clk := &clock{t: time.UnixMilli(1_800_000_000_000)}
	p := &timeless{fake: newFake(), clk: clk, msgs: map[string][]provider.RemoteMessage{
		"main": {say("u1", protocol.RoleUser, "ping", 0), say("a1", protocol.RoleAssistant, "pong", 0)},
		"side": {say("s1", protocol.RoleAssistant, "旁聊回复", 0)},
	}}
	club := New(Options{Version: "test", Registry: provider.NewRegistry(p), Now: clk.Now, FollowUps: []time.Duration{}, WatchLimit: -1})
	t.Cleanup(club.Close)
	ctx := context.Background()
	space, err := club.Space(ctx, DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "muse", Credential: "good-credential"})
	if err != nil {
		t.Fatal(err)
	}
	mainKey, sideKey := SessionKey("muse", acct.AccountID, "main"), SessionKey("muse", acct.AccountID, "side")
	list := func() map[string]protocol.Session {
		t.Helper()
		res, err := space.Sessions(ctx)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]protocol.Session{}
		for _, s := range res.Sessions {
			out[s.SessionKey] = s
		}
		return out
	}
	order := func() []string {
		t.Helper()
		res, _ := space.Sessions(ctx)
		var keys []string
		for _, s := range res.Sessions {
			keys = append(keys, s.SessionKey)
		}
		return keys
	}
	advance := func(d time.Duration) int64 {
		clk.mu.Lock()
		defer clk.mu.Unlock()
		clk.t = clk.t.Add(d)
		return clk.t.UnixMilli()
	}
	space.SyncAll(ctx)
	first := list()
	if len(first) != 2 || first[mainKey].PreviewText != "pong" || first[mainKey].PreviewIsMine || first[sideKey].PreviewText != "旁聊回复" {
		t.Fatalf("first sync: %+v", first)
	}
	for _, s := range first {
		if s.RankTimeMs <= 0 || s.AvatarURL != names.Icon("muse") || !names.HTTPS(s.AvatarURL) {
			t.Fatalf("first sync: %+v", s)
		}
	}
	firstOrder := order()

	// An hour apart, five rounds with nothing new: nothing moves, nothing is unread anew.
	for round := 1; round <= 5; round++ {
		advance(time.Hour)
		if err := space.SyncAccount(ctx, acct.AccountID); err != nil {
			t.Fatal(err)
		}
		now := list()
		for k, was := range first {
			got := now[k]
			if got.RankTimeMs != was.RankTimeMs || got.PreviewText != was.PreviewText || got.PreviewIsMine != was.PreviewIsMine ||
				got.UnreadCount != was.UnreadCount || got.AvatarURL != was.AvatarURL {
				t.Fatalf("round %d: idle %s moved: %+v, was %+v", round, k, got, was)
			}
		}
		if o := order(); o[0] != firstOrder[0] || o[1] != firstOrder[1] {
			t.Fatalf("round %d: order %v, was %v", round, o, firstOrder)
		}
	}

	// A reply without a time: ranked at the sync that first sees it, previewed, unread.
	seen := advance(time.Hour)
	p.add("main", say("a2", protocol.RoleAssistant, "新回复", 0))
	space.SyncAll(ctx)
	got := list()[mainKey]
	if got.RankTimeMs < seen || got.RankTimeMs > seen+1000 || got.PreviewText != "新回复" || got.PreviewIsMine || got.UnreadCount != 1 {
		t.Fatalf("new reply: %+v (seen at %d)", got, seen)
	}
	if o := order(); o[0] != mainKey {
		t.Fatalf("the conversation with the newest message is not first: %v", o)
	}
	// It stays put after: the next rounds do not move it again.
	for round := 1; round <= 3; round++ {
		advance(time.Hour)
		space.SyncAll(ctx)
		if again := list()[mainKey]; again.RankTimeMs != got.RankTimeMs || again.UnreadCount != 1 {
			t.Fatalf("round %d after the reply: %+v, was %+v", round, again, got)
		}
	}

	// The owner's message with its own time in the side chat: ranked at that time, mine, and first.
	sent := advance(time.Hour) - 30_000
	p.add("side", say("s2", protocol.RoleUser, "我的旁聊", sent))
	space.SyncAll(ctx)
	side := list()[sideKey]
	if side.RankTimeMs != sent || side.PreviewText != "我的旁聊" || !side.PreviewIsMine {
		t.Fatalf("side after my message: %+v", side)
	}
	if o := order(); o[0] != sideKey {
		t.Fatalf("order after my side message: %v", o)
	}
}

// A coarse list time still places a conversation the club sees for the first time.
func TestApproxActivityPlacesNewSessions(t *testing.T) {
	rows := knownRanks(
		sessionRows(store.Account{Provider: "muse", AccountID: "a1"}, []provider.RemoteSession{
			{ExternalID: "old", LastActivityMs: 5000, ActivityApprox: true},
			{ExternalID: "new", LastActivityMs: 6000, ActivityApprox: true},
			{ExternalID: "exact", LastActivityMs: 7000},
		}),
		[]provider.RemoteSession{
			{ExternalID: "old", LastActivityMs: 5000, ActivityApprox: true},
			{ExternalID: "new", LastActivityMs: 6000, ActivityApprox: true},
			{ExternalID: "exact", LastActivityMs: 7000},
		},
		sessionRows(store.Account{Provider: "muse", AccountID: "a1"}, []provider.RemoteSession{{ExternalID: "old"}, {ExternalID: "exact"}}))
	if rows[0].RankTimeMs != 0 || rows[1].RankTimeMs != 6000 || rows[2].RankTimeMs != 7000 {
		t.Fatalf("ranks %d %d %d, want 0 6000 7000", rows[0].RankTimeMs, rows[1].RankTimeMs, rows[2].RankTimeMs)
	}
}
