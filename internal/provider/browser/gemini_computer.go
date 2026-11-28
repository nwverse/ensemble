package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/provider"
)

// Gemini Remote Computer (2026-11):
//   - Settings page: https://gemini.google.com/gemini-spark (<gemini-agent-settings>), with "使用任务模式委托工作"
//     (.disable-agent-section) and "远程计算机的网络访问权限" (.allow-remote-computer-access-section button[role="switch"]).
//   - W8adn (/BardFrontendService.GetVmSettings) []: read-only RPC returning [[true]] or [[false]] for the remote
//     computer's outbound network access toggle (vmInternetToggle).
//   - Task mode composer switch on /app: chat-spark-toggle gem-segmented-button[data-value="40"] button (whereas
//     plain chat mode is data-value="0").
//   - Ensemble keeps its dedicated remote-computer WebShell task conversation id in localStorage under
//     "ensemble.gemini.computer", strictly separate from "ensemble.gemini.main" so the main chat is never disturbed.

const (
	// geminiComputerKey is where the page keeps the dedicated remote-computer task conversation id in localStorage.
	geminiComputerKey = "ensemble.gemini.computer"
	// geminiSparkURL is Gemini's task & remote-computer settings page.
	geminiSparkURL = geminiOrigin + "/gemini-spark"
	// geminiComputerSpec describes Gemini's remote computer environment as verified in task mode.
	geminiComputerSpec = "Debian 12 · 2 vCPU · 8 GB"
	// geminiTaskModeSel selects the composer's 任务 (Tasks, data-value="40") mode on a new conversation page.
	geminiTaskModeSel = `chat-spark-toggle gem-segmented-button[data-value="40"] button`
	// geminiAgentSettingsSel is the root custom element on /gemini-spark.
	geminiAgentSettingsSel = `gemini-agent-settings`
	// geminiDisableAgentButtonSel is the "关闭" / "Turn off" button inside the task-mode delegation card on /gemini-spark.
	geminiDisableAgentButtonSel = `gemini-agent-settings .disable-agent-section button.action-button`
	// geminiInternetSwitchSel is the remote computer network access switch on /gemini-spark.
	geminiInternetSwitchSel = `gemini-agent-settings .allow-remote-computer-access-section button[role="switch"][aria-checked]`
)

// siteBoxStater is an optional capability of a siteImpl that reports read-only cloud sandbox or remote computer state.
type siteBoxStater interface {
	boxState(ctx context.Context, a *Adapter, credential string) (provider.BoxState, error)
}

// siteComputerRunner is an optional capability of a siteImpl that runs an owner-confirmed command in a dedicated
// remote computer session without disturbing the main chat.
type siteComputerRunner interface {
	runComputer(ctx context.Context, a *Adapter, credential, command string) (provider.ComputerRunResult, error)
}

var (
	_ provider.BoxStater         = (*Adapter)(nil)
	_ provider.BoxStateSupporter = (*Adapter)(nil)
	_ provider.ComputerRunner    = (*Adapter)(nil)
)

// SupportsBoxState reports whether this browser site supports read-only BoxState queries (Gemini Tasks does).
func (a *Adapter) SupportsBoxState() bool {
	_, ok := a.site.impl.(siteBoxStater)
	return ok
}

// BoxState reads the site's cloud sandbox or remote computer state read-only without waking or modifying it.
func (a *Adapter) BoxState(ctx context.Context, credential string) (provider.BoxState, error) {
	bs, ok := a.site.impl.(siteBoxStater)
	if !ok {
		return provider.BoxState{}, nil
	}
	return bs.boxState(ctx, a, credential)
}

// SupportsComputer reports whether this browser site has a dedicated remote computer command runner (Gemini Tasks does).
func (a *Adapter) SupportsComputer() bool {
	_, ok := a.site.impl.(siteComputerRunner)
	return ok
}

// RunComputer executes an owner-confirmed command in the site's dedicated remote computer session.
func (a *Adapter) RunComputer(ctx context.Context, credential, command string) (provider.ComputerRunResult, error) {
	cr, ok := a.site.impl.(siteComputerRunner)
	if !ok {
		return provider.ComputerRunResult{}, &provider.Error{Code: provider.CodeUnavailable}
	}
	return cr.runComputer(ctx, a, credential, command)
}

type geminiComputerStatePayload struct {
	Error        string `json:"error"`
	TaskMode     *bool  `json:"taskMode"`
	Internet     *bool  `json:"internet"`
	Running      int    `json:"running"`
	Waiting      int    `json:"waiting"`
	Ready        int    `json:"ready"`
	SessionID    string `json:"sessionId"`
	SessionTitle string `json:"sessionTitle"`
	ComputerBusy bool   `json:"computerBusy"`
}

// boxState reads Gemini's remote computer settings (W8adn / GetVmSettings and any /gemini-spark DOM present) and active
// task counts (MaZiqc) read-only without navigating an active conversation or sending any message.
func (geminiSite) boxState(ctx context.Context, a *Adapter, credential string) (provider.BoxState, error) {
	var r geminiComputerStatePayload
	script := geminiScript(geminiOrigin+"/app", map[string]any{
		"key":         geminiComputerKey,
		"composerSel": geminiComposerSel,
		"taskSel":     geminiTaskModeSel,
		"agentSel":    geminiAgentSettingsSel,
		"disableSel":  geminiDisableAgentButtonSel,
		"switchSel":   geminiInternetSwitchSel,
	}, `
    const vm = await __rpc("W8adn", []).catch(e => {
      if (e && (e.message === "auth" || e.message === "human check" || e.message === "http 429")) throw e;
      return null;
    });
    let internet = Array.isArray(vm) && Array.isArray(vm[0]) && typeof vm[0][0] === "boolean" ? vm[0][0] : null;
    const sw = document.querySelector(ARGS.switchSel);
    if (internet === null && sw) {
      const attr = sw.getAttribute("aria-checked");
      if (attr === "true" || attr === "false") internet = attr === "true";
    }
    let taskMode = null;
    const agentRoot = document.querySelector(ARGS.agentSel);
    if (agentRoot) {
      taskMode = agentRoot.classList.contains("is-remy-user") || !!document.querySelector(ARGS.disableSel);
    } else if (document.querySelector(ARGS.taskSel)) {
      taskMode = true;
    } else if (document.querySelector("chat-spark-toggle") || (location.pathname === "/app" && document.querySelector(ARGS.composerSel))) {
      taskMode = false;
    }
    const kept = (() => { try { return String(localStorage.getItem(ARGS.key) || ""); } catch (e) { return ""; } })().replace(/^c_/, "").trim();
    const running = await __tasks([2], 30);
    const waiting = await __tasks([3, 5], 30);
    const ready = await __tasks([4], 30);
    const all = [...running, ...waiting, ...ready];
    let sessionId = kept;
    let sessionTitle = "";
    if (sessionId) {
      const hit = all.find(t => t.id === sessionId);
      if (hit && hit.title) sessionTitle = hit.title;
    } else if (all.length > 0) {
      sessionId = all[0].id;
      sessionTitle = all[0].title || "";
    }
    const computerBusy = !!sessionId && running.some(t => t.id === sessionId);
    return {
      taskMode,
      internet,
      running: running.length,
      waiting: waiting.length,
      ready: ready.length,
      sessionId,
      sessionTitle,
      computerBusy,
    };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.BoxState{}, err
	}
	if err := geminiError(r.Error); err != nil {
		return provider.BoxState{}, err
	}
	return geminiBuildBoxState(r), nil
}

func geminiBuildBoxState(r geminiComputerStatePayload) provider.BoxState {
	state := "stopped"
	switch {
	case r.TaskMode != nil && !*r.TaskMode:
		state = "stopped"
	case r.ComputerBusy || r.Running > 0:
		state = "running"
	case r.Waiting > 0 || r.Ready > 0 || strings.TrimSpace(r.SessionID) != "":
		state = "paused"
	}
	sid := strings.TrimPrefix(strings.TrimSpace(r.SessionID), "c_")
	sessionURL := ""
	if sid != "" {
		sessionURL = geminiOrigin + "/app/" + sid
	}
	return provider.BoxState{
		State:          state,
		Image:          geminiComputerSpec,
		SourceNote:     "gemini:W8adn+MaZiqc",
		TaskMode:       r.TaskMode,
		InternetAccess: r.Internet,
		Running:        r.Running,
		Waiting:        r.Waiting,
		Ready:          r.Ready,
		SettingsURL:    geminiSparkURL,
		SessionID:      sid,
		SessionTitle:   strings.TrimSpace(r.SessionTitle),
		SessionURL:     sessionURL,
	}
}

func geminiComputerPrompt(command string) string {
	return "请在远程计算机终端里原样执行下面这条命令，只在一个代码块里返回终端完整输出与退出状态（不要改动命令）：\n```sh\n" + strings.TrimSpace(command) + "\n```"
}

func (geminiSite) runComputer(ctx context.Context, a *Adapter, credential, command string) (provider.ComputerRunResult, error) {
	cmd := strings.TrimSpace(command)
	if cmd == "" {
		return provider.ComputerRunResult{}, errors.New("empty command")
	}
	prompt := geminiComputerPrompt(cmd)
	start := time.Now()
	var r struct {
		Error        string `json:"error"`
		Sent         bool   `json:"sent"`
		Clicked      bool   `json:"clicked"`
		Answered     bool   `json:"answered"`
		ID           string `json:"id"`
		Conversation string `json:"conversation"`
		Answer       string `json:"answer"`
	}
	if err := a.runSend(ctx, credential, geminiComputerRunScript(prompt), &r); err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Code == provider.CodeAuthFailed {
			return provider.ComputerRunResult{}, err
		}
		return provider.ComputerRunResult{
			Status:     provider.Unconfirmed,
			DurationMs: time.Since(start).Milliseconds(),
			Error:      provider.CodeOf(err),
		}, nil
	}
	dur := time.Since(start).Milliseconds()
	switch r.Error {
	case "auth", "no task":
		return provider.ComputerRunResult{}, geminiError(r.Error)
	case "human check", "http 429":
		return provider.ComputerRunResult{}, geminiError(r.Error)
	case "busy", "not sent":
		return provider.ComputerRunResult{}, &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	if !r.Sent {
		code := provider.CodeUpstreamError
		if r.Error != "" {
			code = provider.CodeOf(geminiError(r.Error))
		}
		return provider.ComputerRunResult{
			Status:     provider.Unconfirmed,
			DurationMs: dur,
			Error:      code,
		}, nil
	}
	sid := strings.TrimPrefix(strings.TrimSpace(r.Conversation), "c_")
	sessionURL := ""
	if sid != "" {
		sessionURL = geminiOrigin + "/app/" + sid
	}
	turnID := strings.TrimSpace(r.ID)
	if turnID != "" {
		turnID += ":u"
	}
	output := extractGeminiComputerOutput(r.Answer, cmd)
	return provider.ComputerRunResult{
		Status:     provider.Sent,
		SessionID:  sid,
		SessionURL: sessionURL,
		TurnID:     turnID,
		Output:     output,
		ExitCode:   parseGeminiComputerExitCode(r.Answer),
		DurationMs: dur,
		Answered:   r.Answered,
	}, nil
}

// geminiComputerRunScript sends a prompt into the dedicated remote-computer task conversation (kept under
// localStorage[geminiComputerKey]), switching any new conversation to 任务 (Tasks, data-value="40") mode, waiting for
// the remote computer's answer, and reading the answer back from hNvQHb. It never reads or writes geminiMainKey.
func geminiComputerRunScript(text string) string {
	args, _ := json.Marshal(map[string]any{
		"text":         text,
		"answerWaitMs": geminiAnswerWaitMs,
		"budgetMs":     int((geminiSendTimeout - 10*time.Second) / time.Millisecond),
	})
	return fmt.Sprintf(`
const ARGS0 = %s;
const COMP_KEY = %q, SEL = %q, SEND = %q, STOP = %q, QUERY = %q, QUERY_TEXT = %q, TASK_MODE = %q;
const sleep = ms => new Promise(r => setTimeout(r, ms));
const T0 = Date.now();
const left = () => Math.max(0, ARGS0.budgetMs - (Date.now() - T0));
const norm = t => String(t || "").replace(/\s+/g, " ").trim();
const onGemini = async () => /^https:\/\/gemini\.google\.com\//.test(String(await Promise.resolve().then(() => page.url()).catch(() => "")));
const busy = () => page.evaluate((stop) => !!document.querySelector(stop), STOP).catch(() => false);
const waitIdle = async (ms) => {
  const end = Date.now() + Math.min(ms, left());
  while (await busy()) {
    if (Date.now() >= end) return false;
    await sleep(1000);
  }
  return true;
};
// 1. Wait if an answer is currently being written in this tab.
if ((await onGemini()) && !(await waitIdle(30000))) return { error: "busy" };
// 2. Resolve the dedicated remote-computer task conversation from COMP_KEY (never MAIN_KEY).
if (!(await onGemini())) {
  await page.goto("https://gemini.google.com/app");
  await page.waitForLoadState("domcontentloaded").catch(() => {});
}
let target = await page.evaluate((k) => { try { return String(localStorage.getItem(k) || ""); } catch (e) { return ""; } }, COMP_KEY).catch(() => "");
target = String(target || "").replace(/^c_/, "").trim();
const fresh = !target;
const here = String(await Promise.resolve().then(() => page.url()).catch(() => "")).replace(/[?#].*$/, "");
if (fresh || here !== "https://gemini.google.com/app/" + target) {
  await page.goto("https://gemini.google.com/app" + (target ? "/" + target : ""));
  await page.waitForLoadState("domcontentloaded").catch(() => {});
}
const signedIn = await page.evaluate(() => {
%s
  return __humanCheck() ? "human check" :
    location.hostname === "gemini.google.com" && !!(window.WIZ_global_data && window.WIZ_global_data.SNlM0e);
});
if (signedIn === "human check") return { error: "human check" };
if (!signedIn) return { error: "auth" };
const look = () => page.evaluate(([sel, query, queryText]) => {
  const qs = [...document.querySelectorAll(query)];
  const lastQ = qs[qs.length - 1];
  const lines = lastQ ? [...lastQ.querySelectorAll(queryText)] : [];
  return {
    turns: qs.length,
    last: lines.length ? lines.map(l => l.innerText).join("\n") : ((lastQ || {}).innerText || ""),
    draft: ((document.querySelector(sel) || {}).innerText) || "",
    path: String(location.pathname),
  };
}, [SEL, QUERY, QUERY_TEXT]);
await page.waitForSelector("loc=css:" + SEL, { state: "visible", timeout: 15000 });
let before = await look();
if (!fresh) {
  for (let i = 0; i < 15 && before.turns === 0 && left() > 0; i++) {
    await sleep(1000);
    before = await look();
  }
  if (before.turns === 0) {
    // Kept computer task conversation was deleted: open a fresh one on /app.
    await page.goto("https://gemini.google.com/app");
    await page.waitForLoadState("domcontentloaded").catch(() => {});
    await page.waitForSelector("loc=css:" + SEL, { state: "visible", timeout: 15000 });
    before = await look();
    before.fresh = true;
  }
}
const isNew = fresh || !!before.fresh;
if (isNew) {
  // Switch the new conversation to 任务 (Tasks, data-value="40") mode so it runs on Gemini's remote computer.
  // If task mode cannot be activated, abort before typing so a plain chat is never saved under COMP_KEY.
  const switched = await page.click("loc=css:" + TASK_MODE, { timeout: 3000 }).then(() => true).catch(() => false);
  const taskActive = switched && await page.evaluate((sel) => {
    const b = document.querySelector(sel);
    if (!b) return true;
    if (typeof b.getAttribute === "function" && b.getAttribute("aria-pressed") === "false") return false;
    return true;
  }, TASK_MODE).catch(() => false);
  if (!taskActive) return { error: "no task" };
}
// 3. Wait for any in-flight answer in this conversation.
if (!(await waitIdle(20000))) return { error: "busy" };
// 4. Clear any restored draft, type the command prompt, and send.
const settle = async () => {
  let prev = null;
  for (let i = 0, same = 0; i < 12 && same < 2 && left() > 0; i++) {
    const d = (await look()).draft;
    same = d === prev ? same + 1 : 0;
    prev = d;
    await sleep(400);
  }
};
const setDraft = async (t) => {
  await page.click("loc=css:" + SEL).catch(() => {});
  await page.evaluate((sel) => {
    const el = document.querySelector(sel);
    if (!el) return;
    el.focus();
    const r = document.createRange();
    r.selectNodeContents(el);
    const s = getSelection();
    s.removeAllRanges();
    s.addRange(r);
    document.execCommand("delete");
  }, SEL);
  if (t) await page.keyboard.insertText(t);
  await sleep(400);
  return norm((await look()).draft) === norm(t);
};
await settle();
let ready = await setDraft(ARGS0.text);
if (!ready) {
  await settle();
  ready = await setDraft(ARGS0.text);
}
if (!ready) {
  await setDraft("");
  return { error: "not sent" };
}
const want = norm(ARGS0.text);
before = { ...(await look()), fresh: before.fresh };
const went = (now) => (now.turns > before.turns || (isNew && now.turns > 0)) && norm(now.last) === want;
const clicked = await page.click("loc=css:" + SEND, { timeout: 3000 }).then(() => true).catch(() => false);
if (!clicked) await page.press("loc=css:" + SEL, "Enter");
let now = before, pressed = !clicked;
for (let i = 0; i < 20 && left() > 0; i++) {
  await sleep(1000);
  now = await look();
  if (went(now)) break;
  if (!pressed && i >= 3 && norm(now.draft).includes(want) && !(await busy())) {
    pressed = true;
    await page.press("loc=css:" + SEL, "Enter").catch(() => {});
  }
}
if (!went(now)) {
  if (norm(now.draft).includes(want)) {
    await setDraft("");
    return { clicked, error: "not sent" };
  }
  return { sent: false, clicked };
}
// 5. Persist the new remote-computer conversation id in COMP_KEY before waiting for the answer.
const pathID = async () => ((await look()).path.match(/\/app\/([A-Za-z0-9_-]{8,})/) || [])[1] || "";
let kept = "";
if (isNew) {
  for (let i = 0; i < 10 && !kept && left() > 0; i++) {
    kept = await pathID();
    if (!kept) await sleep(500);
  }
  if (kept) await page.evaluate(([k, v]) => { try { localStorage.setItem(k, v); } catch (e) {} }, [COMP_KEY, kept]).catch(() => {});
}
const answered = await waitIdle(Math.min(ARGS0.answerWaitMs, left() - 8000));
const found = await page.evaluate(async ([want, fresh]) => {
%s
  const n = t => String(t || "").replace(/\s+/g, " ").trim();
  const turnOf = async (conv) => {
    const v = await __rpc("hNvQHb", ["c_" + conv, 3, null, 1, [0], [4], null, 1]);
    const t = ((v && v[0]) || []).find(t => Array.isArray(t) && Array.isArray(t[0]) && n(t[2] && t[2][0] && t[2][0][0]).includes(want));
    if (!t) return null;
    const cand = t[3] && t[3][0] && t[3][0][0];
    const answer = (cand && cand[1] && typeof cand[1][0] === "string") ? cand[1][0] : "";
    return { turn: String(t[0][1] || ""), answer };
  };
  try {
    let conv = (String(location.pathname).match(/\/app\/([A-Za-z0-9_-]{8,})/) || [])[1] || "";
    if (conv) {
      const hit = await turnOf(conv);
      return { conv, turn: (hit && hit.turn) || "", answer: (hit && hit.answer) || "" };
    }
    if (!fresh) return { conv: "", turn: "", answer: "" };
    for (const c of await __allTasks(5)) {
      const hit = await turnOf(c.id);
      if (hit && hit.turn) return { conv: c.id, turn: hit.turn, answer: hit.answer || "" };
    }
  } catch (e) {}
  return { conv: "", turn: "", answer: "" };
}, [want, isNew]).catch(() => ({ conv: "", turn: "", answer: "" }));
const conv = found.conv || kept || target;
const turn = found.turn || "";
let answer = found.answer || "";
if (isNew && conv && conv !== kept) {
  await page.evaluate(([k, v]) => { try { localStorage.setItem(k, v); } catch (e) {} }, [COMP_KEY, conv]).catch(() => {});
}
return { sent: true, clicked, answered, id: turn, conversation: conv, answer };
`, args, geminiComputerKey, geminiComposerSel, geminiSendSel, geminiStopSel, geminiQuerySel, geminiQueryTextSel, geminiTaskModeSel,
		geminiHumanCheckJS, geminiPrelude)
}

// extractGeminiComputerOutput extracts clean shell stdout/stderr from a Gemini task-mode Markdown reply.
// When the reply contains fenced code blocks (```...```), code blocks that are not merely echoing the input command
// are preferred; otherwise the full reply text is returned.
func extractGeminiComputerOutput(answer, command string) string {
	raw := strings.TrimSpace(answer)
	if raw == "" {
		return ""
	}
	cmdNorm := strings.TrimSpace(command)
	var blocks []string
	var nonEcho []string
	rest := raw
	for {
		start := strings.Index(rest, "```")
		if start < 0 {
			break
		}
		afterFence := rest[start+3:]
		nl := strings.IndexByte(afterFence, '\n')
		if nl < 0 {
			break
		}
		body := afterFence[nl+1:]
		end := strings.Index(body, "```")
		if end < 0 {
			break
		}
		block := strings.TrimSpace(body[:end])
		if block != "" {
			blocks = append(blocks, block)
			if block != cmdNorm && strings.TrimPrefix(block, "$ ") != cmdNorm {
				nonEcho = append(nonEcho, block)
			}
		}
		rest = body[end+3:]
	}
	if len(nonEcho) > 0 {
		return strings.Join(nonEcho, "\n")
	}
	if len(blocks) > 0 {
		return strings.Join(blocks, "\n")
	}
	return raw
}

var geminiExitCodeRE = regexp.MustCompile(`(?i)(?:exit\s+(?:code|status)|退出码|返回码)\s*[:：=]?\s*(\d{1,3})\b`)

func parseGeminiComputerExitCode(answer string) int {
	m := geminiExitCodeRE.FindAllStringSubmatch(answer, -1)
	if len(m) == 0 {
		return 0
	}
	last := m[len(m)-1]
	code, err := strconv.Atoi(last[1])
	if err != nil || code < 0 {
		return 0
	}
	return code
}
