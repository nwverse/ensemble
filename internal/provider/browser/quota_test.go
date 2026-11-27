package browser

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

func staticDriver(output string) *fakeDriver {
	return &fakeDriver{
		name: "ego",
		answer: func(RunRequest) (RunResult, error) {
			return RunResult{Backend: "ego", Output: output}, nil
		},
	}
}

func TestClaudeDuetReadQuota(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})
	reset5h := time.Date(2026, 11, 27, 10, 30, 0, 0, time.UTC)
	reset7d := time.Date(2026, 11, 28, 1, 0, 0, 0, time.UTC)

	// Five-hour + seven-day + maple_sundial (Claude Duet weekly credit $190.87 / $500).
	drv := staticDriver(`{"value":{"usage":{"five_hour":{"utilization":10,"resets_at":"2026-11-27T10:30:00Z"},"seven_day":{"utilization":73,"resets_at":"2026-11-28T01:00:00Z"},"maple_sundial":{"utilization":61.8262876,"resets_at":"2026-11-28T01:00:00Z","limit_dollars":500,"used_dollars":309.131438,"remaining_dollars":190.868562,"state":"available"}}}}`)
	a := New(SiteClaudeDuet, drv)
	readings, err := a.ReadQuota(ctx, cred)
	if err != nil || len(readings) != 3 {
		t.Fatalf("readings = %+v, err = %v", readings, err)
	}
	if readings[0].Window != "5h" || readings[0].Remaining != 90 || readings[0].Exhausted || readings[0].ResetsAt != reset5h.UnixMilli() {
		t.Fatalf("five_hour = %+v", readings[0])
	}
	if readings[1].Window != "week" || readings[1].Remaining != 27 || readings[1].Exhausted || readings[1].ResetsAt != reset7d.UnixMilli() {
		t.Fatalf("seven_day = %+v", readings[1])
	}
	if readings[2].Kind != protocol.QuotaKindCredits || readings[2].Window != "week" || readings[2].Limit != 500 ||
		math.Abs(readings[2].Remaining-190.868562) > 0.001 || readings[2].Exhausted || readings[2].ResetsAt != reset7d.UnixMilli() {
		t.Fatalf("maple_sundial = %+v", readings[2])
	}

	// Depleted maple_sundial ($0 left) must NOT emit a blocking 0% bucket while plan limits still have room.
	drvFallback := staticDriver(`{"value":{"usage":{"five_hour":{"utilization":20,"resets_at":"2026-11-27T10:30:00Z"},"seven_day":{"utilization":40,"resets_at":"2026-11-28T01:00:00Z"},"maple_sundial":{"utilization":100,"resets_at":"2026-11-28T01:00:00Z","limit_dollars":500,"remaining_dollars":0}}}}`)
	fb, err := New(SiteClaudeDuet, drvFallback).ReadQuota(ctx, cred)
	if err != nil || len(fb) != 2 {
		t.Fatalf("depleted maple_sundial should fall back to plan limits: got=%+v err=%v", fb, err)
	}

	drv429 := staticDriver(`{"value":{"error":"http 429"}}`)
	if _, err := New(SiteClaudeDuet, drv429).ReadQuota(ctx, cred); provider.CodeOf(err) != provider.CodeRateLimited {
		t.Fatalf("429 err = %v", err)
	}
	drvUnavail := staticDriver(`{"value":{"unavailable":true}}`)
	if got, err := New(SiteClaudeDuet, drvUnavail).ReadQuota(ctx, cred); err != nil || len(got) != 0 {
		t.Fatalf("unavailable got=%+v err=%v", got, err)
	}
}

func TestDotsReadQuota(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})
	reset5h := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	reset7d := time.Date(2026, 12, 2, 8, 0, 0, 0, time.UTC)

	drv := staticDriver(fmt.Sprintf(`{"value":{"rate_limit":{"allowed":false,"limit_reached":true,"primary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":35,"limit_window_seconds":604800,"reset_at":%d}}}}`, reset5h.Unix(), reset7d.Unix()))
	a := New(SiteDots, drv)
	readings, err := a.ReadQuota(ctx, cred)
	if err != nil || len(readings) != 2 {
		t.Fatalf("readings = %+v, err = %v", readings, err)
	}
	if readings[0].Window != "5h" || readings[0].Remaining != 0 || !readings[0].Exhausted || readings[0].ResetsAt != reset5h.UnixMilli() {
		t.Fatalf("primary = %+v (want reset %d)", readings[0], reset5h.UnixMilli())
	}
	if readings[1].Window != "week" || readings[1].Remaining != 65 || readings[1].Exhausted || readings[1].ResetsAt != reset7d.UnixMilli() {
		t.Fatalf("secondary = %+v (want reset %d)", readings[1], reset7d.UnixMilli())
	}
}

func TestMuseReadQuota(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})
	resetSec := int64(1796110698)

	// Server action payload with weekly usage (4% used) + extra top-up credits.
	drv := staticDriver(fmt.Sprintf(`{"value":{"source":"action","percentUsed":4,"resetsAt":%d,"quotaStatus":"SUFFICIENT","topupBalance":40000000,"topupTotal":40000000}}`, resetSec))
	readings, err := New(SiteMuse, drv).ReadQuota(ctx, cred)
	if err != nil || len(readings) != 2 {
		t.Fatalf("muse action readings = %+v, err = %v", readings, err)
	}
	if readings[0].Kind != protocol.QuotaKindWindow || readings[0].Window != "week" || readings[0].Remaining != 96 ||
		readings[0].ResetsAt != resetSec*1000 || readings[0].Exhausted || readings[0].SourceNote != "muse:settings/usage" {
		t.Fatalf("muse weekly = %+v", readings[0])
	}
	if readings[1].Kind != protocol.QuotaKindCredits || readings[1].Limit != 40000000 || readings[1].Remaining != 40000000 {
		t.Fatalf("muse topup = %+v", readings[1])
	}

	// Exhausted with no top-up marks exhausted.
	drvEx := staticDriver(fmt.Sprintf(`{"value":{"source":"action","percentUsed":100,"resetsAt":%d,"quotaStatus":"EXHAUSTED","topupBalance":0,"topupTotal":0}}`, resetSec))
	ex, err := New(SiteMuse, drvEx).ReadQuota(ctx, cred)
	if err != nil || len(ex) != 1 || !ex[0].Exhausted || ex[0].Remaining != 0 {
		t.Fatalf("muse exhausted = %+v, err = %v", ex, err)
	}

	// DOM fallback parses Chinese Settings -> General -> Usage text cleanly.
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	domReadings := parseMuseQuota("dom", nil, 0, "", 0, 0, "使用情况\n免费版\n周限额重置时间：12月1日\n已使用 18%\n", now)
	if len(domReadings) != 1 || domReadings[0].Remaining != 82 || domReadings[0].ResetsAt == 0 {
		t.Fatalf("muse dom fallback = %+v", domReadings)
	}
}

func TestGeminiReadQuota(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})
	reset5h := int64(1795772160000)
	reset7d := int64(1796268960000)

	drv := staticDriver(fmt.Sprintf(`{"value":{"current":{"used":8,"resetsAt":%d},"weekly":{"used":1,"resetsAt":%d}}}`, reset5h, reset7d))
	readings, err := New(SiteGeminiTasks, drv).ReadQuota(ctx, cred)
	if err != nil || len(readings) != 2 {
		t.Fatalf("gemini readings = %+v, err = %v", readings, err)
	}
	if readings[0].Window != "5h" || readings[0].Remaining != 92 || readings[0].ResetsAt != reset5h || readings[0].SourceNote != "gemini:usage" {
		t.Fatalf("gemini current = %+v", readings[0])
	}
	if readings[1].Window != "week" || readings[1].Remaining != 99 || readings[1].ResetsAt != reset7d || readings[1].SourceNote != "gemini:usage" {
		t.Fatalf("gemini weekly = %+v", readings[1])
	}

	drvEx := staticDriver(fmt.Sprintf(`{"value":{"current":{"used":100,"resetsAt":%d}}}`, reset5h))
	ex, err := New(SiteGeminiTasks, drvEx).ReadQuota(ctx, cred)
	if err != nil || len(ex) != 1 || !ex[0].Exhausted || ex[0].Remaining != 0 {
		t.Fatalf("gemini exhausted = %+v, err = %v", ex, err)
	}
}
