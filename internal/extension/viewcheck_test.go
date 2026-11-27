package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/store"
)

// The hub holds every view an extension answers to wire.CheckView and answers HTTP 502 「扩展返回的界面无法显示」
// when one does not fit. exttest.CheckView does not hold a segmented control to 12 characters a choice, so these
// tests hold the pages to wire.CheckView themselves (checkView does too), and the ring selector to 2–5 choices
// that hold its value.

var bothLocales = []lang{"zh-Hans", "en"}

// segmentedFits holds every segmented control of v to what the hub and the schema allow, beyond wire.CheckView.
func segmentedFits(t *testing.T, where string, v *wire.View) {
	t.Helper()
	if err := wire.CheckView(v); err != nil {
		raw, _ := json.Marshal(v)
		t.Fatalf("%s: wire.CheckView: %v\n%s", where, err, raw)
	}
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type != "segmented" {
				continue
			}
			if n := len(c.Choices); n < 2 || n > segmentedChoicesMost {
				t.Errorf("%s: segmented %s has %d choices: %+v", where, c.Name, n, c.Choices)
			}
			if !slices.ContainsFunc(c.Choices, func(ch wire.Choice) bool { return ch.Value == c.Value }) {
				t.Errorf("%s: segmented %s value %q is not one of %+v", where, c.Name, c.Value, c.Choices)
			}
			values, titles := map[string]bool{}, map[string]bool{}
			for _, ch := range c.Choices {
				if ch.Value == "" || strings.TrimSpace(ch.Title) == "" {
					t.Errorf("%s: segmented %s has an empty choice %+v", where, c.Name, ch)
				}
				if n := utf8.RuneCountInString(ch.Title); n > segmentedTitleMost {
					t.Errorf("%s: choice %q is %d characters", where, ch.Title, n)
				}
				if values[ch.Value] || titles[ch.Title] {
					t.Errorf("%s: segmented %s has %q / %q twice: %+v", where, c.Name, ch.Value, ch.Title, c.Choices)
				}
				values[ch.Value], titles[ch.Title] = true, true
			}
		}
	}
}

func pct(n int) *int { return &n }

func TestQuotaChoiceTitleIsShortEverywhere(t *testing.T) {
	kinds := []string{protocol.QuotaKindWindow, protocol.QuotaKindCredits, protocol.QuotaKindRequests, protocol.QuotaKindTokens,
		"", "something_new_and_long", protocol.QuotaDisplayAuto, protocol.QuotaDisplayOff, quotaChoiceCurrent}
	windows := []string{"", "minute", "hour", "3h", "5h", "day", "week", "month", "2d", "8h", "a-very-long-window-name", "七十二小时的超长窗口名"}
	pcts := []*int{nil, pct(0), pct(9), pct(91), pct(100), pct(123456789)}
	for _, l := range bothLocales {
		for _, k := range kinds {
			for _, w := range windows {
				for _, p := range pcts {
					got := quotaChoiceTitle(l, k, w, p)
					if n := utf8.RuneCountInString(got); n == 0 || n > segmentedTitleMost || !utf8.ValidString(got) || strings.TrimSpace(got) != got {
						t.Errorf("%s %q %q %v: %q (%d characters)", l, k, w, p, got, n)
					}
				}
			}
		}
	}
	for _, c := range []struct {
		l            lang
		kind, window string
		pct          *int
		want         string
	}{
		{"zh-Hans", protocol.QuotaDisplayAuto, "", nil, "自动"},
		{"en", protocol.QuotaDisplayAuto, "", nil, "Auto"},
		{"zh-Hans", protocol.QuotaDisplayOff, "", nil, "不显示"},
		{"en", protocol.QuotaDisplayOff, "", nil, "Off"},
		{"zh-Hans", quotaChoiceCurrent, "", nil, "当前项"},
		{"en", quotaChoiceCurrent, "", nil, "Current"},
		{"zh-Hans", protocol.QuotaKindWindow, "5h", pct(91), "5小时 91%"},
		{"en", protocol.QuotaKindWindow, "5h", pct(91), "5h 91%"},
		{"zh-Hans", protocol.QuotaKindWindow, "3h", pct(40), "3小时 40%"},
		{"zh-Hans", protocol.QuotaKindWindow, "hour", pct(40), "每时 40%"},
		{"en", protocol.QuotaKindWindow, "hour", pct(40), "1h 40%"},
		{"zh-Hans", protocol.QuotaKindWindow, "week", pct(80), "每周 80%"},
		{"en", protocol.QuotaKindWindow, "week", pct(80), "Week 80%"},
		{"en", protocol.QuotaKindWindow, "month", pct(100), "Month 100%"},
		{"zh-Hans", protocol.QuotaKindCredits, "week", pct(75), "每周点数 75%"},
		{"en", protocol.QuotaKindCredits, "week", pct(75), "Credit 75%"},
		{"zh-Hans", protocol.QuotaKindCredits, "", pct(50), "点数 50%"},
		{"zh-Hans", protocol.QuotaKindRequests, "day", pct(80), "每天次数 80%"},
		{"en", protocol.QuotaKindRequests, "day", pct(80), "Req/day 80%"},
		{"zh-Hans", protocol.QuotaKindTokens, "minute", pct(3), "每分字数 3%"},
		{"en", protocol.QuotaKindTokens, "minute", pct(3), "Tok/min 3%"},
		{"zh-Hans", protocol.QuotaKindWindow, "2d", pct(7), "2天 7%"},
		{"zh-Hans", "", "", nil, "额度"},
		{"en", "", "", nil, "Quota"},
		// an unexpected long window is cut, never the percent
		{"en", protocol.QuotaKindWindow, "a-very-long-window-name", pct(91), "a-very-l 91%"},
	} {
		if got := quotaChoiceTitle(c.l, c.kind, c.window, c.pct); got != c.want {
			t.Errorf("%s %q %q: %q, want %q", c.l, c.kind, c.window, got, c.want)
		}
	}
}

// dotsQuota is what ChatGPT Dots reads from wham/usage: a 5-hour window with 91% left and a weekly one.
func dotsQuota() *protocol.Quota {
	return &protocol.Quota{State: protocol.QuotaOK, Percent: pct(80), Tone: protocol.QuotaToneOK, Kind: protocol.QuotaKindWindow,
		Selected: "window:week", CheckedAt: time.Now().UnixMilli(), Source: protocol.QuotaSourceProvider, SourceNote: "dots:wham/usage",
		Buckets: []protocol.QuotaBucket{
			{ID: "window:5h", Kind: protocol.QuotaKindWindow, Window: "5h", Percent: pct(91), State: protocol.QuotaOK, Tone: protocol.QuotaToneOK},
			{ID: "window:week", Kind: protocol.QuotaKindWindow, Window: "week", Percent: pct(80), State: protocol.QuotaOK, Tone: protocol.QuotaToneOK},
		}}
}

// The ChatGPT Dots account page answered HTTP 502: its ring selector titled a choice with the long wording,
// 「每 5 小时的时段额度 91%」 (15 characters), and 「Auto (the lowest)」 (17) in English. It is the short one now, and
// the long wording stays on the bars.
func TestDotsAccountPageNoLongerRefused(t *testing.T) {
	c := clock{now: time.Now()}
	for _, l := range bothLocales {
		long := quotaWords(l, protocol.QuotaKindWindow, "5h", pct(91))
		if l == "zh-Hans" && long != "每 5 小时的时段额度 91%" {
			t.Fatalf("long wording is %q", long)
		}
		before := &wire.View{Sections: []wire.Section{{Items: []wire.Component{{
			Type: "segmented", Name: fieldQuotaDisplay, Value: protocol.QuotaDisplayAuto, Action: &wire.Action{ID: actQuotaDisplay},
			Choices: []wire.Choice{
				{Value: protocol.QuotaDisplayAuto, Title: l.t("自动（剩得最少的那项）", "Auto (the lowest)")},
				{Value: "window:5h", Title: long},
				{Value: "window:week", Title: quotaWords(l, protocol.QuotaKindWindow, "week", pct(80))},
				{Value: protocol.QuotaDisplayOff, Title: l.t("不显示", "Off")},
			}}}}}}
		if err := wire.CheckView(before); err == nil || !strings.Contains(err.Error(), "at most 12 characters") {
			t.Fatalf("%s: the old selector should be refused for its long titles, got %v", l, err)
		}

		acct := protocol.Account{AccountID: "acct-dots", Provider: "dots", ShowQuota: true, Quota: dotsQuota()}
		s := quotaDisplaySection(l, acct, c)
		v := &wire.View{Title: "ChatGPT Dots", Sections: []wire.Section{s}}
		if err := wire.CheckView(v); err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		segmentedFits(t, string(l)+" dots", v)
		raw := text(v)
		if !strings.Contains(raw, `"label":"`+long+`"`) {
			t.Errorf("%s: the bar lost its long wording %q: %s", l, long, raw)
		}
		if want := l.t("5小时 91%", "5h 91%"); !strings.Contains(raw, `"title":"`+want+`"`) {
			t.Errorf("%s: the selector has no %q: %s", l, want, raw)
		}
	}
}

func bucket(kind, window string, left int, state string) protocol.QuotaBucket {
	id := kind
	if window != "" {
		id += ":" + window
	}
	tone := map[string]string{protocol.QuotaExhausted: protocol.QuotaToneExhausted, protocol.QuotaLow: protocol.QuotaToneLow}[state]
	if tone == "" {
		tone = protocol.QuotaToneOK
	}
	r := float64(left) / 100
	return protocol.QuotaBucket{ID: id, Kind: kind, Window: window, Percent: pct(left), Remaining: &r, State: state, Tone: tone}
}

// Every service's allowance block in both languages, every state, every display choice (and one saved before that no
// bucket has now), with up to seven buckets: the hub draws all of them.
func TestQuotaSectionHoldsToCheckViewForEveryService(t *testing.T) {
	now := time.Now()
	c := clock{now: now}
	ok, low, out := protocol.QuotaOK, protocol.QuotaLow, protocol.QuotaExhausted
	W, C, R, T := protocol.QuotaKindWindow, protocol.QuotaKindCredits, protocol.QuotaKindRequests, protocol.QuotaKindTokens
	services := []struct {
		name, provider, note string
		buckets              []protocol.QuotaBucket
	}{
		{"dots 5h 91% + week", "dots", "dots:wham/usage", []protocol.QuotaBucket{bucket(W, "5h", 91, ok), bucket(W, "week", 80, ok)}},
		{"dots 5h only", "dots", "dots:wham/usage", []protocol.QuotaBucket{bucket(W, "5h", 91, ok)}},
		{"claude duet 3 buckets", "claude_duet", "claude_duet:organizations/usage",
			[]protocol.QuotaBucket{bucket(W, "5h", 15, low), bucket(W, "week", 68, ok), bucket(C, "week", 40, ok)}},
		{"gemini 5h + week", "gemini_tasks", "gemini:usage", []protocol.QuotaBucket{bucket(W, "5h", 55, ok), bucket(W, "week", 90, ok)}},
		{"grok week + month credits", grokBot, "grok_bot:GetCurrentPeriodUsage",
			[]protocol.QuotaBucket{bucket(W, "week", 0, out), bucket(C, "month", 100, ok)}},
		{"groq requests + tokens", "groq", "groq:headers", []protocol.QuotaBucket{bucket(R, "day", 12, low), bucket(T, "minute", 3, low)}},
		{"muse week + credits", providerMuse, "muse:settings/usage", []protocol.QuotaBucket{bucket(W, "week", 70, ok), bucket(C, "", 100, ok)}},
		{"four buckets", "dots", "dots:wham/usage",
			[]protocol.QuotaBucket{bucket(W, "hour", 60, ok), bucket(W, "5h", 91, ok), bucket(W, "day", 10, low), bucket(W, "week", 30, ok)}},
		{"seven buckets", "claude_duet", "claude_duet:organizations/usage", []protocol.QuotaBucket{
			bucket(W, "minute", 99, ok), bucket(W, "hour", 5, low), bucket(W, "3h", 50, ok), bucket(W, "5h", 0, out),
			bucket(W, "day", 75, ok), bucket(W, "week", 20, ok), bucket(C, "month", 45, ok)}},
		{"two buckets that read alike", "dots", "dots:wham/usage", []protocol.QuotaBucket{
			bucket(C, "week", 40, ok), bucket(C, "month", 40, ok), bucket(W, "8h", 9, low)}},
		{"the same bucket twice, and one without an id", "dots", "dots:wham/usage", []protocol.QuotaBucket{
			bucket(W, "5h", 91, ok), bucket(W, "5h", 91, ok), {Kind: W, Percent: pct(50), State: ok}, bucket(W, "week", 80, ok)}},
	}
	type state struct {
		name  string
		state string
		reset bool
		stale bool
	}
	states := []state{{"ok", ok, false, false}, {"low", low, true, false}, {"exhausted", out, true, false},
		{"exhausted no reset", out, false, false}, {"stale", ok, false, true}}
	for _, svc := range services {
		vals := []string{"", protocol.QuotaDisplayAuto, protocol.QuotaDisplayOff, "gone:bucket"}
		for _, b := range svc.buckets {
			vals = append(vals, b.ID)
		}
		for _, st := range states {
			for _, val := range vals {
				for _, l := range bothLocales {
					q := &protocol.Quota{State: st.state, Percent: svc.buckets[0].Percent, Tone: svc.buckets[0].Tone, Kind: svc.buckets[0].Kind,
						Selected: svc.buckets[0].ID, CheckedAt: now.Add(-time.Minute).UnixMilli(), Source: protocol.QuotaSourceProvider,
						SourceNote: svc.note, Stale: st.stale, Buckets: svc.buckets}
					if st.reset {
						q.ResetsAt = now.Add(2 * time.Hour).UnixMilli()
					}
					acct := protocol.Account{AccountID: "a", Provider: svc.provider, ShowQuota: val != protocol.QuotaDisplayOff, QuotaDisplay: val, Quota: q}
					where := fmt.Sprintf("%s/%s/%q/%s", svc.name, st.name, val, l)
					v := &wire.View{Sections: []wire.Section{quotaDisplaySection(l, acct, c)}}
					segmentedFits(t, where, v)
					if val == "gone:bucket" {
						i := slices.IndexFunc(v.Sections[0].Items, func(c wire.Component) bool { return c.Type == "segmented" })
						if ch := v.Sections[0].Items[i].Choices; !slices.Contains(ch, wire.Choice{Value: val, Title: l.t("当前项", "Current")}) {
							t.Errorf("%s: the choice saved before is not kept: %+v", where, ch)
						}
					}
				}
			}
		}
	}

	// With more buckets than places, Auto and Off stay, then the chosen one, then the ones with the least left.
	seven := services[8].buckets
	for _, c2 := range []struct {
		val  string
		want []string
	}{
		{protocol.QuotaDisplayAuto, []string{"auto", "window:hour", "window:5h", "window:week", "off"}},
		{"window:minute", []string{"auto", "window:minute", "window:hour", "window:5h", "off"}},
		{"gone:bucket", []string{"auto", "window:hour", "window:5h", "gone:bucket", "off"}},
	} {
		acct := protocol.Account{Provider: "claude_duet", ShowQuota: true, QuotaDisplay: c2.val,
			Quota: &protocol.Quota{State: ok, CheckedAt: now.UnixMilli(), Buckets: seven}}
		s := quotaDisplaySection("zh-Hans", acct, c)
		var got []string
		for _, it := range s.Items {
			if it.Type == "segmented" {
				for _, ch := range it.Choices {
					got = append(got, ch.Value)
				}
			}
		}
		if !slices.Equal(got, c2.want) {
			t.Errorf("val %q: choices %v, want %v", c2.val, got, c2.want)
		}
	}

	// Before the first real reading: the estimate, for every service.
	for _, p := range []string{"dots", "claude_duet", "gemini_tasks", grokBot, "groq", providerMuse, "cue", "muse2api", "echo", ""} {
		for _, l := range bothLocales {
			acct := protocol.Account{Provider: p, Usage: &protocol.UsageSummary{Sent5h: 3, Sent24h: 9}}
			segmentedFits(t, "estimated "+p+" "+string(l), &wire.View{Sections: []wire.Section{quotaDisplaySection(l, acct, c)}})
		}
	}
}

// renderPage is a page of ui in locale, held to what the hub draws.
func renderPage(t *testing.T, h *exttest.Host, ui string, l lang, st pageState) *wire.View {
	t.Helper()
	placement, size := wire.PlacementPluginPage, "full"
	if ui == HomeUI {
		placement = wire.PlacementNavPage
	}
	if ui == HomeCardUI {
		placement, size = wire.PlacementOverviewCard, wire.SizeCompact
	}
	var raw json.RawMessage
	if err := h.Call("ui.render", wire.UIParams{Contribution: ui, Placement: placement, Size: size, UIVersion: 11,
		Locale: string(l), State: stateOf(st)}, &raw); err != nil {
		t.Fatalf("ui.render %s %s %+v: %v", ui, l, st, err)
	}
	if err := exttest.CheckView(raw, ui == HomeCardUI); err != nil {
		t.Fatalf("%s %s %+v: exttest.CheckView: %v\n%s", ui, l, st, err, raw)
	}
	var v wire.View
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if _, err := wire.DecodeCheckedViewJSON(raw); err != nil {
		t.Fatalf("%s %s %+v: wire.DecodeCheckedViewJSON: %v\n%s", ui, l, st, err, raw)
	}
	segmentedFits(t, fmt.Sprintf("%s %s %+v", ui, l, st), &v)
	return &v
}

// Every page of the home and 账号, in both languages, with an account of every service holding real readings
// (ChatGPT Dots at 5h 91% + week, Claude Duet's three buckets, Grok Bot used up, Groq low), one with seven buckets,
// and again once the readings are stale.
func TestEveryPageHoldsToCheckView(t *testing.T) {
	var nowMs atomic.Int64
	nowMs.Store(time.Date(2026, 11, 27, 9, 0, 0, 0, time.UTC).UnixMilli())
	now := func() time.Time { return time.UnixMilli(nowMs.Load()) }
	in := func(d time.Duration) int64 { return now().Add(d).UnixMilli() }
	W, C, R, T := protocol.QuotaKindWindow, protocol.QuotaKindCredits, protocol.QuotaKindRequests, protocol.QuotaKindTokens
	reading := func(kind, window string, left float64, note string) provider.QuotaReading {
		return provider.QuotaReading{Kind: kind, Window: window, Limit: 100, Remaining: left, ResetsAt: in(3 * time.Hour),
			Exhausted: left <= 0, SourceNote: note}
	}
	providers := []*quotaTestProvider{
		{name: "dots", readings: []provider.QuotaReading{reading(W, "5h", 91, "dots:wham/usage"), reading(W, "week", 80, "dots:wham/usage")}},
		{name: "claude_duet", readings: []provider.QuotaReading{reading(W, "5h", 15, "claude_duet:organizations/usage"),
			reading(W, "week", 68, "claude_duet:organizations/usage"), reading(C, "week", 40, "claude_duet:organizations/usage")}},
		{name: "gemini_tasks", readings: []provider.QuotaReading{reading(W, "5h", 55, "gemini:usage"), reading(W, "week", 90, "gemini:usage")}},
		{name: grokBot, readings: []provider.QuotaReading{reading(W, "week", 0, "grok_bot:GetSandUsageStatus"),
			reading(C, "month", 100, "grok_bot:GetCurrentPeriodUsage")}},
		{name: "groq", readings: []provider.QuotaReading{reading(R, "day", 12, "groq:headers"), reading(T, "minute", 3, "groq:headers")}},
		{name: providerMuse, readings: []provider.QuotaReading{reading(W, "week", 70, "muse:settings/usage"), {Kind: C, Limit: 20, Remaining: 20, SourceNote: "muse:settings/usage"}}},
		{name: "cue"}, // nothing read: the estimate
		{name: "muse2api"},
		{name: "overflow", readings: []provider.QuotaReading{reading(W, "minute", 99, ""), reading(W, "hour", 5, ""), reading(W, "3h", 50, ""),
			reading(W, "5h", 0, ""), reading(W, "day", 75, ""), reading(W, "week", 20, ""), reading(C, "month", 45, "")}},
	}
	var adapters []provider.Adapter
	for _, p := range providers {
		adapters = append(adapters, p)
	}
	club := core.New(core.Options{Registry: provider.NewRegistry(adapters...), FollowUps: []time.Duration{}, WatchLimit: -1, Now: now})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true,"browser_providers":true,"inbox_conversations":true,"assistant_runtime":true}`))
	h.Answer("browser.machines", func(json.RawMessage) (any, error) {
		return wire.BrowserMachinesResult{Machines: []wire.BrowserMachine{
			{MachineID: "mac", Name: "MacBook", Backend: "ego", Online: true, Ready: true}}}, nil
	})
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)

	pages := func(ids []string) []pageState {
		sts := []pageState{{}, {Page: pageAdd}, {Page: pageGrok}, {Page: pagePair, Provider: "cue"}, {Page: pagePair, Provider: "groq"},
			{Page: pagePair, Provider: "muse2api"}, {Page: pagePair, Provider: "echo"}, {Page: pageBrowserPair, Provider: "dots"},
			{Note: "一条很长的提示，" + strings.Repeat("说明", 30), NoteTone: wire.ToneCaution}}
		for _, id := range ids {
			sts = append(sts, pageState{Page: pageAccount, Account: id}, pageState{Page: pageTerminal, Account: id},
				pageState{Page: pageRepos, Account: id})
		}
		return sts
	}
	sweep := func(when string, ids []string) {
		t.Helper()
		for _, l := range bothLocales {
			renderPage(t, h, HomeCardUI, l, pageState{})
			for _, ui := range []string{HomeUI, AccountsUI} {
				for _, st := range pages(ids) {
					v := renderPage(t, h, ui, l, st)
					if st.Page == pageAccount && len(v.Sections) == 0 {
						t.Errorf("%s: %s %s account page is empty", when, ui, l)
					}
				}
			}
		}
	}

	sweep("no accounts", nil)
	var ids []string
	for _, p := range providers {
		a, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: p.name, Credential: "fixture-credential-" + p.name})
		if err != nil {
			t.Fatalf("%s: %v", p.name, err)
		}
		ids = append(ids, a.AccountID)
	}
	dots := renderPage(t, h, HomeUI, "zh-Hans", pageState{Page: pageAccount, Account: ids[0]})
	if !strings.Contains(text(dots), `"title":"5小时 91%"`) || !strings.Contains(text(dots), "每 5 小时的时段额度 91%") {
		t.Errorf("dots page has not read 5h 91%%: %s", text(dots))
	}
	sweep("real readings", ids)

	// every display choice saved, among them one that falls off the seven-bucket selector
	for i, p := range providers {
		for _, r := range p.readings {
			id := r.Kind
			if r.Window != "" {
				id += ":" + r.Window
			}
			act(t, h, pageState{Page: pageAccount, Account: ids[i]}, actQuotaDisplay, map[string]string{fieldQuotaDisplay: id})
			for _, l := range bothLocales {
				renderPage(t, h, HomeUI, l, pageState{Page: pageAccount, Account: ids[i]})
			}
		}
		act(t, h, pageState{Page: pageAccount, Account: ids[i]}, actQuotaDisplay, map[string]string{fieldQuotaDisplay: protocol.QuotaDisplayOff})
	}
	sweep("display chosen", ids)

	nowMs.Add((20 * time.Minute).Milliseconds())
	sweep("stale", ids)

	// a level a newer version wrote: the 助手指挥 selector shows off, not a value none of its choices has
	newer := "some_newer_level"
	if err := space.Store().UpdateAccount(ctx, ids[0], store.AccountPatch{AssistantControl: &newer}, 1); err != nil {
		t.Fatal(err)
	}
	for _, l := range bothLocales {
		for _, ui := range []string{HomeUI, AccountsUI} {
			renderPage(t, h, ui, l, pageState{Page: pageAccount, Account: ids[0]})
		}
	}
}

// The 仓库 page's list of granted repositories stays within a view's strings however many an installation has (an
// "All repositories" grant on a large org), and its links are what the hub draws.
func TestReposListsAndLinksFitTheHub(t *testing.T) {
	for _, l := range bothLocales {
		var names []string
		var repos []repoStatus
		for i := range 400 {
			names = append(names, fmt.Sprintf("nwverse/a-rather-long-repository-name-%03d", i))
			repos = append(repos, repoStatus{URL: fmt.Sprintf("https://github.com/nwverse/repo-%03d", i), Authorized: i%2 == 0})
		}
		for _, n := range []int{0, 1, 2, 40, 400} {
			got := joinFit(l, names[:n], wire.StringMost)
			if utf8.RuneCountInString(got) > wire.StringMost {
				t.Errorf("%s %d names: %d characters", l, n, utf8.RuneCountInString(got))
			}
			if n == 400 && !strings.Contains(got, l.t("还有", "more")) {
				t.Errorf("%s: the rest is not counted: …%s", l, got[len(got)-40:])
			}
		}
		if got := joinFit(l, []string{strings.Repeat("x", 3000)}, wire.StringMost); utf8.RuneCountInString(got) > wire.StringMost {
			t.Errorf("one long name: %d characters", utf8.RuneCountInString(got))
		}
		var inv provider.RepoInventory
		for _, r := range repos {
			inv.Authorized = append(inv.Authorized, provider.Repo{URL: r.URL})
		}
		sections := []wire.Section{grantSection(l, inv)}
		sections = append(sections, outcomeSections(l, repoOutcome{Channel: "#c", State: repoNeedsGitHub, Repos: repos[:20], // a mount names at most 20
			Guide: &repoGuide{Why: "why", Repos: names[:20], URL: "https://github.com/settings/installations/1"}})...)
		segmentedFits(t, "repos "+string(l), &wire.View{Sections: sections})
	}
	for _, u := range []string{"https://user@github.com/x", "https://:443/x", "HTTPS://github.com/x", "http://github.com/x", "", "github.com"} {
		if httpsURL(u) {
			t.Errorf("httpsURL(%q) let through a link the hub refuses", u)
		}
	}
	if !httpsURL("https://github.com/apps/claude/installations/new") {
		t.Error("a GitHub link is refused")
	}
}
