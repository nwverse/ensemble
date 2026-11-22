package browser

import (
	"context"
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
	wrapped := fmt.Sprintf(`const task = await taskSpace(%q);
const page = task.page("p1");
try {
%s
} finally {
  await task.finish({ keep: [] }).catch(() => {});
}
`, spaceName, req.Script)
	cmd := exec.CommandContext(rctx, d.bin(), "nodejs")
	cmd.Stdin = strings.NewReader(wrapped)
	// ego-browser emits script console.log output on stderr; capture combined output
	// just like Persona's node/browser.go does.
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return RunResult{Backend: "ego", Output: string(out), Error: msg}, nil
	}
	return RunResult{Backend: "ego", Output: string(out)}, nil
}
