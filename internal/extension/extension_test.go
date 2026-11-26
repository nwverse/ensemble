package extension

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/ext"
	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/conformance"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

func TestManifest(t *testing.T) {
	if err := wire.ValidateJSON("Manifest", pac.Manifest); err != nil {
		t.Fatalf("persona-extension.json: %v", err)
	}
	var m wire.Manifest
	if err := json.Unmarshal(pac.Manifest, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != ID || m.Contributes == nil || len(m.Contributes.AgentSources) != 1 || m.Contributes.AgentSources[0].ID != Contribution {
		t.Fatalf("manifest %+v", m)
	}
}

// The SDK's own check: handshake, error codes for unknown methods, exit when the hub goes.
func TestExttestCheck(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry()})
	var m wire.Manifest
	_ = json.Unmarshal(pac.Manifest, &m)
	res := exttest.Check(t, pac.Manifest, exttest.Serve(t, Program(club, m.Version)))
	if res.ID != ID {
		t.Fatalf("initialize answered %+v", res)
	}
}

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T, club *core.Club) conformance.Shell {
		var m wire.Manifest
		_ = json.Unmarshal(pac.Manifest, &m)
		h := startHub(t, exttest.Serve(t, Program(club, m.Version)))
		info := &wire.ExtensionInfo{ID: ID, Version: m.Version, Dir: t.TempDir(), DataDir: t.TempDir()}
		var res wire.InitializeResult
		if err := h.call("initialize", wire.InitializeParams{Protocol: ext.Protocol, Extension: info, Plugin: info,
			Config: json.RawMessage(`{"demo_echo":false,"browser_providers":false}`)}, &res); err != nil {
			t.Fatal(err)
		}
		var raw json.RawMessage
		err := h.call("agentsrc.status", map[string]any{"contribution": "someone-else"}, &raw)
		if e, ok := err.(*ext.Error); !ok || e.Code != wire.ErrorCodeNoContribution {
			t.Fatalf("another contribution: %v", err)
		}
		return h
	})
}

// hub is the hub's side of the extension protocol, enough to call agentsrc.* and hear agentsrc.updated. (exttest's
// Host holds every answer to snake_case keys, which agentsrc's camelCase results are not, so it cannot call them.)
type hub struct {
	t       *testing.T
	w       io.Writer
	wmu     sync.Mutex
	next    atomic.Int64
	mu      sync.Mutex
	pending map[string]chan frame
	events  chan protocol.UpdatedEvent
}

type frame struct {
	ID     string          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Event  string          `json:"event,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *ext.Error      `json:"error,omitempty"`
}

func startHub(t *testing.T, run exttest.Run) *hub {
	h := &hub{t: t, w: run.ToProgram, pending: map[string]chan frame{}, events: make(chan protocol.UpdatedEvent, 256)}
	go func() {
		sc := bufio.NewScanner(run.FromProgram)
		sc.Buffer(make([]byte, 64*1024), 16<<20)
		for sc.Scan() {
			var f frame
			if err := json.Unmarshal(sc.Bytes(), &f); err != nil {
				t.Errorf("program wrote a line that is not JSON: %s", sc.Bytes())
				continue
			}
			switch {
			case f.Event == "ui.invalidate":
				// the accounts page redrawn on a change: nothing to check here
			case f.Event != "":
				if f.Event != protocol.EventUpdated {
					t.Errorf("unexpected event %s", f.Event)
					continue
				}
				var e protocol.UpdatedEvent
				if err := json.Unmarshal(f.Params, &e); err != nil || e.Contribution != Contribution {
					t.Errorf("agentsrc.updated %s: %v", f.Params, err)
				}
				h.events <- e
			case f.Method != "":
				t.Errorf("the program called the hub: %s", f.Method)
			case f.ID != "":
				h.mu.Lock()
				ch := h.pending[f.ID]
				delete(h.pending, f.ID)
				h.mu.Unlock()
				if ch != nil {
					ch <- f
				}
			}
		}
	}()
	t.Cleanup(func() { _ = run.ToProgram.Close() })
	return h
}

func (h *hub) call(method string, params, out any) error {
	id := fmt.Sprintf("h%d", h.next.Add(1))
	ch := make(chan frame, 1)
	h.mu.Lock()
	h.pending[id] = ch
	h.mu.Unlock()
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(frame{ID: id, Method: method, Params: raw})
	h.wmu.Lock()
	_, err = h.w.Write(append(b, '\n'))
	h.wmu.Unlock()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case f := <-ch:
		if f.Error != nil {
			return f.Error
		}
		if out != nil {
			return json.Unmarshal(f.Result, out)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s: no answer", method)
	}
}

// Call is the conformance suite's: the hub adds the contribution to every agentsrc.* call, and a coded refusal is
// turned into the protocol's error.
func (h *hub) Call(method string, params, out any) error {
	var p map[string]any
	if params != nil {
		b, _ := json.Marshal(params)
		if json.Unmarshal(b, &p) != nil {
			// Not an object: sent as it is, as a broken hub might.
			err := h.call(method, params, out)
			return asProtocol(err)
		}
	}
	if p == nil {
		p = map[string]any{}
	}
	p["contribution"] = Contribution
	return asProtocol(h.call(method, p, out))
}

// asProtocol turns the wire's refusal into the protocol's. One code differs by transport: a method the source does
// not have is unknown_method on the extension protocol (the hub's own check, `personad extension test`, insists on
// it), where the serve shell says unsupported; to the suite both are "not this method".
func asProtocol(err error) error {
	if e, ok := err.(*ext.Error); ok && e.Code == wire.ErrorCodeUnknownMethod {
		return &protocol.Error{Code: protocol.CodeUnsupported, Message: e.Message}
	}
	if e, ok := err.(*ext.Error); ok && e.Code != "" {
		return &protocol.Error{Code: e.Code, Message: e.Message}
	}
	return err
}

func (h *hub) Event(d time.Duration) (protocol.UpdatedEvent, bool) {
	select {
	case e := <-h.events:
		return e, true
	case <-time.After(d):
		return protocol.UpdatedEvent{}, false
	}
}

func TestDemoEchoConfig(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry()})
	h := startHub(t, exttest.Serve(t, Program(club, "0.1.0")))
	info := &wire.ExtensionInfo{ID: ID, Version: "0.1.0", Dir: t.TempDir(), DataDir: t.TempDir()}
	if err := h.call("initialize", wire.InitializeParams{Protocol: ext.Protocol, Extension: info, Plugin: info,
		Config: json.RawMessage(`{"demo_echo":true}`)}, nil); err != nil {
		t.Fatal(err)
	}
	var st protocol.StatusResult
	if err := h.Call("agentsrc.status", nil, &st); err != nil {
		t.Fatal(err)
	}
	for _, p := range st.Providers {
		if p.Provider == "echo" && p.Available {
			return
		}
	}
	t.Fatalf("demo_echo did not add echo: %+v", st.Providers)
}
