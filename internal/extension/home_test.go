package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/echo"
	"github.com/nwverse/ensemble/internal/store"
)

// jargon are words a first-time user should not meet on the Chinese pages: English UI words, the program's own
// terms and raw ids.
var jargon = []string{"provider", "Provider", "runtime", "bridge", "connector", "Hub", "hub", "ego-lite", "插件",
	"BrowserBackend", "夺舍", "Persona Link", "muse2api", "MUSE2API_KEY", "claude_duet", "gemini_tasks", "grok_bot",
	"requests:", "tokens:", "AUTH_FAILED", "RATE_LIMITED", "结果", "已更新", "此版本未接入", "Accounts", "Pair", "Sync",
	"Demo", "Echo（", "Error", "Active"}

func noJargon(t *testing.T, where string, v *wire.View) {
	t.Helper()
	var visible []string
	visible = append(visible, v.Title)
	for _, s := range v.Sections {
		visible = append(visible, s.Title, s.Footer)
		for _, c := range s.Items {
			visible = append(visible, c.Title, c.Subtitle, c.Value, c.Text, c.Label, c.Detail, c.Placeholder)
			if c.Badge != nil {
				visible = append(visible, c.Badge.Text)
			}
			for _, ch := range c.Choices {
				visible = append(visible, ch.Title)
			}
			if c.Action != nil {
				visible = append(visible, c.Action.Confirm)
				if c.Action.Push != nil {
					visible = append(visible, c.Action.Push.Title)
				}
			}
		}
	}
	all := strings.Join(visible, "\n")
	// The browser Persona drives is named once a page, in parentheses after what it is.
	if n := strings.Count(all, browserName); n > 1 || n != strings.Count(all, "（"+browserName) {
		t.Errorf("%s names %s %d times, or not as （%s…）:\n%s", where, browserName, n, browserName, all)
	}
	for _, w := range jargon {
		if strings.Contains(all, w) {
			t.Errorf("%s shows %q:\n%s", where, w, all)
		}
	}
}

func renderUI(t *testing.T, h *exttest.Host, ui, placement string, st pageState) *wire.View {
	t.Helper()
	var v wire.View
	if err := h.Call("ui.render", wire.UIParams{Contribution: ui, Placement: placement, Size: "full",
		UIVersion: 11, Locale: "zh-Hans", State: stateOf(st)}, &v); err != nil {
		t.Fatalf("ui.render %s %+v: %v", ui, st, err)
	}
	checkView(t, &v)
	return &v
}

func sectionIndex(v *wire.View, title string) int {
	for i, s := range v.Sections {
		if strings.HasPrefix(s.Title, title) {
			return i
		}
	}
	return -1
}

// The manifest puts the home in the main navigation and on Overview, keeps the settings page's link in Chinese, and
// the program draws every one of them.
func TestManifestPagesAndSettingsAreChinese(t *testing.T) {
	var m struct {
		Description string `json:"description"`
		Contributes struct {
			UI []wire.UIItem `json:"ui"`
		} `json:"contributes"`
		Configuration struct {
			Properties map[string]struct {
				Title       string `json:"title"`
				Description string `json:"description"`
			} `json:"properties"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	han := func(s string) bool {
		for _, r := range s {
			if r >= 0x4e00 && r <= 0x9fff {
				return true
			}
		}
		return false
	}
	if !han(m.Description) {
		t.Errorf("description is not Chinese: %q", m.Description)
	}
	places := map[string]wire.UIItem{}
	for _, u := range m.Contributes.UI {
		places[u.ID] = u
		if !han(u.Title) {
			t.Errorf("page %s title %q is not Chinese", u.ID, u.Title)
		}
	}
	if p := places[HomeUI]; p.Placement != wire.PlacementNavPage || p.Icon == "" {
		t.Errorf("home is not in the main navigation: %+v", p)
	}
	if p := places[HomeCardUI]; p.Placement != wire.PlacementOverviewCard || p.Opens != HomeUI {
		t.Errorf("no overview card opening the home: %+v", p)
	}
	if p := places[AccountsUI]; p.Title != "账号" {
		t.Errorf("the settings link is %q; the assistant's guide sends people to 账号", p.Title)
	}
	for k, p := range m.Configuration.Properties {
		if !han(p.Title) || !han(p.Description) {
			t.Errorf("setting %s is not in Chinese: %+v", k, p)
		}
		if n := strings.Count(p.Title+p.Description, browserName); n > strings.Count(p.Title+p.Description, "（"+browserName) {
			t.Errorf("setting %s names %s outside parentheses", k, browserName)
		}
		for _, w := range []string{"provider", "runtime", "bridge", "CDP", "socket"} {
			if strings.Contains(p.Title+p.Description, w) {
				t.Errorf("setting %s says %q", k, w)
			}
		}
	}
	for _, setting := range []string{settingBrowser, settingRuntime} {
		found := false
		for _, p := range m.Configuration.Properties {
			found = found || p.Title == setting
		}
		if !found {
			t.Errorf("the pages send people to the setting %q, which the manifest does not have", setting)
		}
	}
	if !strings.HasPrefix(m.Configuration.Properties["muse2api_url"].Title, settingMuse2API) {
		t.Errorf("the pages send people to the setting %q", settingMuse2API)
	}
}

// The home says what this is, where it stands and the next step, for a first visit and once accounts are there.
func TestHomeSaysWhatWhereAndNext(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
	v := renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{})
	noJargon(t, "home (empty)", v)
	for _, want := range []string{"接进 Persona", "现在的状态", "还没有接入任何账号", "下一步", "连接你的第一个账号",
		"怎么选：", "借用电脑上已登录的网页", "pair-claude_duet"} {
		if !strings.Contains(text(v), want) {
			t.Errorf("home (empty) lacks %q: %s", want, text(v))
		}
	}
	card := renderUI(t, h, HomeCardUI, wire.PlacementOverviewCard, pageState{})
	if !strings.Contains(text(card), "还没有接入任何账号") || !strings.Contains(text(card), "连接你的第一个账号") {
		t.Errorf("card: %s", text(card))
	}

	// paired, but nothing handed to the assistant: the next step is 助手指挥, and it opens the account
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential, fieldName: "演示一号"})
	v = renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{})
	noJargon(t, "home (one account)", v)
	for _, want := range []string{"已接入 1 个账号", "一个都用不了", "让 Persona 助手能帮你看这些账号", "助手指挥", "演示一号",
		"Persona 助手：不能用"} {
		if !strings.Contains(text(v), want) {
			t.Errorf("home (one account) lacks %q: %s", want, text(v))
		}
	}
	var step *wire.Component
	for i, c := range v.Sections[1].Items {
		if c.Type == "row" {
			step = &v.Sections[1].Items[i]
		}
	}
	if step == nil || step.Action == nil || step.Action.Push == nil || step.Action.Push.UI != HomeUI {
		t.Fatalf("the next step does not open the account in the home: %+v", step)
	}

	// a broken account comes first, with what to do
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	id := status.Accounts[0].AccountID
	errStatus, code := protocol.AccountError, "AUTH_FAILED"
	_ = space.Store().UpdateAccount(context.Background(), id, storeStatus(errStatus, code), 1)
	v = renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{})
	noJargon(t, "home (broken)", v)
	for _, want := range []string{"1 个要你处理", "先修好「演示一号」", "密钥失效了", "要处理"} {
		if !strings.Contains(text(v), want) {
			t.Errorf("home (broken) lacks %q: %s", want, text(v))
		}
	}

	// all well: the state says the assistant may use it, and there is no step
	_ = space.Store().UpdateAccount(context.Background(), id, storeStatus(protocol.AccountActive, ""), 1)
	setControl(t, club, id, core.ControlRead)
	v = renderUI(t, h, HomeUI, wire.PlacementNavPage, pageState{})
	if !strings.Contains(text(v), "Persona 助手能用其中 1 个") || strings.Contains(text(v), "下一步") ||
		!strings.Contains(text(v), "Persona 助手：只读") {
		t.Errorf("home (all well): %s", text(v))
	}
}

// The account page leads with the state and the controls people look for, above the allowance and the advanced
// parts; it renames; every change says what changed.
func TestAccountPageControlsFirstAndRename(t *testing.T) {
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	page := pageState{Page: pageAccount, Account: status.Accounts[0].AccountID}
	v := render(t, h, page)
	noJargon(t, "account page", v)
	control, main, quota := sectionIndex(v, "助手指挥"), sectionIndex(v, "主助手"), sectionIndex(v, "额度")
	if control < 0 || main < 0 || quota < 0 || control > quota || main > quota || renameSection(v) > control {
		t.Fatalf("order: control %d, main %d, quota %d: %s", control, main, quota, text(v))
	}
	if f := v.Sections[control].Footer; !strings.Contains(f, "只读：Persona 助手能读") || !strings.Contains(f, "可代发：还能替你给它发消息") {
		t.Errorf("助手指挥 is not explained: %q", f)
	}
	if f := v.Sections[main].Footer; !strings.Contains(f, "和上面的「助手指挥」不一样") {
		t.Errorf("主助手 is not told apart from 助手指挥: %q", f)
	}

	if res := act(t, h, page, actRename, map[string]string{fieldName: "  "}); !strings.Contains(res.Toast, "名字不能是空的") {
		t.Errorf("empty name: %+v", res)
	}
	if res := act(t, h, page, actRename, map[string]string{fieldName: strings.Repeat("长", 81)}); !strings.Contains(res.Toast, "名字太长了") {
		t.Errorf("long name: %+v", res)
	}
	res := act(t, h, page, actRename, map[string]string{fieldName: "工作用演示"})
	if res.Toast != "已改名为「工作用演示」" || res.View.Title != "工作用演示" {
		t.Fatalf("rename: %+v", res)
	}
	if a, _ := space.Account(context.Background(), page.Account); a.DisplayName != "工作用演示" {
		t.Fatalf("renamed to %q", a.DisplayName)
	}
	for level, want := range map[string]string{"read": "已设为只读", "confirm": "已设为可代发", "off": "已关闭"} {
		if res := act(t, h, page, actControl, map[string]string{fieldControl: level}); !strings.HasPrefix(res.Toast, want) {
			t.Errorf("control %s: %q", level, res.Toast)
		}
	}
	if res := act(t, h, page, actControl, map[string]string{fieldControl: "bogus"}); res.Toast == "" || strings.Contains(res.Toast, "assistant_control") {
		t.Errorf("a bad level: %+v", res)
	}
	if res := act(t, h, page, actQuotaDisplay, map[string]string{fieldQuotaDisplay: protocol.QuotaDisplayOff}); res.Toast != "头像上不再显示额度" {
		t.Errorf("quota: %+v", res)
	}
}

// With the main assistant bridge switched off, the page says what to switch on instead of a switch that does nothing,
// and refuses one anyway.
func TestMainAssistantNeedsTheSetting(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true,"assistant_runtime":false}`))
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	page := pageState{Page: pageAccount, Account: status.Accounts[0].AccountID}
	v := render(t, h, page)
	main := sectionIndex(v, "主助手")
	if main < 0 || strings.Contains(text(&wire.View{Sections: v.Sections[main : main+1]}), fieldAssistantBackend) ||
		!strings.Contains(text(v), "打开「"+settingRuntime+"」") {
		t.Fatalf("main assistant with the bridge off: %s", text(v))
	}
	if res := act(t, h, page, actAssistantBackend, map[string]string{fieldAssistantBackend: "on"}); !strings.Contains(res.Toast, "现在开不了") {
		t.Fatalf("switched on anyway: %+v", res)
	}
	if a, _ := space.Account(context.Background(), page.Account); a.AssistantBackend {
		t.Fatal("the account became a main assistant")
	}
}

// With the browser accounts switched off, their rows and pages say why and what to switch on before anything is
// asked, and pairing one anyway is refused in words.
func TestBrowserAccountsSwitchedOffSayWhy(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"browser_providers":false}`))
	v := render(t, h, pageState{Page: pageAdd})
	for _, s := range browserProviders {
		for _, c := range v.Sections[sectionIndex(v, "借用")].Items {
			if c.Key == "pair-"+s.Provider && (c.Badge == nil || !strings.Contains(c.Subtitle, settingBrowser)) {
				t.Errorf("row %s does not say what to switch on: %+v", s.Provider, c)
			}
		}
		st := pageState{Page: pageBrowserPair, Provider: s.Provider}
		pv := render(t, h, st)
		noJargon(t, "pair "+s.Provider, pv)
		if !strings.Contains(text(pv), settingBrowser) || strings.Contains(text(pv), fieldMachine) {
			t.Errorf("pair page %s: %s", s.Provider, text(pv))
		}
		if res := act(t, h, st, actBrowserPair, map[string]string{fieldMachine: "m|b"}); !strings.Contains(res.Toast, settingBrowser) {
			t.Errorf("pairing %s anyway: %+v", s.Provider, res)
		}
	}
}

// By default (no settings saved) the browser accounts are on.
func TestBrowserAccountsOnByDefault(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), nil)
	for _, s := range browserProviders {
		if _, ok := club.Registry().Get(s.Provider); !ok {
			t.Errorf("%s is off by default", s.Provider)
		}
	}
}

func TestQuotaWords(t *testing.T) {
	p := 62
	for _, c := range []struct{ kind, window, want string }{
		{protocol.QuotaKindRequests, "day", "每天的次数 62%"},
		{protocol.QuotaKindTokens, "minute", "每分钟的用量（字数） 62%"},
		{"other", "", "额度 62%"},
	} {
		if got := quotaWords("zh-Hans", c.kind, c.window, &p); got != c.want {
			t.Errorf("quotaWords(%s, %s) = %q, want %q", c.kind, c.window, got, c.want)
		}
	}
}

func TestReasonsInWords(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{protocol.Conflict("this account is already paired as x").Because(protocol.ReasonAlreadyPaired), "已经添加过了"},
		{protocol.Unsupported("provider muse is not available in this build").Because(protocol.ReasonProviderUnavailable), "还没打开"},
		{protocol.Invalid("the provider refused the credential: AUTH_FAILED").Because("AUTH_FAILED"), "重新复制"},
		{protocol.Failure("SYNC_FAILED", "verify failed"), "连不上对方"},
	} {
		got := reason("zh-Hans", c.err, "")
		if !strings.Contains(got, c.want) || strings.ContainsAny(got, "abcdefghijklmnopqrstuvwxyz") {
			t.Errorf("reason(%v) = %q", c.err, got)
		}
	}
}

func storeStatus(status, lastError string) store.AccountPatch {
	return store.AccountPatch{Status: &status, LastError: &lastError}
}

// flaky is a key provider whose reads fail while failing is set (an expired key), for 重新检查.
type flaky struct {
	failing atomic.Bool
}

func (f *flaky) Name() string       { return "groq" }
func (f *flaky) Connection() string { return protocol.ConnectionServer }
func (f *flaky) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: "flaky-1", DisplayName: "Groq"}, nil
}
func (f *flaky) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	if f.failing.Load() {
		return nil, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: 401}
	}
	return []provider.RemoteSession{{ExternalID: "main", Title: "Groq", Kind: protocol.KindMain, CanSend: true}}, nil
}
func (f *flaky) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}
func (f *flaky) Send(context.Context, string, provider.SessionRef, provider.Outgoing, provider.SendContext) (provider.SendResult, error) {
	return provider.SendResult{Status: provider.Sent}, nil
}

// 重新检查 says whether the account works now: club.accounts.sync answers ok either way.
func TestCheckAgainSaysWhetherItIsFixed(t *testing.T) {
	f := &flaky{}
	club := core.New(core.Options{Registry: provider.NewRegistry(f), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), nil)
	res := act(t, h, pageState{Page: pagePair, Provider: "groq"}, actPair, map[string]string{fieldCredential: "gsk_fixture_0000001"})
	if !pairedDone(res) {
		t.Fatalf("pair: %+v", res)
	}
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	page := pageState{Page: pageAccount, Account: status.Accounts[0].AccountID}

	f.failing.Store(true)
	res = act(t, h, page, actSync, nil)
	if !strings.HasPrefix(res.Toast, "检查过了，还没好。密钥失效了") || strings.Contains(res.Toast, "已同步") {
		t.Fatalf("a failing check said: %+v", res)
	}
	if !strings.Contains(text(res.View), "要你处理") || !strings.Contains(text(res.View), `"tone":"caution"`) {
		t.Fatalf("a failing check's page: %s", text(res.View))
	}
	f.failing.Store(false)
	if res = act(t, h, page, actSync, nil); res.Toast != "检查过了，已经恢复正常，最新的对话也拉下来了" {
		t.Fatalf("a check that fixed it said: %+v", res)
	}
	if res = act(t, h, page, actSync, nil); res.Toast != "已同步，最新的对话已经拉下来了" {
		t.Fatalf("a check of a working account said: %+v", res)
	}
}

// Switching to auto requires an explicit secondary confirmation card before saving, and explains the safety guards;
// repository authorization and binding stay in confirm_always.
func TestAutoLevelRequiresExplicitConfirmAndExplainsGuards(t *testing.T) {
	if strings.Contains(string(pac.Manifest), `"club_agent_*"`) ||
		!strings.Contains(string(pac.Manifest), `"club_agent_repo_authorize"`) ||
		!strings.Contains(string(pac.Manifest), `"club_agent_repo_bind"`) {
		t.Fatalf("manifest confirm_always should be narrowed to repo tools: %s", pac.Manifest)
	}
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	id := status.Accounts[0].AccountID
	page := pageState{Page: pageAccount, Account: id}

	// Selecting auto on the segmented control does NOT persist immediately: it stages the confirmation card.
	res := act(t, h, page, actControl, map[string]string{fieldControl: "auto"})
	if !strings.Contains(res.Toast, "确认卡片") {
		t.Fatalf("expected staging toast for auto, got %q", res.Toast)
	}
	noJargon(t, "auto confirm card", res.View)
	if got, _ := space.AssistantControl(context.Background(), id); got != core.ControlOff {
		t.Fatalf("auto was persisted before secondary confirmation: %q", got)
	}
	for _, want := range []string{"确认开启「直接放行」？", "风险与安全护栏说明", "确认开启直接放行", "取消，保持当前档位"} {
		if !strings.Contains(text(res.View), want) {
			t.Errorf("confirmation card lacks %q: %s", want, text(res.View))
		}
	}

	// Canceling keeps the previous level.
	var stagedState pageState
	_ = json.Unmarshal([]byte(res.View.State), &stagedState)
	cancelRes := act(t, h, stagedState, actControlAutoCancel, nil)
	if !strings.Contains(cancelRes.Toast, "已取消") {
		t.Fatalf("cancel toast: %q", cancelRes.Toast)
	}
	if got, _ := space.AssistantControl(context.Background(), id); got != core.ControlOff {
		t.Fatalf("level changed after cancel: %q", got)
	}

	// Staging again and confirming persists ControlAuto.
	res = act(t, h, page, actControl, map[string]string{fieldControl: "auto"})
	_ = json.Unmarshal([]byte(res.View.State), &stagedState)
	confirmRes := act(t, h, stagedState, actControlAutoConfirm, nil)
	if !strings.Contains(confirmRes.Toast, "已开启直接放行") || !strings.Contains(confirmRes.Toast, "仓库强制确认") {
		t.Fatalf("confirm auto toast: %q", confirmRes.Toast)
	}
	if got, _ := space.AssistantControl(context.Background(), id); got != core.ControlAuto {
		t.Fatalf("expected ControlAuto after confirmation, got %q", got)
	}
	if v := render(t, h, pageState{}); !strings.Contains(text(v), "Persona 助手：直接放行（自动护栏保护）") {
		t.Errorf("list: %s", text(v))
	}
}

// The browser pairing page gives one instruction: sign in in the browser Persona drives on that computer.
func TestBrowserPairSaysWhichBrowser(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), nil)
	h.Answer("browser.machines", func(json.RawMessage) (any, error) {
		return wire.BrowserMachinesResult{Machines: []wire.BrowserMachine{{MachineID: "m", Name: "Mac", Backend: "ego", Online: true, Ready: true}}}, nil
	})
	v := render(t, h, pageState{Page: pageBrowserPair, Provider: "claude_duet"})
	noJargon(t, "browser pair", v)
	for _, want := range []string{"打开 Persona 控制的那个浏览器（ego lite，不是你平时用的其他浏览器），在里面登录 claude.ai", "用的就是你在这个浏览器里的登录"} {
		if !strings.Contains(text(v), want) {
			t.Errorf("lacks %q: %s", want, text(v))
		}
	}
	for _, not := range []string{"就像平时", "插件", "单独开一个浏览器空间"} {
		if strings.Contains(text(v), not) {
			t.Errorf("says %q: %s", not, text(v))
		}
	}
	if res := act(t, h, pageState{Page: pageBrowserPair, Provider: "claude_duet"}, actBrowserPair, map[string]string{fieldMachine: "|ego"}); !strings.HasPrefix(res.Toast, "还没选电脑") {
		t.Errorf("no computer: %+v", res)
	}
}

// English pages quote the settings and the navigation entry by the titles the (Chinese-only) manifest shows.
func TestEnglishPagesQuoteTheChineseTitles(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true,"browser_providers":false,"assistant_runtime":false}`))
	en := func(st pageState) string {
		var v wire.View
		if err := h.Call("ui.render", wire.UIParams{Contribution: HomeUI, Placement: wire.PlacementNavPage, Size: "full",
			UIVersion: 11, Locale: "en", State: stateOf(st)}, &v); err != nil {
			t.Fatal(err)
		}
		return text(&v)
	}
	home := en(pageState{})
	if !strings.Contains(home, `"title":"我的 AI 助手 (My AI assistants)"`) || !strings.Contains(home, `\"`+settingBrowser+`\"`) ||
		!strings.Contains(home, `\"`+settingMuse2API+`\"`) {
		t.Errorf("en home: %s", home)
	}
	if !strings.Contains(en(pageState{Page: pageBrowserPair, Provider: "dots"}), `\"`+settingBrowser+`\"`) {
		t.Error("en browser pair page does not quote the setting")
	}
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	if home := en(pageState{}); !strings.Contains(home, "1 account;") {
		t.Errorf("en state: %s", home)
	}
	if acct := en(pageState{Page: pageAccount, Account: status.Accounts[0].AccountID}); !strings.Contains(acct, `\"`+settingRuntime+`\"`) {
		t.Errorf("en account page does not quote the setting: %s", acct)
	}
	// and the unnamed demo account is the demo account in Chinese, not "Echo"
	if v := render(t, h, pageState{}); !strings.Contains(text(v), `"title":"演示账号"`) || strings.Contains(text(v), `"Echo"`) {
		t.Errorf("zh demo title: %s", text(v))
	}
}

// Refusals without words of their own carry no English on the Chinese pages.
func TestChineseReasonHasNoEnglish(t *testing.T) {
	for _, err := range []error{protocol.Invalid("choose a computer"), protocol.Unsupported("x"), errors.New("boom")} {
		if got := reason("zh-Hans", err, ""); strings.ContainsAny(got, "abcdefghijklmnopqrstuvwxyz") {
			t.Errorf("reason(%v) = %q", err, got)
		}
	}
	if got := reason("en", protocol.Invalid("choose a computer"), ""); !strings.Contains(got, "choose a computer") {
		t.Errorf("en keeps the detail: %q", got)
	}
}

func TestFollowLeaseUICreateListRevokeAndListTool(t *testing.T) {
	h, club := start(t)
	ctx := context.Background()
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential, fieldName: "跟进测试号"})
	space, _ := club.Space(ctx, core.DefaultSpace)
	status, _ := space.Status(ctx)
	id := status.Accounts[0].AccountID
	page := pageState{Page: pageAccount, Account: id}

	// Switch account to follow mode via UI.
	res := act(t, h, page, actControl, map[string]string{fieldControl: "follow"})
	if !strings.Contains(res.Toast, "已设为持续跟进") {
		t.Fatalf("set follow toast: %q", res.Toast)
	}
	noJargon(t, "account at follow", res.View)
	rawView, _ := json.Marshal(res.View)
	if _, err := wire.DecodeCheckedViewJSON(rawView); err != nil {
		t.Fatalf("DecodeCheckedViewJSON: %v", err)
	}

	// Create a follow lease via UI (6h, 20 turns, background=on, note="接口回归").
	grantRes := act(t, h, page, actLeaseGrant, map[string]string{
		fieldLeaseSession:    "main",
		fieldLeaseTTL:        "6h",
		fieldLeaseTurns:      "20",
		fieldLeaseBackground: "on",
		fieldLeaseNote:       "接口回归",
	})
	if !strings.Contains(grantRes.Toast, "已为「演示对话」新建跟进租约") || !strings.Contains(grantRes.Toast, "20 轮") || !strings.Contains(grantRes.Toast, "6 小时") {
		t.Fatalf("grant lease toast: %q", grantRes.Toast)
	}
	noJargon(t, "account with active lease", grantRes.View)
	rawGranted, _ := json.Marshal(grantRes.View)
	if _, err := wire.DecodeCheckedViewJSON(rawGranted); err != nil {
		t.Fatalf("DecodeCheckedViewJSON after grant: %v", err)
	}
	for _, want := range []string{"剩余 20 / 20 轮", "允许后台跟进", "接口回归", "撤销「演示对话」的跟进租约"} {
		if !strings.Contains(text(grantRes.View), want) {
			t.Errorf("account view lacks %q: %s", want, text(grantRes.View))
		}
	}

	// Verify club_agents_list exposes assistant_control="follow" and active follow_leases without leaking credential.
	var listRes wire.ConnectorCallResult
	if err := h.Call("connector.tools.call", wire.ConnectorCallParams{
		Contribution: Connector,
		Name:         toolList,
		Args:         json.RawMessage(`{}`),
	}, &listRes); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(listRes.Text, demoCredential) {
		t.Fatalf("club_agents_list leaked credential: %s", listRes.Text)
	}
	if !strings.Contains(listRes.Text, `"assistant_control":"follow"`) ||
		!strings.Contains(listRes.Text, `"follow_leases":[`) ||
		!strings.Contains(listRes.Text, `"remaining_turns":20`) ||
		!strings.Contains(listRes.Text, `"allow_background":true`) ||
		!strings.Contains(listRes.Text, `"note":"接口回归"`) {
		t.Fatalf("club_agents_list missing follow_leases details: %s", listRes.Text)
	}

	// Manually revoke the lease via UI.
	leases, err := space.ListFollowLeases(ctx, protocol.ListFollowLeasesParams{AccountID: id, ActiveOnly: true})
	if err != nil || len(leases.Leases) != 1 {
		t.Fatalf("active leases: %+v %v", leases, err)
	}
	revRes := act(t, h, page, actLeaseRevoke, map[string]string{valLeaseID: leases.Leases[0].LeaseID})
	if !strings.Contains(revRes.Toast, "已撤销这条跟进租约") {
		t.Fatalf("revoke toast: %q", revRes.Toast)
	}
	noJargon(t, "account after revoke", revRes.View)
	if !strings.Contains(text(revRes.View), "现在没有生效中的跟进租约。") {
		t.Fatalf("expected no active lease after revoke: %s", text(revRes.View))
	}

	// Re-grant a lease, then downgrade control to confirm via UI: the lease must be automatically revoked.
	act(t, h, page, actLeaseGrant, map[string]string{
		fieldLeaseSession:    "main",
		fieldLeaseTTL:        "2h",
		fieldLeaseTurns:      "10",
		fieldLeaseBackground: "off",
	})
	if leases, _ := space.ListFollowLeases(ctx, protocol.ListFollowLeasesParams{AccountID: id, ActiveOnly: true}); len(leases.Leases) != 1 {
		t.Fatalf("expected 1 active lease before downgrade, got %+v", leases)
	}
	downRes := act(t, h, page, actControl, map[string]string{fieldControl: "confirm"})
	if !strings.Contains(downRes.Toast, "已设为可代发") {
		t.Fatalf("downgrade toast: %q", downRes.Toast)
	}
	if leases, _ := space.ListFollowLeases(ctx, protocol.ListFollowLeasesParams{AccountID: id, ActiveOnly: true}); len(leases.Leases) != 0 {
		t.Fatalf("expected 0 active leases after downgrade to confirm, got %+v", leases)
	}
}

func TestFollowAndAutoToolsGuardsAndAudit(t *testing.T) {
	var nowMs atomic.Int64
	nowMs.Store(time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC).UnixMilli())
	now := func() time.Time { return time.UnixMilli(nowMs.Load()) }
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}, Now: now})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
	ctx := context.Background()

	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(ctx, core.DefaultSpace)
	status, _ := space.Status(ctx)
	id := status.Accounts[0].AccountID
	sessions, _ := space.Sessions(ctx)
	var mainKey string
	for _, s := range sessions.Sessions {
		if s.CanSend {
			mainKey = s.SessionKey
		}
	}
	// Add a second writable session on the same account to verify exact session_key matching.
	otherKey := core.SessionKey(echo.Name, id, "second")
	_, err := space.Store().UpsertSessions(ctx, []store.Session{{
		SessionKey: otherKey,
		AccountID:  id,
		Provider:   echo.Name,
		ExternalID: "second",
		Title:      "第二对话",
		Kind:       protocol.KindMain,
		CanSend:    true,
		RankTimeMs: nowMs.Load(),
	}}, nowMs.Load())
	if err != nil {
		t.Fatal(err)
	}

	describeTrack := func(name, trackKind string, args any) (wire.ConnectorDescribeResult, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorDescribeResult
		err := h.Call("connector.actions.describe", wire.ConnectorCallParams{
			Contribution: Connector,
			Name:         name,
			Args:         raw,
			TrackKind:    trackKind,
		}, &res)
		return res, err
	}
	doTrack := func(name, trackKind, reqID string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorCallResult
		err := h.Call("connector.actions.do", wire.ConnectorDoParams{
			Contribution: Connector,
			Name:         name,
			Args:         raw,
			RequestID:    reqID,
			TrackKind:    trackKind,
		}, &res)
		return res.Text, err
	}

	// 1. Set control to follow and grant a 2-turn lease with allow_background=false on mainKey.
	setControl(t, club, id, core.ControlFollow)
	lease, err := space.GrantFollowLease(ctx, protocol.GrantFollowLeaseParams{
		AccountID:       id,
		SessionKey:      mainKey,
		TTL:             "2h",
		MaxTurns:        2,
		AllowBackground: false,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Unattended background call must be rejected because AllowBackground is false.
	if _, err := describeTrack(toolSend, "background", map[string]any{"session_key": mainKey, "text": "bg-1"}); err == nil {
		t.Fatal("expected background send rejected when allow_background=false")
	}
	if _, err := doTrack(toolSend, "background", "req-bg-1", map[string]any{"session_key": mainKey, "text": "bg-1"}); err == nil {
		t.Fatal("expected background do rejected when allow_background=false")
	}

	// Attended call on mainKey consumes turn 1 and turn 2.
	if d, err := describeTrack(toolSend, "main", map[string]any{"session_key": mainKey, "text": "turn-1"}); err != nil || !strings.Contains(d.Action.Name, "跟进租约生效：剩余 2/2 轮") {
		t.Fatalf("describe with lease: %+v %v", d, err)
	}
	if _, err := doTrack(toolSend, "main", "req-f-1", map[string]any{"session_key": mainKey, "text": "turn-1"}); err != nil {
		t.Fatalf("do turn 1: %v", err)
	}
	if _, err := doTrack(toolSend, "main", "req-f-2", map[string]any{"session_key": mainKey, "text": "turn-2"}); err != nil {
		t.Fatalf("do turn 2: %v", err)
	}
	if got, ok, _ := space.Store().FollowLease(ctx, lease.LeaseID); !ok || got.UsedTurns != 2 || got.Status(nowMs.Load()) != store.LeaseExhausted {
		t.Fatalf("expected lease exhausted at 2/2, got %+v", got)
	}

	// Once exhausted, background is denied and attended degrades back to Confirm (no lease badge in describe, audit verdict=confirm).
	if d, err := describeTrack(toolSend, "main", map[string]any{"session_key": mainKey, "text": "turn-3"}); err != nil || strings.Contains(d.Action.Name, "跟进租约生效") {
		t.Fatalf("describe after exhaustion should degrade to confirm: %+v %v", d, err)
	}
	if _, err := describeTrack(toolSend, "background", map[string]any{"session_key": mainKey, "text": "turn-3"}); err == nil {
		t.Fatal("background send after exhaustion must be denied")
	}

	// 2. Grant a background-enabled lease on mainKey (max_turns=1): works for mainKey in background once, denied on otherKey.
	_, err = space.GrantFollowLease(ctx, protocol.GrantFollowLeaseParams{
		AccountID:       id,
		SessionKey:      mainKey,
		TTL:             "2h",
		MaxTurns:        1,
		AllowBackground: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := describeTrack(toolSend, "background", map[string]any{"session_key": otherKey, "text": "wrong-session"}); err == nil {
		t.Fatal("background send on mismatched session_key must be denied")
	}
	if _, err := doTrack(toolSend, "background", "req-bg-ok", map[string]any{"session_key": mainKey, "text": "bg-ok"}); err != nil {
		t.Fatalf("background send with allow_background=true failed: %v", err)
	}
	if _, err := doTrack(toolSend, "background", "req-bg-over", map[string]any{"session_key": mainKey, "text": "bg-over"}); err == nil {
		t.Fatal("background send exceeding max_turns must be denied")
	}

	// 3. Switch to ControlAuto and test safety guards: burst rate limit, LocalUnconfirmed, and audit log.
	setControl(t, club, id, core.ControlAuto)
	// Advance clock by 2 minutes to clear previous burst window.
	nowMs.Add(2 * 60_000)

	// Burst rate limit: BurstMaxWrites (5) succeed in 60s, the 6th is blocked with RATE_LIMITED.
	for i := range core.BurstMaxWrites {
		reqID := fmt.Sprintf("req-auto-burst-%d", i)
		if _, err := doTrack(toolSend, "main", reqID, map[string]any{"session_key": mainKey, "text": fmt.Sprintf("auto-%d", i)}); err != nil {
			t.Fatalf("auto send %d failed: %v", i, err)
		}
	}
	if _, err := describeTrack(toolSend, "main", map[string]any{"session_key": mainKey, "text": "auto-overflow"}); err == nil || !strings.Contains(err.Error(), "突发频率限制") {
		t.Fatalf("expected burst rate limit on 6th describe, got %v", err)
	}
	if _, err := doTrack(toolSend, "main", "req-auto-overflow", map[string]any{"session_key": mainKey, "text": "auto-overflow"}); err == nil || !strings.Contains(err.Error(), "突发频率限制") {
		t.Fatalf("expected burst rate limit on 6th do, got %v", err)
	}

	// Advance clock past burst window and trigger LocalUnconfirmed via echo.MarkUnconfirmed ("#unconfirmed").
	nowMs.Add(2 * 60_000)
	outUnconf, err := doTrack(toolSend, "main", "req-unconf-1", map[string]any{"session_key": mainKey, "text": "ping " + echo.MarkUnconfirmed})
	if err != nil || !strings.Contains(outUnconf, "没有得到确认") {
		t.Fatalf("expected unconfirmed send result, got %q %v", outUnconf, err)
	}
	// Next send on the same session must be blocked by LocalUnconfirmed guard!
	if _, err := describeTrack(toolSend, "main", map[string]any{"session_key": mainKey, "text": "blind-follow-up"}); err == nil || !strings.Contains(err.Error(), "尚未确认是否送达") {
		t.Fatalf("expected LocalUnconfirmed guard to block describe, got %v", err)
	}
	if _, err := doTrack(toolSend, "main", "req-unconf-2", map[string]any{"session_key": mainKey, "text": "blind-follow-up"}); err == nil || !strings.Contains(err.Error(), "尚未确认是否送达") {
		t.Fatalf("expected LocalUnconfirmed guard to block do, got %v", err)
	}

	// Unattended background write under ControlAuto is always denied.
	if _, err := doTrack(toolSend, "background", "req-auto-bg", map[string]any{"session_key": otherKey, "text": "bg-auto"}); err == nil {
		t.Fatal("unattended write under ControlAuto must be denied")
	}

	// Verify audit entries recorded follow lease consumption, auto bypass, burst rate limit, and local_unconfirmed without leaking secrets.
	audits := space.AuditEntries()
	var sawLease, sawAutoBypass, sawBurstGuard, sawUnconfirmedGuard bool
	for _, a := range audits {
		if strings.Contains(a.Reason, demoCredential) || strings.Contains(a.Guard, demoCredential) {
			t.Fatalf("audit leaked credential: %+v", a)
		}
		if a.Level == core.ControlFollow && a.Verdict == core.Allow && a.LeaseID != "" {
			sawLease = true
		}
		if a.Level == core.ControlAuto && a.Verdict == core.Allow && a.Bypass {
			sawAutoBypass = true
		}
		if a.Guard == "burst_rate_limit" && a.Verdict == core.Deny {
			sawBurstGuard = true
		}
		if a.Guard == "local_unconfirmed" && a.Verdict == core.Deny {
			sawUnconfirmedGuard = true
		}
	}
	if !sawLease || !sawAutoBypass || !sawBurstGuard || !sawUnconfirmedGuard {
		t.Fatalf("missing expected audit records: lease=%v auto=%v burst=%v unconfirmed=%v",
			sawLease, sawAutoBypass, sawBurstGuard, sawUnconfirmedGuard)
	}
}
