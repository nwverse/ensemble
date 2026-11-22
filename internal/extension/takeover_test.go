package extension

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/persona/sdk/go/wire"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
)

func TestTakeoverAndWakeFlow(t *testing.T) {
	h, club := start(t)
	ctx := context.Background()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{
		Provider:    echo.Name,
		Credential:  "echo-takeover",
		DisplayName: "Demo VM",
	})
	if err != nil {
		t.Fatal(err)
	}
	wantMID := takeoverMachineID(acct)

	machineState := machineStatusResult{MachineID: wantMID, Exists: false}
	var pairedReq machinePairLinkParams
	h.Answer(methodMachinesStatus, func(raw json.RawMessage) (any, error) {
		var p machineStatusParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.MachineID != wantMID {
			t.Errorf("machines.status machine_id = %q, want %q", p.MachineID, wantMID)
		}
		return machineState, nil
	})
	h.Answer(methodMachinesPairLink, func(raw json.RawMessage) (any, error) {
		if err := json.Unmarshal(raw, &pairedReq); err != nil {
			return nil, err
		}
		machineState = machineStatusResult{
			MachineID: wantMID,
			Name:      "Demo VM",
			Exists:    true,
			Pending:   true,
			Online:    false,
		}
		return machinePairLinkResult{
			MachineID:    wantMID,
			Name:         "Demo VM",
			Link:         "persona://link?token=jp_test&machine=" + wantMID,
			ExpiresAt:    time.Now().Add(10 * time.Minute).UTC(),
			Pending:      true,
			InstallLinux: "curl -fsSL https://example.com/install.sh | sh -s -- --node --pair 'persona://link?token=jp_test&machine=" + wantMID + "'",
		}, nil
	})

	// 1. Account view shows unpaired state + Pair as Machine button.
	st := pageState{Page: pageAccount, Account: acct.AccountID}
	view := render(t, h, st)
	viewJSON, _ := json.Marshal(view)
	if !strings.Contains(string(viewJSON), `"id":"takeover"`) {
		t.Fatalf("expected takeover button in view: %s", viewJSON)
	}

	// 2. Click Pair as Machine -> calls machines.pair_link and sends bootstrap message to session.
	var res wire.ActionResult
	if err := h.Call("ui.action", wire.UIParams{
		Contribution: AccountsUI,
		Placement:    wire.PlacementPluginPage,
		Size:         "full",
		UIVersion:    6,
		Locale:       "zh-Hans",
		State:        stateOf(st),
		Action:       actTakeover,
	}, &res); err != nil {
		t.Fatalf("ui.action takeover: %v", err)
	}
	if pairedReq.MachineID != wantMID || pairedReq.OS != "linux" {
		t.Fatalf("machines.pair_link params = %+v", pairedReq)
	}
	if res.View == nil || res.View.RefreshAfter != 10 {
		t.Fatalf("expected RefreshAfter=10 while pending, got %+v", res.View)
	}
	sessions, err := space.Sessions(ctx)
	if err != nil || len(sessions.Sessions) == 0 {
		t.Fatalf("sessions = %+v err=%v", sessions, err)
	}
	msgs, err := space.Messages(ctx, protocol.MessagesParams{SessionKey: sessions.Sessions[0].SessionKey})
	if err != nil {
		t.Fatalf("messages: %v", err)
	}
	foundBootstrap := false
	for _, m := range msgs.Messages {
		for _, b := range m.Blocks {
			if strings.Contains(b.Text, "persona://link?token=jp_test&machine="+wantMID) {
				foundBootstrap = true
			}
		}
	}
	if !foundBootstrap {
		t.Fatalf("bootstrap prompt not found in messages: %+v", msgs.Messages)
	}

	// 3. Machine confirmed & then goes offline -> Wake Machine sends wake prompt.
	machineState = machineStatusResult{
		MachineID: wantMID,
		Name:      "Demo VM",
		Exists:    true,
		Pending:   false,
		Online:    false,
	}
	var wakeRes wire.ActionResult
	if err := h.Call("ui.action", wire.UIParams{
		Contribution: AccountsUI,
		Placement:    wire.PlacementPluginPage,
		Size:         "full",
		UIVersion:    6,
		Locale:       "zh-Hans",
		State:        stateOf(st),
		Action:       actWake,
	}, &wakeRes); err != nil {
		t.Fatalf("ui.action wake: %v", err)
	}
	if !strings.Contains(wakeRes.Toast, wantMID) {
		t.Fatalf("wake toast = %q", wakeRes.Toast)
	}
}
