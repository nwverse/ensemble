package extension

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

// Times read like the owner's other extensions: relative within 7 days, then month, day and time, the year only when
// it is not this year's; English the same in its words.
func TestTimesReadRelativeThenAsADate(t *testing.T) {
	shanghai := time.FixedZone("UTC+8", 8*3600)
	now := time.Date(2026, 11, 26, 12, 4, 0, 0, shanghai)
	c := clock{now: now, loc: shanghai}
	for _, x := range []struct {
		at     time.Time
		zh, en string
	}{
		{now, "刚刚", "just now"},
		{now.Add(-59 * time.Second), "刚刚", "just now"},
		{now.Add(-time.Minute), "1 分钟前", "1 minute ago"},
		{now.Add(-5 * time.Minute), "5 分钟前", "5 minutes ago"},
		{now.Add(-3*time.Hour - 20*time.Minute), "3 小时前", "3 hours ago"},
		{now.Add(-3 * 24 * time.Hour), "3 天前", "3 days ago"},
		{now.Add(-7*24*time.Hour + time.Minute), "6 天前", "6 days ago"},
		{time.Date(2026, 11, 14, 12, 4, 0, 0, shanghai), "11月14日 12:04", "Nov 14 12:04"},
		{time.Date(2026, 3, 2, 8, 30, 0, 0, shanghai), "3月2日 08:30", "Mar 2 08:30"},
		{time.Date(2025, 12, 30, 9, 0, 0, 0, shanghai), "2025年12月30日 09:00", "Dec 30, 2025 09:00"},
		// in the viewer's zone: 20:00 UTC on Nov 13 is the 14th in UTC+8
		{time.Date(2026, 11, 13, 20, 0, 0, 0, time.UTC), "11月14日 04:00", "Nov 14 04:00"},
		// ahead (an allowance that comes back)
		{now.Add(30 * time.Second), "马上", "in a moment"},
		{now.Add(5 * time.Minute), "5 分钟后", "in 5 minutes"},
		{now.Add(2 * time.Hour), "2 小时后", "in 2 hours"},
		{now.Add(10 * 24 * time.Hour), "12月6日 12:04", "Dec 6 12:04"},
	} {
		if got := c.at("zh-Hans", x.at.UnixMilli()); got != x.zh {
			t.Errorf("zh %v: %q, want %q", x.at, got, x.zh)
		}
		if got := c.at("en", x.at.UnixMilli()); got != x.en {
			t.Errorf("en %v: %q, want %q", x.at, got, x.en)
		}
	}
	if got := c.lastSynced("zh-Hans", 0); got != "还没同步过" {
		t.Errorf("never synced: %q", got)
	}
}

// The pages write their times against the club's clock: the list and the account page say when an account last
// synced the one way, and a used-up allowance says when it comes back the same way.
func TestPagesWriteTimesAgainstTheClubsClock(t *testing.T) {
	var mu sync.Mutex
	now := time.Date(2026, 11, 26, 12, 4, 0, 0, time.UTC)
	clockNow := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	set := func(t time.Time) { mu.Lock(); now = t; mu.Unlock() }
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}, Now: clockNow})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential, fieldName: "Wren"})
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)
	status, _ := space.Status(ctx)
	acct := status.Accounts[0]
	if acct.LastSyncedAt != now.UnixMilli() {
		t.Fatalf("the demo did not sync at the club's now: %+v", acct)
	}
	subtitle := func() string {
		for _, s := range render(t, h, pageState{}).Sections {
			for _, c := range s.Items {
				if c.Key == "acct-"+acct.AccountID {
					return c.Subtitle
				}
			}
		}
		t.Fatal("no row for the account")
		return ""
	}
	synced := func() string {
		for _, s := range render(t, h, pageState{Page: pageAccount, Account: acct.AccountID}).Sections {
			for _, c := range s.Items {
				if c.Type == "row" && c.Title == "上次同步" {
					return c.Value
				}
			}
		}
		t.Fatal("no last sync on the account page")
		return ""
	}
	for _, x := range []struct {
		at   time.Time
		want string
	}{
		{now.Add(20 * time.Second), "刚刚"},
		{now.Add(3 * time.Hour), "3 小时前"},
		{now.Add(12 * 24 * time.Hour), "11月26日 12:04"},
		{time.Date(2027, 1, 5, 0, 0, 0, 0, time.UTC), "2026年11月26日 12:04"},
	} {
		set(x.at)
		if got := subtitle(); !strings.Contains(got, " · 上次同步："+x.want+" · ") {
			t.Errorf("at %v the list says %q, want 上次同步：%s", x.at, got, x.want)
		}
		if got := synced(); got != x.want {
			t.Errorf("at %v the account page says %q, want %q", x.at, got, x.want)
		}
	}

	// a used-up allowance comes back "in" a while, or on a date
	set(time.Date(2026, 11, 26, 12, 4, 0, 0, time.UTC))
	for _, x := range []struct {
		back time.Time
		want string
	}{
		{now.Add(3 * time.Hour), "用完了，3 小时后恢复"},
		{now.Add(10 * 24 * time.Hour), "用完了，12月6日 12:04 恢复"},
	} {
		c := clock{now: now, loc: time.UTC}
		s := quotaDisplaySection("zh-Hans", protocol.Account{Quota: &protocol.Quota{State: protocol.QuotaExhausted,
			ResetsAt: x.back.UnixMilli()}}, c)
		var got string
		for _, it := range s.Items {
			if it.Type == "row" && it.Title == "现在还剩" {
				got = it.Value
			}
		}
		if got != x.want {
			t.Errorf("used up, back at %v: %q, want %q", x.back, got, x.want)
		}
	}
}
