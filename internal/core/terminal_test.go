package core

import (
	"context"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

func TestValidateSSHTarget(t *testing.T) {
	valid := []struct {
		in   string
		dest string
		port string
	}{
		{"muse-box", "muse-box", ""},
		{"ubuntu@muse-vm.ts.net", "ubuntu@muse-vm.ts.net", ""},
		{"root@100.64.1.9:2222", "root@100.64.1.9", "2222"},
		{"dev_1@box-01.internal:22", "dev_1@box-01.internal", "22"},
	}
	for _, tc := range valid {
		dest, port, err := SplitSSHTarget(tc.in)
		if err != nil || dest != tc.dest || port != tc.port {
			t.Errorf("SplitSSHTarget(%q) = (%q, %q, %v), want (%q, %q, nil)", tc.in, dest, port, err, tc.dest, tc.port)
		}
	}

	invalid := []string{
		"",
		"   ",
		"-oProxyCommand=id",
		"-t muse-box",
		"user@-host",
		"user@host;id",
		"user@host && whoami",
		"user@host:0",
		"user@host:70000",
		"user@host\nid",
		"ssh user@host",
		"tailscale ssh muse-box",
		"user@a..b",
		"-----BEGIN OPENSSH PRIVATE KEY-----",
	}
	for _, bad := range invalid {
		if err := ValidateSSHTarget(bad); err == nil {
			t.Errorf("ValidateSSHTarget(%q) = nil, want error", bad)
		}
	}
}

type boxStateProvider struct {
	name  string
	state provider.BoxState
}

func (b *boxStateProvider) Name() string       { return b.name }
func (b *boxStateProvider) Connection() string { return protocol.ConnectionServer }
func (b *boxStateProvider) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: b.name + "-1"}, nil
}
func (b *boxStateProvider) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "main", Title: "Main", Kind: protocol.KindMain, CanSend: true}}, nil
}
func (b *boxStateProvider) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}
func (b *boxStateProvider) Send(context.Context, string, provider.SessionRef, provider.Outgoing, provider.SendContext) (provider.SendResult, error) {
	return provider.SendResult{Status: provider.Sent}, nil
}
func (b *boxStateProvider) BoxState(context.Context, string) (provider.BoxState, error) {
	return b.state, nil
}

func TestAccountSSHTargetAndBoxState(t *testing.T) {
	bs := &boxStateProvider{
		name:  "grok_bot",
		state: provider.BoxState{State: "running", Image: "sand-v2", SourceNote: "grok_bot:GetSandBoxRunState"},
	}
	club := New(Options{Registry: provider.NewRegistry(bs), FollowUps: []time.Duration{}})
	t.Cleanup(club.Close)
	ctx := context.Background()
	space, err := club.Space(ctx, DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}

	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "grok_bot", Credential: "tok-12345678"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := space.AccountSSHTarget(ctx, acct.AccountID); err != nil || ok {
		t.Fatalf("initial target = %v, %v", ok, err)
	}
	if err := space.SetAccountSSHTarget(ctx, acct.AccountID, SSHTarget{Target: "-oProxyCommand=id"}); protocol.CodeOf(err) != protocol.CodeInvalid {
		t.Fatalf("option injection err = %v", err)
	}
	if err := space.SetAccountSSHTarget(ctx, acct.AccountID, SSHTarget{Target: "ubuntu@muse-1.ts.net", Transport: TransportTailscale}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := space.AccountSSHTarget(ctx, acct.AccountID)
	if err != nil || !ok || got.Target != "ubuntu@muse-1.ts.net" || got.Transport != TransportTailscale {
		t.Fatalf("saved target = %+v, ok=%v, err=%v", got, ok, err)
	}
	st, supported, err := space.AccountBoxState(ctx, acct.AccountID)
	if err != nil || !supported || st.State != "running" || st.Image != "sand-v2" {
		t.Fatalf("box state = %+v, supported=%v, err=%v", st, supported, err)
	}
	// Removing the account cleans up its saved SSH target.
	if err := space.RemoveAccount(ctx, protocol.RemoveAccountParams{AccountID: acct.AccountID}); err != nil {
		t.Fatal(err)
	}
	if m, err := space.readSSHTargets(ctx); err != nil || len(m) != 0 {
		t.Fatalf("ssh target not cleaned up after RemoveAccount: %+v %v", m, err)
	}
}

type computerTestProvider struct {
	boxStateProvider
	supportsBox bool
	supportsCmp bool
	ranCmd      string
	runResult   provider.ComputerRunResult
	runErr      error
}

func (c *computerTestProvider) SupportsBoxState() bool { return c.supportsBox }
func (c *computerTestProvider) SupportsComputer() bool { return c.supportsCmp }
func (c *computerTestProvider) RunComputer(_ context.Context, _, cmd string) (provider.ComputerRunResult, error) {
	c.ranCmd = cmd
	if c.runErr != nil {
		return provider.ComputerRunResult{}, c.runErr
	}
	return c.runResult, nil
}

func TestAccountBoxStateSupporterAndRunComputer(t *testing.T) {
	netOn := true
	gemini := &computerTestProvider{
		boxStateProvider: boxStateProvider{
			name: "gemini_tasks",
			state: provider.BoxState{
				State:          "running",
				Image:          "Debian 12 · 2 vCPU · 8 GB",
				InternetAccess: &netOn,
			},
		},
		supportsBox: true,
		supportsCmp: true,
		runResult: provider.ComputerRunResult{
			Status:    provider.Sent,
			SessionID: "comp_123",
			Output:    "Linux localhost 6.6.137+ x86_64",
			Answered:  true,
		},
	}
	duet := &computerTestProvider{
		boxStateProvider: boxStateProvider{name: "claude_duet"},
		supportsBox:      false,
		supportsCmp:      false,
	}
	club := New(Options{Registry: provider.NewRegistry(gemini, duet), FollowUps: []time.Duration{}})
	t.Cleanup(club.Close)
	ctx := context.Background()
	space, err := club.Space(ctx, DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	gAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "gemini_tasks", Credential: "fixture-gemini-cred"})
	if err != nil {
		t.Fatal(err)
	}
	dAcct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "claude_duet", Credential: "fixture-duet-cred"})
	if err != nil {
		t.Fatal(err)
	}

	if _, supported, err := space.AccountBoxState(ctx, dAcct.AccountID); err != nil || supported {
		t.Fatalf("duet AccountBoxState supported=%v err=%v, want false, nil", supported, err)
	}
	if space.SupportsAccountComputer(ctx, dAcct.AccountID) {
		t.Fatal("duet should not support AccountComputer")
	}

	gst, supported, err := space.AccountBoxState(ctx, gAcct.AccountID)
	if err != nil || !supported || gst.State != "running" || gst.InternetAccess == nil || !*gst.InternetAccess {
		t.Fatalf("gemini AccountBoxState = %+v, supported=%v, err=%v", gst, supported, err)
	}
	if !space.SupportsAccountComputer(ctx, gAcct.AccountID) {
		t.Fatal("gemini should support AccountComputer")
	}
	res, err := space.RunAccountComputer(ctx, gAcct.AccountID, "uname -srm")
	if err != nil || res.Status != provider.Sent || res.Output != "Linux localhost 6.6.137+ x86_64" || gemini.ranCmd != "uname -srm" {
		t.Fatalf("RunAccountComputer = %+v, %v (cmd=%q)", res, err, gemini.ranCmd)
	}
}
