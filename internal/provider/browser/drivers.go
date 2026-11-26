package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
)

// PersonaHubDriver implements Driver by calling Persona's cross-machine browser.machines and browser.run host RPCs
// (nwverse/persona#12866). It works with any BrowserBackend registered on the Persona manager (ego lite, future CDP,
// etc.).
type PersonaHubDriver struct {
	hub *atomic.Pointer[ext.Hub]
}

// NewPersonaHubDriver creates a Driver backed by an extension's ext.Hub pointer.
func NewPersonaHubDriver(hub *atomic.Pointer[ext.Hub]) *PersonaHubDriver {
	return &PersonaHubDriver{hub: hub}
}

var _ Driver = (*PersonaHubDriver)(nil)

func (d *PersonaHubDriver) Name() string { return "persona-hub" }

func (d *PersonaHubDriver) Machines(ctx context.Context) ([]Machine, error) {
	h := d.hub.Load()
	if h == nil {
		return nil, fmt.Errorf("persona hub not connected")
	}
	var res struct {
		Machines []Machine `json:"machines"`
	}
	if err := h.Call(ctx, "browser.machines", map[string]any{}, &res); err != nil {
		return nil, err
	}
	return res.Machines, nil
}

func (d *PersonaHubDriver) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	h := d.hub.Load()
	if h == nil {
		return RunResult{}, fmt.Errorf("persona hub not connected")
	}
	var res RunResult
	if err := h.Call(ctx, "browser.run", req, &res); err != nil {
		return RunResult{}, err
	}
	return res, nil
}

// LocalEgoDriver implements Driver by invoking the local `ego-browser nodejs` CLI directly (useful in standalone
// mode and live E2E verification).
type LocalEgoDriver struct {
	Bin string // default: "ego-browser"
}

var _ Driver = (*LocalEgoDriver)(nil)

func (d *LocalEgoDriver) Name() string { return "ego" }

func (d *LocalEgoDriver) bin() string {
	if strings.TrimSpace(d.Bin) != "" {
		return d.Bin
	}
	return "ego-browser"
}

func (d *LocalEgoDriver) Machines(context.Context) ([]Machine, error) {
	_, err := exec.LookPath(d.bin())
	ready := err == nil
	return []Machine{{MachineID: "local", Name: "Local ego lite", Backend: "ego", Online: ready, Ready: ready}}, nil
}

func (d *LocalEgoDriver) Run(ctx context.Context, req RunRequest) (RunResult, error) {
	timeout := 45 * time.Second
	if req.TimeoutMS > 0 {
		timeout = time.Duration(req.TimeoutMS) * time.Millisecond
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	spaceName := fmt.Sprintf("pac-%s-%d", req.Slot, time.Now().UnixNano())
	// Wrapped as Persona's node wraps browser.run scripts (server/internal/node/browser.go browserScript): the script
	// is the body of an async function with page in scope, and one marked line carries what it returned or threw.
	wrapped := fmt.Sprintf(`const task = await taskSpace(%q);
const page = task.page("p1");
const __r = {};
try {
  __r.value = await (async () => {
%s
  })() ?? null;
} catch (e) { __r.error = String(e && e.message || e); }
try { __r.url = await page.url(); __r.title = await page.title(); } catch (e) {}
await task.finish({ keep: [] }).catch(() => {});
console.log(%q + JSON.stringify(__r));
`, spaceName, req.Script, egoResultMark)
	cmd := exec.CommandContext(rctx, d.bin(), "nodejs")
	cmd.Stdin = strings.NewReader(wrapped)
	// ego-browser emits script console.log output on stderr; capture combined output
	// just like Persona's node/browser.go does.
	out, runErr := cmd.CombinedOutput()
	text := string(out)
	i := strings.LastIndex(text, egoResultMark)
	if i < 0 {
		msg := strings.TrimSpace(text)
		if msg == "" && runErr != nil {
			msg = runErr.Error()
		}
		if msg == "" {
			msg = "ego-browser gave no result"
		}
		return RunResult{Backend: "ego", Output: text, Error: msg}, nil
	}
	line, _, _ := strings.Cut(text[i+len(egoResultMark):], "\n")
	var result map[string]any
	if err := json.Unmarshal([]byte(line), &result); err != nil {
		return RunResult{Backend: "ego", Output: text, Error: "ego-browser result unreadable"}, nil
	}
	if console := strings.TrimSpace(text[:i]); console != "" {
		result["console"] = console
	}
	raw, _ := json.Marshal(result)
	res := RunResult{Backend: "ego", Output: string(raw)}
	if e, _ := result["error"].(string); strings.TrimSpace(e) != "" {
		res.Error = strings.TrimSpace(e)
	}
	return res, nil
}

// egoResultMark starts the line a wrapped script prints its result on (as Persona's node does).
const egoResultMark = "@@pac-browser@@"
