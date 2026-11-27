package core

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
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
	out := []provider.RemoteSession{
		{ExternalID: "main", Title: "Muse", Kind: protocol.KindMain, CanSend: true},
		{ExternalID: "side", Title: "旁聊", Kind: protocol.KindSide, ParentExternalID: "main", CanSend: true,
			LastActivityMs: now, ActivityApprox: true},
		// A chat the list gives no time either, with only the agent's greeting: none of it is unread when found.
		{ExternalID: "greet", Title: "Hello", Kind: protocol.KindSide, ParentExternalID: "main", CanSend: true},
	}
	// Enough side chats dated "今天" (read as now, so always due) to fill a sync's 20 reads: the main chat, which no
	// date tells changed, must still be read at every sync.
	for i := range 20 {
		out = append(out, provider.RemoteSession{ExternalID: fmt.Sprintf("today-%02d", i), Title: "今天", Kind: protocol.KindSide,
			ParentExternalID: "main", CanSend: true, LastActivityMs: now, ActivityApprox: true})
	}
	return out, nil
}
func (p *timeless) ListMessages(_ context.Context, _ string, ref provider.SessionRef, _ provider.ListOptions) (provider.RemotePage, error) {
	// A page takes a second to read: the club's clock moves between finding a chat and storing what it read.
	p.clk.mu.Lock()
	p.clk.t = p.clk.t.Add(time.Second)
	p.clk.mu.Unlock()
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
	return provider.RemoteMessage{ExternalID: id, Role: role, Blocks: []protocol.Block{protocol.TextBlock(text)}, CreateTimeMs: at,
		Position: sayPos.Add(1)}
}

var sayPos atomic.Int64

// Syncing a site that gives no times, round after round with nothing new, leaves every conversation where it was in
// Persona's Messages page: its rankTimeMs, preview and unread count do not move (the main chat was stamped with the
// time of the read, and its messages with it, so it rose to the top at every sync). A new message, first seen or with
// its own time, moves its conversation to that time, with its preview and whose it is.
func TestIdleSyncsKeepRankAndNewMessagesMoveIt(t *testing.T) {
	clk := &clock{t: time.UnixMilli(1_800_000_000_000)}
	p := &timeless{fake: newFake(), clk: clk, msgs: map[string][]provider.RemoteMessage{
		"main":  {say("u1", protocol.RoleUser, "ping", 0), say("a1", protocol.RoleAssistant, "pong", 0)},
		"side":  {say("s1", protocol.RoleAssistant, "旁聊回复", 0)},
		"greet": {say("g1", protocol.RoleAssistant, "你好，我是 Muse", 0), say("g2", protocol.RoleAssistant, "有什么可以帮你？", 0)},
	}}
	for i := range 20 {
		id := fmt.Sprintf("today-%02d", i)
		p.msgs[id] = []provider.RemoteMessage{say(id+"-a", protocol.RoleAssistant, "今天的回复 "+id, 0)}
	}
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
	// 23 chats, 20 reads a sync: pairing read 20 of them, a sync an hour later the rest. A chat first read in the
	// second round shows its history as of when it was found, not as news of the second round.
	second := advance(time.Hour)
	space.SyncAll(ctx)
	first := list()
	for k, s := range first {
		if s.PreviewText == "" || s.UnreadCount != 0 || s.RankTimeMs >= second {
			t.Fatalf("after the first reads %s: %+v (second round at %d)", k, s, second)
		}
	}
	if g := first[SessionKey("muse", acct.AccountID, "greet")]; g.UnreadCount != 0 || g.PreviewText != "有什么可以帮你？" {
		t.Fatalf("a chat found with only the agent's messages counts them unread: %+v", g)
	}
	if len(first) != 23 || first[mainKey].PreviewText != "pong" || first[mainKey].PreviewIsMine || first[sideKey].PreviewText != "旁聊回复" {
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
		if o := order(); !slices.Equal(o, firstOrder) {
			t.Fatalf("round %d: order %v, was %v", round, o, firstOrder)
		}
	}

	// A reply without a time: ranked at the sync that first sees it, previewed, unread.
	seen := advance(time.Hour)
	p.add("main", say("a2", protocol.RoleAssistant, "新回复", 0))
	space.SyncAll(ctx)
	got := list()[mainKey]
	if got.RankTimeMs < seen || got.RankTimeMs > seen+60_000 || got.PreviewText != "新回复" || got.PreviewIsMine || got.UnreadCount != 1 {
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

// A session whose list time moved after its last read stays due until a sync reads it, even when the sync that saw
// it move had more due than it reads: its time is then older than the account's last sync, but not than its read.
func TestMovedSessionStaysDueUntilRead(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()
	const lastSync = int64(1_900_000_000_000)
	acct := store.Account{AccountID: fx.acct.AccountID, Provider: "fake", LastSyncedAt: lastSync}
	var sessions, before []store.Session
	listed, activity := map[string]bool{}, map[string]int64{}
	for i := range 8 {
		id := fmt.Sprintf("s%d", i)
		x := store.Session{SessionKey: SessionKey("fake", acct.AccountID, id), AccountID: acct.AccountID, ExternalID: id,
			RankTimeMs: lastSync - 1_000_000}
		sessions, before = append(sessions, x), append(before, x)
		listed[id], activity[id] = true, lastSync-1_000_000
	}
	// s7 was read last (so the rotation does not pick it) at lastSync-100_000, and its list time moved after that read
	// but before the account's last sync, whose reads were all taken.
	fx.space.mu.Lock()
	for i, x := range sessions {
		fx.space.fetchedAt[x.SessionKey] = lastSync - 500_000 + int64(i)
	}
	fx.space.fetchedAt[sessions[7].SessionKey] = lastSync - 100_000
	fx.space.mu.Unlock()
	activity["s7"] = lastSync - 50_000
	due, err := fx.space.sessionsDue(ctx, acct, before, sessions, activity, listed)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(due, func(x store.Session) bool { return x.ExternalID == "s7" }) {
		t.Fatalf("a session that moved since its last read is not due: %v", due)
	}
	if len(due) != 1+rotatePerSync {
		t.Fatalf("due %d sessions, want the moved one and the rotation: %v", len(due), due)
	}
}
