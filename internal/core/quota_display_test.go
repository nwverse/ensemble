package core

import (
	"context"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

func TestQuotaBucketsAndDisplaySelection(t *testing.T) {
	now := int64(1_794_500_000_000)
	q := &quotaState{
		checkedAt: now - 1000,
		readings: []provider.QuotaReading{
			{Kind: protocol.QuotaKindTokens, Window: "5h", Remaining: 800, Limit: 1000, ResetsAt: now + quotaStaleMs + 60_000},
			{Kind: protocol.QuotaKindRequests, Window: "day", Remaining: 15, Limit: 100, ResetsAt: now + quotaStaleMs + 120_000},
		},
	}

	// Auto selects the tightest bucket (requests:day, 15%, low).
	autoQ := buildQuota(q, true, protocol.QuotaDisplayAuto, now)
	if autoQ == nil || autoQ.Selected != "requests:day" || autoQ.Percent == nil || *autoQ.Percent != 15 || autoQ.Tone != protocol.QuotaToneLow || !autoQ.Show {
		t.Fatalf("auto quota = %+v", autoQ)
	}
	if len(autoQ.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want 2", autoQ.Buckets)
	}

	// Selecting tokens:5h switches top-level display to 80% / ok while keeping both buckets.
	tokQ := buildQuota(q, true, "tokens:5h", now)
	if tokQ == nil || tokQ.Selected != "tokens:5h" || tokQ.Percent == nil || *tokQ.Percent != 80 || tokQ.Tone != protocol.QuotaToneOK || !tokQ.Show {
		t.Fatalf("tokens:5h quota = %+v", tokQ)
	}
	if len(tokQ.Buckets) != 2 {
		t.Fatalf("buckets = %+v, want 2", tokQ.Buckets)
	}

	// Selecting by kind ("tokens") also matches tokens:5h.
	byKind := buildQuota(q, true, protocol.QuotaKindTokens, now)
	if byKind == nil || byKind.Selected != "tokens:5h" {
		t.Fatalf("byKind quota = %+v", byKind)
	}

	// Selecting "off" hides the indicator.
	offQ := buildQuota(q, true, protocol.QuotaDisplayOff, now)
	if offQ == nil || offQ.Show {
		t.Fatalf("off quota = %+v, want Show=false", offQ)
	}

	// Stale reading sets tone to stale.
	staleQ := buildQuota(q, true, "tokens:5h", now+quotaStaleMs+10_000)
	if staleQ == nil || !staleQ.Stale || staleQ.Tone != protocol.QuotaToneStale {
		t.Fatalf("stale quota = %+v", staleQ)
	}

	// Round-trip via Space.UpdateAccount persists quota_display.
	e := newEnv(t, "echo")
	ctx := context.Background()
	a, err := e.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "echo", Credential: "echo-quota"})
	if err != nil {
		t.Fatal(err)
	}
	sel := "tokens:5h"
	updated, err := e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, QuotaDisplay: &sel})
	if err != nil {
		t.Fatal(err)
	}
	if updated.QuotaDisplay != sel || !updated.ShowQuota {
		t.Fatalf("updated account = %+v", updated)
	}
	off := protocol.QuotaDisplayOff
	updated, err = e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, QuotaDisplay: &off})
	if err != nil {
		t.Fatal(err)
	}
	if updated.QuotaDisplay != off || updated.ShowQuota {
		t.Fatalf("updated off account = %+v", updated)
	}
}

type quotaReaderProvider struct {
	*scripted
	calls    int
	readings []provider.QuotaReading
	err      error
}

func (q *quotaReaderProvider) ReadQuota(context.Context, string) ([]provider.QuotaReading, error) {
	q.calls++
	return q.readings, q.err
}

func TestQuotaReaderSyncThrottlePreSendBlockAndUsageSummary(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, "grok_bot")
	e.p.sessions = []provider.RemoteSession{{ExternalID: "bot/", CanSend: true}}
	resetAt := e.club.Now().Add(2 * time.Hour).UnixMilli()
	qr := &quotaReaderProvider{
		scripted: e.p,
		readings: []provider.QuotaReading{{
			Kind: protocol.QuotaKindWindow, Window: "week", Limit: 100, Remaining: 0,
			ResetsAt: resetAt, Exhausted: true, SourceNote: "grok_bot:GetSandUsageStatus",
		}},
	}
	e.club.Registry().Register(qr)

	a, err := e.space.AddAccount(ctx, protocol.AddAccountParams{Provider: "grok_bot", Credential: "fixture-credential-1"})
	if err != nil {
		t.Fatal(err)
	}
	if qr.calls != 1 || a.Quota == nil || a.Quota.State != protocol.QuotaExhausted || a.Quota.ResetsAt != resetAt {
		t.Fatalf("after pair: calls=%d quota=%+v", qr.calls, a.Quota)
	}
	// Periodic SyncAll within 5 minutes does not re-poll ReadQuota.
	e.space.SyncAll(ctx)
	if qr.calls != 1 {
		t.Fatalf("periodic sync should throttle ReadQuota: calls=%d", qr.calls)
	}
	// Sending while exhausted is refused before reaching the provider with a clear reset message.
	key := SessionKey("grok_bot", a.AccountID, "bot/")
	sentToProvider := 0
	e.p.send = func(string, provider.Outgoing) (provider.SendResult, error) {
		sentToProvider++
		return provider.SendResult{Status: provider.Sent}, nil
	}
	_, err = e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(1), Text: "hi"})
	if protocol.CodeOf(err) != protocol.CodeConflict || protocol.ReasonOf(err) != protocol.ReasonQuotaExhausted || sentToProvider != 0 {
		t.Fatalf("exhausted send: err=%v sentToProvider=%d", err, sentToProvider)
	}
	// Manual SyncAccount forces a fresh ReadQuota and unlocks sending once restored.
	qr.readings = []provider.QuotaReading{{
		Kind: protocol.QuotaKindWindow, Window: "week", Limit: 100, Remaining: 80,
		ResetsAt: resetAt, SourceNote: "grok_bot:GetSandUsageStatus",
	}}
	if err := e.space.SyncAccount(ctx, a.AccountID); err != nil {
		t.Fatal(err)
	}
	if qr.calls != 2 {
		t.Fatalf("manual sync should force ReadQuota: calls=%d", qr.calls)
	}
	if _, err := e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(2), Text: "hi again"}); err != nil {
		t.Fatalf("send after recovery: %v", err)
	}
	if sentToProvider != 1 {
		t.Fatalf("sentToProvider = %d, want 1", sentToProvider)
	}
	got, err := e.space.Account(ctx, a.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Usage == nil || got.Usage.Sent5h != 1 || got.Usage.Sent24h != 1 || got.Usage.Source != protocol.QuotaSourceEstimated {
		t.Fatalf("usage summary = %+v", got.Usage)
	}

	// Regression: pinning a healthy bucket (window:week) must not bypass QUOTA_EXHAUSTED when window:5h is exhausted,
	// while retrying an already-enqueued client_id remains idempotent.
	qr.readings = []provider.QuotaReading{
		{Kind: protocol.QuotaKindWindow, Window: "5h", Limit: 100, Remaining: 0, Exhausted: true, ResetsAt: resetAt},
		{Kind: protocol.QuotaKindWindow, Window: "week", Limit: 100, Remaining: 70, ResetsAt: resetAt + int64(24*time.Hour/time.Millisecond)},
	}
	if err := e.space.SyncAccount(ctx, a.AccountID); err != nil {
		t.Fatal(err)
	}
	week := "window:week"
	if _, err := e.space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: a.AccountID, QuotaDisplay: &week}); err != nil {
		t.Fatal(err)
	}
	// Idempotent retry of uuid(2) returns the existing sent message instead of 409 QUOTA_EXHAUSTED.
	if msg, err := e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(2), Text: "hi again"}); err != nil || msg.Local == nil || msg.Local.ClientID != uuid(2) {
		t.Fatalf("idempotent retry while exhausted: msg=%+v err=%v", msg, err)
	}
	// A fresh send is still blocked despite the healthy pinned bucket.
	if _, err := e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(3), Text: "blocked"}); protocol.ReasonOf(err) != protocol.ReasonQuotaExhausted {
		t.Fatalf("pinned healthy bucket bypassed exhaustion: err=%v", err)
	}
	// Manual sync with empty ReadQuota clears exhaustion so the user can unblock after resetting externally.
	qr.readings = nil
	if err := e.space.SyncAccount(ctx, a.AccountID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.space.Send(ctx, protocol.SendParams{SessionKey: key, ClientID: uuid(4), Text: "unblocked"}); err != nil {
		t.Fatalf("send after manual sync clear: %v", err)
	}
}
