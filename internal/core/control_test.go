package core

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/store"
)

func TestDecide(t *testing.T) {
	type row struct {
		level      Control
		op         Operation
		unattended bool
		want       Verdict
	}
	var rows []row
	reads := []Operation{OpList, OpRead}
	writes := []Operation{OpSend, OpStart, OpInterrupt}
	for _, unattended := range []bool{false, true} {
		for _, op := range reads {
			rows = append(rows,
				row{ControlOff, op, unattended, Deny},
				row{ControlRead, op, unattended, Allow},
				row{ControlConfirm, op, unattended, Allow},
				row{ControlFollow, op, unattended, Allow},
				row{ControlAuto, op, unattended, Allow},
			)
		}
		for _, op := range writes {
			want := func(attended Verdict) Verdict {
				if unattended {
					return Deny
				}
				return attended
			}
			rows = append(rows,
				row{ControlOff, op, unattended, Deny},
				row{ControlRead, op, unattended, Deny},
				row{ControlConfirm, op, unattended, want(Confirm)},
				row{ControlFollow, op, unattended, want(Confirm)},
				row{ControlAuto, op, unattended, want(Allow)},
			)
		}
		for _, level := range []Control{ControlOff, ControlRead, ControlConfirm, ControlFollow, ControlAuto, "", "bogus"} {
			rows = append(rows, row{level, OpManage, unattended, Deny}, row{level, "bogus", unattended, Deny})
		}
		rows = append(rows, row{"", OpRead, unattended, Deny}, row{"bogus", OpRead, unattended, Deny})
	}
	for _, r := range rows {
		if got := Decide(r.level, r.op, r.unattended); got != r.want {
			t.Errorf("Decide(%q, %q, unattended=%v) = %s, want %s", r.level, r.op, r.unattended, got, r.want)
		}
	}
}

func TestDecideWithLease(t *testing.T) {
	const (
		now     = int64(1_800_000_000_000)
		key     = "agent:fake:a000000000001:main"
		other   = "agent:fake:a000000000001:other"
		acctID  = "a000000000001"
		leaseID = "fl_000000000001"
	)
	activeNoBg := &store.FollowLease{
		LeaseID:         leaseID,
		AccountID:       acctID,
		SessionKey:      key,
		CreatedAt:       now - 10_000,
		ExpiresAt:       now + 3_600_000,
		MaxTurns:        10,
		UsedTurns:       3,
		AllowBackground: false,
	}
	activeWithBg := &store.FollowLease{
		LeaseID:         leaseID,
		AccountID:       acctID,
		SessionKey:      key,
		CreatedAt:       now - 10_000,
		ExpiresAt:       now + 3_600_000,
		MaxTurns:        10,
		UsedTurns:       9,
		AllowBackground: true,
	}
	expiredLease := &store.FollowLease{
		LeaseID:         leaseID,
		AccountID:       acctID,
		SessionKey:      key,
		CreatedAt:       now - 20_000,
		ExpiresAt:       now,
		MaxTurns:        10,
		UsedTurns:       1,
		AllowBackground: true,
	}
	exhaustedLease := &store.FollowLease{
		LeaseID:         leaseID,
		AccountID:       acctID,
		SessionKey:      key,
		CreatedAt:       now - 20_000,
		ExpiresAt:       now + 3_600_000,
		MaxTurns:        10,
		UsedTurns:       10,
		AllowBackground: true,
	}
	revokedLease := &store.FollowLease{
		LeaseID:         leaseID,
		AccountID:       acctID,
		SessionKey:      key,
		CreatedAt:       now - 20_000,
		ExpiresAt:       now + 3_600_000,
		MaxTurns:        10,
		UsedTurns:       1,
		AllowBackground: true,
		RevokedAt:       now - 1000,
	}

	// OpManage is always denied at every level, even with an active background lease.
	for _, level := range []Control{ControlOff, ControlRead, ControlConfirm, ControlFollow, ControlAuto} {
		for _, unattended := range []bool{false, true} {
			for _, l := range []*store.FollowLease{nil, activeNoBg, activeWithBg} {
				if got := DecideWithLease(level, OpManage, unattended, key, l, now); got != Deny {
					t.Fatalf("OpManage allowed for level=%s unattended=%v lease=%+v: %s", level, unattended, l, got)
				}
			}
		}
	}

	// ControlFollow with matching active lease allows OpSend and OpInterrupt.
	for _, op := range []Operation{OpSend, OpInterrupt} {
		if got := DecideWithLease(ControlFollow, op, false, key, activeNoBg, now); got != Allow {
			t.Errorf("ControlFollow attended %s with activeNoBg = %s, want Allow", op, got)
		}
		if got := DecideWithLease(ControlFollow, op, true, key, activeNoBg, now); got != Deny {
			t.Errorf("ControlFollow unattended %s with activeNoBg = %s, want Deny", op, got)
		}
		if got := DecideWithLease(ControlFollow, op, true, key, activeWithBg, now); got != Allow {
			t.Errorf("ControlFollow unattended %s with activeWithBg = %s, want Allow", op, got)
		}
		// Mismatched session_key degrades to Confirm when attended, Deny when unattended.
		if got := DecideWithLease(ControlFollow, op, false, other, activeWithBg, now); got != Confirm {
			t.Errorf("ControlFollow attended %s mismatched session = %s, want Confirm", op, got)
		}
		if got := DecideWithLease(ControlFollow, op, true, other, activeWithBg, now); got != Deny {
			t.Errorf("ControlFollow unattended %s mismatched session = %s, want Deny", op, got)
		}
		// Expired, exhausted, revoked, or nil lease degrades to Confirm when attended, Deny when unattended.
		for name, bad := range map[string]*store.FollowLease{
			"nil":       nil,
			"expired":   expiredLease,
			"exhausted": exhaustedLease,
			"revoked":   revokedLease,
		} {
			if got := DecideWithLease(ControlFollow, op, false, key, bad, now); got != Confirm {
				t.Errorf("ControlFollow attended %s with %s lease = %s, want Confirm", op, name, got)
			}
			if got := DecideWithLease(ControlFollow, op, true, key, bad, now); got != Deny {
				t.Errorf("ControlFollow unattended %s with %s lease = %s, want Deny", op, name, got)
			}
		}
	}
	// OpStart has no pre-existing session lease: degrades to Confirm when attended, Deny when unattended.
	if got := DecideWithLease(ControlFollow, OpStart, false, key, activeWithBg, now); got != Confirm {
		t.Errorf("ControlFollow attended OpStart = %s, want Confirm", got)
	}
	if got := DecideWithLease(ControlFollow, OpStart, true, key, activeWithBg, now); got != Deny {
		t.Errorf("ControlFollow unattended OpStart = %s, want Deny", got)
	}
}

func TestParseControl(t *testing.T) {
	if c, err := ParseControl(""); err != nil || c != ControlOff {
		t.Fatalf("default: %q %v", c, err)
	}
	for _, s := range []string{"off", "read", "confirm", "follow", "auto"} {
		if c, err := ParseControl(s); err != nil || string(c) != s {
			t.Fatalf("%s: %q %v", s, c, err)
		}
	}
	if _, err := ParseControl("always"); err == nil {
		t.Fatal("unknown level accepted")
	}
}

func TestFollowLeaseSpaceLifecycleDowngradeAndGuards(t *testing.T) {
	fx := setup(t)
	ctx := context.Background()

	// Set account control to follow.
	follow := string(ControlFollow)
	if _, err := fx.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: fx.acct.AccountID, AssistantControl: &follow}); err != nil {
		t.Fatal(err)
	}

	// Read-only task session cannot receive a follow lease.
	if _, err := fx.space.GrantFollowLease(ctx, protocol.GrantFollowLeaseParams{SessionKey: fx.task, TTL: "2h", MaxTurns: 10}); protocol.ReasonOf(err) != protocol.ReasonSessionReadOnly {
		t.Fatalf("expected SESSION_READ_ONLY on task session, got %v", err)
	}
	// Invalid TTL or MaxTurns rejected.
	if _, err := fx.space.GrantFollowLease(ctx, protocol.GrantFollowLeaseParams{SessionKey: fx.main, TTL: "72h", MaxTurns: 10}); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("expected invalid on 72h TTL, got %v", err)
	}
	if _, err := fx.space.GrantFollowLease(ctx, protocol.GrantFollowLeaseParams{SessionKey: fx.main, TTL: "2h", MaxTurns: 100}); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("expected invalid on max_turns=100, got %v", err)
	}

	// Grant a 2h, 3-turn lease with allow_background=true.
	lease, err := fx.space.GrantFollowLease(ctx, protocol.GrantFollowLeaseParams{
		AccountID:       fx.acct.AccountID,
		SessionKey:      fx.main,
		TTL:             "2h",
		MaxTurns:        3,
		AllowBackground: true,
		Note:            "联调跟进",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Status != store.LeaseActive || lease.RemainingTurns != 3 || !lease.AllowBackground {
		t.Fatalf("unexpected lease: %+v", lease)
	}

	// Consume with idempotent requestID.
	c1, ok, err := fx.space.ConsumeFollowLease(ctx, fx.main, true, "req-1")
	if err != nil || !ok || c1.UsedTurns != 1 {
		t.Fatalf("consume req-1: %+v %v %v", c1, ok, err)
	}
	c1Dup, ok, err := fx.space.ConsumeFollowLease(ctx, fx.main, true, "req-1")
	if err != nil || !ok || c1Dup.UsedTurns != 1 {
		t.Fatalf("duplicate req-1 should not double consume: %+v %v %v", c1Dup, ok, err)
	}

	// Concurrent consumption of remaining 2 turns across 10 goroutines.
	var wg sync.WaitGroup
	var consumedCount int
	var mu sync.Mutex
	for i := range 10 {
		reqID := fmt.Sprintf("req-conc-%d", i)
		wg.Go(func() {
			if _, ok, err := fx.space.ConsumeFollowLease(ctx, fx.main, false, reqID); err == nil && ok {
				mu.Lock()
				consumedCount++
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if consumedCount != 2 {
		t.Fatalf("expected exactly 2 remaining turns consumed, got %d", consumedCount)
	}
	if _, ok, _ := fx.space.ActiveFollowLease(ctx, fx.main); ok {
		t.Fatal("lease should be exhausted after 3 turns")
	}

	// Grant another lease and verify downgrading to confirm, read, or off automatically revokes active leases.
	for _, target := range []Control{ControlConfirm, ControlRead, ControlOff} {
		if _, err := fx.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: fx.acct.AccountID, AssistantControl: &follow}); err != nil {
			t.Fatal(err)
		}
		l, err := fx.space.GrantFollowLease(ctx, protocol.GrantFollowLeaseParams{SessionKey: fx.main, TTL: "6h", MaxTurns: 10})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := fx.space.ActiveFollowLease(ctx, fx.main); !ok {
			t.Fatalf("lease %s not active before downgrade to %s", l.LeaseID, target)
		}
		lvl := string(target)
		if _, err := fx.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: fx.acct.AccountID, AssistantControl: &lvl}); err != nil {
			t.Fatal(err)
		}
		if _, ok, _ := fx.space.ActiveFollowLease(ctx, fx.main); ok {
			t.Fatalf("lease still active after downgrading account control to %s", target)
		}
	}

	// Burst rate limit guard: allows BurstMaxWrites in BurstWindowMs, blocks the 6th, and recovers after window passes.
	for i := range BurstMaxWrites {
		reqID := fmt.Sprintf("burst-%d", i)
		if ok, _ := fx.space.CheckBurstLimit(fx.main); !ok {
			t.Fatalf("check burst failed at %d", i)
		}
		if ok, _ := fx.space.RecordBurstWrite(fx.main, reqID); !ok {
			t.Fatalf("record burst failed at %d", i)
		}
	}
	// Duplicate requestID is idempotent even when at limit.
	if ok, _ := fx.space.RecordBurstWrite(fx.main, "burst-0"); !ok {
		t.Fatal("duplicate requestID should pass burst check")
	}
	if ok, wait := fx.space.CheckBurstLimit(fx.main); ok || wait <= 0 {
		t.Fatalf("expected burst limit reached, got ok=%v wait=%d", ok, wait)
	}
	if ok, wait := fx.space.RecordBurstWrite(fx.main, "burst-overflow"); ok || wait <= 0 {
		t.Fatalf("expected burst write rejected, got ok=%v wait=%d", ok, wait)
	}
	fx.clock.mu.Lock()
	fx.clock.t = fx.clock.t.Add(time.Duration(BurstWindowMs+1000) * time.Millisecond)
	fx.clock.mu.Unlock()
	if ok, _ := fx.space.RecordBurstWrite(fx.main, "burst-after-window"); !ok {
		t.Fatal("burst limit did not reset after window")
	}

	// LocalUnconfirmed guard: when a send ends up unconfirmed, HasUnconfirmedSend detects it.
	fx.fake.mu.Lock()
	fx.fake.sendFn = func(provider.Outgoing) (provider.SendResult, error) {
		return provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, nil
	}
	fx.fake.mu.Unlock()
	msg, err := fx.space.Send(ctx, protocol.SendParams{
		SessionKey: fx.main,
		ClientID:   "00000000-0000-4000-8000-000000000099",
		Text:       "unconfirmed ping",
	})
	if err != nil || msg.Local == nil || msg.Local.Status != protocol.LocalUnconfirmed {
		t.Fatalf("expected unconfirmed send, got %+v %v", msg, err)
	}
	if row, unconfirmed, err := fx.space.HasUnconfirmedSend(ctx, fx.main); err != nil || !unconfirmed || row.ClientID != "00000000-0000-4000-8000-000000000099" {
		t.Fatalf("HasUnconfirmedSend failed to detect unconfirmed send: %+v %v %v", row, unconfirmed, err)
	}

	// Audit log contains entries and never leaks credentials.
	audits := fx.space.AuditEntries()
	if len(audits) == 0 {
		t.Fatal("expected audit entries")
	}
	for _, a := range audits {
		if strings.Contains(a.Reason, "good-credential") || strings.Contains(a.Guard, "good-credential") {
			t.Fatalf("audit entry leaked credential: %+v", a)
		}
	}
}

func TestIDs(t *testing.T) {
	id := NewAccountID()
	if !ValidAccountID(id) {
		t.Fatalf("bad account id %q", id)
	}
	key := SessionKey("echo", id, "thread:1")
	p, a, e, ok := ParseSessionKey(key)
	if !ok || p != "echo" || a != id || e != "thread:1" {
		t.Fatalf("parse %q: %q %q %q %v", key, p, a, e, ok)
	}
	for _, bad := range []string{"", "agent:echo:a1", "session:echo:a1:x", "agent::a1:x"} {
		if _, _, _, ok := ParseSessionKey(bad); ok {
			t.Errorf("%q parsed", bad)
		}
	}
	for name, want := range map[string]bool{"default": true, "u-1_x": true, "": false, "../x": false, "a/b": false, "-x": false} {
		if ValidSpace(name) != want {
			t.Errorf("ValidSpace(%q) != %v", name, want)
		}
	}
}
