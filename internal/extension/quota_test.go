package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

type quotaTestProvider struct {
	name     string
	readings []provider.QuotaReading
}

func (p *quotaTestProvider) Name() string       { return p.name }
func (p *quotaTestProvider) Connection() string { return protocol.ConnectionServer }
func (p *quotaTestProvider) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: p.name + "-1"}, nil
}
func (p *quotaTestProvider) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "main", Title: "Main", Kind: protocol.KindMain, CanSend: true}}, nil
}
func (p *quotaTestProvider) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}
func (p *quotaTestProvider) Send(context.Context, string, provider.SessionRef, provider.Outgoing, provider.SendContext) (provider.SendResult, error) {
	return provider.SendResult{Status: provider.Sent}, nil
}
func (p *quotaTestProvider) ReadQuota(context.Context, string) ([]provider.QuotaReading, error) {
	return p.readings, nil
}

func TestQuotaPagesRealAndEstimatedAndNoJargon(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	resetIn2h := now.Add(2 * time.Hour).UnixMilli()
	resetIn5d := now.Add(5 * 24 * time.Hour).UnixMilli()

	grok := &quotaTestProvider{name: grokBot, readings: []provider.QuotaReading{
		{Kind: protocol.QuotaKindWindow, Window: "week", Limit: 100, Remaining: 0, ResetsAt: resetIn2h, Exhausted: true, SourceNote: "grok_bot:GetSandUsageStatus"},
	}}
	duet := &quotaTestProvider{name: "claude_duet", readings: []provider.QuotaReading{
		{Kind: protocol.QuotaKindWindow, Window: "5h", Limit: 100, Remaining: 15, ResetsAt: resetIn2h, SourceNote: "claude_duet:organizations/usage"},
		{Kind: protocol.QuotaKindWindow, Window: "week", Limit: 100, Remaining: 68, ResetsAt: resetIn5d, SourceNote: "claude_duet:organizations/usage"},
	}}
	muse := &quotaTestProvider{name: providerMuse} // no real quota -> local estimation

	club := core.New(core.Options{
		Registry:  provider.NewRegistry(grok, duet, muse),
		FollowUps: []time.Duration{},
		Now:       func() time.Time { return now },
	})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), nil)
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)

	grokAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: "fixture-grok-cred"})
	if err != nil {
		t.Fatal(err)
	}
	duetAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "claude_duet", Credential: "fixture-duet-cred"})
	if err != nil {
		t.Fatal(err)
	}
	museAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: providerMuse, Credential: "fixture-muse-cred"})
	if err != nil {
		t.Fatal(err)
	}
	// Send one message on Muse so local estimation shows 5h/24h counts.
	museKey := core.SessionKey(providerMuse, museAcct.AccountID, "main")
	if _, err := space.Send(ctx, protocol.SendParams{SessionKey: museKey, ClientID: "11111111-1111-4111-8111-111111111111", Text: "hello"}); err != nil {
		t.Fatal(err)
	}

	// Home page: shows exhausted + low counts, ranks exhausted first, and has zero jargon.
	home := renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{})
	noJargon(t, "home quota", home)
	homeText := text(home)
	for _, want := range []string{"1 个额度用完", "1 个额度快用完", "额度用完了（2 小时后恢复）", "额度快用完（剩 15%）", "近 24 小时已发 1 条（估算）"} {
		if !strings.Contains(homeText, want) {
			t.Errorf("home lacks %q:\n%s", want, homeText)
		}
	}

	// Overview card: surfaces exhausted account cleanly within card limits.
	card := renderCard(t, h)
	if !strings.Contains(text(card), "额度用完") {
		t.Errorf("card lacks exhausted indicator: %s", text(card))
	}

	// Accounts list: displays remaining quota, reset time, and estimation.
	list := renderUI(t, h, AccountsUI, wire.PlacementPluginPage, pageState{})
	noJargon(t, "accounts quota", list)
	listText := text(list)
	for _, want := range []string{"额度已用完，2 小时后恢复", "额度仅剩 15%", "用量估算：近 24 小时已发 1 条"} {
		if !strings.Contains(listText, want) {
			t.Errorf("accounts list lacks %q:\n%s", want, listText)
		}
	}

	// Account detail (exhausted Grok Bot): pre-send warning banner, reset time, progress bar, real data source.
	grokView := renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{Page: pageAccount, Account: grokAcct.AccountID})
	noJargon(t, "grok account", grokView)
	grokText := text(grokView)
	for _, want := range []string{"发消息前会先提示拦截", "用完了，2 小时后恢复", "重置时间", "Grok Bot 官方用量", `"type":"progress"`} {
		if !strings.Contains(grokText, want) {
			t.Errorf("grok detail lacks %q:\n%s", want, grokText)
		}
	}

	// Account detail (low Claude Duet): two buckets, low warning, real web source.
	duetView := renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{Page: pageAccount, Account: duetAcct.AccountID})
	noJargon(t, "duet account", duetView)
	duetText := text(duetView)
	for _, want := range []string{"额度快用完了", "每 5 小时的时段额度 15%", "每周的时段额度 68%", "Claude 网页端用量"} {
		if !strings.Contains(duetText, want) {
			t.Errorf("duet detail lacks %q:\n%s", want, duetText)
		}
	}

	// Account detail (Muse before first real sync): local counts + honest real-source explanation.
	museView := renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{Page: pageAccount, Account: museAcct.AccountID})
	noJargon(t, "muse account", museView)
	museText := text(museView)
	for _, want := range []string{"近 5 小时已发 1 条 · 近 24 小时已发 1 条", "等待读取真实额度", "muse.ai", "立即同步"} {
		if !strings.Contains(museText, want) {
			t.Errorf("muse detail lacks %q:\n%s", want, museText)
		}
	}

	// Assistant tools: club_agents_list exposes quota, describe blocks exhausted and warns on low.
	setControl(t, club, grokAcct.AccountID, core.ControlConfirm)
	setControl(t, club, duetAcct.AccountID, core.ControlConfirm)
	var listRes wire.ConnectorCallResult
	if err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: Connector, Name: toolList}, &listRes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listRes.Text, `"state":"exhausted"`) || !strings.Contains(listRes.Text, `"state":"low"`) {
		t.Errorf("club_agents_list missing quota fields: %s", listRes.Text)
	}
	grokKey := core.SessionKey(grokBot, grokAcct.AccountID, "main")
	duetKey := core.SessionKey("claude_duet", duetAcct.AccountID, "main")
	if err := h.Call("connector.actions.describe", wire.ConnectorCallParams{Contribution: Connector, Name: toolSend,
		Args: json.RawMessage(`{"session_key":"` + grokKey + `","text":"hi"}`)}, &wire.ConnectorDescribeResult{}); err == nil || !strings.Contains(err.Error(), "额度已用尽") {
		t.Errorf("exhausted describe should refuse clearly, got %v", err)
	}
	var descRes wire.ConnectorDescribeResult
	if err := h.Call("connector.actions.describe", wire.ConnectorCallParams{Contribution: Connector, Name: toolSend,
		Args: json.RawMessage(`{"session_key":"` + duetKey + `","text":"hi"}`)}, &descRes); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(descRes.Action.Name, "仅剩 15%") {
		t.Errorf("low quota describe should warn: %+v", descRes.Action)
	}
}
