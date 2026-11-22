package core

import (
	"context"
	"testing"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
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
