package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

type fakeTermRunner struct {
	calls []struct {
		cfg core.SSHTarget
		cmd string
	}
	answer func(core.SSHTarget, string) (termRunOutput, error)
}

func (f *fakeTermRunner) Run(_ context.Context, cfg core.SSHTarget, command string) (termRunOutput, error) {
	f.calls = append(f.calls, struct {
		cfg core.SSHTarget
		cmd string
	}{cfg: cfg, cmd: command})
	if f.answer != nil {
		return f.answer(cfg, command)
	}
	return termRunOutput{Output: "ok", ExitCode: 0, DurationMs: 42}, nil
}

type termBoxProvider struct {
	quotaTestProvider
	box      provider.BoxState
	sentText []string
}

func (p *termBoxProvider) BoxState(context.Context, string) (provider.BoxState, error) {
	return p.box, nil
}

func (p *termBoxProvider) Send(_ context.Context, _ string, _ provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	p.sentText = append(p.sentText, msg.Text)
	return provider.SendResult{Status: provider.Sent}, nil
}

func TestTerminalSSHReadOnlyProbeAndTwoStepConfirmation(t *testing.T) {
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	muse := &termBoxProvider{quotaTestProvider: quotaTestProvider{name: providerMuse}}
	grok := &termBoxProvider{
		quotaTestProvider: quotaTestProvider{
			name: grokBot,
			readings: []provider.QuotaReading{{
				Kind: protocol.QuotaKindWindow, Window: "week", Limit: 100, Remaining: 0,
				Exhausted: true, ResetsAt: now.Add(2 * time.Hour).UnixMilli(),
			}},
		},
		box: provider.BoxState{State: "paused", Image: "sand:v2", SourceNote: "grok_bot:GetSandBoxRunState"},
	}
	duet := &termBoxProvider{quotaTestProvider: quotaTestProvider{name: "claude_duet"}}

	club := core.New(core.Options{
		Registry:  provider.NewRegistry(muse, grok, duet),
		FollowUps: []time.Duration{},
		Now:       func() time.Time { return now },
	})
	ctx := context.Background()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	museAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: providerMuse, Credential: "fixture-muse-cred"})
	if err != nil {
		t.Fatal(err)
	}
	grokAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: grokBot, Credential: "fixture-grok-cred"})
	if err != nil {
		t.Fatal(err)
	}
	duetAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "claude_duet", Credential: "fixture-duet-cred"})
	if err != nil {
		t.Fatal(err)
	}

	runner := &fakeTermRunner{
		answer: func(_ core.SSHTarget, cmd string) (termRunOutput, error) {
			if cmd == termProbeCommand {
				return termRunOutput{
					Output:     cleanTerminalOutput("\x1b[32mLinux muse-vm 6.8.0 x86_64\x1b[0m\n12:00 up 3 days, load average: 0.12"),
					ExitCode:   0,
					DurationMs: 55,
				}, nil
			}
			return termRunOutput{
				Output:     cleanTerminalOutput("Filesystem Size Used Avail\n/dev/vda1  50G  12G  38G\n" + strings.Repeat("log-line-0123456789\n", 130)),
				ExitCode:   0,
				DurationMs: 88,
			}, nil
		},
	}
	a := &accounts{club: club, terms: newTermState(), termExec: runner}

	checkSchema := func(label string, v *wire.View) {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s marshal: %v", label, err)
		}
		if _, err := wire.DecodeCheckedViewJSON(b); err != nil {
			t.Fatalf("%s failed wire.DecodeCheckedViewJSON: %v\n%s", label, err, string(b))
		}
	}
	renderTerm := func(acctID string) *wire.View {
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
			State: ext.EncodeState(pageState{Page: pageTerminal, Account: acctID}),
		}}
		v, err := a.render(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		checkSchema("terminal "+acctID, v)
		noJargon(t, "terminal "+acctID, v)
		return v
	}
	doAct := func(acctID, action string, vals map[string]string) *wire.ActionResult {
		r := &ext.UIRequest{UIParams: wire.UIParams{
			Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
			State:  ext.EncodeState(pageState{Page: pageTerminal, Account: acctID}),
			Action: action,
			Values: vals,
		}}
		res, err := a.action(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if res != nil && res.View != nil {
			checkSchema("terminal act "+action, res.View)
			noJargon(t, "terminal act "+action, res.View)
		}
		return res
	}

	// 1. Account detail page includes the Remote terminal entry without jargon.
	acctReq := &ext.UIRequest{UIParams: wire.UIParams{
		Contribution: HomeUI, Placement: wire.PlacementNavPage, Locale: "zh-Hans",
		State: ext.EncodeState(pageState{Page: pageAccount, Account: museAcct.AccountID}),
	}}
	acctView, err := a.render(ctx, acctReq)
	if err != nil {
		t.Fatal(err)
	}
	noJargon(t, "account with terminal entry", acctView)
	if !strings.Contains(text(acctView), "远端终端与命令行") {
		t.Fatalf("account detail missing terminal entry:\n%s", text(acctView))
	}

	// 2. Muse terminal page: notes static snapshot; rejects option injection and Tailscale custom port; saves clean target.
	museInit := renderTerm(museAcct.AccountID)
	if !strings.Contains(text(museInit), "静态配置快照") {
		t.Fatalf("muse terminal missing static snapshot note:\n%s", text(museInit))
	}
	badRes := doAct(museAcct.AccountID, actTermSaveSSH, map[string]string{
		fieldTermSSHTarget:    "-oProxyCommand=id",
		fieldTermSSHTransport: core.TransportSSH,
	})
	if !strings.Contains(badRes.Toast, "格式不对") {
		t.Fatalf("expected invalid target refusal, got %q", badRes.Toast)
	}
	badPort := doAct(museAcct.AccountID, actTermSaveSSH, map[string]string{
		fieldTermSSHTarget:    "ubuntu@muse-box:2222",
		fieldTermSSHTransport: core.TransportTailscale,
	})
	if !strings.Contains(badPort.Toast, "Tailscale SSH") {
		t.Fatalf("expected tailscale port refusal, got %q", badPort.Toast)
	}
	saveRes := doAct(museAcct.AccountID, actTermSaveSSH, map[string]string{
		fieldTermSSHTarget:    "ubuntu@muse-vm.ts.net",
		fieldTermSSHTransport: core.TransportTailscale,
	})
	if !strings.Contains(saveRes.Toast, "已保存 SSH 目标") {
		t.Fatalf("save target toast = %q", saveRes.Toast)
	}

	// 3. Read-only probe runs fixed probe command and strips ANSI escape codes.
	probeRes := doAct(museAcct.AccountID, actTermProbe, nil)
	if !strings.Contains(probeRes.Toast, "只读状态检查通过") {
		t.Fatalf("probe toast = %q", probeRes.Toast)
	}
	if len(runner.calls) != 1 || runner.calls[0].cmd != termProbeCommand {
		t.Fatalf("probe calls = %+v", runner.calls)
	}
	if probeTxt := text(probeRes.View); !strings.Contains(probeTxt, "Linux muse-vm 6.8.0") || strings.Contains(probeTxt, "\x1b[") {
		t.Fatalf("probe output not cleaned: %s", probeTxt)
	}

	// 4. Two-step confirmed command execution: stage -> reject bad digest -> confirm valid digest -> reject replay.
	stageRes := doAct(museAcct.AccountID, actTermStage, map[string]string{fieldTermCmd: "df -h"})
	if !strings.Contains(stageRes.Toast, "已准备好命令") {
		t.Fatalf("stage toast = %q", stageRes.Toast)
	}
	sc, ok := a.term().getStaged(museAcct.AccountID, now)
	if !ok || sc.Command != "df -h" || sc.Digest == "" {
		t.Fatalf("staged = %+v, ok=%v", sc, ok)
	}
	// Staged view must require explicit Confirm dialog on the execute button.
	if !strings.Contains(text(stageRes.View), `"confirm":"确认在 ubuntu@muse-vm.ts.net 上执行这条命令`) {
		t.Fatalf("confirm dialog missing on execute button:\n%s", text(stageRes.View))
	}
	// Wrong digest must refuse without invoking runner.
	wrongRes := doAct(museAcct.AccountID, actTermExec, map[string]string{valTermDigest: "deadbeef"})
	if !strings.Contains(wrongRes.Toast, "过期或已变动") || len(runner.calls) != 1 {
		t.Fatalf("wrong digest: toast=%q calls=%d", wrongRes.Toast, len(runner.calls))
	}
	// Matching digest runs once and records history.
	execRes := doAct(museAcct.AccountID, actTermExec, map[string]string{valTermDigest: sc.Digest})
	if !strings.Contains(execRes.Toast, "退出码 0") || len(runner.calls) != 2 || runner.calls[1].cmd != "df -h" {
		t.Fatalf("exec: toast=%q calls=%+v", execRes.Toast, runner.calls)
	}
	if !strings.Contains(text(execRes.View), "/dev/vda1") {
		t.Fatalf("exec output missing from view:\n%s", text(execRes.View))
	}
	// Replay of the already-consumed digest must be rejected (one-shot).
	replayRes := doAct(museAcct.AccountID, actTermExec, map[string]string{valTermDigest: sc.Digest})
	if !strings.Contains(replayRes.Toast, "过期或已变动") || len(runner.calls) != 2 {
		t.Fatalf("replay was not rejected: toast=%q calls=%d", replayRes.Toast, len(runner.calls))
	}

	// 5. Chat-assisted sandbox execution: succeeds on Muse, blocked by QUOTA_EXHAUSTED on exhausted Grok Bot.
	chatStage := doAct(museAcct.AccountID, actTermChatStage, map[string]string{fieldTermChatCmd: "uname -a"})
	if !strings.Contains(chatStage.Toast, "已准备好命令") {
		t.Fatalf("chat stage toast = %q", chatStage.Toast)
	}
	csc, ok := a.term().getStaged(museAcct.AccountID, now)
	if !ok || csc.Mode != termModeChat {
		t.Fatalf("chat staged = %+v, ok=%v", csc, ok)
	}
	chatSend := doAct(museAcct.AccountID, actTermChatSend, map[string]string{valTermDigest: csc.Digest})
	if !strings.Contains(chatSend.Toast, "已发到主对话") || len(muse.sentText) != 1 || !strings.Contains(muse.sentText[0], "uname -a") {
		t.Fatalf("chat send: toast=%q sent=%+v", chatSend.Toast, muse.sentText)
	}

	// Grok Bot view shows read-only sandbox state ("已休眠") and blocks chat send when quota is exhausted.
	grokView := renderTerm(grokAcct.AccountID)
	grokTxt := text(grokView)
	for _, want := range []string{"云端沙箱状态（只读）", "已休眠", "sand:v2", "当前额度已用完"} {
		if !strings.Contains(grokTxt, want) {
			t.Errorf("grok terminal view lacks %q:\n%s", want, grokTxt)
		}
	}
	_ = doAct(grokAcct.AccountID, actTermChatStage, map[string]string{fieldTermChatCmd: "uptime"})
	gsc, _ := a.term().getStaged(grokAcct.AccountID, now)
	grokBlocked := doAct(grokAcct.AccountID, actTermChatSend, map[string]string{valTermDigest: gsc.Digest})
	if !strings.Contains(grokBlocked.Toast, "额度已用完") || len(grok.sentText) != 0 {
		t.Fatalf("exhausted grok chat send was not blocked: toast=%q sent=%d", grokBlocked.Toast, len(grok.sentText))
	}

	// 6. Non-terminal hosted service (Claude Duet): clearly marked unsupported + blockers + no SSH form.
	duetView := renderTerm(duetAcct.AccountID)
	duetTxt := text(duetView)
	for _, want := range []string{"不支持终端", "托管网页服务", "nwverse/persona#16741", "卡点 1", "卡点 2", "卡点 3"} {
		if !strings.Contains(duetTxt, want) {
			t.Errorf("duet terminal view lacks %q:\n%s", want, duetTxt)
		}
	}
	if strings.Contains(duetTxt, "保存 SSH 目标") {
		t.Errorf("non-terminal service should not offer SSH form:\n%s", duetTxt)
	}
}
