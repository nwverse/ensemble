package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/exttest"
	"github.com/nwverse/persona/sdk/go/wire"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// The Dots pairing as the hub runs it: the computers from browser.machines, the page the apps can draw (a choice
// field, not a segmented without an action), and the pairing through browser.run answered as Persona's node answers.
func TestBrowserPairDots(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), json.RawMessage(`{"browser_providers":true}`))
	h.Answer("browser.machines", func(json.RawMessage) (any, error) {
		return wire.BrowserMachinesResult{Machines: []wire.BrowserMachine{
			{MachineID: "mac", Name: "MacBook", Backend: "ego", Online: true, Ready: true}}}, nil
	})
	var runs []wire.BrowserRunParams
	h.Answer("browser.run", func(raw json.RawMessage) (any, error) {
		var p wire.BrowserRunParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		runs = append(runs, p)
		var value any
		switch {
		case strings.Contains(p.Script, "return { userId"):
			value = map[string]any{"userId": "user-X", "accountId": "acc-1", "name": "Ada", "dots": 1}
		case strings.Contains(p.Script, "/backend-api/messaging/rooms/"):
			value = map[string]any{"messages": []any{}}
		default:
			value = map[string]any{"items": []map[string]any{{"id": "dot-1", "messaging_room_id": "room-1", "status": "active"}}}
		}
		out, _ := json.Marshal(map[string]any{"value": value, "url": "https://chatgpt.com/", "title": "ChatGPT"})
		return wire.BrowserRunResult{Backend: "ego", Output: string(out)}, nil
	})

	st := pageState{Page: pageBrowserPair, Provider: "dots"}
	v := render(t, h, st)
	if !strings.Contains(text(v), "MacBook · 在线") || strings.Contains(text(v), "(ego)") {
		t.Fatalf("pair page: %s", text(v))
	}
	res := act(t, h, st, actBrowserPair, map[string]string{fieldMachine: "mac|ego"})
	if !res.Pop || !strings.Contains(res.Toast, "Dots · Ada") {
		t.Fatalf("pair = %+v", res)
	}
	if len(runs) == 0 || runs[0].MachineID != "mac" || runs[0].Backend != "ego" || runs[0].Slot != "pac-dots" {
		t.Fatalf("browser.run = %+v", runs)
	}
	space, err := club.Space(context.Background(), core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := space.Sessions(context.Background())
	if err != nil || len(sessions.Sessions) != 1 || !strings.HasPrefix(sessions.Sessions[0].SessionKey, "agent:dots:") {
		t.Fatalf("sessions = %+v, %v", sessions, err)
	}
}

// With nothing paired the connector is not configured; the hub then offers none of its tools.
func TestConnectorNotConfiguredWithoutAccounts(t *testing.T) {
	club := core.New(core.Options{Registry: provider.NewRegistry(), FollowUps: []time.Duration{}})
	h := startHost(t, pac.Manifest, exttest.Serve(t, Program(club, pac.Version())), nil)
	var cs wire.ConnectorStatus
	if err := h.Call("connector.status", wire.ConnectorParams{Contribution: Connector}, &cs); err != nil || cs.State != wire.ConnectorStateNotConfigured {
		t.Fatalf("status: %+v %v", cs, err)
	}
}
