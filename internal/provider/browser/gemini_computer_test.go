package browser

import (
	"context"
	"strings"
	"testing"

	"github.com/nwverse/ensemble/internal/provider"
)

func TestGeminiComputerCapabilities(t *testing.T) {
	gemini := New(SiteGeminiTasks, nil)
	if !gemini.SupportsBoxState() || !gemini.SupportsComputer() {
		t.Fatalf("SiteGeminiTasks capabilities: box=%v computer=%v", gemini.SupportsBoxState(), gemini.SupportsComputer())
	}
	for _, site := range []Site{SiteMuse, SiteClaudeDuet, SiteDots} {
		a := New(site, nil)
		if a.SupportsBoxState() || a.SupportsComputer() {
			t.Errorf("%s should not report Gemini computer capabilities", site.Provider)
		}
		if _, err := a.RunComputer(context.Background(), "mac-1", "uname -a"); provider.CodeOf(err) != provider.CodeUnavailable {
			t.Errorf("%s RunComputer error = %v, want %s", site.Provider, err, provider.CodeUnavailable)
		}
	}
}

func TestGeminiBoxStateReadOnly(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})

	fd := &fakeDriver{name: "ego", answer: func(RunRequest) (RunResult, error) {
		return hubOutput(t, map[string]any{
			"taskMode":     true,
			"internet":     true,
			"running":      1,
			"waiting":      0,
			"ready":        2,
			"sessionId":    "c_b8f40e9a12c3d456",
			"sessionTitle": "云电脑环境探查",
			"computerBusy": true,
		}), nil
	}}
	a := New(SiteGeminiTasks, fd)
	st, err := a.BoxState(ctx, cred)
	if err != nil {
		t.Fatalf("BoxState error: %v", err)
	}
	if st.State != "running" || st.TaskMode == nil || !*st.TaskMode || st.InternetAccess == nil || !*st.InternetAccess {
		t.Fatalf("BoxState = %+v", st)
	}
	if st.Running != 1 || st.Ready != 2 || st.SettingsURL != "https://gemini.google.com/gemini-spark" ||
		st.SessionID != "b8f40e9a12c3d456" || st.SessionURL != "https://gemini.google.com/app/b8f40e9a12c3d456" ||
		st.SessionTitle != "云电脑环境探查" || !strings.Contains(st.Image, "Debian 12") {
		t.Fatalf("BoxState metadata = %+v", st)
	}
	sc := fd.gotReqs[0].Script
	for _, want := range []string{`"W8adn"`, `"ensemble.gemini.computer"`, openJS("https://gemini.google.com/app", geminiStay)} {
		if !strings.Contains(sc, want) {
			t.Errorf("BoxState script missing %q", want)
		}
	}

	// Internet off & idle tasks -> paused state, InternetAccess=false.
	fdOff := &fakeDriver{name: "ego", answer: func(RunRequest) (RunResult, error) {
		return hubOutput(t, map[string]any{
			"taskMode":  true,
			"internet":  false,
			"running":   0,
			"waiting":   0,
			"ready":     1,
			"sessionId": "b8f40e9a12c3d456",
		}), nil
	}}
	stOff, err := New(SiteGeminiTasks, fdOff).BoxState(ctx, cred)
	if err != nil || stOff.State != "paused" || stOff.InternetAccess == nil || *stOff.InternetAccess {
		t.Fatalf("BoxState (internet off) = %+v, %v", stOff, err)
	}

	// Task mode disabled -> stopped.
	fdDisabled := &fakeDriver{name: "ego", answer: func(RunRequest) (RunResult, error) {
		return hubOutput(t, map[string]any{
			"taskMode": false,
			"internet": false,
		}), nil
	}}
	stDis, err := New(SiteGeminiTasks, fdDisabled).BoxState(ctx, cred)
	if err != nil || stDis.State != "stopped" || stDis.TaskMode == nil || *stDis.TaskMode {
		t.Fatalf("BoxState (task mode off) = %+v, %v", stDis, err)
	}

	// Auth, human check, rate limit errors are classified accurately.
	for _, tc := range []struct {
		errStr string
		code   string
	}{
		{"auth", provider.CodeAuthFailed},
		{"human check", provider.CodeHumanCheck},
		{"http 429", provider.CodeRateLimited},
	} {
		d := &fakeDriver{answer: func(RunRequest) (RunResult, error) {
			return hubOutput(t, map[string]any{"error": tc.errStr}), nil
		}}
		if _, err := New(SiteGeminiTasks, d).BoxState(ctx, cred); provider.CodeOf(err) != tc.code {
			t.Errorf("BoxState error(%q) = %v, want %s", tc.errStr, err, tc.code)
		}
	}
}

func TestGeminiComputerRunScriptUsesTaskModeAndIsolatesFromMain(t *testing.T) {
	// 1. Fresh computer run: clicks Task mode (data-value="40"), never Chat mode (data-value="0"),
	// and writes new conversation id to ensemble.gemini.computer without touching ensemble.gemini.main.
	r := runGeminiScript(t, geminiComputerRunScript("uname -srm"), geminiPage{
		Href:        "https://gemini.google.com/app",
		Storage:     map[string]string{geminiMainKey: "main_chat_c0ffee"},
		ClickTakes:  true,
		AnswerLooks: 2,
		NewID:       "comp_9876543210ab",
		Record:      "r_comp_1",
		Answer:      "```text\nLinux localhost 6.6.137+ x86_64\n```",
	})
	if r.Value["sent"] != true || r.Value["answered"] != true || r.Value["id"] != "r_comp_1" {
		t.Fatalf("fresh computer run = %+v", r)
	}
	if r.Storage[geminiComputerKey] != "comp_9876543210ab" {
		t.Fatalf("computer key = %q, want comp_9876543210ab", r.Storage[geminiComputerKey])
	}
	if r.Storage[geminiMainKey] != "main_chat_c0ffee" {
		t.Fatalf("main key was disturbed: %q", r.Storage[geminiMainKey])
	}
	if r.called("click loc=css:"+geminiTaskModeSel) != 1 || r.called("click loc=css:"+geminiChatModeSel) != 0 {
		t.Fatalf("mode clicks: task=%d chat=%d", r.called("click loc=css:"+geminiTaskModeSel), r.called("click loc=css:"+geminiChatModeSel))
	}

	// 2. Subsequent computer run: reuses ensemble.gemini.computer without opening a new conversation.
	r2 := runGeminiScript(t, geminiComputerRunScript("df -h"), geminiPage{
		Href: "https://gemini.google.com/app",
		Storage: map[string]string{
			geminiMainKey:     "main_chat_c0ffee",
			geminiComputerKey: "comp_9876543210ab",
		},
		Turns:      []string{"uname -srm"},
		ClickTakes: true,
		Record:     "r_comp_2",
		Answer:     "```text\n/dev/root 49G 8.0G 41G 17% /\n```",
	})
	if r2.Value["sent"] != true || r2.called("goto https://gemini.google.com/app/comp_9876543210ab") != 1 ||
		r2.called("click loc=css:"+geminiTaskModeSel) != 0 {
		t.Fatalf("subsequent computer run = %+v", r2)
	}

	// 3. If task mode cannot be activated on a new conversation, abort before typing or saving COMP_KEY.
	r3 := runGeminiScript(t, geminiComputerRunScript("uname -srm"), geminiPage{
		Href:       "https://gemini.google.com/app",
		Storage:    map[string]string{geminiMainKey: "main_chat_c0ffee"},
		ClickTakes: true,
		NoTaskMode: true,
		NewID:      "plain_chat_id",
	})
	if r3.Value["error"] != "no task" || r3.called("type") != 0 || r3.Storage[geminiComputerKey] != "" {
		t.Fatalf("missing task mode should abort before typing: %+v", r3)
	}
}

func TestGeminiRunComputerAndExtractOutput(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1"})

	fd := &fakeDriver{answer: func(RunRequest) (RunResult, error) {
		return hubOutput(t, map[string]any{
			"sent":         true,
			"clicked":      true,
			"answered":     true,
			"id":           "r_12",
			"conversation": "comp_9876543210ab",
			"answer":       "已为你执行命令：\n```sh\nuname -srm\n```\n输出如下：\n```text\nLinux localhost 6.6.137+ x86_64\n```",
		}), nil
	}}
	res, err := New(SiteGeminiTasks, fd).RunComputer(ctx, cred, "uname -srm")
	if err != nil {
		t.Fatalf("RunComputer: %v", err)
	}
	if res.Status != provider.Sent || !res.Answered || res.SessionID != "comp_9876543210ab" ||
		res.SessionURL != "https://gemini.google.com/app/comp_9876543210ab" || res.TurnID != "r_12:u" ||
		res.Output != "Linux localhost 6.6.137+ x86_64" || res.ExitCode != 0 {
		t.Fatalf("RunComputer result = %+v", res)
	}
	if code := parseGeminiComputerExitCode("```text\ncmd not found\nExit code: 127\n```"); code != 127 {
		t.Fatalf("parseGeminiComputerExitCode = %d, want 127", code)
	}

	for _, tc := range []struct {
		value  any
		status provider.SendStatus
		code   string
	}{
		{map[string]any{"sent": false, "clicked": true}, provider.Unconfirmed, ""},
		{map[string]any{"error": "auth"}, "", provider.CodeAuthFailed},
		{map[string]any{"error": "human check"}, "", provider.CodeHumanCheck},
		{map[string]any{"error": "http 429"}, "", provider.CodeRateLimited},
		{map[string]any{"error": "busy"}, "", provider.CodeUpstreamError},
		{map[string]any{"error": "not sent"}, "", provider.CodeUpstreamError},
	} {
		d := &fakeDriver{answer: func(RunRequest) (RunResult, error) { return hubOutput(t, tc.value), nil }}
		got, err := New(SiteGeminiTasks, d).RunComputer(ctx, cred, "ls -la")
		if tc.code != "" {
			if provider.CodeOf(err) != tc.code {
				t.Errorf("%v: err=%v, want %s", tc.value, err, tc.code)
			}
			continue
		}
		if err != nil || got.Status != tc.status {
			t.Errorf("%v: got=%+v, err=%v", tc.value, got, err)
		}
	}

	timeoutDriver := &fakeDriver{answer: func(RunRequest) (RunResult, error) { return RunResult{Error: "timeout"}, nil }}
	got, err := New(SiteGeminiTasks, timeoutDriver).RunComputer(ctx, cred, "ls -la")
	if err != nil || got.Status != provider.Unconfirmed {
		t.Fatalf("timeout RunComputer = %+v, %v", got, err)
	}
}
