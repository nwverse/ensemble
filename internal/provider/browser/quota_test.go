package browser

import (
	"context"
	"fmt"
	"testing"
	"time"

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
	reset5h := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	reset7d := time.Date(2026, 12, 2, 8, 0, 0, 0, time.UTC)

	drv := staticDriver(`{"value":{"usage":{"five_hour":{"utilization":85,"resets_at":"` + reset5h.Format(time.RFC3339) + `"},"seven_day":{"utilization":40,"resets_at":"` + reset7d.Format(time.RFC3339) + `"}}}}`)
	a := New(SiteClaudeDuet, drv)
	readings, err := a.ReadQuota(ctx, cred)
	if err != nil || len(readings) != 2 {
		t.Fatalf("readings = %+v, err = %v", readings, err)
	}
	if readings[0].Window != "5h" || readings[0].Remaining != 15 || readings[0].ResetsAt != reset5h.UnixMilli() || readings[0].SourceNote != "claude_duet:organizations/usage" {
		t.Fatalf("5h = %+v", readings[0])
	}
	if readings[1].Window != "week" || readings[1].Remaining != 60 || readings[1].ResetsAt != reset7d.UnixMilli() {
		t.Fatalf("week = %+v", readings[1])
	}

	// 429 propagates; unavailable returns nil, nil.
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

	// Muse and Gemini have no real-time quota endpoint and return nil without invoking the browser.
	for _, site := range []Site{SiteMuse, SiteGeminiTasks} {
		unused := staticDriver(`{"value":{"error":"should not run"}}`)
		got, err := New(site, unused).ReadQuota(ctx, cred)
		if err != nil || len(got) != 0 || len(unused.gotReqs) != 0 {
			t.Fatalf("%s: got=%+v err=%v runs=%d", site.Provider, got, err, len(unused.gotReqs))
		}
	}
}
