package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

// pairedDone says whether a pairing ended on the done step (in place of the pairing page).
func pairedDone(res *wire.ActionResult) bool {
	if res == nil || res.View == nil || res.Pop {
		return false
	}
	var st pageState
	return json.Unmarshal([]byte(res.View.State), &st) == nil && st.Page == pageDone && st.Account != ""
}

func stateFrom(t *testing.T, v *wire.View) pageState {
	t.Helper()
	var st pageState
	if err := json.Unmarshal([]byte(v.State), &st); err != nil {
		t.Fatalf("state %q: %v", v.State, err)
	}
	return st
}

// segmentedValue is the chosen value of the page's segmented control called name.
func segmentedValue(v *wire.View, name string) (string, bool) {
	for _, s := range v.Sections {
		for _, c := range s.Items {
			if c.Type == "segmented" && c.Name == name {
				return c.Value, true
			}
		}
	}
	return "", false
}

// A key pairing ends on a step that says what was added and where its chats will show, and asks about 助手指挥 with
// each level explained and the current one (off) chosen. Choosing saves at once and says what changed; leaving
// without choosing keeps it off and says so. New accounts still start off: the step asks, it does not decide.
func TestKeyPairingEndsOnTheDoneStep(t *testing.T) {
	if core.DefaultControl != core.ControlOff {
		t.Fatalf("new accounts start at %q, not off", core.DefaultControl)
	}
	h, club := start(t)
	ctx := context.Background()
	res := act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential, fieldName: "工作用演示"})
	if !pairedDone(res) || res.Toast != "已添加：工作用演示" {
		t.Fatalf("pair: %+v", res)
	}
	v := res.View
	noJargon(t, "done step", v)
	all := text(v)
	for _, want := range []string{"已添加「工作用演示」", "「Agents」页", "要让 Persona 助手用这个账号吗？",
		"关闭：Persona 助手完全看不到这个账号", "只读：Persona 助手能读它的对话", "可代发（逐次确认）：还能替你给它发消息，每次发之前都先在手机上问你",
		"不选就一直保持关闭", "要不要让它直接当你的主助手？", "和上面不一样", "好了", "打开这个账号"} {
		if !strings.Contains(all, want) {
			t.Errorf("done step lacks %q: %s", want, all)
		}
	}
	if val, ok := segmentedValue(v, fieldControl); !ok || val != string(core.ControlOff) {
		t.Fatalf("助手指挥 preselects %q (%v)", val, ok)
	}
	st := stateFrom(t, v)
	space, _ := club.Space(ctx, core.DefaultSpace)
	if c, _ := space.AssistantControl(ctx, st.Account); c != core.ControlOff {
		t.Fatalf("pairing set the level to %q", c)
	}

	// Each level saves at once, is confirmed in words, and the step stays, showing it chosen.
	for _, c := range []struct {
		level core.Control
		toast string
		now   string
	}{
		{core.ControlRead, "已设为只读：Persona 助手现在能读这个账号的对话，但不会替你发消息", "现在是「只读」"},
		{core.ControlConfirm, "已设为可代发：Persona 助手能读，也能替你发消息，每次发之前先在手机上问你", "现在是「可代发（逐次确认）」"},
		{core.ControlOff, "已关闭：Persona 助手现在看不到这个账号", "现在是「关闭」"},
	} {
		res := act(t, h, st, actControl, map[string]string{fieldControl: string(c.level)})
		if res.Toast != c.toast || res.View == nil || stateFrom(t, res.View).Page != pageDone {
			t.Fatalf("choose %s: %+v", c.level, res)
		}
		if val, _ := segmentedValue(res.View, fieldControl); val != string(c.level) || !strings.Contains(text(res.View), c.now) ||
			!strings.Contains(text(res.View), c.toast) {
			t.Fatalf("after %s: %s", c.level, text(res.View))
		}
		if got, _ := space.AssistantControl(ctx, st.Account); got != c.level {
			t.Fatalf("saved %q, want %q", got, c.level)
		}
	}

	// Leaving without choosing (off): back to the list, saying the assistant still cannot use it.
	if res := act(t, h, st, actFinish, nil); !res.Pop || !strings.Contains(res.Toast, "Persona 助手还不能用这个账号") {
		t.Fatalf("finish while off: %+v", res)
	}
	if c, _ := space.AssistantControl(ctx, st.Account); c != core.ControlOff {
		t.Fatalf("leaving set the level to %q", c)
	}
	act(t, h, st, actControl, map[string]string{fieldControl: string(core.ControlRead)})
	if res := act(t, h, st, actFinish, nil); !res.Pop || res.Toast != "" {
		t.Fatalf("finish after choosing: %+v", res)
	}

	// The main assistant question saves too.
	res = act(t, h, st, actAssistantBackend, map[string]string{fieldAssistantBackend: "on"})
	if !strings.HasPrefix(res.Toast, "已设为主助手") || stateFrom(t, res.View).Page != pageDone || !strings.Contains(text(res.View), "现在是主助手") {
		t.Fatalf("main assistant: %+v", res)
	}
	if a, _ := space.Account(ctx, st.Account); !a.AssistantBackend {
		t.Fatal("not a main assistant")
	}

	// 打开这个账号 shows its page in place of the step.
	res = act(t, h, st, actOpenAccount, nil)
	if res.View == nil || res.View.Title != "工作用演示" || stateFrom(t, res.View) != (pageState{Page: pageAccount, Account: st.Account}) {
		t.Fatalf("open account: %+v", res)
	}

	// The list row says the level in words.
	if list := render(t, h, pageState{}); !strings.Contains(text(list), "Persona 助手：只读") {
		t.Fatalf("list row: %s", text(list))
	}
}

func TestDoneStepEnglish(t *testing.T) {
	h, _ := start(t)
	var res wire.ActionResult
	if err := h.Call("ui.action", wire.UIParams{Contribution: AccountsUI, Placement: wire.PlacementPluginPage, Size: "full",
		UIVersion: 6, Locale: "en", State: stateOf(pageState{Page: pagePair, Provider: echo.Name}), Action: actPair,
		Values: map[string]string{fieldCredential: demoCredential}}, &res); err != nil {
		t.Fatal(err)
	}
	if !pairedDone(&res) || res.View.Title != "Added" || !strings.Contains(text(res.View), "Should the Persona assistant use this account?") ||
		!strings.Contains(text(res.View), "Leave it and it stays off") {
		t.Fatalf("en done step: %+v %s", res, text(res.View))
	}
	for _, r := range text(res.View) {
		if r >= 0x4e00 && r <= 0x9fff {
			t.Fatalf("en done step has Chinese: %s", text(res.View))
		}
	}
}

// mute is a key provider whose account has no chat to write in and cannot start one: it cannot be a main assistant.
type mute struct{}

func (mute) Name() string       { return "cue" }
func (mute) Connection() string { return protocol.ConnectionServer }
func (mute) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: "mute-1", DisplayName: "Cue · 小明"}, nil
}
func (mute) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "main", Title: "Cue", Kind: protocol.KindMain}}, nil
}
func (mute) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}
func (mute) Send(context.Context, string, provider.SessionRef, provider.Outgoing, provider.SendContext) (provider.SendResult, error) {
	return provider.SendResult{}, &provider.Error{Code: "READ_ONLY"}
}

// The 主助手 question is asked only when the setting lets accounts be main assistants and the account can chat.
func TestDoneStepMainAssistantOnlyWhenItCanBeOne(t *testing.T) {
	for _, c := range []struct {
		name    string
		adapter provider.Adapter
		config  string
		asked   bool
	}{
		{"can chat, setting on", &flaky{}, `{}`, true},
		{"can chat, setting off", &flaky{}, `{"assistant_runtime":false}`, false},
		{"cannot chat, setting on", mute{}, `{}`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			club := core.New(core.Options{Registry: provider.NewRegistry(c.adapter), FollowUps: []time.Duration{}})
			h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(c.config))
			res := act(t, h, pageState{Page: pagePair, Provider: c.adapter.Name()}, actPair, map[string]string{fieldCredential: "fixture-key-0000001"})
			if !pairedDone(res) {
				t.Fatalf("pair: %+v", res)
			}
			noJargon(t, "done step", res.View)
			_, asked := segmentedValue(res.View, fieldAssistantBackend)
			if asked != c.asked || strings.Contains(text(res.View), "主助手") != c.asked {
				t.Fatalf("主助手 asked = %v, want %v: %s", asked, c.asked, text(res.View))
			}
			if _, ok := segmentedValue(res.View, fieldControl); !ok {
				t.Fatalf("助手指挥 not asked: %s", text(res.View))
			}
		})
	}
}

// A new Grok Bot sign-in ends on the same step, with no second 开始登录; choosing there saves for the new account.
func TestGrokBotSignInEndsOnTheDoneStep(t *testing.T) {
	grok := &fakeGrok{}
	club := core.New(core.Options{Registry: provider.NewRegistry(grok), FollowUps: []time.Duration{}, WatchLimit: -1})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{}`))
	res := act(t, h, pageState{Page: pageGrok}, actGrokStart, nil)
	st := stateFrom(t, res.View)
	grok.answer(grokCredential)
	v := render(t, h, st)
	noJargon(t, "grok done step", v)
	done := stateFrom(t, v)
	if done.Page != pageDone || done.Login != "" || strings.Contains(text(v), "开始登录") || strings.Contains(text(v), grokCredential) ||
		!strings.Contains(text(v), "已添加「Grok Bot」") || !strings.Contains(text(v), "要不要让它直接当你的主助手？") {
		t.Fatalf("grok done: %s", text(v))
	}
	// Drawn again (the page refreshes), it stays the done step and does not poll the finished sign-in.
	if again := render(t, h, done); stateFrom(t, again).Page != pageDone || strings.Contains(text(again), "已过期") {
		t.Fatalf("grok done redrawn: %s", text(again))
	}
	if res := act(t, h, done, actControl, map[string]string{fieldControl: string(core.ControlConfirm)}); !strings.HasPrefix(res.Toast, "已设为可代发") {
		t.Fatalf("choose: %+v", res)
	}
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	if c, _ := space.AssistantControl(context.Background(), done.Account); c != core.ControlConfirm {
		t.Fatalf("saved %q", c)
	}
	if res := act(t, h, done, actFinish, nil); !res.Pop {
		t.Fatalf("finish: %+v", res)
	}
}
