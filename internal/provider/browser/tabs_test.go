package browser

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/provider"
)

// tabRun is what a node-side script did in a stand-in tab: its value and the page calls it made (goto <url>,
// fill, click, finish).
type tabRun struct {
	Value json.RawMessage `json:"v"`
	Calls []string        `json:"calls"`
}

func (r tabRun) gotos() []string {
	var out []string
	for _, c := range r.Calls {
		if u, ok := strings.CutPrefix(c, "goto "); ok {
			out = append(out, u)
		}
	}
	return out
}

func (r tabRun) called(prefix string) bool {
	for _, c := range r.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// runTabScript runs script as browser.run does (the body of an async function with page and task in scope) under
// Node, in a tab at href whose document is in state ("complete" when empty). The page's fetch answers fetch[path
// prefix] (a JSON body, __NOW__ replaced by the time of the request); page.goto moves the tab and loads it.
// The scripts' waits take at most 5 ms. Skipped where there is no Node.
func runTabScript(t *testing.T, script, href, state string, fetch map[string]string) tabRun {
	t.Helper()
	return runTabScriptIn(t, script, href, state, fetch, tabStub{})
}

// tabStub is how the stand-in tab misbehaves: its first EvalFails evaluates throw (its context is being replaced),
// and with LoadsOnWait a document still loading finishes when waited for.
type tabStub struct {
	EvalFails   int  `json:"evalFails"`
	LoadsOnWait bool `json:"loadsOnWait"`
}

func runTabScriptIn(t *testing.T, script, href, state string, fetch map[string]string, stub tabStub) tabRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	if state == "" {
		state = "complete"
	}
	env, _ := json.Marshal(map[string]any{"href": href, "state": state, "fetch": fetch, "stub": stub})
	src := `
const P = ` + string(env) + `;
const calls = [];
let href = P.href, state = P.state;
globalThis.window = globalThis;
const __setTimeout = setTimeout;
globalThis.setTimeout = (fn, ms, ...rest) => __setTimeout(fn, Math.min(ms || 0, 5), ...rest); // the scripts' waits, cut short
Object.defineProperty(globalThis, "location", { get: () => { const u = new URL(href); return { href, origin: u.origin, hostname: u.hostname, pathname: u.pathname }; } });
globalThis.document = { get readyState() { return state; }, title: "", body: { innerText: "" }, documentElement: { lang: "en" }, querySelector: () => null, querySelectorAll: () => [] };
globalThis.fetch = async (path) => {
  const key = Object.keys(P.fetch || {}).filter(k => String(path).startsWith(k)).sort((a, b) => b.length - a.length)[0];
  if (key === undefined) return { status: 404, ok: false, url: href, json: async () => ({}), text: async () => "" };
  const body = P.fetch[key].replaceAll("__NOW__", new Date().toISOString());
  return { status: 200, ok: true, url: href, json: async () => JSON.parse(body), text: async () => body };
};
const page = {
  goto: async u => { calls.push("goto " + u); href = u; state = "complete"; },
  waitForLoadState: async () => { if (P.stub.loadsOnWait) state = "complete"; },
  waitForSelector: async () => {},
  waitForFunction: async () => {},
  evaluate: async (fn, arg) => { if (P.stub.evalFails > 0) { P.stub.evalFails--; throw new Error("Execution context was destroyed"); } return fn(arg); },
  url: async () => href,
  title: async () => "",
  fill: async () => { calls.push("fill"); },
  click: async () => { calls.push("click"); },
  press: async () => { calls.push("press"); },
};
const task = { finish: async o => { calls.push("finish " + JSON.stringify(o)); } };
(async () => {
  let v;
  try { v = await (async () => {
` + script + `
  })(); } catch (e) { v = { thrown: String(e && e.message || e) }; }
  console.log(JSON.stringify({ v: v === undefined ? null : v, calls }));
})();
`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var r tabRun
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &r); err != nil {
		t.Fatalf("node output: %v\n%s", err, out)
	}
	return r
}

// scriptDriver keeps each script a run was given and answers it with answer.
type scriptDriver struct {
	mu      sync.Mutex
	scripts []string
	answer  func(script string) (RunResult, error)
}

func (d *scriptDriver) Name() string                                { return "ego" }
func (d *scriptDriver) Machines(context.Context) ([]Machine, error) { return nil, nil }
func (d *scriptDriver) Run(_ context.Context, req RunRequest) (RunResult, error) {
	d.mu.Lock()
	d.scripts = append(d.scripts, req.Script)
	d.mu.Unlock()
	return d.answer(req.Script)
}

// The scripts Dots polls with: what verify, listSessions and listMessages run in the tab.
func dotsReadScripts(t *testing.T) map[string]string {
	t.Helper()
	cred := EncodeCredential(Credential{MachineID: "mac", Backend: "ego"})
	d := &scriptDriver{answer: func(string) (RunResult, error) { return RunResult{Output: `{}`}, nil }}
	a := New(SiteDots, d)
	a.Verify(context.Background(), cred)
	a.ListSessions(context.Background(), cred)
	a.ListMessages(context.Background(), cred, provider.SessionRef{ExternalID: "d1", Ref: map[string]string{"room": "r1"}}, provider.ListOptions{})
	if len(d.scripts) != 3 {
		t.Fatalf("scripts = %d", len(d.scripts))
	}
	return map[string]string{"verify": d.scripts[0], "listSessions": d.scripts[1], "listMessages": d.scripts[2]}
}

var dotsFetch = map[string]string{
	"/api/auth/session":                      `{"user":{"id":"user-1","name":"Ada"},"account":{"id":"acc"},"accessToken":"x"}`,
	"/backend-api/tbo":                       `{"items":[{"id":"d1","display_name":"Dot","messaging_room_id":"r1","status":"active"}]}`,
	"/backend-api/tbo/d1":                    `{"messaging_room_id":"r1"}`,
	"/backend-api/messaging/rooms/r1/messag": `{"items":[{"id":"m1","created_at":"__NOW__","account_user_id":"user-1__d1","content":{"text":"hello there"}}]}`,
}

// A read in a tab already on a loaded chatgpt.com page runs there: no reload. On a blank tab, another site, or a
// document still loading it opens chatgpt.com first, as before.
func TestDotsReadsDoNotReloadATabAlreadyOnTheSite(t *testing.T) {
	for name, script := range dotsReadScripts(t) {
		for _, href := range []string{"https://chatgpt.com/", "https://chatgpt.com/dots/d1"} {
			r := runTabScript(t, script, href, "complete", dotsFetch)
			if len(r.gotos()) != 0 {
				t.Errorf("%s on %s: reloaded %v", name, href, r.gotos())
			}
			if strings.Contains(string(r.Value), `"error"`) || string(r.Value) == "null" {
				t.Errorf("%s on %s: answered %s", name, href, r.Value)
			}
		}
		for _, tc := range []struct{ href, state string }{
			{"about:blank", "complete"},
			{"https://auth.openai.com/log-in", "complete"},
			{"https://chatgpt.com.example/", "complete"},
			{"https://chatgpt.com/", "loading"},
		} {
			r := runTabScript(t, script, tc.href, tc.state, dotsFetch)
			if g := r.gotos(); len(g) != 1 || g[0] != "https://chatgpt.com/" {
				t.Errorf("%s on %s (%s): gotos = %v, want one to https://chatgpt.com/", name, tc.href, tc.state, g)
			}
		}
		// After a failed answer (signed out, a human check) the next read loads the page again.
		if g := runTabScript(t, reloadScript(script), "https://chatgpt.com/", "complete", dotsFetch).gotos(); len(g) != 1 {
			t.Errorf("%s with __pacReload: gotos = %v", name, g)
		}
	}
	// Signed out on the site: still told apart, without a reload.
	r := runTabScript(t, dotsReadScripts(t)["verify"], "https://chatgpt.com/", "complete", map[string]string{"/api/auth/session": `{}`})
	if len(r.gotos()) != 0 || !strings.Contains(string(r.Value), `"error":"auth"`) {
		t.Fatalf("signed out: %+v", r)
	}
}

// A send still opens the Dot's page, with its composer, even when the tab is already on chatgpt.com.
func TestDotsSendStillNavigates(t *testing.T) {
	cred := EncodeCredential(Credential{MachineID: "mac", Backend: "ego"})
	d := &scriptDriver{answer: func(string) (RunResult, error) { return RunResult{Output: `{"sent":true,"id":"m1"}`}, nil }}
	a := New(SiteDots, d)
	res, err := a.Send(context.Background(), cred, provider.SessionRef{ExternalID: "d1", Ref: map[string]string{"room": "r1"}}, provider.Outgoing{Text: "hello there"}, provider.SendContext{})
	if err != nil || res.Status != provider.Sent {
		t.Fatalf("Send = %+v, %v", res, err)
	}
	for _, href := range []string{"https://chatgpt.com/", "https://chatgpt.com/dots/d1", "about:blank"} {
		r := runTabScript(t, d.scripts[0], href, "complete", dotsFetch)
		if g := r.gotos(); len(g) != 1 || g[0] != "https://chatgpt.com/dots/d1" {
			t.Errorf("send on %s: gotos = %v", href, g)
		}
		if !r.called("fill") || !strings.Contains(string(r.Value), `"sent":true`) || r.called("finish") {
			t.Errorf("send on %s: %+v", href, r)
		}
	}
}

// Claude Duet's API reads stay on a claude.ai page, but not on its sign-in page; Gemini's on gemini.google.com.
// The sends, and the reads that read a page's DOM (Duet's channel list, Muse, the generic site's), still load it.
func TestOtherSitesStayOnlyWhereTheirReadsWork(t *testing.T) {
	cred := EncodeCredential(Credential{MachineID: "mac", Backend: "ego"})
	d := &scriptDriver{answer: func(string) (RunResult, error) { return RunResult{Output: `{}`}, nil }}
	duet := New(SiteClaudeDuet, d)
	duet.Verify(context.Background(), cred)
	duet.ListMessages(context.Background(), cred, provider.SessionRef{ExternalID: "chan_abc"}, provider.ListOptions{})
	duet.ListSessions(context.Background(), cred)
	duet.Send(context.Background(), cred, provider.SessionRef{ExternalID: "chan_abc"}, provider.Outgoing{Text: "hi"}, provider.SendContext{})
	gem := New(SiteGeminiTasks, d)
	gem.Verify(context.Background(), cred)
	gem.Send(context.Background(), cred, provider.SessionRef{ExternalID: "8857"}, provider.Outgoing{Text: "hi"}, provider.SendContext{})
	gen := New(genericSite, d)
	gen.Verify(context.Background(), cred)
	if len(d.scripts) != 7 {
		t.Fatalf("scripts = %d", len(d.scripts))
	}
	duetFetch := map[string]string{"/api/account": `{"uuid":"u1","display_name":"Ada"}`, "/v1/code/": `{"data":[]}`}
	for _, tc := range []struct {
		name, script, href, want string // want: the page it opens, "" when it stays
	}{
		{"duet verify on a channel", d.scripts[0], "https://claude.ai/duet/chan_abc", ""},
		{"duet verify on the sign-in page", d.scripts[0], "https://claude.ai/login", "https://claude.ai/duet"},
		{"duet verify on a blank tab", d.scripts[0], "about:blank", "https://claude.ai/duet"},
		{"duet messages on Duet", d.scripts[1], "https://claude.ai/duet", ""},
		{"duet channel list", d.scripts[2], "https://claude.ai/duet", "https://claude.ai/duet"},
		{"duet send", d.scripts[3], "https://claude.ai/duet/chan_abc", "https://claude.ai/duet/chan_abc"},
		{"gemini verify on a task", d.scripts[4], "https://gemini.google.com/app/8857", ""},
		{"gemini verify on Google's sign-in", d.scripts[4], "https://accounts.google.com/", "https://gemini.google.com/app"},
		{"gemini send", d.scripts[5], "https://gemini.google.com/app", "https://gemini.google.com/app/8857"},
		{"generic verify on its page", d.scripts[6], "https://agent.example/", ""},
		{"generic verify elsewhere on the site", d.scripts[6], "https://agent.example/other", "https://agent.example/"},
	} {
		r := runTabScript(t, tc.script, tc.href, "complete", duetFetch)
		g := r.gotos()
		if (tc.want == "" && len(g) != 0) || (tc.want != "" && (len(g) == 0 || g[0] != tc.want)) {
			t.Errorf("%s: gotos = %v, want %q", tc.name, g, tc.want)
		}
	}
}

// After TabRecycle's runs, the next read closes the space's tab once it has its answer, and the count starts over.
func TestReadsRecycleTheTabAtTheirThreshold(t *testing.T) {
	cred := EncodeCredential(Credential{MachineID: "mac", Backend: "ego"})
	d := &scriptDriver{answer: func(script string) (RunResult, error) {
		if strings.Contains(script, "task.finish") {
			return hubOutput(t, map[string]any{"userId": "user-1", "dots": 1, "tabRecycled": true}), nil
		}
		return hubOutput(t, map[string]any{"userId": "user-1", "dots": 1}), nil
	}}
	a := New(SiteDots, d)
	a.SetTabRecycle(TabRecycle{Runs: 3})
	for i := 0; i < 7; i++ {
		if _, err := a.Verify(context.Background(), cred); err != nil {
			t.Fatal(err)
		}
	}
	var recycled []int
	for i, s := range d.scripts {
		if strings.Contains(s, "task.finish") {
			recycled = append(recycled, i+1)
		}
	}
	if len(recycled) != 2 || recycled[0] != 3 || recycled[1] != 6 {
		t.Fatalf("recycled on runs %v, want [3 6]", recycled)
	}
	// What the recycling run does in the tab: its answer first, then the tab closed.
	r := runTabScript(t, d.scripts[2], "https://chatgpt.com/", "complete", dotsFetch)
	if len(r.gotos()) != 0 || !r.called(`finish {"keep":[]}`) || !strings.Contains(string(r.Value), `"tabRecycled":true`) || !strings.Contains(string(r.Value), `"userId":"user-1"`) {
		t.Fatalf("recycling run: %+v", r)
	}
	// An answer with an error keeps the tab (a person may need it), and the next read tries again, reloading.
	r = runTabScript(t, d.scripts[2], "https://chatgpt.com/", "complete", map[string]string{"/api/auth/session": `{}`})
	if r.called("finish") || strings.Contains(string(r.Value), "tabRecycled") {
		t.Fatalf("recycled a signed-out tab: %+v", r)
	}
}

func TestTabIsRecycledByAgeAndRetriedAfterAFailedAnswer(t *testing.T) {
	cred := EncodeCredential(Credential{MachineID: "mac", Backend: "ego"})
	answer := map[string]any{"error": "auth"}
	d := &scriptDriver{answer: func(script string) (RunResult, error) {
		v := map[string]any{}
		for k, x := range answer {
			v[k] = x
		}
		if strings.Contains(script, "task.finish") && answer["error"] == nil {
			v["tabRecycled"] = true
		}
		return hubOutput(t, v), nil
	}}
	a := New(SiteDots, d)
	a.SetTabRecycle(TabRecycle{Age: 10 * time.Minute})
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	a.tabs.now = func() time.Time { return now }

	a.Verify(context.Background(), cred) // signed out: the next read reloads
	if s := d.scripts[0]; strings.Contains(s, "__pacReload = true") || strings.Contains(s, "task.finish") {
		t.Fatalf("first read: %s", s[:80])
	}
	answer = map[string]any{"userId": "user-1", "dots": 1}
	a.Verify(context.Background(), cred)
	if s := d.scripts[1]; !strings.Contains(s, "__pacReload = true") {
		t.Fatalf("a read after a failed answer does not reload")
	}
	a.Verify(context.Background(), cred)
	if s := d.scripts[2]; strings.Contains(s, "__pacReload = true") || strings.Contains(s, "task.finish") {
		t.Fatalf("a read after a good answer reloads or recycles")
	}
	now = now.Add(10 * time.Minute)
	answer = map[string]any{"error": "human check"}
	a.Verify(context.Background(), cred) // due, but the answer failed: the tab is kept
	answer = map[string]any{"userId": "user-1", "dots": 1}
	a.Verify(context.Background(), cred) // tried again
	a.Verify(context.Background(), cred) // a fresh tab: not due
	if !strings.Contains(d.scripts[3], "task.finish") || !strings.Contains(d.scripts[4], "task.finish") || strings.Contains(d.scripts[5], "task.finish") {
		t.Fatalf("age recycling: %v %v %v", strings.Contains(d.scripts[3], "task.finish"), strings.Contains(d.scripts[4], "task.finish"), strings.Contains(d.scripts[5], "task.finish"))
	}
}

// A send never closes the tab and is never closed under: a tab due while a send runs is closed by a later read,
// and a send waits for a read that is closing the tab.
func TestSendsAreNeverRecycled(t *testing.T) {
	tb := newTabs(TabRecycle{Runs: 1})
	ctx := context.Background()
	send, _ := tb.begin(ctx, "k", false)
	if send.recycle || send.reload {
		t.Fatalf("send lease = %+v", send)
	}
	read, _ := tb.begin(ctx, "k", true)
	if read.recycle || read.reload {
		t.Fatalf("read under a send = %+v", read)
	}
	tb.end(read, true, false)
	if again, _ := tb.begin(ctx, "k", true); again.recycle || again.reload {
		t.Fatalf("a read under a send closes or reloads its tab: %+v", again)
	} else {
		tb.end(again, false, false)
	}
	tb.end(send, false, false)
	closing, _ := tb.begin(ctx, "k", true)
	if !closing.recycle {
		t.Fatalf("a due read alone in the tab does not close it")
	}
	started := make(chan tabLease)
	go func() { l, _ := tb.begin(ctx, "k", false); started <- l }()
	select {
	case <-started:
		t.Fatal("a send started while a read was closing the tab")
	case <-time.After(50 * time.Millisecond):
	}
	tb.end(closing, false, true)
	if l := <-started; l.recycle {
		t.Fatalf("send lease = %+v", l)
	}

	// Through the Adapter: Dots' send script has no recycling in it, however many reads ran.
	cred := EncodeCredential(Credential{MachineID: "mac", Backend: "ego"})
	d := &scriptDriver{answer: func(string) (RunResult, error) { return RunResult{Output: `{"sent":true,"id":"m1"}`}, nil }}
	a := New(SiteDots, d)
	a.SetTabRecycle(TabRecycle{Runs: 1})
	for i := 0; i < 3; i++ {
		a.Send(ctx, cred, provider.SessionRef{ExternalID: "d1", Ref: map[string]string{"room": "r1"}}, provider.Outgoing{Text: "hi"}, provider.SendContext{})
	}
	for _, s := range d.scripts {
		if strings.Contains(s, "task.finish") || strings.Contains(s, "__pacReload") {
			t.Fatalf("send script recycles or carries a reload flag")
		}
	}
}

// Each computer's tab is counted on its own.
func TestTabsAreCountedPerComputer(t *testing.T) {
	tb := newTabs(TabRecycle{Runs: 2})
	ctx := context.Background()
	for _, key := range []string{"a", "b"} {
		l, _ := tb.begin(ctx, key, true)
		if l.recycle {
			t.Fatalf("%s recycled on its first read", key)
		}
		tb.end(l, false, false)
	}
	if l, _ := tb.begin(ctx, "a", true); !l.recycle {
		t.Fatal("a not recycled on its second read")
	}
}

// A read that finds its tab mid-navigation (another run's, such as a Gemini send opening its task) does not navigate
// it away: a loading document is waited for, and a page that cannot be looked into is judged by its address.
func TestReadsDoNotNavigateAwayFromAPageBeingLoaded(t *testing.T) {
	cred := EncodeCredential(Credential{MachineID: "mac", Backend: "ego"})
	d := &scriptDriver{answer: func(string) (RunResult, error) { return RunResult{Output: `{}`}, nil }}
	New(SiteGeminiTasks, d).Verify(context.Background(), cred)
	verify := d.scripts[0]
	wiz := map[string]string{}
	for _, tc := range []struct {
		name, href, state string
		stub              tabStub
		want              string
	}{
		{"loading, then loaded", "https://gemini.google.com/app/8857", "loading", tabStub{LoadsOnWait: true}, ""},
		{"evaluate fails, on Gemini", "https://gemini.google.com/app/8857", "complete", tabStub{EvalFails: 2}, ""},
		{"evaluate fails once, then looks", "https://gemini.google.com/app/8857", "complete", tabStub{EvalFails: 1}, ""},
		{"evaluate fails, elsewhere", "about:blank", "complete", tabStub{EvalFails: 2}, "https://gemini.google.com/app"},
		{"loading elsewhere", "https://accounts.google.com/", "loading", tabStub{LoadsOnWait: true}, "https://gemini.google.com/app"},
		{"never finishes loading", "https://gemini.google.com/app", "loading", tabStub{}, "https://gemini.google.com/app"},
	} {
		g := runTabScriptIn(t, verify, tc.href, tc.state, wiz, tc.stub).gotos()
		if (tc.want == "" && len(g) != 0) || (tc.want != "" && (len(g) != 1 || g[0] != tc.want)) {
			t.Errorf("%s: gotos = %v, want %q", tc.name, g, tc.want)
		}
	}
}

// The reload flag and the recycling wrapper together: the read reloads, answers, then closes the tab.
func TestReloadAndRecycleTogether(t *testing.T) {
	script := recycleScript(reloadScript(dotsReadScripts(t)["verify"]))
	r := runTabScript(t, script, "https://chatgpt.com/", "complete", dotsFetch)
	if g := r.gotos(); len(g) != 1 || !r.called(`finish {"keep":[]}`) || !strings.Contains(string(r.Value), `"tabRecycled":true`) {
		t.Fatalf("reload + recycle: %+v", r)
	}
}

// A send waiting for a read closing the tab gives up with its context.
func TestWaitingSendHonoursItsContext(t *testing.T) {
	tb := newTabs(TabRecycle{Runs: 1})
	closing, _ := tb.begin(context.Background(), "k", true)
	if !closing.recycle {
		t.Fatal("not closing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := tb.begin(ctx, "k", false); err == nil {
		t.Fatal("a send waited past its context")
	}
	tb.end(closing, false, true)
	if l, err := tb.begin(context.Background(), "k", false); err != nil || l.recycle {
		t.Fatalf("after the close: %+v, %v", l, err)
	}
}
