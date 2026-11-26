package extension

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/datadir"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
	pacruntime "github.com/nwverse/personal-agent-club/internal/runtime"
	"github.com/nwverse/personal-agent-club/internal/store"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

func storePatch(level string) store.AccountPatch { return store.AccountPatch{AssistantControl: &level} }

const demoCredential = "echo-synthetic-0001"

// startHost is exttest.Start with machines.status answered as a hub without the computer does: the accounts page
// reads each account's takeover machine, and the host's own answer ({}) does not hold to the call's schema.
func startHost(t *testing.T, manifest []byte, run exttest.Run, config json.RawMessage) *exttest.Host {
	t.Helper()
	h := exttest.Start(t, manifest, run, config)
	h.Answer(methodMachinesStatus, func(raw json.RawMessage) (any, error) {
		var p machineStatusParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return machineStatusResult{MachineID: p.MachineID}, nil
	})
	return h
}

func start(t *testing.T) (*exttest.Host, *core.Club) {
	t.Helper()
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true}`))
	return h, club
}

func render(t *testing.T, h *exttest.Host, st pageState) *wire.View {
	t.Helper()
	var v wire.View
	if err := h.Call("ui.render", wire.UIParams{Contribution: AccountsUI, Placement: wire.PlacementPluginPage, Size: "full",
		UIVersion: 6, Locale: "zh-Hans", State: stateOf(st)}, &v); err != nil {
		t.Fatalf("ui.render %+v: %v", st, err)
	}
	checkView(t, &v)
	return &v
}

func act(t *testing.T, h *exttest.Host, st pageState, action string, values map[string]string) *wire.ActionResult {
	t.Helper()
	var res wire.ActionResult
	if err := h.Call("ui.action", wire.UIParams{Contribution: AccountsUI, Placement: wire.PlacementPluginPage, Size: "full",
		UIVersion: 6, Locale: "zh-Hans", State: stateOf(st), Action: action, Values: values}, &res); err != nil {
		t.Fatalf("ui.action %s: %v", action, err)
	}
	if res.View != nil {
		checkView(t, res.View)
	}
	return &res
}

func stateOf(st pageState) string {
	if st == (pageState{}) {
		return ""
	}
	b, _ := json.Marshal(st)
	return string(b)
}

func checkView(t *testing.T, v *wire.View) {
	t.Helper()
	raw, _ := json.Marshal(v)
	if err := exttest.CheckView(raw, false); err != nil {
		t.Fatalf("view the apps cannot draw: %v\n%s", err, raw)
	}
	if strings.Contains(string(raw), demoCredential) {
		t.Fatalf("a view carries the credential: %s", raw)
	}
}

func text(v *wire.View) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func TestAccountsPage(t *testing.T) {
	h, club := start(t)
	v := render(t, h, pageState{})
	if !strings.Contains(text(v), "还没有接入任何账号") || !strings.Contains(text(v), "从下面选一个你常用的服务添加") || !strings.Contains(text(v), "演示账号") {
		t.Fatalf("empty list: %s", text(v))
	}

	// pairing: the key goes in and comes back nowhere
	if res := act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: "  "}); !strings.Contains(res.Toast, "还没填密钥") {
		t.Fatalf("empty key: %+v", res)
	}
	res := act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential, fieldName: "Demo"})
	if !pairedDone(res) || !strings.Contains(res.Toast, "Demo") {
		t.Fatalf("pair: %+v", res)
	}
	st, err := club.Space(context.Background(), core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	status, _ := st.Status(context.Background())
	if len(status.Accounts) != 1 {
		t.Fatalf("accounts: %+v", status.Accounts)
	}
	id := status.Accounts[0].AccountID
	if v := render(t, h, pageState{}); !strings.Contains(text(v), "Demo") || !strings.Contains(text(v), "正常") {
		t.Fatalf("list: %s", text(v))
	}
	page := pageState{Page: pageAccount, Account: id}
	if v := render(t, h, page); !strings.Contains(text(v), fieldControl) || !strings.Contains(text(v), "立即同步") {
		t.Fatalf("account: %s", text(v))
	}
	if res := act(t, h, page, actSync, nil); res.Toast != "已同步，最新的对话已经拉下来了" {
		t.Fatalf("sync: %+v", res)
	}
	// pause and the assistant control level go through club.accounts.update
	if res := act(t, h, page, actPause, nil); !strings.Contains(res.Toast, "已暂停同步") || !strings.Contains(text(res.View), "已暂停") {
		t.Fatalf("pause: %+v", res)
	}
	if res := act(t, h, page, actResume, nil); !strings.Contains(res.Toast, "已恢复同步") || !strings.Contains(text(res.View), "正常") {
		t.Fatalf("resume: %+v", res)
	}
	if res := act(t, h, page, actControl, map[string]string{fieldControl: "read"}); !strings.Contains(res.Toast, "已设为只读") {
		t.Fatalf("control: %+v", res)
	}
	if c, _ := st.AssistantControl(context.Background(), id); c != core.ControlRead {
		t.Fatalf("control is %s", c)
	}
	// without the grok_bot provider in this club, its sign-in says it is not available
	if res := act(t, h, pageState{Page: pageGrok}, actGrokStart, nil); !strings.Contains(res.Toast, "还没打开") {
		t.Fatalf("grok start: %+v", res)
	}
	if res := act(t, h, page, actRemove, nil); !res.Pop {
		t.Fatalf("remove: %+v", res)
	}
	if v := render(t, h, page); !strings.Contains(text(v), "已不在") {
		t.Fatalf("removed account: %s", text(v))
	}
}

func TestAccountsPageEnglish(t *testing.T) {
	h, _ := start(t)
	var v wire.View
	if err := h.Call("ui.render", wire.UIParams{Contribution: AccountsUI, Placement: wire.PlacementPluginPage, Size: "full",
		UIVersion: 3, Locale: "en"}, &v); err != nil {
		t.Fatal(err)
	}
	checkView(t, &v)
	if !strings.Contains(text(&v), "No accounts yet") {
		t.Fatalf("en: %s", text(&v))
	}
}

func TestConnectorChecks(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry()})
	h := exttest.CheckConnectors(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())))
	var items []wire.ConnectorItem
	if err := json.Unmarshal(h.Init.Contributes["connectors"], &items); err != nil || len(items) != 1 || items[0].ID != Connector {
		t.Fatalf("initialize does not say it has the connector: %s (%v)", h.Init.Contributes["connectors"], err)
	}
	// the hub tells an unknown method from one the source cannot do
	err := h.Call("agentsrc.nonexistent_method", map[string]any{"contribution": Contribution}, nil)
	if e, ok := err.(*ext.Error); !ok || e.Code != wire.ErrorCodeUnknownMethod {
		t.Fatalf("unknown agentsrc method: %v", err)
	}
}

func setControl(t *testing.T, club *core.Club, id string, level core.Control) {
	t.Helper()
	s, _ := club.Space(context.Background(), core.DefaultSpace)
	v := string(level)
	if err := s.Store().UpdateAccount(context.Background(), id, storePatch(v), 1); err != nil {
		t.Fatal(err)
	}
}

func TestAssistantTools(t *testing.T) {
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	id := status.Accounts[0].AccountID
	sessions, _ := space.Sessions(context.Background())
	var main, notes string
	for _, s := range sessions.Sessions {
		if s.CanSend {
			main = s.SessionKey
		} else {
			notes = s.SessionKey
		}
	}
	if main == "" || notes == "" {
		t.Fatalf("sessions: %+v", sessions)
	}

	callTool := func(name string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorCallResult
		err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: Connector, Name: name, Args: raw}, &res)
		return res.Text, err
	}
	describe := func(name string, args any) (wire.ConnectorDescribeResult, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorDescribeResult
		err := h.Call("connector.actions.describe", wire.ConnectorCallParams{Contribution: Connector, Name: name, Args: raw}, &res)
		return res, err
	}
	do := func(name string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorCallResult
		err := h.Call("connector.actions.do", wire.ConnectorDoParams{Contribution: Connector, Name: name, Args: raw, RequestID: "req-1"}, &res)
		return res.Text, err
	}

	// off (the default): the account does not exist for the assistant, but the connector stays ok so the assistant
	// can list and learn that something is paired and where the user hands it over
	var cs wire.ConnectorStatus
	if err := h.Call("connector.status", wire.ConnectorParams{Contribution: Connector}, &cs); err != nil || cs.State != wire.ConnectorStateOk || cs.Error != "" {
		t.Fatalf("status at off: %+v %v", cs, err)
	}
	if out, err := callTool(toolList, map[string]any{}); err != nil || strings.Contains(out, id) || strings.Contains(out, main) ||
		!strings.Contains(out, "助手指挥") || !strings.Contains(out, "只读") {
		t.Fatalf("list at off: %q %v", out, err)
	}
	if _, err := callTool(toolRead, map[string]any{"session_key": main}); err == nil {
		t.Fatal("read at off answered")
	}

	// read: list and read, no writes
	setControl(t, club, id, core.ControlRead)
	if err := h.Call("connector.status", wire.ConnectorParams{Contribution: Connector}, &cs); err != nil || cs.State != wire.ConnectorStateOk {
		t.Fatalf("status at read: %+v %v", cs, err)
	}
	if out, err := callTool(toolList, map[string]any{"provider": echo.Name}); err != nil || !strings.Contains(out, main) {
		t.Fatalf("list at read: %q %v", out, err)
	}
	if out, err := callTool(toolRead, map[string]any{"session_key": main, "turns": 2}); err != nil || !strings.Contains(out, "不可信") {
		t.Fatalf("read at read: %q %v", out, err)
	}
	if _, err := describe(toolSend, map[string]any{"session_key": main, "text": "hi"}); err == nil {
		t.Fatal("send described at read")
	}
	if _, err := do(toolSend, map[string]any{"session_key": main, "text": "hi"}); err == nil {
		t.Fatal("send done at read")
	}

	// confirm: writes, after the gate
	setControl(t, club, id, core.ControlConfirm)
	d, err := describe(toolSend, map[string]any{"session_key": main, "text": "hello"})
	if err != nil || d.Summary != "hello" {
		t.Fatalf("describe send: %+v %v", d, err)
	}
	if _, err := describe(toolSend, map[string]any{"session_key": notes, "text": "hello"}); err == nil {
		t.Fatal("send to a read-only session described")
	}
	out, err := do(toolSend, map[string]any{"session_key": main, "text": "hello"})
	if err != nil || !strings.Contains(out, "已发出") {
		t.Fatalf("send: %q %v", out, err)
	}
	if again, err := do(toolSend, map[string]any{"session_key": main, "text": "hello"}); err != nil || again != out {
		t.Fatalf("the same request again: %q %v", again, err)
	}
	if out, err := callTool(toolRead, map[string]any{"session_key": main}); err != nil || !strings.Contains(out, "echo: hello") {
		t.Fatalf("read after send: %q %v", out, err)
	}
	// start and interrupt: not in this core yet
	if _, err := describe(toolStart, map[string]any{"account_id": id, "text": "x"}); err == nil || !strings.Contains(err.Error(), "还不能") {
		t.Fatalf("start: %v", err)
	}
	if _, err := describe(toolInterrupt, map[string]any{"session_key": main}); err == nil || !strings.Contains(err.Error(), "不能打断") {
		t.Fatalf("interrupt: %v", err)
	}
	if d, err := describe(toolInterrupt, map[string]any{"session_key": main, "then": "stop"}); err != nil || !strings.Contains(d.Action.Name, "排队") {
		t.Fatalf("interrupt with then: %+v %v", d, err)
	}
}

func TestClientIDIsAUUID(t *testing.T) {
	a, b := clientID("r1"), clientID("r1")
	if a != b || !core.ValidClientID(a) || clientID("r2") == a {
		t.Fatalf("client ids %q %q", a, b)
	}
}

func TestReasonHidesTheSecret(t *testing.T) {
	got := reason("zh", protocol.Invalid("bad key "+demoCredential), demoCredential)
	if strings.Contains(got, demoCredential) || !strings.HasPrefix(got, "填写的内容不对") {
		t.Fatalf("reason: %q", got)
	}
}

func TestNewClubKeepsItsDataInTheDataDirectory(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	t.Setenv("PERSONA_EXTENSION_DATA", data)
	open := func() (*core.Club, *core.Space) {
		t.Helper()
		club, h, err := NewClub("0.0.0", discard())
		if err != nil {
			t.Fatal(err)
		}
		if h.DataDir != data || h.KeyFile != filepath.Join(data, KeyFileName) || h.HTTP == nil {
			t.Fatalf("host %+v", h)
		}
		for _, p := range []string{"cue", "grok_bot", "groq"} {
			if _, ok := club.Registry().Get(p); !ok {
				t.Fatalf("no %s provider", p)
			}
		}
		club.Registry().Register(echo.New())
		s, err := club.Space(ctx, core.DefaultSpace)
		if err != nil {
			t.Fatal(err)
		}
		return club, s
	}
	club, s := open()
	a, err := s.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: demoCredential})
	if err != nil {
		t.Fatal(err)
	}
	// a second owner is refused while the program runs
	if _, err := datadir.Open(data, filepath.Join(data, KeyFileName)); !errors.Is(err, datadir.ErrInUse) {
		t.Fatalf("a second owner: %v", err)
	}
	club.Close()
	for _, p := range []string{KeyFileName, "club.lock", "spaces/default/club.db", "spaces/default/credentials.enc"} {
		fi, err := os.Stat(filepath.Join(data, p))
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(data, "spaces", "default")); fi.Mode().Perm() != 0o700 {
		t.Fatalf("space directory mode %v", fi.Mode())
	}
	raw, _ := os.ReadFile(filepath.Join(data, "spaces", "default", "credentials.enc"))
	if strings.Contains(string(raw), demoCredential) {
		t.Fatal("the credential is stored in clear")
	}

	// closing the club released the directory; the next start finds the account and its credential
	club, s = open()
	defer club.Close()
	if got, err := s.Account(ctx, a.AccountID); err != nil || got.Provider != echo.Name {
		t.Fatalf("%+v %v", got, err)
	}
	if err := s.SyncAccount(ctx, a.AccountID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Account(ctx, a.AccountID); got.Status != protocol.AccountActive || got.LastError != "" || got.LastSyncedAt == 0 {
		t.Fatalf("a sync after the restart: %+v", got)
	}
}

func TestNewClubWithoutADataDirectoryKeepsItInMemory(t *testing.T) {
	t.Setenv("PERSONA_EXTENSION_DATA", "")
	club, h, err := NewClub("0.0.0", discard())
	if err != nil {
		t.Fatal(err)
	}
	defer club.Close()
	if h.DataDir != "" || h.KeyFile != "" {
		t.Fatalf("%+v", h)
	}
	if _, err := club.Space(context.Background(), core.DefaultSpace); err != nil {
		t.Fatal(err)
	}
}

// The providers' client goes out through the proxy the hub names in the environment, as the sandbox requires. The
// environment is read once per process, so the check runs in a child.
func TestHTTPClientUsesTheHubProxy(t *testing.T) {
	if os.Getenv("PAC_PROXY_CHILD") != "" {
		res, err := HTTPClient().Get("http://api.groq.com/openai/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusTeapot {
			t.Fatalf("status %d", res.StatusCode)
		}
		return
	}
	hosts := make(chan string, 1)
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts <- r.Host
		w.WriteHeader(http.StatusTeapot)
	}))
	defer proxy.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHTTPClientUsesTheHubProxy$")
	cmd.Env = append(os.Environ(), "PAC_PROXY_CHILD=1", "HTTP_PROXY="+proxy.URL, "http_proxy="+proxy.URL, "NO_PROXY=", "no_proxy=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child: %v\n%s", err, out)
	}
	select {
	case host := <-hosts:
		if host != "api.groq.com" {
			t.Fatalf("the proxy saw %q", host)
		}
	default:
		t.Fatal("the request did not go through the proxy")
	}
}

// fakeGrok is a grok_bot provider whose sign-in a test steers: each poll takes the next answer ("" pending, a
// credential, or an error).
type fakeGrok struct {
	mu      sync.Mutex
	polls   []any
	started int
}

const grokCredential = "fixture-grok-signin-token-0001"

func (f *fakeGrok) Name() string       { return grokBot }
func (f *fakeGrok) Connection() string { return protocol.ConnectionServer }
func (f *fakeGrok) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: "grok-remote-1"}, nil
}
func (f *fakeGrok) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "bot/", Title: "Grok 秘书", CanSend: true}}, nil
}
func (f *fakeGrok) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}
func (f *fakeGrok) Send(context.Context, string, provider.SessionRef, provider.Outgoing, provider.SendContext) (provider.SendResult, error) {
	return provider.SendResult{Status: provider.Sent}, nil
}
func (f *fakeGrok) StartLogin() (provider.LoginFlow, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	return provider.LoginFlow{URL: fmt.Sprintf("https://login.example.com/approve?n=%d", f.started)}, nil
}
func (f *fakeGrok) PollLogin(context.Context, provider.LoginFlow) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.polls) == 0 {
		return "", nil
	}
	next := f.polls[0]
	f.polls = f.polls[1:]
	if err, ok := next.(error); ok {
		return "", err
	}
	return next.(string), nil
}

func (f *fakeGrok) answer(polls ...any) {
	f.mu.Lock()
	f.polls = polls
	f.mu.Unlock()
}

func TestGrokBotSignIn(t *testing.T) {
	grok := &fakeGrok{}
	club := core.New(core.Options{Registry: provider.NewRegistry(grok), FollowUps: []time.Duration{}, WatchLimit: -1})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{}`))
	ctx := context.Background()
	noToken := func(v *wire.View) {
		t.Helper()
		if strings.Contains(text(v), grokCredential) {
			t.Fatalf("a view carries the token: %s", text(v))
		}
	}

	// start: the approval page opens, the page polls and stays pending
	res := act(t, h, pageState{Page: pageGrok}, actGrokStart, nil)
	if res.OpenURL != "https://login.example.com/approve?n=1" || !strings.Contains(text(res.View), "打开授权页") {
		t.Fatalf("start: %+v %s", res, text(res.View))
	}
	var st pageState
	if err := json.Unmarshal([]byte(res.View.State), &st); err != nil || st.Login == "" || st.LoginURL != res.OpenURL {
		t.Fatalf("state %q %v", res.View.State, err)
	}
	if v := render(t, h, st); !strings.Contains(text(v), "立即检查") || v.RefreshAfter != 30 {
		t.Fatalf("pending: %s", text(v))
	}

	// two sign-ins may wait at once, a third is refused with words for it
	second := act(t, h, pageState{Page: pageGrok}, actGrokStart, nil)
	if second.OpenURL == "" {
		t.Fatalf("a second sign-in: %+v", second)
	}
	if third := act(t, h, pageState{Page: pageGrok}, actGrokStart, nil); third.OpenURL != "" || !strings.Contains(third.Toast, "两个登录") {
		t.Fatalf("a third sign-in: %+v", third)
	}
	var st2 pageState
	_ = json.Unmarshal([]byte(second.View.State), &st2)
	if res := act(t, h, st2, actGrokStop, nil); !strings.Contains(res.Toast, "已取消") {
		t.Fatalf("cancel: %+v", res)
	}
	if v := render(t, h, st2); !strings.Contains(text(v), "已过期") {
		t.Fatalf("a canceled sign-in: %s", text(v))
	}

	// a provider hiccup keeps the sign-in; approval pairs the account, and the token is shown nowhere
	grok.answer(&provider.Error{Code: "NETWORK_ERROR", Retryable: true}, grokCredential)
	if v := render(t, h, st); !strings.Contains(text(v), "暂时查不到登录结果") || strings.Contains(text(v), "NETWORK_ERROR") || !strings.Contains(text(v), "立即检查") {
		t.Fatalf("a retryable failure: %s", text(v))
	}
	v := render(t, h, st)
	noToken(v)
	if !strings.Contains(text(v), "要让 Persona 助手用这个账号吗？") || strings.Contains(text(v), "开始登录") {
		t.Fatalf("done: %s", text(v))
	}
	space, _ := club.Space(ctx, core.DefaultSpace)
	status, _ := space.Status(ctx)
	if len(status.Accounts) != 1 || status.Accounts[0].Provider != grokBot {
		t.Fatalf("%+v", status.Accounts)
	}
	id := status.Accounts[0].AccountID
	// the finished sign-in is gone: polling it again says so
	if v := render(t, h, st); !strings.Contains(text(v), "已过期") {
		t.Fatalf("a finished sign-in again: %s", text(v))
	}

	// signing the account in again replaces its credential; a refusal ends the sign-in
	page := render(t, h, pageState{Page: pageAccount, Account: id})
	if !strings.Contains(text(page), "重新登录") {
		t.Fatalf("no sign-in-again on a Grok Bot account: %s", text(page))
	}
	again := act(t, h, pageState{Page: pageGrok, Account: id}, actGrokStart, nil)
	var sa pageState
	_ = json.Unmarshal([]byte(again.View.State), &sa)
	if sa.Account != id || sa.Login == "" {
		t.Fatalf("%+v", sa)
	}
	grok.answer(&provider.Error{Code: "LOGIN_REFUSED", HTTPStatus: 403})
	if v := render(t, h, sa); !strings.Contains(text(v), "Grok 没有同意这次登录") || strings.Contains(text(v), "LOGIN_REFUSED") || !strings.Contains(text(v), "开始登录") {
		t.Fatalf("a refused sign-in: %s", text(v))
	}
	again = act(t, h, pageState{Page: pageGrok, Account: id}, actGrokStart, nil)
	_ = json.Unmarshal([]byte(again.View.State), &sa)
	grok.answer("fixture-grok-signin-token-0002")
	if v := render(t, h, sa); !strings.Contains(text(v), "已重新登录") || strings.Contains(text(v), "开始登录") {
		t.Fatalf("signed in again: %s", text(v))
	}
	if status, _ := space.Status(ctx); len(status.Accounts) != 1 {
		t.Fatalf("signing in again paired another account: %+v", status.Accounts)
	}
}

func TestAnExhaustedAccountIsNotSentToByTheAssistant(t *testing.T) {
	if err := exhausted(nil); err != nil {
		t.Fatalf("unknown quota: %v", err)
	}
	if err := exhausted(&protocol.Quota{State: protocol.QuotaLow}); err != nil {
		t.Fatalf("low quota: %v", err)
	}
	for _, q := range []*protocol.Quota{{State: protocol.QuotaExhausted}, {State: protocol.QuotaExhausted, ResetsAt: 1_800_000_000_000}} {
		if err := exhausted(q); protocol.CodeOf(err) != protocol.CodeConflict {
			t.Fatalf("exhausted quota %+v: %v", q, err)
		}
	}
}

func TestAssistantToolsExcludeActiveAssistantSessions(t *testing.T) {
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	id := status.Accounts[0].AccountID
	setControl(t, club, id, core.ControlConfirm)

	sessions, _ := space.Sessions(context.Background())
	var main, notes string
	for _, s := range sessions.Sessions {
		if s.CanSend {
			main = s.SessionKey
		} else {
			notes = s.SessionKey
		}
	}
	if err := space.SetAssistantSessionKeys(context.Background(), []string{main}); err != nil {
		t.Fatal(err)
	}

	callTool := func(name string, args any) (string, error) {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorCallResult
		err := h.Call("connector.tools.call", wire.ConnectorCallParams{Contribution: Connector, Name: name, Args: raw}, &res)
		return res.Text, err
	}
	describe := func(name string, args any) error {
		raw, _ := json.Marshal(args)
		var res wire.ConnectorDescribeResult
		return h.Call("connector.actions.describe", wire.ConnectorCallParams{Contribution: Connector, Name: name, Args: raw}, &res)
	}

	out, err := callTool(toolList, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, main) || !strings.Contains(out, notes) {
		t.Fatalf("expected active assistant session %s excluded and side session %s kept, got: %s", main, notes, out)
	}
	if _, err := callTool(toolRead, map[string]any{"session_key": main}); err == nil {
		t.Fatal("club_agent_read should reject active assistant backing session")
	}
	if err := describe(toolSend, map[string]any{"session_key": main, "text": "loop"}); err == nil {
		t.Fatal("club_agent_send should reject active assistant backing session")
	}
}

func TestAccountMainAssistantToggle(t *testing.T) {
	h, club := start(t)
	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, err := club.Space(context.Background(), core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	status, err := space.Status(context.Background())
	if err != nil || len(status.Accounts) != 1 {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	id := status.Accounts[0].AccountID

	v := render(t, h, pageState{Page: pageAccount, Account: id})
	if !strings.Contains(text(v), "主助手") {
		t.Fatalf("account page missing Main assistant section: %s", text(v))
	}

	act(t, h, pageState{Page: pageAccount, Account: id}, actAssistantBackend, map[string]string{fieldAssistantBackend: "on"})
	status, _ = space.Status(context.Background())
	if !status.Accounts[0].AssistantBackend {
		t.Fatalf("expected AssistantBackend=true after enabling, got %+v", status.Accounts[0])
	}
	listV := render(t, h, pageState{})
	if !strings.Contains(text(listV), "主助手") {
		t.Fatalf("account list missing main assistant tag: %s", text(listV))
	}

	act(t, h, pageState{Page: pageAccount, Account: id}, actAssistantBackend, map[string]string{fieldAssistantBackend: "off"})
	status, _ = space.Status(context.Background())
	if status.Accounts[0].AssistantBackend {
		t.Fatalf("expected AssistantBackend=false after disabling, got %+v", status.Accounts[0])
	}
}

func TestEmbeddedAssistantRuntimeBridgeConnectsToPersonaSocket(t *testing.T) {
	sockDir, err := os.MkdirTemp("/tmp", "pac-rt-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })

	sock := filepath.Join(sockDir, "rt.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	type helloMsg struct {
		Caps      pacruntime.RuntimeCaps `json:"caps"`
		Launchers []pacruntime.Launcher  `json:"launchers"`
	}
	hellos := make(chan helloMsg, 4)
	launcherUpdates := make(chan []pacruntime.Launcher, 8)
	pctx, pcancel := context.WithCancel(context.Background())
	var stopPeers sync.WaitGroup
	t.Cleanup(stopPeers.Wait)
	t.Cleanup(pcancel)

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			stopPeers.Add(1)
			go func() {
				defer stopPeers.Done()
				defer conn.Close()
				peer := pacruntime.NewPeer("fake-node", conn)
				peer.OnRequest = func(_ context.Context, method string, params json.RawMessage) (any, error) {
					if method == "runtime.hello" {
						var msg helloMsg
						_ = json.Unmarshal(params, &msg)
						hellos <- msg
						return map[string]any{"protocol": 1, "features": []string{"launchers", "remote_agent", "memory_tools"}}, nil
					}
					return map[string]any{"ok": true}, nil
				}
				peer.OnEvent = func(event string, params json.RawMessage) {
					if event == "runtime.launchers" {
						var p struct {
							Launchers []pacruntime.Launcher `json:"launchers"`
						}
						_ = json.Unmarshal(params, &p)
						launcherUpdates <- p.Launchers
					}
				}
				_ = peer.Serve(pctx)
			}()
		}
	}()

	t.Setenv("PERSONA_RUNTIME_SOCKET", sock)
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"demo_echo":true,"assistant_runtime":true}`))

	select {
	case msg := <-hellos:
		if !msg.Caps.Assistant || !msg.Caps.RemoteAgent || !msg.Caps.Start || !msg.Caps.Send {
			t.Fatalf("unexpected runtime.hello caps: %+v", msg.Caps)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for embedded runtime.Bridge runtime.hello")
	}

	act(t, h, pageState{Page: pagePair, Provider: echo.Name}, actPair, map[string]string{fieldCredential: demoCredential})
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	id := status.Accounts[0].AccountID
	act(t, h, pageState{Page: pageAccount, Account: id}, actAssistantBackend, map[string]string{fieldAssistantBackend: "on"})

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ls := <-launcherUpdates:
			if len(ls) > 0 {
				goto checkDisabled
			}
		case <-deadline:
			t.Fatal("timed out waiting for enabled Main assistant account to publish a launcher")
		}
	}

checkDisabled:
	offSock := filepath.Join(sockDir, "off.sock")
	offLn, err := net.Listen("unix", offSock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = offLn.Close() })
	var dialed atomic.Int32
	go func() {
		for {
			c, err := offLn.Accept()
			if err != nil {
				return
			}
			dialed.Add(1)
			_ = c.Close()
		}
	}()
	t.Setenv("PERSONA_RUNTIME_SOCKET", offSock)
	clubOff := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	_ = startHost(t, pac.Manifest, exttest.Serve(t, Program(clubOff, pac.Version())), json.RawMessage(`{"demo_echo":true,"assistant_runtime":false}`))
	time.Sleep(150 * time.Millisecond)
	if dialed.Load() != 0 {
		t.Fatalf("assistant_runtime=false unexpectedly dialed Persona runtime socket (%d times)", dialed.Load())
	}
}

// muse2api waits for its server's address in the settings; with it, the key is paired against that server.
func TestMuse2APIPairsWithTheConfiguredServer(t *testing.T) {
	const key = "fixture-muse2api-key-0001"
	var paths []string
	var mu sync.Mutex
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer "+key {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"detail":"API key 无效"}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"muse-spark","object":"model"}]}`))
	}))
	t.Cleanup(up.Close)

	// without an address: listed, and the badge says what it waits for
	h, _ := start(t)
	if v := render(t, h, pageState{}); !strings.Contains(text(v), "Muse 自建转接") || !strings.Contains(text(v), "里填「自建 Muse 转接服务的地址」") {
		t.Fatalf("list without an address: %s", text(v))
	}

	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	cfg, _ := json.Marshal(map[string]any{"muse2api_url": up.URL, "muse2api_model": "muse-spark"})
	h = startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), cfg)
	if v := render(t, h, pageState{}); strings.Contains(text(v), "里填「自建 Muse 转接服务的地址」") {
		t.Fatalf("list with an address: %s", text(v))
	}
	st := pageState{Page: pagePair, Provider: "muse2api"}
	if res := act(t, h, st, actPair, map[string]string{fieldCredential: "wrong-key"}); pairedDone(res) {
		t.Fatalf("a wrong key was paired: %+v", res)
	}
	res := act(t, h, st, actPair, map[string]string{fieldCredential: key})
	if !pairedDone(res) || !strings.Contains(res.Toast, "Muse 自建转接") || strings.Contains(res.Toast, key) {
		t.Fatalf("pair: %+v", res)
	}
	space, _ := club.Space(context.Background(), core.DefaultSpace)
	status, _ := space.Status(context.Background())
	if len(status.Accounts) != 1 || status.Accounts[0].Provider != "muse2api" {
		t.Fatalf("accounts: %+v", status.Accounts)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) == 0 || paths[0] != "/v1/models" {
		t.Fatalf("requests: %v", paths)
	}
}
