package extension

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/browser"
	"github.com/nwverse/ensemble/internal/provider/echo"
)

// geminiHub is a hub with one browser computer whose Gemini answers as mode says: "ok", "human" (Google's
// unusual-traffic check), "429" (a real HTTP 429 from Gemini's RPC), "auth" (signed out) or "offline" (the computer
// does not answer).
type geminiHub struct {
	h        *exttest.Host
	club     *core.Club
	mode     string
	runs     int
	machines int // browser.machines calls
}

const humanMachine = "Study Mac"

func startGeminiHub(t *testing.T) *geminiHub {
	t.Helper()
	g := &geminiHub{mode: "ok"}
	g.club = core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	g.h = startHost(t, pac.Manifest, exttest.Serve(t, Program(g.club, pac.Version())), json.RawMessage(`{"browser_providers":true}`))
	g.h.Answer("browser.machines", func(json.RawMessage) (any, error) {
		g.machines++
		return wire.BrowserMachinesResult{Machines: []wire.BrowserMachine{
			{MachineID: "mac-7f3a", Name: humanMachine, Backend: "ego", Online: g.mode != "offline", Ready: g.mode != "offline"}}}, nil
	})
	g.h.Answer("browser.run", func(raw json.RawMessage) (any, error) {
		var p wire.BrowserRunParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		g.runs++
		var value any
		switch g.mode {
		case "offline":
			return nil, errors.New("machine is offline")
		case "human":
			value = map[string]any{"error": "human check"}
		case "429":
			value = map[string]any{"error": "http 429"}
		case "auth":
			value = map[string]any{"error": "auth"}
		default:
			switch {
			case strings.Contains(p.Script, "return { email"):
				value = map[string]any{"email": "ada@example.com", "name": "Ada", "tasks": 1}
			case strings.Contains(p.Script, "ARGS0"):
				value = map[string]any{"sent": true, "clicked": true}
			case strings.Contains(p.Script, `"checkRunning":true`):
				value = map[string]any{"messages": []any{}}
			default:
				value = map[string]any{"tasks": []map[string]any{{"id": "t1", "title": "Trip plan", "at": 1, "status": "ready"}}}
			}
		}
		out, _ := json.Marshal(map[string]any{"value": value, "url": "https://gemini.google.com/app", "title": "Gemini"})
		return wire.BrowserRunResult{Backend: "ego", Output: string(out)}, nil
	})
	return g
}

func (g *geminiHub) pair(t *testing.T) *wire.ActionResult {
	t.Helper()
	return act(t, g.h, pageState{Page: pageBrowserPair, Provider: "gemini_tasks"}, actBrowserPair, map[string]string{fieldMachine: "mac-7f3a|ego"})
}

func (g *geminiHub) space(t *testing.T) *core.Space {
	t.Helper()
	s, err := g.club.Space(context.Background(), core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// surface is what a person reads about the account: its list row, its account page's status and problem rows,
// and what the assistant's club_agents_list says about it.
type surface struct {
	Row, Page, Tool string
	Recheck         bool
	Status          string
}

func (g *geminiHub) surface(t *testing.T, id string) surface {
	t.Helper()
	var s surface
	for _, sec := range render(t, g.h, pageState{}).Sections {
		for _, c := range sec.Items {
			if c.Key == "acct-"+id {
				s.Row = "[" + c.Badge.Text + "] " + c.Subtitle
			}
		}
	}
	page := render(t, g.h, pageState{Page: pageAccount, Account: id})
	var rows []string
	for _, sec := range page.Sections {
		for _, c := range sec.Items {
			switch {
			case c.Title == "现在":
				rows = append(rows, "现在 "+c.Value+" ["+c.Badge.Text+"]")
			case c.Type == "text" && len(sec.Items) > 3 && sec.Items[1].Title == "现在":
				rows = append(rows, c.Text)
			}
		}
	}
	s.Page = strings.Join(rows, " | ")
	for _, sec := range page.Sections {
		for _, c := range sec.Items {
			if c.Type == "button" && c.Title == "重新检查" {
				s.Recheck = true
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{})
	var res wire.ConnectorCallResult
	if err := g.h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: Connector, Name: toolList, Args: raw}, &res); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Accounts []listedAccount `json:"accounts"`
	}
	_ = json.Unmarshal([]byte(strings.SplitN(res.Text, "\n", 2)[0]), &listed)
	for _, a := range listed.Accounts {
		if a.AccountID == id {
			s.Status = a.Status
			s.Tool = "status=" + a.Status + " problem=" + a.Problem
		}
	}
	return s
}

func (g *geminiHub) send(t *testing.T, key string) string {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"session_key": key, "text": "继续"})
	var res wire.ConnectorCallResult
	if err := g.h.Call("connector.actions.do", wire.ConnectorDoParams{Contribution: Connector, Name: toolSend, Args: raw,
		RequestID: "req-" + g.mode + time.Now().String()}, &res); err != nil {
		return "error: " + err.Error()
	}
	return res.Text
}

// paired pairs the account with Gemini answering, hands it to the assistant (confirm) and answers its id and task.
func (g *geminiHub) paired(t *testing.T) (string, string) {
	t.Helper()
	g.mode = "ok"
	if res := g.pair(t); !pairedDone(res) {
		t.Fatalf("pair: %+v", res)
	}
	st, err := g.space(t).Status(context.Background())
	if err != nil || len(st.Accounts) != 1 {
		t.Fatalf("status: %+v %v", st, err)
	}
	id := st.Accounts[0].AccountID
	setControl(t, g.club, id, core.ControlConfirm)
	ss, err := g.space(t).Sessions(context.Background())
	if err != nil || len(ss.Sessions) != 1 {
		t.Fatalf("sessions: %+v %v", ss, err)
	}
	return id, ss.Sessions[0].SessionKey
}

// Google's human check at pairing: the pairing is refused in words that say what happened and what to do, on the
// computer chosen (by its name, never its id), and it is not taken for a rate limit.
func TestGeminiHumanCheckAtPairing(t *testing.T) {
	g := startGeminiHub(t)
	g.mode = "human"
	res := g.pair(t)
	t.Logf("pairing result: %s", res.Toast)
	for _, want := range []string{"Google 怀疑是机器人在访问", "「" + humanMachine + "」", "打开 Persona 控制的那个浏览器（ego lite）",
		"gemini.google.com", "「我不是机器人」", "「检查登录并添加」"} {
		if !strings.Contains(res.Toast, want) {
			t.Errorf("pairing result misses %q: %s", want, res.Toast)
		}
	}
	if res.Pop || strings.Contains(res.Toast, "RATE_LIMITED") || strings.Contains(res.Toast, "HUMAN_CHECK") || strings.Contains(res.Toast, "mac-7f3a") {
		t.Errorf("pairing result: %+v", res)
	}
	g.mode = "429"
	if res := g.pair(t); !strings.Contains(res.Toast, "限流") || strings.Contains(res.Toast, "机器人") ||
		!strings.Contains(res.Toast, "过一会儿再点「检查登录并添加」试一次") || strings.Contains(res.Toast, "不用处理") {
		t.Errorf("a real 429 at pairing: %s", res.Toast)
	}
	g.mode = "auth"
	// Signed out at pairing keeps the pairing page's own words.
	if res := g.pair(t); !strings.Contains(res.Toast, "还没登录好") || !strings.Contains(res.Toast, "「检查登录并添加」") {
		t.Errorf("signed out at pairing: %s", res.Toast)
	}
	t.Logf("pairing result, signed out: %s", g.pair(t).Toast)
	g.mode = "offline"
	if res := g.pair(t); !strings.Contains(res.Toast, "不在线") || !strings.Contains(res.Toast, humanMachine) {
		t.Errorf("offline at pairing: %s", res.Toast)
	}
	t.Logf("pairing result, offline: %s", g.pair(t).Toast)
	g.mode = "429"
	t.Logf("pairing result, 429: %s", g.pair(t).Toast)
}

// Google's human check at sync: the account needs the user (not "正常"), every place says the same words, the poll
// backs off instead of knocking on Google each round, and once the check is solved 「重新检查」 clears it.
func TestGeminiHumanCheckAtSyncAndRecovery(t *testing.T) {
	g := startGeminiHub(t)
	id, _ := g.paired(t)
	g.mode = "human"
	act(t, g.h, pageState{Page: pageAccount, Account: id}, actSync, nil)
	s := g.surface(t, id)
	t.Logf("human check at sync: row=%q page=%q tool=%q recheck=%v", s.Row, s.Page, s.Tool, s.Recheck)
	if !strings.HasPrefix(s.Row, "[要处理]") || !strings.Contains(s.Row, "被 Google 的人机验证拦住了") || strings.Contains(s.Row, "正常") {
		t.Errorf("list row: %s", s.Row)
	}
	if !strings.Contains(s.Page, "现在 要你处理 [要处理]") || !strings.Contains(s.Page, "「"+humanMachine+"」") || !strings.Contains(s.Page, "「重新检查」") || !s.Recheck {
		t.Errorf("account page: %s (recheck %v)", s.Page, s.Recheck)
	}
	if s.Status != protocol.AccountError || !strings.Contains(s.Tool, "Google 怀疑是机器人在访问") || !strings.Contains(s.Tool, humanMachine) {
		t.Errorf("assistant: %s", s.Tool)
	}
	for _, x := range []string{s.Row, s.Page, s.Tool} {
		if strings.Contains(x, "RATE_LIMITED") || strings.Contains(x, "HUMAN_CHECK") || strings.Contains(x, "mac-7f3a") {
			t.Errorf("a code or machine id shown: %s", x)
		}
	}
	acct, _ := g.space(t).Account(context.Background(), id)
	if acct.LastError != provider.CodeHumanCheck {
		t.Fatalf("last error = %q", acct.LastError)
	}

	// Blocked, the poll does not go back to Google at every round.
	before := g.runs
	g.space(t).SyncAll(context.Background())
	if g.runs != before {
		t.Errorf("the poll ran %d browser scripts against a blocked Gemini", g.runs-before)
	}

	// Still blocked, 「重新检查」 says so; solved, it clears.
	before = g.runs
	if res := act(t, g.h, pageState{Page: pageAccount, Account: id}, actRecheck, nil); !strings.HasPrefix(res.Toast, "检查过了，还没好。Google 怀疑") {
		t.Errorf("recheck while blocked: %s", res.Toast)
	}
	// Still failing, it asks once more with a sync of its own (the first may have joined one already running).
	if g.runs-before != 2 {
		t.Errorf("recheck while blocked ran %d browser scripts, want 2", g.runs-before)
	}
	g.mode = "ok"
	res := act(t, g.h, pageState{Page: pageAccount, Account: id}, actRecheck, nil)
	t.Logf("recheck after solving: %s", res.Toast)
	if !strings.Contains(res.Toast, "已经恢复正常") {
		t.Errorf("recheck after solving: %s", res.Toast)
	}
	s = g.surface(t, id)
	t.Logf("recovered: row=%q page=%q tool=%q recheck=%v", s.Row, s.Page, s.Tool, s.Recheck)
	if !strings.HasPrefix(s.Row, "[正常]") || strings.Contains(s.Row, "人机验证") || s.Recheck || s.Status != protocol.AccountActive || strings.Contains(s.Tool, "problem=G") {
		t.Errorf("after recovery: %+v", s)
	}

	// The poll itself clears it too, once it is due again.
	g.mode = "human"
	act(t, g.h, pageState{Page: pageAccount, Account: id}, actSync, nil)
	g.mode = "ok"
	act(t, g.h, pageState{Page: pageAccount, Account: id}, actSync, nil)
	if acct, _ := g.space(t).Account(context.Background(), id); acct.LastError != "" || acct.Status != protocol.AccountActive {
		t.Errorf("after a sync that got through: %+v", acct)
	}
}

// Google's human check at send: refused (nothing typed), said in words to the assistant, and the account needs the
// user from then on.
func TestGeminiHumanCheckAtSend(t *testing.T) {
	g := startGeminiHub(t)
	id, key := g.paired(t)
	g.mode = "human"
	out := g.send(t, key)
	t.Logf("assistant send: %s", out)
	if !strings.HasPrefix(out, "没有发出去：Google 怀疑是机器人在访问") || !strings.Contains(out, humanMachine) || !strings.Contains(out, "转告用户") {
		t.Errorf("send: %s", out)
	}
	if acct, _ := g.space(t).Account(context.Background(), id); acct.Status != protocol.AccountError || acct.LastError != provider.CodeHumanCheck {
		t.Errorf("after a blocked send: %+v", acct)
	}
	g.mode = "ok"
	if out := g.send(t, key); !strings.HasPrefix(out, "已发出") {
		t.Errorf("send after solving: %s", out)
	}
}

// The other errors at sync: a plain sentence and a next step each, distinct from the human check.
func TestGeminiOtherProblemsAtSync(t *testing.T) {
	for _, tc := range []struct {
		mode, row, tool string
		needsUser       bool
	}{
		{"429", "[有点小问题]", "稍后会自动再试", false},
		{"auth", "[要处理]", "重新登录", true},
		{"offline", "[有点小问题]", "确认那台电脑开着", false},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			g := startGeminiHub(t)
			id, key := g.paired(t)
			g.mode = tc.mode
			act(t, g.h, pageState{Page: pageAccount, Account: id}, actSync, nil)
			s := g.surface(t, id)
			t.Logf("%s at sync: row=%q page=%q tool=%q recheck=%v", tc.mode, s.Row, s.Page, s.Tool, s.Recheck)
			t.Logf("%s assistant send: %s", tc.mode, g.send(t, key))
			if !strings.HasPrefix(s.Row, tc.row) || !strings.Contains(s.Tool, tc.tool) || strings.Contains(s.Tool, "机器人") {
				t.Errorf("%s: %+v", tc.mode, s)
			}
		})
	}
}

// problemOf never names a computer by its id and has words for every code.
func TestProblemWords(t *testing.T) {
	for _, code := range []string{provider.CodeHumanCheck, provider.CodeAuthFailed, provider.CodeNetworkError, provider.CodeRateLimited,
		provider.CodeUpstreamError, provider.CodeNotFound, "SOMETHING_NEW"} {
		for _, w := range []place{{Provider: "gemini_tasks", Browser: true}, {Provider: "dots", Browser: true, Machine: "Mac"}, {Provider: "groq"}, {Provider: grokBot}} {
			for _, l := range []lang{"zh-Hans", "en"} {
				p, ok := problemOf(l, code, w)
				if !ok || p.Short == "" || p.What == "" || p.Do == "" {
					t.Errorf("%s %s %+v: %+v", l, code, w, p)
				}
			}
		}
	}
	if _, ok := problemOf("zh", "", place{}); ok {
		t.Error("no error has words")
	}
	p, _ := problemOf("zh", provider.CodeHumanCheck, place{Provider: "gemini_tasks", Browser: true})
	if !strings.Contains(p.Do, "配对这个账号时选的那台电脑") {
		t.Errorf("unknown computer: %s", p.Do)
	}
	p, _ = problemOf("zh", provider.CodeHumanCheck, place{Provider: "dots", Browser: true, Machine: "Mac"})
	if !strings.Contains(p.Text("zh"), "ChatGPT 怀疑") || !strings.Contains(p.Do, "chatgpt.com") {
		t.Errorf("dots: %s", p.Text("zh"))
	}
}

// refusing is a key provider whose Verify refuses with code.
type refusing struct {
	provider.Adapter
	code string
}

func (r refusing) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{}, &provider.Error{Code: r.code}
}

// A failed key pairing sends the user back to the key page's own button, 「添加」, never the browser page's.
func TestKeyPairingNamesItsOwnButton(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{provider.CodeRateLimited, "过一会儿再点「添加」试一次"},
		{provider.CodeNetworkError, "过一会儿再点「添加」试一次"},
		{provider.CodeUpstreamError, "过一会儿再点「添加」试一次"},
		{provider.CodeAuthFailed, "请检查后重新填写，再点「添加」"},
	} {
		reg := provider.NewRegistry()
		reg.Register(refusing{Adapter: echo.New(), code: tc.code})
		club := core.New(core.Options{Registry: reg, FollowUps: []time.Duration{}})
		h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
		res := act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: "echo-12345678"})
		t.Logf("key pairing, %s: %s", tc.code, res.Toast)
		if !strings.Contains(res.Toast, tc.want) || strings.Contains(res.Toast, "检查登录并添加") || strings.Contains(res.Toast, "不用处理") {
			t.Errorf("%s: %s", tc.code, res.Toast)
		}
	}
}

// The computers are asked for only to explain an error, and at most once for one list.
func TestPlacesAreLookedUpOnlyForErrors(t *testing.T) {
	g := startGeminiHub(t)
	id, _ := g.paired(t)
	g.machines = 0
	g.surface(t, id)
	if g.machines != 0 {
		t.Errorf("an account without an error asked for the computers %d times", g.machines)
	}
	// A second account in error on the same computer: one list, one lookup.
	g.mode = "ok"
	st, _ := g.space(t).Status(context.Background())
	_ = st
	g.mode = "human"
	act(t, g.h, pageState{Page: pageAccount, Account: id}, actSync, nil)
	s := g.space(t)
	if err := s.Store().UpdateAccount(context.Background(), id, storePatch(string(core.ControlRead)), 1); err != nil {
		t.Fatal(err)
	}
	g.machines = 0
	raw, _ := json.Marshal(map[string]any{})
	var res wire.ConnectorCallResult
	if err := g.h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: Connector, Name: toolList, Args: raw}, &res); err != nil {
		t.Fatal(err)
	}
	if g.machines != 1 || !strings.Contains(res.Text, humanMachine) {
		t.Errorf("list asked for the computers %d times: %s", g.machines, res.Text)
	}
}

// The standalone driver's own browser is "this computer", never its listed name ("Local ego lite").
func TestLocalComputerIsThisComputer(t *testing.T) {
	pl := newPlacer(nil, &browser.LocalEgoDriver{Bin: "/nonexistent/ego-browser"})
	w := place{Provider: "gemini_tasks", Browser: true}
	pl.name(context.Background(), &w, "local")
	p, _ := problemOf("zh", provider.CodeHumanCheck, w)
	t.Logf("local: %s", p.Text("zh"))
	if !strings.Contains(p.Do, "请到这台电脑上") || strings.Contains(p.Text("zh"), "Local ego lite") {
		t.Errorf("local: %s", p.Text("zh"))
	}
}

// The caveat against another browser is for ego lite only: another backend's borrowed browser may be Chrome itself.
func TestOtherBrowserCaveatOnlyForEgo(t *testing.T) {
	for _, tc := range []struct {
		backend string
		caveat  bool
	}{{"ego", true}, {"", true}, {"cdp", false}} {
		p, _ := problemOf("zh", provider.CodeHumanCheck, place{Provider: "gemini_tasks", Browser: true, Machine: "Mac", Backend: tc.backend})
		if strings.Contains(p.Do, "Chrome") != tc.caveat || strings.Contains(p.Do, "ego lite") != tc.caveat {
			t.Errorf("backend %q: %s", tc.backend, p.Do)
		}
	}
}

// 「重新检查」 on a paused account says it is paused (nothing was checked); a tap asks for the computers once.
func TestRecheckPausedAndOneLookup(t *testing.T) {
	g := startGeminiHub(t)
	id, _ := g.paired(t)
	g.mode = "human"
	act(t, g.h, pageState{Page: pageAccount, Account: id}, actSync, nil)
	g.machines = 0
	act(t, g.h, pageState{Page: pageAccount, Account: id}, actRecheck, nil)
	if g.machines != 1 {
		t.Errorf("one tap asked for the computers %d times", g.machines)
	}
	act(t, g.h, pageState{Page: pageAccount, Account: id}, actPause, nil)
	before := g.runs
	res := act(t, g.h, pageState{Page: pageAccount, Account: id}, actRecheck, nil)
	t.Logf("recheck while paused: %s", res.Toast)
	if !strings.Contains(res.Toast, "已暂停") || !strings.Contains(res.Toast, "「恢复同步」") || strings.Contains(res.Toast, "还没好") || g.runs != before {
		t.Errorf("recheck while paused: %s (%d runs)", res.Toast, g.runs-before)
	}
}

// Standalone, a missing browser program is not "this computer is offline"; an unknown code stays out of the words.
func TestLocalBrowserMissingAndUnknownCode(t *testing.T) {
	pl := newPlacer(nil, &browser.LocalEgoDriver{Bin: "/nonexistent/ego-browser"})
	w := place{Provider: "gemini_tasks", Browser: true}
	pl.name(context.Background(), &w, "local")
	p, _ := problemOf("zh", provider.CodeNetworkError, w)
	t.Logf("local browser missing: %s", p.Text("zh"))
	if !w.Offline || strings.Contains(p.Text("zh"), "不在线") || !strings.Contains(p.What, "这台电脑上用不了") {
		t.Errorf("local browser missing: %s", p.Text("zh"))
	}
	for _, l := range []lang{"zh", "en"} {
		p, _ = problemOf(l, "SOME_NEW_CODE", place{Provider: "gemini_tasks", Browser: true})
		t.Logf("unknown code: %s / %s", p.Short, p.Text(l))
		if strings.Contains(p.Short+p.Text(l), "SOME_NEW_CODE") || p.Code != "SOME_NEW_CODE" {
			t.Errorf("unknown code: %+v", p)
		}
	}
}
