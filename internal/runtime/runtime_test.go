package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
	"github.com/nwverse/personal-agent-club/internal/provider/groq"
	"github.com/nwverse/personal-agent-club/internal/serve"
)

func shortSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "pacrt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// fakePersonaNode implements Persona's side of runtime.sock in tests.
type fakePersonaNode struct {
	t        *testing.T
	sockPath string
	ln       net.Listener

	mu           sync.Mutex
	peer         *Peer
	hello        map[string]any
	sessions     []SessionRow
	launchers    []Launcher
	items        []Item
	usageWindows []UsageAccountEvent
	mcpTools     []MCPTool
	mcpCalls     []mcpCallRecord

	readyCh  chan struct{}
	sessCh   chan []SessionRow
	itemCh   chan []Item
	usageCh  chan UsageAccountEvent
	callHook func(ctx context.Context, name string, args json.RawMessage, sessionID string, assistant bool) (MCPCallResult, error)
}

type mcpCallRecord struct {
	Name      string
	Args      json.RawMessage
	SessionID string
	Assistant bool
}

func startFakePersonaNode(t *testing.T, sockPath string) *fakePersonaNode {
	t.Helper()
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen %s: %v", sockPath, err)
	}
	n := &fakePersonaNode{
		t:        t,
		sockPath: sockPath,
		ln:       ln,
		readyCh:  make(chan struct{}, 4),
		sessCh:   make(chan []SessionRow, 32),
		itemCh:   make(chan []Item, 64),
		usageCh:  make(chan UsageAccountEvent, 16),
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go n.serveConn(ctx, c)
		}
	}()
	return n
}

func (n *fakePersonaNode) serveConn(ctx context.Context, c net.Conn) {
	p := NewPeer("n", c)
	p.OnRequest = func(_ context.Context, method string, params json.RawMessage) (any, error) {
		switch method {
		case "runtime.hello":
			var h map[string]any
			_ = json.Unmarshal(params, &h)
			n.mu.Lock()
			n.peer = p
			n.hello = h
			n.mu.Unlock()
			select {
			case n.readyCh <- struct{}{}:
			default:
			}
			return map[string]any{
				"protocol":     ProtocolVersion,
				"node_version": "test",
				"hub":          true,
				"features":     []string{"prefer_running", "launchers", "start_params", "usage_windows"},
			}, nil
		case "mcp.tools":
			n.mu.Lock()
			tools := slices.Clone(n.mcpTools)
			n.mu.Unlock()
			return map[string]any{"tools": tools}, nil
		case "mcp.call":
			var req struct {
				Name      string          `json:"name"`
				Args      json.RawMessage `json:"args"`
				SessionID string          `json:"session_id"`
				Assistant bool            `json:"assistant"`
			}
			_ = json.Unmarshal(params, &req)
			n.mu.Lock()
			n.mcpCalls = append(n.mcpCalls, mcpCallRecord{
				Name:      req.Name,
				Args:      req.Args,
				SessionID: req.SessionID,
				Assistant: req.Assistant,
			})
			hook := n.callHook
			n.mu.Unlock()
			if hook != nil {
				return hook(ctx, req.Name, req.Args, req.SessionID, req.Assistant)
			}
			return MCPCallResult{Content: []MCPContent{{Type: "text", Text: `{"machines":["machine-a"]}`}}}, nil
		default:
			return map[string]any{}, nil
		}
	}
	p.OnEvent = func(event string, params json.RawMessage) {
		switch event {
		case "runtime.sessions":
			var ev struct {
				Sessions []SessionRow `json:"sessions"`
			}
			if json.Unmarshal(params, &ev) == nil {
				n.mu.Lock()
				n.sessions = ev.Sessions
				n.mu.Unlock()
				select {
				case n.sessCh <- ev.Sessions:
				default:
				}
			}
		case "runtime.launchers":
			var ev struct {
				Launchers []Launcher `json:"launchers"`
			}
			if json.Unmarshal(params, &ev) == nil {
				n.mu.Lock()
				n.launchers = ev.Launchers
				n.mu.Unlock()
			}
		case "session.items":
			var ev struct {
				Items []Item `json:"items"`
			}
			if json.Unmarshal(params, &ev) == nil {
				n.mu.Lock()
				n.items = append(n.items, ev.Items...)
				n.mu.Unlock()
				select {
				case n.itemCh <- ev.Items:
				default:
				}
			}
		case "usage.windows":
			var ev UsageAccountEvent
			if json.Unmarshal(params, &ev) == nil {
				n.mu.Lock()
				n.usageWindows = append(n.usageWindows, ev)
				n.mu.Unlock()
				select {
				case n.usageCh <- ev:
				default:
				}
			}
		}
	}
	_ = p.Serve(ctx)
}

func (n *fakePersonaNode) waitReady(t *testing.T) *Peer {
	t.Helper()
	select {
	case <-n.readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("runtime did not complete runtime.hello")
	}
	// Drain initial empty runtime.sessions push cleanly if quick.
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.peer
}

func (n *fakePersonaNode) waitStatus(t *testing.T, sid, wantStatus string) SessionRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		for _, r := range n.sessions {
			if r.SessionID == sid && r.Status == wantStatus {
				n.mu.Unlock()
				return r
			}
		}
		n.mu.Unlock()
		select {
		case <-n.sessCh:
		case <-time.After(25 * time.Millisecond):
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	t.Fatalf("timed out waiting for session %s status=%s (got %+v)", sid, wantStatus, n.sessions)
	return SessionRow{}
}

// scriptedAsyncProvider exercises async turns, LiveStater, Interrupter, LocalUnconfirmed, and tool-relay.
type scriptedAsyncProvider struct {
	name string
	mu   sync.Mutex

	sends     []provider.Outgoing
	messages  []provider.RemoteMessage
	running   bool
	composing bool
	lastErr   string
	interrupt int
	unconfirm bool
	onSend    func(msg provider.Outgoing) string // optional custom reply or "" for async control
	seq       int
}

func newScriptedAsync(name string) *scriptedAsyncProvider {
	return &scriptedAsyncProvider{name: name}
}

func (p *scriptedAsyncProvider) Name() string       { return p.name }
func (p *scriptedAsyncProvider) Connection() string { return protocol.ConnectionServer }
func (p *scriptedAsyncProvider) Verify(context.Context, string) (provider.RemoteAccount, error) {
	return provider.RemoteAccount{ExternalAccountID: "ext-" + p.name, DisplayName: "Async Bot"}, nil
}
func (p *scriptedAsyncProvider) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "main", Title: "Main", Kind: protocol.KindMain, CanSend: true}}, nil
}
func (p *scriptedAsyncProvider) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return provider.RemotePage{Messages: slices.Clone(p.messages)}, nil
}
func (p *scriptedAsyncProvider) Send(_ context.Context, _ string, _ provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sends = append(p.sends, msg)
	if p.unconfirm {
		p.unconfirm = false
		return provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, nil
	}
	p.seq++
	p.messages = append(p.messages, provider.RemoteMessage{
		ExternalID: fmt.Sprintf("u-%d", p.seq),
		Role:       protocol.RoleUser,
		Blocks:     []protocol.Block{protocol.TextBlock(msg.Text)},
		Position:   int64(p.seq),
	})
	if p.onSend != nil {
		if reply := p.onSend(msg); reply != "" {
			p.seq++
			p.messages = append(p.messages, provider.RemoteMessage{
				ExternalID: fmt.Sprintf("a-%d", p.seq),
				Role:       protocol.RoleAssistant,
				Blocks:     []protocol.Block{protocol.TextBlock(reply)},
				Position:   int64(p.seq),
			})
		}
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: fmt.Sprintf("u-%d", p.seq)}, nil
}
func (p *scriptedAsyncProvider) TurnState(context.Context, string, provider.SessionRef) (provider.TurnState, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return provider.TurnState{Running: p.running, Composing: p.composing, LastTurnError: p.lastErr}, nil
}
func (p *scriptedAsyncProvider) Interrupt(context.Context, string, provider.SessionRef) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.interrupt++
	p.running = false
	p.composing = false
	return nil
}

func (p *scriptedAsyncProvider) addAssistantReply(text string, running bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.messages = append(p.messages, provider.RemoteMessage{
		ExternalID: fmt.Sprintf("a-%d", p.seq),
		Role:       protocol.RoleAssistant,
		Blocks:     []protocol.Block{protocol.TextBlock(text)},
		Position:   int64(p.seq),
	})
	p.running = running
}

func TestHelpersContextAndPaging(t *testing.T) {
	in := "<persona-context>\nmachine: machine-b\n</persona-context>\n\n<persona-tools>\n- foo\n</persona-tools>\n\nHello <persona-context attr=\"1\">keep</persona-context> world"
	if got := StripPersonaContext(in); got != `Hello <persona-context attr="1">keep</persona-context> world` {
		t.Fatalf("StripPersonaContext = %q", got)
	}

	longBoot := strings.Repeat("你好世界abcde\n", 1500)
	capped := CapBootstrap(longBoot, 8192)
	if len(capped) > 8192 || !strings.HasSuffix(capped, "[truncated]") {
		t.Fatalf("CapBootstrap len=%d suffixOK=%v", len(capped), strings.HasSuffix(capped, "[truncated]"))
	}

	hugePrefix := strings.Repeat("x", protocol.MaxText)
	fitted := FitMaxText(hugePrefix, "actual question")
	if len16(fitted) > protocol.MaxText || !strings.HasSuffix(fitted, "actual question") {
		t.Fatalf("FitMaxText len16=%d suffixOK=%v", len16(fitted), strings.HasSuffix(fitted, "actual question"))
	}

	name, args, ok := ParseToolCall("Thinking...\n<persona-tool-call>{\"name\":\"machines_list\",\"args\":{\"online\":true}}</persona-tool-call>")
	if !ok || name != "machines_list" || string(args) != `{"online":true}` {
		t.Fatalf("ParseToolCall = %q %s %v", name, args, ok)
	}

	// Test PageItems slicing (turns, limit, before_seq, after_seq, from_seq/through_seq, light clipping).
	bigText := strings.Repeat("a", pageTextBytes+500)
	all := []Item{
		{ID: "i1", Seq: 1, Role: "user", Kind: "text", Text: "first"},
		{ID: "i2", Seq: 2, Role: "assistant", Kind: "text", Text: bigText},
		{ID: "i3", Seq: 3, Role: "user", Kind: "text", Text: "second"},
		{ID: "i4", Seq: 4, Role: "assistant", Kind: "text", Text: "reply2"},
	}
	resTurns := PageItems(all, ItemsRequest{Turns: 1}, defaultHistoryName)
	if len(resTurns.Items) != 2 || resTurns.Items[0].ID != "i3" || !resTurns.HasMore {
		t.Fatalf("PageItems turns=1: %+v", resTurns)
	}
	resBefore := PageItems(all, ItemsRequest{BeforeSeq: 3, Limit: 2}, defaultHistoryName)
	if len(resBefore.Items) != 2 || resBefore.Items[1].FullSize != len(bigText) || resBefore.HasMore {
		t.Fatalf("PageItems before_seq=3: %+v", resBefore)
	}
	after := 2
	resAfter := PageItems(all, ItemsRequest{AfterSeq: &after}, defaultHistoryName)
	if len(resAfter.Items) != 2 || resAfter.ToSeq != 4 {
		t.Fatalf("PageItems after_seq=2: %+v", resAfter)
	}
	resRange := PageItems(all, ItemsRequest{FromSeq: 2, ThroughSeq: 3}, defaultHistoryName)
	if len(resRange.Items) != 2 || resRange.Items[0].ID != "i2" || resRange.Items[1].ID != "i3" {
		t.Fatalf("PageItems range: %+v", resRange)
	}
}

func TestBridgeDirectAndServeFullFlow(t *testing.T) {
	for _, mode := range []string{"direct", "serve"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			ep := echo.New()
			club := core.New(core.Options{Version: "test", Registry: provider.NewRegistry(ep), FollowUps: []time.Duration{}})
			defer club.Close()
			space, err := club.Space(ctx, core.DefaultSpace)
			if err != nil {
				t.Fatal(err)
			}
			// Account 1 enabled as assistant backend; Account 2 NOT enabled.
			acctEnabled, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: "echo-enabled-1", DisplayName: "Echo Main"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: echo.Name, Credential: "echo-hidden-2", DisplayName: "Echo Hidden"}); err != nil {
				t.Fatal(err)
			}
			on := true
			if _, err := space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: acctEnabled.AccountID, AssistantBackend: &on}); err != nil {
				t.Fatal(err)
			}

			var client ClubClient = NewDirectClient(club, core.DefaultSpace)
			if mode == "serve" {
				sdir := shortSockDir(t)
				clubSock := filepath.Join(sdir, "club.sock")
				_, serveDone, err := serve.Run(ctx, club, serve.Config{Socket: clubSock})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					cancel()
					<-serveDone
				})
				sc, err := NewServeClient(clubSock, core.DefaultSpace, "")
				if err != nil {
					t.Fatal(err)
				}
				client = sc
			}

			pdir := shortSockDir(t)
			personaSock := filepath.Join(pdir, "runtime.sock")
			node := startFakePersonaNode(t, personaSock)

			stateFile := filepath.Join(t.TempDir(), "state.json")
			bridge := New(Options{
				Client:       client,
				SocketPath:   personaSock,
				StatePath:    stateFile,
				PollInterval: 20 * time.Millisecond,
				IdleDebounce: 20 * time.Millisecond,
			})
			runDone := make(chan error, 1)
			go func() { runDone <- bridge.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				<-runDone
			})

			peer := node.waitReady(t)

			// Verify handshake caps & launchers (only enabled account, no supervision_policy, empty initial sessions).
			node.mu.Lock()
			hello := node.hello
			initialSessions := slices.Clone(node.sessions)
			node.mu.Unlock()
			caps, _ := hello["caps"].(map[string]any)
			if caps["assistant"] != true || caps["supervision_policy"] != false {
				t.Fatalf("unexpected caps: %+v", caps)
			}
			launchers, _ := hello["launchers"].([]any)
			if len(launchers) != 1 {
				t.Fatalf("expected 1 enabled launcher, got %+v", launchers)
			}
			if len(initialSessions) != 0 {
				t.Fatalf("expected 0 initial regular sessions in runtime.sessions, got %+v", initialSessions)
			}

			wantLauncher := LauncherName(echo.Name, acctEnabled.AccountID)

			// Supervision instructions must fail closed on session.start.
			var startOut map[string]any
			err = peer.Call(ctx, "session.start", map[string]any{
				"cwd":                      "/tmp",
				"assistant":                true,
				"launcher":                 wantLauncher,
				"supervision_instructions": "strict policy",
			}, &startOut)
			var rpcErr *RPCError
			if !errors.As(err, &rpcErr) || rpcErr.Code != "assistant_supervision_runtime" {
				t.Fatalf("expected assistant_supervision_runtime refusal, got %v", err)
			}

			// Valid session.start.
			var started struct {
				Session SessionRow `json:"session"`
			}
			if err := peer.Call(ctx, "session.start", map[string]any{
				"cwd":       "/tmp",
				"assistant": true,
				"launcher":  wantLauncher,
			}, &started); err != nil {
				t.Fatalf("session.start: %v", err)
			}
			sid := started.Session.SessionID
			if !ValidRuntimeSessionID(sid) || started.Session.Launcher != wantLauncher {
				t.Fatalf("unexpected started session: %+v", started.Session)
			}

			// Watch & confirm anti-loop active session key recorded in club.
			if err := peer.Call(ctx, "session.watch", map[string]any{"session_id": sid, "since_seq": 0}, nil); err != nil {
				t.Fatalf("session.watch: %v", err)
			}
			activeKeys, err := space.AssistantSessionKeys(ctx)
			backingKey := core.SessionKey(echo.Name, acctEnabled.AccountID, "main")
			if err != nil || !activeKeys[backingKey] {
				t.Fatalf("expected backing key %s registered as active assistant session: %+v err=%v", backingKey, activeKeys, err)
			}

			// Supervision instructions must fail closed on session.send too.
			err = peer.Call(ctx, "session.send", map[string]any{
				"session_id":               sid,
				"text":                     "hi",
				"supervision_instructions": "do triage",
			}, nil)
			if !errors.As(err, &rpcErr) || rpcErr.Code != "assistant_supervision_runtime" {
				t.Fatalf("expected session.send supervision refusal, got %v", err)
			}

			// First turn with oversized >8 KiB bootstrap context & client_msg_id dedupe check.
			largeCtx := strings.Repeat("MEMORY note line\n", 800)
			if err := peer.Call(ctx, "session.send", map[string]any{
				"session_id":    sid,
				"text":          "ping first",
				"client_msg_id": "persona-msg-1",
				"assistant":     true,
				"context":       largeCtx,
			}, nil); err != nil {
				t.Fatalf("session.send: %v", err)
			}

			row := node.waitStatus(t, sid, "done")
			if !strings.HasPrefix(row.LastText, "echo:") {
				t.Fatalf("unexpected row last_text: %+v", row)
			}

			// Duplicate send with same client_msg_id must not deliver a second time.
			if err := peer.Call(ctx, "session.send", map[string]any{
				"session_id":    sid,
				"text":          "ping first",
				"client_msg_id": "persona-msg-1",
				"assistant":     true,
			}, nil); err != nil {
				t.Fatalf("duplicate session.send: %v", err)
			}
			if len(node.items) != 2 {
				t.Fatalf("expected 2 items (no duplicate), got %d: %+v", len(node.items), node.items)
			}

			// Outbound message inside club store should carry capped <= 8 KiB bootstrap.
			storedPage, _ := space.Messages(ctx, protocol.MessagesParams{SessionKey: backingKey, Limit: 10})
			if len(storedPage.Messages) != 2 {
				t.Fatalf("expected 2 club messages (user+assistant), got %d", len(storedPage.Messages))
			}
			sentRaw := storedPage.Messages[0].Blocks[0].Text
			if !strings.Contains(sentRaw, "<persona-context>") || !strings.Contains(sentRaw, "[truncated]") || len(sentRaw) > 8500 {
				t.Fatalf("expected capped first-turn bootstrap in club message, len=%d", len(sentRaw))
			}

			// Verify Persona user item has clean text and second turn has clean assistant LastText.
			var pageRes ItemsResult
			if err := peer.Call(ctx, "session.items", map[string]any{"session_id": sid, "limit": 10}, &pageRes); err != nil {
				t.Fatalf("session.items: %v", err)
			}
			if len(pageRes.Items) != 2 || strings.Contains(pageRes.Items[0].Text, "<persona-context>") || !strings.Contains(pageRes.Items[1].Text, "ping first") {
				t.Fatalf("unexpected items: %+v", pageRes.Items)
			}

			// Second turn (empty context) + full item retrieval via session.item.
			if err := peer.Call(ctx, "session.send", map[string]any{
				"session_id":    sid,
				"text":          "ping second",
				"client_msg_id": "persona-msg-2",
				"assistant":     true,
			}, nil); err != nil {
				t.Fatalf("session.send 2: %v", err)
			}
			row = node.waitStatus(t, sid, "done")
			// waitStatus sees done immediately because turn 1 was done unless we wait for Rev >= 4.
			deadline := time.Now().Add(3 * time.Second)
			for row.LastText != "echo: ping second" && time.Now().Before(deadline) {
				time.Sleep(15 * time.Millisecond)
				row = node.waitStatus(t, sid, "done")
			}
			if row.LastText != "echo: ping second" {
				t.Fatalf("expected echo: ping second, got %+v", row)
			}
			var fullItem Item
			if err := peer.Call(ctx, "session.item", map[string]any{"session_id": sid, "item_id": pageRes.Items[1].ID}, &fullItem); err != nil {
				t.Fatalf("session.item: %v", err)
			}
			if !strings.Contains(fullItem.Text, "ping first") {
				t.Fatalf("session.item missing content: %+v", fullItem)
			}

			// Verify state file persisted and session.close removes active key.
			if _, err := os.Stat(stateFile); err != nil {
				t.Fatalf("state file missing: %v", err)
			}
			if err := peer.Call(ctx, "session.close", map[string]any{"session_id": sid}, nil); err != nil {
				t.Fatalf("session.close: %v", err)
			}
			activeKeys, _ = space.AssistantSessionKeys(ctx)
			if len(activeKeys) != 0 {
				t.Fatalf("expected active assistant keys cleared on close, got %+v", activeKeys)
			}
		})
	}
}

func TestBridgeAsyncLiveDebounceInterruptAndUnconfirmed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	asyncP := newScriptedAsync("cue")
	var clockMu sync.Mutex
	now := time.UnixMilli(1_800_000_000_000)
	nowFn := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return now
	}
	advanceClock := func(d time.Duration) {
		clockMu.Lock()
		now = now.Add(d)
		clockMu.Unlock()
	}

	club := core.New(core.Options{
		Version:   "test",
		Registry:  provider.NewRegistry(asyncP),
		Now:       nowFn,
		FollowUps: []time.Duration{},
	})
	defer club.Close()
	space, err := club.Space(ctx, core.DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "cue", Credential: "cue-cred-1", DisplayName: "Cue Async"})
	if err != nil {
		t.Fatal(err)
	}
	on := true
	if _, err := space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: acct.AccountID, AssistantBackend: &on}); err != nil {
		t.Fatal(err)
	}

	pdir := shortSockDir(t)
	sock := filepath.Join(pdir, "runtime.sock")
	node := startFakePersonaNode(t, sock)

	bridge := New(Options{
		Client:       NewDirectClient(club, core.DefaultSpace),
		SocketPath:   sock,
		PollInterval: 20 * time.Millisecond,
		IdleDebounce: 30 * time.Millisecond,
		Log:          nil,
	})
	go func() { _ = bridge.Run(ctx) }()
	peer := node.waitReady(t)

	var started struct {
		Session SessionRow `json:"session"`
	}
	if err := peer.Call(ctx, "session.start", map[string]any{"cwd": "/tmp", "assistant": true}, &started); err != nil {
		t.Fatal(err)
	}
	sid := started.Session.SessionID
	_ = peer.Call(ctx, "session.watch", map[string]any{"session_id": sid, "since_seq": 0}, nil)

	// 1. Running async turn stays working until reply arrives + Live.Running goes false across IdleDebounce.
	asyncP.mu.Lock()
	asyncP.running = true
	asyncP.mu.Unlock()
	if err := peer.Call(ctx, "session.send", map[string]any{
		"session_id":    sid,
		"text":          "slow research",
		"client_msg_id": "m-async-1",
	}, nil); err != nil {
		t.Fatal(err)
	}
	node.waitStatus(t, sid, "working")

	asyncP.addAssistantReply("working step result", false)
	advanceClock(4 * time.Second) // expire core.Space's 3s LiveState cache immediately
	row := node.waitStatus(t, sid, "done")
	if row.LastText != "working step result" {
		t.Fatalf("expected settled reply, got %+v", row)
	}

	// 2. Interrupt halts running turn and calls Interrupter.
	asyncP.mu.Lock()
	asyncP.running = true
	asyncP.mu.Unlock()
	if err := peer.Call(ctx, "session.send", map[string]any{
		"session_id":    sid,
		"text":          "abort me",
		"client_msg_id": "m-async-2",
	}, nil); err != nil {
		t.Fatal(err)
	}
	node.waitStatus(t, sid, "working")

	var intRes map[string]any
	if err := peer.Call(ctx, "session.interrupt", map[string]any{"session_id": sid}, &intRes); err != nil {
		t.Fatalf("session.interrupt: %v", err)
	}
	if intRes["settled"] != true {
		t.Fatalf("expected settled=true, got %+v", intRes)
	}
	node.waitStatus(t, sid, "idle")
	asyncP.mu.Lock()
	ints := asyncP.interrupt
	asyncP.mu.Unlock()
	if ints != 1 {
		t.Fatalf("expected 1 interrupt call, got %d", ints)
	}

	// 3. Unconfirmed send returns send_unconfirmed with delivered="unknown" and never auto-resends.
	asyncP.mu.Lock()
	asyncP.unconfirm = true
	beforeSends := len(asyncP.sends)
	asyncP.mu.Unlock()
	err = peer.Call(ctx, "session.send", map[string]any{
		"session_id":    sid,
		"text":          "maybe lost",
		"client_msg_id": "m-unconf-1",
	}, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != "send_unconfirmed" || !strings.Contains(string(rpcErr.Data), `"unknown"`) {
		t.Fatalf("expected send_unconfirmed unknown, got %v", err)
	}
	asyncP.mu.Lock()
	afterSends := len(asyncP.sends)
	asyncP.mu.Unlock()
	if afterSends != beforeSends+1 {
		t.Fatalf("expected exactly 1 send attempt, got %d", afterSends-beforeSends)
	}
	// session.send_unseen allows explicit owner-approved retry.
	if err := peer.Call(ctx, "session.send_unseen", map[string]any{
		"session_id":    sid,
		"text":          "maybe lost",
		"client_msg_id": "m-unconf-1",
	}, nil); err != nil {
		t.Fatalf("session.send_unseen: %v", err)
	}
	asyncP.addAssistantReply("received retry", false)
	advanceClock(4 * time.Second)
	node.waitStatus(t, sid, "done")
}

func TestBridgeChatToolRelayAndStepCap(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	chatP := newScriptedAsync("cue")
	var relayStep atomic.Int32
	chatP.onSend = func(msg provider.Outgoing) string {
		if strings.Contains(msg.Text, "start relay") {
			relayStep.Store(1)
			return `<persona-tool-call>{"name":"machines_list","args":{"online":true}}</persona-tool-call>`
		}
		if strings.Contains(msg.Text, "<persona-tool-result") && relayStep.Load() == 1 {
			relayStep.Store(2)
			return "Your machine machine-a is online."
		}
		if strings.Contains(msg.Text, "loop forever") || strings.Contains(msg.Text, "<persona-tool-result") {
			return `<persona-tool-call>{"name":"machines_list","args":{}}</persona-tool-call>`
		}
		return "ok"
	}

	club := core.New(core.Options{Version: "test", Registry: provider.NewRegistry(chatP), FollowUps: []time.Duration{}})
	defer club.Close()
	space, _ := club.Space(ctx, core.DefaultSpace)
	acct, _ := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "cue", Credential: "cue-relay-1", DisplayName: "Cue Relay"})
	on := true
	_, _ = space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: acct.AccountID, AssistantBackend: &on})

	pdir := shortSockDir(t)
	sock := filepath.Join(pdir, "runtime.sock")
	node := startFakePersonaNode(t, sock)
	node.mcpTools = []MCPTool{{Name: "machines_list", Description: "List online machines"}}

	bridge := New(Options{
		Client:       NewDirectClient(club, core.DefaultSpace),
		SocketPath:   sock,
		ToolRelay:    true,
		PollInterval: 10 * time.Millisecond,
		IdleDebounce: 10 * time.Millisecond,
	})
	go func() { _ = bridge.Run(ctx) }()
	peer := node.waitReady(t)

	var started struct {
		Session SessionRow `json:"session"`
	}
	if err := peer.Call(ctx, "session.start", map[string]any{"cwd": "/tmp", "assistant": true}, &started); err != nil {
		t.Fatal(err)
	}
	sid := started.Session.SessionID
	_ = peer.Call(ctx, "session.watch", map[string]any{"session_id": sid, "since_seq": 0}, nil)

	if err := peer.Call(ctx, "session.send", map[string]any{
		"session_id":    sid,
		"text":          "start relay",
		"client_msg_id": "relay-1",
		"assistant":     true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	row := node.waitStatus(t, sid, "done")
	if row.LastText != "Your machine machine-a is online." {
		t.Fatalf("unexpected relay final row: %+v", row)
	}
	node.mu.Lock()
	calls := slices.Clone(node.mcpCalls)
	node.mu.Unlock()
	if len(calls) != 1 || calls[0].Name != "machines_list" || !calls[0].Assistant || calls[0].SessionID != sid {
		t.Fatalf("unexpected mcp calls: %+v", calls)
	}

	// Verify infinite loop halts cleanly at maxToolSteps (8).
	if err := peer.Call(ctx, "session.send", map[string]any{
		"session_id":    sid,
		"text":          "loop forever",
		"client_msg_id": "relay-loop",
		"assistant":     true,
	}, nil); err != nil {
		t.Fatal(err)
	}
	node.waitStatus(t, sid, "done")
	node.mu.Lock()
	totalCalls := len(node.mcpCalls)
	node.mu.Unlock()
	if totalCalls != 1+maxToolSteps {
		t.Fatalf("expected %d total mcp calls after 8-step cap, got %d", 1+maxToolSteps, totalCalls)
	}
}

func TestBridgeGroqNativeToolLoopAndUsageWindows(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var step atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Header().Set("x-ratelimit-limit-requests", "100")
		w.Header().Set("x-ratelimit-remaining-requests", "40")
		w.Header().Set("x-ratelimit-reset-requests", "10m")
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"`+groq.DefaultModel+`"}]}`)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			n := step.Add(1)
			if n == 1 && strings.Contains(string(body), `"tools"`) {
				_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_g1","type":"function","function":{"name":"agent_read","arguments":"{\"id\":\"a1\"}"}}]}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Agent a1 finished tests."}}]}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	gq := groq.New(groq.Options{Origin: srv.URL + "/openai/v1"})
	club := core.New(core.Options{Version: "test", Registry: provider.NewRegistry(gq), FollowUps: []time.Duration{}})
	defer club.Close()
	space, _ := club.Space(ctx, core.DefaultSpace)
	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: groq.Name, Credential: "fixture-groq-key", DisplayName: "Groq Chief"})
	if err != nil {
		t.Fatal(err)
	}
	on := true
	_, _ = space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: acct.AccountID, AssistantBackend: &on})

	pdir := shortSockDir(t)
	sock := filepath.Join(pdir, "runtime.sock")
	node := startFakePersonaNode(t, sock)
	node.mcpTools = []MCPTool{{
		Name:        "agent_read",
		Description: "Read agent output",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"id":{"type":"string"}}}`),
	}}
	node.callHook = func(_ context.Context, name string, _ json.RawMessage, _ string, _ bool) (MCPCallResult, error) {
		return MCPCallResult{Content: []MCPContent{{Type: "text", Text: "tests passed"}}}, nil
	}

	bridge := New(Options{
		Client:     NewDirectClient(club, core.DefaultSpace),
		SocketPath: sock,
		ToolRelay:  true,
	})
	go func() { _ = bridge.Run(ctx) }()
	peer := node.waitReady(t)

	// Confirm usage.windows emitted on connect with valid window_minutes and used_percent.
	select {
	case uw := <-node.usageCh:
		if uw.Provider != groq.Name || len(uw.Windows) == 0 || uw.Windows[0].WindowMinutes <= 0 || uw.Windows[0].UsedPercent != 60 {
			t.Fatalf("unexpected usage.windows event: %+v", uw)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("expected usage.windows event")
	}

	var started struct {
		Session SessionRow `json:"session"`
	}
	if err := peer.Call(ctx, "session.start", map[string]any{"cwd": "/tmp", "assistant": true}, &started); err != nil {
		t.Fatal(err)
	}
	sid := started.Session.SessionID
	_ = peer.Call(ctx, "session.watch", map[string]any{"session_id": sid, "since_seq": 0}, nil)

	if err := peer.Call(ctx, "session.send", map[string]any{
		"session_id":    sid,
		"text":          "Check agent a1",
		"client_msg_id": "groq-msg-1",
		"assistant":     true,
		"context":       "machine: machine-b",
	}, nil); err != nil {
		t.Fatal(err)
	}
	row := node.waitStatus(t, sid, "done")
	if row.LastText != "Agent a1 finished tests." {
		t.Fatalf("unexpected groq final row: %+v", row)
	}

	var pageRes ItemsResult
	if err := peer.Call(ctx, "session.items", map[string]any{"session_id": sid, "limit": 20}, &pageRes); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, it := range pageRes.Items {
		kinds = append(kinds, it.Kind)
	}
	if !slices.Equal(kinds, []string{"text", "tool_use", "tool_result", "text"}) {
		t.Fatalf("unexpected groq item kinds: %v (%+v)", kinds, pageRes.Items)
	}
}

func TestBridgeGroqRateLimitSetsUsageLimitTurnError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/models") {
			_, _ = io.WriteString(w, `{"data":[{"id":"`+groq.DefaultModel+`"}]}`)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/chat/completions") {
			w.Header().Set("Retry-After", "45")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"rate limit"}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	gq := groq.New(groq.Options{Origin: srv.URL + "/openai/v1"})
	club := core.New(core.Options{Version: "test", Registry: provider.NewRegistry(gq), FollowUps: []time.Duration{}})
	defer club.Close()
	space, _ := club.Space(ctx, core.DefaultSpace)
	acct, _ := space.AddAccount(ctx, protocol.AddAccountParams{Provider: groq.Name, Credential: "fixture-groq-key"})
	on := true
	_, _ = space.UpdateAccount(ctx, protocol.UpdateAccountParams{AccountID: acct.AccountID, AssistantBackend: &on})

	pdir := shortSockDir(t)
	sock := filepath.Join(pdir, "runtime.sock")
	node := startFakePersonaNode(t, sock)

	bridge := New(Options{Client: NewDirectClient(club, core.DefaultSpace), SocketPath: sock})
	go func() { _ = bridge.Run(ctx) }()
	peer := node.waitReady(t)

	var started struct {
		Session SessionRow `json:"session"`
	}
	if err := peer.Call(ctx, "session.start", map[string]any{"cwd": "/tmp", "assistant": true}, &started); err != nil {
		t.Fatal(err)
	}
	sid := started.Session.SessionID
	if err := peer.Call(ctx, "session.send", map[string]any{"session_id": sid, "text": "hi", "client_msg_id": "rl-1"}, nil); err != nil {
		t.Fatal(err)
	}
	row := node.waitStatus(t, sid, "done")
	if row.TurnErrorKind != "usage_limit" || row.TurnErrorCode != "rate_limited" {
		t.Fatalf("expected usage_limit/rate_limited, got %+v", row)
	}
}

// Verify bufio scanner helper isn't accidentally importing unused packages.
var _ = bufio.ErrTooLong
