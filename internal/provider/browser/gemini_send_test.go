package browser

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/provider"
)

// geminiPage is how the stand-in Gemini page behaves (as the recorded pages in testdata/gemini show it): the turns
// on it, whether an answer is being written (the stop button) and for how many looks, whether it takes the send
// button's click or Enter, and what Gemini's own record (hNvQHb) answers.
type geminiPage struct {
	Href      string            `json:"href"`
	Storage   map[string]string `json:"storage"`
	Turns     []string          `json:"turns"`
	BusyLooks int               `json:"busyLooks"` // looks for which the page is still writing an answer
	// AnswerLooks is how long the answer to a sent message is written, in looks.
	AnswerLooks int  `json:"answerLooks"`
	ClickTakes  bool `json:"clickTakes"`
	EnterTakes  bool `json:"enterTakes"`
	// NewID is the address a new conversation gets once sent.
	NewID string `json:"newId"`
	// Record is the turn id Gemini's record names for the sent text ("" when it has none).
	Record string `json:"record"`
	// Answer is the markdown text Gemini's record (hNvQHb) carries for the turn's answer candidate.
	Answer string `json:"answer"`
	// NoTaskMode simulates an account where the task-mode toggle (data-value="40") is missing or refuses selection.
	NoTaskMode bool `json:"noTaskMode"`
}

type geminiRun struct {
	Value   map[string]any    `json:"v"`
	Calls   []string          `json:"calls"`
	Storage map[string]string `json:"storage"`
	Turns   []string          `json:"turns"`
	Draft   string            `json:"draft"`
}

func (r geminiRun) called(prefix string) int {
	n := 0
	for _, c := range r.Calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// runGeminiScript runs a Gemini script in node against the stand-in page; time runs a second per look.
func runGeminiScript(t *testing.T, script string, p geminiPage) geminiRun {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	if p.Storage == nil {
		p.Storage = map[string]string{}
	}
	env, _ := json.Marshal(p)
	sels, _ := json.Marshal(map[string]string{"composer": geminiComposerSel, "send": geminiSendSel, "stop": geminiStopSel,
		"query": geminiQuerySel, "queryText": geminiQueryTextSel, "chatMode": geminiChatModeSel, "taskMode": geminiTaskModeSel})
	src := `
const P = ` + string(env) + `, S = ` + string(sels) + `;
const calls = [];
let href = P.href, draft = "", busyLooks = P.busyLooks, turns = (P.turns || []).slice();
let now = Date.parse("2026-11-27T00:00:00Z");
Date.now = () => (now += 1000);
const __setTimeout = setTimeout;
globalThis.setTimeout = (fn, ms, ...rest) => __setTimeout(fn, 0, ...rest);
globalThis.window = globalThis;
window.WIZ_global_data = { SNlM0e: "tok" };
globalThis.localStorage = { getItem: k => (k in P.storage ? P.storage[k] : null), setItem: (k, v) => { P.storage[k] = String(v); } };
Object.defineProperty(globalThis, "location", { get: () => { const u = new URL(href); return { href, origin: u.origin, hostname: u.hostname, pathname: u.pathname }; } });
const composer = { focus() {}, get innerText() { return draft || "\n"; } };
globalThis.getSelection = () => ({ removeAllRanges() {}, addRange() {} });
globalThis.document = {
  readyState: "complete", title: "", body: { innerText: "" }, documentElement: { lang: "zh" },
  createRange: () => ({ selectNodeContents() {} }),
  execCommand: c => { if (c === "delete") draft = ""; },
  querySelector: s => {
    if (s === S.composer) return composer;
    if (s === S.stop) { if (busyLooks > 0) { busyLooks--; return {}; } return null; }
    return null;
  },
  querySelectorAll: s => s === S.query ? turns.map(t => ({ innerText: "你说 " + t + "\n\n" + t, querySelectorAll: q => q === S.queryText ? [{ innerText: t }] : [] })) : [],
};
globalThis.fetch = async (url, opts) => {
  const body = String((opts && opts.body) || "");
  let payload = null;
  if (body.includes("hNvQHb") && P.record) {
    const cands = P.answer ? [[["rc_1", [P.answer]]]] : [];
    payload = [[[["c_x", P.record], null, [[turns[turns.length - 1]]], cands, [1, 0]]]];
  }
  if (body.includes("MaZiqc")) payload = [null, null, []];
  const text = ")]}'\n" + JSON.stringify([["wrb.fr", body.includes("hNvQHb") ? "hNvQHb" : "MaZiqc", JSON.stringify(payload)]]);
  return { status: 200, ok: true, url: String(url), text: async () => text, json: async () => JSON.parse(text) };
};
const send = (how) => {
  calls.push("sent " + how);
  turns.push(draft);
  draft = "";
  busyLooks = P.answerLooks;
  if (/\/app\/?$/.test(new URL(href).pathname) && P.newId) href = "https://gemini.google.com/app/" + P.newId;
};
const page = {
  goto: async u => { calls.push("goto " + u + (busyLooks > 0 ? " (busy)" : "")); if (u !== href) { turns = /\/app\/?$/.test(u) ? [] : turns; } href = u; },
  waitForLoadState: async () => {},
  waitForSelector: async () => {},
  waitForFunction: async () => {},
  evaluate: async (fn, arg) => fn(arg),
  url: () => href,
  title: async () => "",
  click: async (sel) => {
    calls.push("click " + sel);
    if (sel === "loc=css:" + S.send) { if (P.clickTakes) send("click"); return; }
    if (sel === "loc=css:" + S.chatMode) return;
    if (sel === "loc=css:" + S.taskMode) { if (P.noTaskMode) throw new Error("no task mode button"); return; }
    if (sel === "loc=css:" + S.composer) return;
    throw new Error("no such element");
  },
  press: async (sel, key) => { calls.push("press " + key); if (key === "Enter" && P.enterTakes) send("enter"); },
  keyboard: { insertText: async t => { calls.push("type"); draft += t; } },
};
const task = { finish: async () => { calls.push("finish"); } };
(async () => {
  let v;
  try { v = await (async () => {
` + script + `
  })(); } catch (e) { v = { thrown: String(e && e.message || e) }; }
  console.log(JSON.stringify({ v, calls, storage: P.storage, turns, draft }));
})();
`
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var r geminiRun
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(out))), &r); err != nil {
		t.Fatalf("node output: %v\n%s", err, out)
	}
	return r
}

const geminiConv = "https://gemini.google.com/app/79c7d8b1b42ae8ad"

// The 11-26 failure: the page did not take the send button's click (the message stayed in the composer) and the
// send was refused. Now it presses Enter once, waits for the answer to be written before it leaves the page (a page
// left before then loses the turn), and names the turn from Gemini's own record.
func TestGeminiSendRetriesWithEnterAndWaitsForTheAnswer(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi there"), geminiPage{
		Href: geminiConv, Turns: []string{"earlier"}, ClickTakes: false, EnterTakes: true, AnswerLooks: 8, Record: "r_9",
	})
	if r.Value["sent"] != true || r.Value["answered"] != true || r.Value["id"] != "r_9" || r.called("sent enter") != 1 {
		t.Fatalf("%+v", r)
	}
	if len(r.Turns) != 2 || r.Turns[1] != "hi there" || r.Draft != "" {
		t.Fatalf("turns %v, draft %q", r.Turns, r.Draft)
	}
}

// A click the page takes is not followed by Enter: the message goes once.
func TestGeminiSendGoesOnce(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi"), geminiPage{
		Href: geminiConv, Turns: []string{"earlier"}, ClickTakes: true, EnterTakes: true, AnswerLooks: 2, Record: "r_1",
	})
	if r.Value["sent"] != true || r.called("sent ") != 1 || r.called("press") != 0 {
		t.Fatalf("%+v", r)
	}
}

// Neither the click nor Enter taken: not sent, and the composer is cleared so it can be sent again.
func TestGeminiSendNotTakenIsRefused(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi"), geminiPage{Href: geminiConv, Turns: []string{"earlier"}})
	if r.Value["error"] != "not sent" || r.Draft != "" || len(r.Turns) != 1 {
		t.Fatalf("%+v", r)
	}
}

// While Gemini still writes an answer in the conversation it takes no message: the send waits, then refuses (busy)
// without typing anything.
func TestGeminiSendRefusesWhileAnAnswerIsWritten(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi"), geminiPage{Href: geminiConv, Turns: []string{"earlier"}, BusyLooks: 1000, ClickTakes: true})
	if r.Value["error"] != "busy" || r.called("type") != 0 {
		t.Fatalf("%+v", r)
	}
	// An answer that is written meanwhile is waited for, and the page is not left before.
	r = runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi"), geminiPage{Href: "https://gemini.google.com/app/other", Turns: []string{"earlier"}, BusyLooks: 5, ClickTakes: true, Record: "r_2"})
	if r.Value["sent"] != true || r.called("goto") != 1 || strings.Contains(strings.Join(r.Calls, ","), "(busy)") {
		t.Fatalf("left a page writing an answer: %+v", r)
	}
}

// The main chat: its first send opens a new conversation in 对话 mode and keeps its id; later sends continue it.
func TestGeminiSendToMain(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("main", "hello"), geminiPage{
		Href: "https://gemini.google.com/app", ClickTakes: true, AnswerLooks: 2, NewID: "fda91e50c7161da9", Record: "r_5",
	})
	if r.Value["sent"] != true || r.Value["id"] != "r_5" || r.Storage[geminiMainKey] != "fda91e50c7161da9" ||
		r.called("click loc=css:"+geminiChatModeSel) != 1 {
		t.Fatalf("%+v", r)
	}
	r = runGeminiScript(t, geminiSendScript("main", "again"), geminiPage{
		Href: "https://gemini.google.com/app", Storage: map[string]string{geminiMainKey: "fda91e50c7161da9"},
		Turns: []string{"hello"}, ClickTakes: true, Record: "r_6",
	})
	if r.Value["sent"] != true || r.called("goto https://gemini.google.com/app/fda91e50c7161da9") != 1 || r.called("click loc=css:"+geminiChatModeSel) != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestGeminiSendOutcomes(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1"})
	for _, tc := range []struct {
		value  any
		status provider.SendStatus
		id     string
		code   string
	}{
		{map[string]any{"sent": true, "clicked": true, "answered": true, "id": "r_9"}, provider.Sent, "r_9:u", ""},
		// Still being answered when the wait ended: it went (the page shows it and Gemini is answering it).
		{map[string]any{"sent": true, "clicked": true}, provider.Sent, "", ""},
		{map[string]any{"sent": false, "clicked": true}, provider.Unconfirmed, "", ""},
		{map[string]any{"error": "auth"}, "", "", provider.CodeAuthFailed},
		{map[string]any{"error": "human check"}, "", "", provider.CodeHumanCheck},
		{map[string]any{"error": "http 429"}, "", "", provider.CodeRateLimited},
		{map[string]any{"error": "not sent", "clicked": true}, "", "", provider.CodeUpstreamError},
		{map[string]any{"error": "busy"}, "", "", provider.CodeUpstreamError},
		{map[string]any{"error": "no task"}, "", "", provider.CodeNotFound},
	} {
		fd := &fakeDriver{answer: func(RunRequest) (RunResult, error) { return hubOutput(t, tc.value), nil }}
		res, err := New(SiteGeminiTasks, fd).Send(ctx, cred, provider.SessionRef{ExternalID: "main"}, provider.Outgoing{Text: "x"}, provider.SendContext{})
		if tc.code != "" {
			if provider.CodeOf(err) != tc.code {
				t.Errorf("%v: %v", tc.value, err)
			}
			continue
		}
		if err != nil || res.Status != tc.status || res.ExternalID != tc.id {
			t.Errorf("%v: %+v, %v", tc.value, res, err)
		}
		if got := fd.gotReqs[0].TimeoutMS; got != int(geminiSendTimeout/time.Millisecond) {
			t.Errorf("send timeout %d", got)
		}
	}
}

// A read finding a Gemini page writing an answer neither reloads it nor lets the tab be recycled under it.
func TestGeminiReadsKeepAPageWritingAnAnswer(t *testing.T) {
	script := geminiScript(geminiOrigin+"/app", nil, `return { ok: true };`)
	r := runGeminiScript(t, recycleScript(reloadScript(script)), geminiPage{Href: geminiConv, BusyLooks: 2})
	if r.called("goto") != 0 || r.called("finish") != 0 || r.Value["busy"] != true {
		t.Fatalf("%+v", r)
	}
	r = runGeminiScript(t, reloadScript(script), geminiPage{Href: geminiConv})
	if r.called("goto") != 1 || r.Value["busy"] != false {
		t.Fatalf("an idle page after a failed read is loaded again: %+v", r)
	}
}

// A page still writing an answer after the wait is not left: the send is refused before it opens its conversation.
func TestGeminiSendDoesNotLeaveAPageStillWritingAnAnswer(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi"), geminiPage{Href: "https://gemini.google.com/app/other", Turns: []string{"a"}, BusyLooks: 1000, ClickTakes: true})
	if r.Value["error"] != "busy" || r.called("goto") != 0 || r.called("type") != 0 {
		t.Fatalf("%+v", r)
	}
	// Already on the conversation: it is not loaded again.
	r = runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi"), geminiPage{Href: geminiConv, Turns: []string{"a"}, ClickTakes: true, Record: "r_1"})
	if r.Value["sent"] != true || r.called("goto") != 0 {
		t.Fatalf("%+v", r)
	}
}

// A newest turn that only contains the text (a click not taken, an earlier "hi there") is not taken for it.
func TestGeminiSendWantsTheExactTurn(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("79c7d8b1b42ae8ad", "hi"), geminiPage{Href: geminiConv, Turns: []string{"hi there"}, EnterTakes: false})
	if r.Value["error"] != "not sent" {
		t.Fatalf("%+v", r)
	}
}

// The main chat's new conversation is kept before the answer is waited for: a run that ends while Gemini still
// answers (the wait over) still keeps it, and reports the message sent.
func TestGeminiSendKeepsMainBeforeTheAnswer(t *testing.T) {
	r := runGeminiScript(t, geminiSendScript("main", "hello"), geminiPage{
		Href: "https://gemini.google.com/app", ClickTakes: true, AnswerLooks: 100000, NewID: "fda91e50c7161da9",
	})
	if r.Value["sent"] != true || r.Value["answered"] != false || r.Storage[geminiMainKey] != "fda91e50c7161da9" {
		t.Fatalf("%+v", r)
	}
}
