package runtime

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"crypto/rand"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

const (
	defaultRuntimeID   = "personal-agent-club"
	defaultRuntimeName = "Personal Agent Club"
	defaultPoll        = 2 * time.Second
	maxPoll            = 10 * time.Second
	defaultDebounce    = 1500 * time.Millisecond
	defaultMaxTurnWait = 30 * time.Minute
	maxToolSteps       = 8
	sendDedupeTTL      = 10 * time.Minute
	rewatchMaxPages    = 8
	rewatchPageSize    = 500
)

// Options configure the Persona assistant runtime bridge.
type Options struct {
	Client       ClubClient
	SocketPath   string
	StatePath    string
	RuntimeID    string
	Name         string
	Version      string
	AccountID    string
	Launcher     string
	AllAccounts  bool
	ToolRelay    bool
	PollInterval time.Duration
	IdleDebounce time.Duration
	MaxTurnWait  time.Duration
	Now          func() time.Time
	Log          *slog.Logger
}

type accountTarget struct {
	account  protocol.Account
	launcher string
}

type sentRecord struct {
	status string // "sent" | "unconfirmed"
	at     time.Time
}

type sessionState struct {
	binding        SessionBinding
	status         string // "idle" | "working" | "blocked" | "done" | "offline"
	rev            int64
	lastText       string
	turnError      string
	turnErrorKind  string
	turnErrorCode  string
	turnErrorReset *time.Time

	items           []Item
	itemByID        map[string]Item
	nextSeq         int
	watched         bool
	turnEpoch       uint64
	turnCancel      context.CancelFunc
	pendingOutboxID string
	sentIDs         map[string]sentRecord
}

type toolCapableClient interface {
	SupportsTools(ctx context.Context, accountID string) bool
}

// Bridge exposes club assistant-enabled accounts as a Persona caps.assistant runtime over ~/.persona/runtime.sock.
type Bridge struct {
	opts     Options
	instance string
	log      *slog.Logger
	now      func() time.Time

	mu           sync.Mutex
	rootCtx      context.Context
	peer         *Peer
	hubConnected bool
	nodeFeatures []string
	accounts     []accountTarget
	sessions     map[string]*sessionState
	turnWG       sync.WaitGroup
}

// New constructs a Bridge from opts.
func New(opts Options) *Bridge {
	if opts.SocketPath == "" {
		opts.SocketPath = DefaultPersonaSocketPath()
	}
	if strings.TrimSpace(opts.RuntimeID) == "" {
		opts.RuntimeID = defaultRuntimeID
	}
	if strings.TrimSpace(opts.Name) == "" {
		opts.Name = defaultRuntimeName
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = defaultPoll
	}
	if opts.IdleDebounce <= 0 {
		opts.IdleDebounce = defaultDebounce
	}
	if opts.MaxTurnWait <= 0 {
		opts.MaxTurnWait = defaultMaxTurnWait
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	return &Bridge{
		opts:     opts,
		instance: randomUUID(),
		log:      opts.Log,
		now:      opts.Now,
		sessions: map[string]*sessionState{},
	}
}

// Run maintains the runtime connection to Persona's runtime.sock until ctx is canceled.
func (b *Bridge) Run(ctx context.Context) error {
	if b.opts.Client == nil {
		return errors.New("runtime.Bridge requires a non-nil Client")
	}
	b.mu.Lock()
	b.rootCtx = ctx
	b.mu.Unlock()

	if err := b.loadPersistedState(ctx); err != nil {
		b.log.Warn("load runtime state", "err", err)
	}
	_ = b.refreshAccounts(ctx)
	b.syncAssistantSessionsBestEffort(ctx)

	go b.watchClubUpdates(ctx)

	backoff := time.Second
	for {
		if ctx.Err() != nil {
			b.waitTurns()
			return nil
		}
		err, conflict := b.connectOnce(ctx)
		if ctx.Err() != nil {
			b.waitTurns()
			return nil
		}
		wait := backoff
		if conflict {
			wait = 30 * time.Second
		} else if _, statErr := os.Stat(b.opts.SocketPath); statErr == nil {
			wait = time.Second
			backoff = time.Second
		} else {
			backoff = min(backoff*2, 30*time.Second)
		}
		if err != nil {
			b.log.Debug("runtime connection ended", "err", err, "retry_in", wait.String())
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			b.waitTurns()
			return nil
		case <-t.C:
		}
	}
}

func (b *Bridge) waitTurns() {
	b.turnWG.Wait()
}

func (b *Bridge) loadPersistedState(ctx context.Context) error {
	bindings, err := loadState(b.opts.StatePath)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sb := range bindings {
		if !ValidRuntimeSessionID(sb.SessionID) {
			continue
		}
		b.sessions[sb.SessionID] = &sessionState{
			binding:  sb,
			status:   "done",
			itemByID: map[string]Item{},
			sentIDs:  map[string]sentRecord{},
		}
	}
	return nil
}

func (b *Bridge) persistStateLocked() {
	bindings := make([]SessionBinding, 0, len(b.sessions))
	for _, s := range b.sessions {
		bindings = append(bindings, s.binding)
	}
	path := b.opts.StatePath
	// Best effort write; avoid blocking long while holding locks by copying slice first.
	if err := saveState(path, bindings); err != nil {
		b.log.Warn("save runtime state", "err", err)
	}
}

func (b *Bridge) syncAssistantSessionsBestEffort(ctx context.Context) {
	b.mu.Lock()
	var keys []string
	for _, s := range b.sessions {
		if s.binding.SessionKey != "" {
			keys = append(keys, s.binding.SessionKey)
		}
	}
	b.mu.Unlock()
	_ = b.opts.Client.SetAssistantSessions(ctx, keys)
}

func (b *Bridge) watchClubUpdates(ctx context.Context) {
	evCh, stop := b.opts.Client.Subscribe(ctx)
	defer stop()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-evCh:
			if !ok {
				return
			}
			b.onAccountsMaybeChanged(ctx)
		case <-tick.C:
			b.onAccountsMaybeChanged(ctx)
		}
	}
}

func (b *Bridge) onAccountsMaybeChanged(ctx context.Context) {
	before := b.launcherSnapshot()
	if err := b.refreshAccounts(ctx); err != nil {
		return
	}
	after := b.launcherSnapshot()
	p := b.currentPeer()
	if p == nil {
		return
	}
	if !slices.Equal(before, after) {
		_ = p.Notify("runtime.launchers", map[string]any{"launchers": after})
	}
	b.emitUsageWindows(p)
}

func (b *Bridge) refreshAccounts(ctx context.Context) error {
	st, err := b.opts.Client.Status(ctx)
	if err != nil {
		return err
	}
	var targets []accountTarget
	override := SanitizeLauncher(b.opts.Launcher)
	for _, acct := range st.Accounts {
		if acct.Status == protocol.AccountPaused {
			continue
		}
		if b.opts.AccountID != "" {
			if acct.AccountID != b.opts.AccountID {
				continue
			}
		} else if !b.opts.AllAccounts && !acct.AssistantBackend {
			continue
		}
		lname := LauncherName(acct.Provider, acct.AccountID)
		if override != "" && len(targets) == 0 && (b.opts.AccountID != "" || len(st.Accounts) == 1) {
			lname = override
		}
		targets = append(targets, accountTarget{account: acct, launcher: lname})
	}
	b.mu.Lock()
	b.accounts = targets
	b.mu.Unlock()
	return nil
}

func (b *Bridge) launcherSnapshot() []Launcher {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Launcher, 0, len(b.accounts))
	for _, t := range b.accounts {
		name := t.account.DisplayName
		if name == "" {
			name = t.account.Provider
		}
		out = append(out, Launcher{
			Name:        t.launcher,
			Agent:       t.account.Provider,
			DisplayName: name,
		})
	}
	return out
}

func (b *Bridge) connectOnce(ctx context.Context) (err error, conflict bool) {
	conn, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", b.opts.SocketPath)
	if err != nil {
		return err, false
	}
	peer := NewPeer("r", conn)
	peer.OnRequest = b.handleRequest
	peer.OnEvent = b.handleEvent

	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- peer.Serve(serveCtx)
	}()

	if err := b.handshakeAndPush(serveCtx, peer); err != nil {
		var re *RPCError
		isConflict := errors.As(err, &re) && re.Code == "runtime_conflict"
		peer.Close(err)
		<-serveDone
		return err, isConflict
	}

	b.mu.Lock()
	b.peer = peer
	b.mu.Unlock()

	err = <-serveDone

	b.mu.Lock()
	if b.peer == peer {
		b.peer = nil
		b.hubConnected = false
		for _, s := range b.sessions {
			s.watched = false
		}
	}
	b.mu.Unlock()
	return err, false
}

func (b *Bridge) handshakeAndPush(ctx context.Context, peer *Peer) error {
	_ = b.refreshAccounts(ctx)
	launchers := b.launcherSnapshot()
	primaryLauncher := ""
	if len(launchers) > 0 {
		primaryLauncher = launchers[0].Name
	}
	helloParams := map[string]any{
		"runtime":   b.opts.RuntimeID,
		"name":      b.opts.Name,
		"agent":     "club",
		"version":   b.opts.Version,
		"instance":  b.instance,
		"launcher":  primaryLauncher,
		"launchers": launchers,
		"caps": RuntimeCaps{
			Start:             true,
			Send:              true,
			Interrupt:         true,
			Close:             true,
			Assistant:         true,
			RemoteAgent:       true,
			SupervisionPolicy: false,
		},
	}
	var res struct {
		Protocol    int      `json:"protocol"`
		NodeVersion string   `json:"node_version"`
		Hub         *bool    `json:"hub"`
		Features    []string `json:"features"`
	}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := peer.Call(hctx, "runtime.hello", helloParams, &res); err != nil {
		return err
	}
	hub := true
	if res.Hub != nil {
		hub = *res.Hub
	}
	b.mu.Lock()
	b.hubConnected = hub
	b.nodeFeatures = res.Features
	b.mu.Unlock()

	if err := peer.Notify("runtime.sessions", map[string]any{"sessions": b.sessionRows()}); err != nil {
		return err
	}
	b.emitUsageWindows(peer)
	return nil
}

func (b *Bridge) currentPeer() *Peer {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peer
}

func (b *Bridge) nodeHasFeature(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Contains(b.nodeFeatures, name)
}

func (b *Bridge) handleEvent(event string, params json.RawMessage) {
	if event == "runtime.hub" {
		var p struct {
			Connected bool `json:"connected"`
		}
		if json.Unmarshal(params, &p) == nil {
			b.mu.Lock()
			b.hubConnected = p.Connected
			b.mu.Unlock()
		}
	}
}

func (b *Bridge) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "session.start":
		return b.handleSessionStart(ctx, params)
	case "session.send":
		return b.handleSessionSend(ctx, params, false)
	case "session.send_unseen":
		return b.handleSessionSend(ctx, params, true)
	case "session.interrupt":
		return b.handleSessionInterrupt(ctx, params)
	case "session.items":
		return b.handleSessionItems(params)
	case "session.item":
		return b.handleSessionItem(params)
	case "session.watch":
		return b.handleSessionWatch(params)
	case "session.unwatch":
		return b.handleSessionUnwatch(params)
	case "session.close":
		return b.handleSessionClose(ctx, params)
	case "session.rename":
		var p struct {
			SessionID string  `json:"session_id"`
			Title     *string `json:"title"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Title == nil || strings.TrimSpace(*p.Title) == "" {
			return map[string]any{"ok": true}, nil
		}
		return nil, &RPCError{Code: "unsupported", Message: "unsupported: rename"}
	default:
		return nil, &RPCError{Code: "unsupported", Message: "unsupported method " + method}
	}
}

func (b *Bridge) sessionRows() []SessionRow {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows := make([]SessionRow, 0, len(b.sessions))
	for _, s := range b.sessions {
		rows = append(rows, b.rowLocked(s))
	}
	slices.SortFunc(rows, func(x, y SessionRow) int {
		if x.SessionID < y.SessionID {
			return -1
		}
		if x.SessionID > y.SessionID {
			return 1
		}
		return 0
	})
	return rows
}

func (b *Bridge) rowLocked(s *sessionState) SessionRow {
	return SessionRow{
		SessionID:         s.binding.SessionID,
		Title:             s.binding.Title,
		Cwd:               s.binding.Cwd,
		Status:            s.status,
		Agent:             "club",
		Launcher:          s.binding.Launcher,
		UpdatedAt:         s.binding.UpdatedAt,
		Rev:               s.rev,
		LastText:          s.lastText,
		SpaceID:           "club:" + s.binding.AccountID,
		SpaceName:         defaultRuntimeName,
		TurnError:         s.turnError,
		TurnErrorKind:     s.turnErrorKind,
		TurnErrorCode:     s.turnErrorCode,
		TurnErrorResetsAt: s.turnErrorReset,
	}
}

func (b *Bridge) pushSessions() {
	p := b.currentPeer()
	if p == nil {
		return
	}
	_ = p.Notify("runtime.sessions", map[string]any{"sessions": b.sessionRows()})
}

func (b *Bridge) emitUsageWindows(p *Peer) {
	if p == nil || !b.nodeHasFeature("usage_windows") {
		return
	}
	b.mu.Lock()
	targets := slices.Clone(b.accounts)
	b.mu.Unlock()
	now := b.now()
	for _, t := range targets {
		ev, ok := QuotaToUsageWindows(t.account.Provider, t.account.DisplayName, t.account.Quota, now)
		if !ok {
			continue
		}
		_ = p.Notify("usage.windows", ev)
		return
	}
}

func newAssistantSessionID() string {
	var raw [6]byte
	_, _ = rand.Read(raw[:])
	return sessionPrefix + hex.EncodeToString(raw[:])
}

func (b *Bridge) pickAccountTarget(ctx context.Context, requestedLauncher string) (accountTarget, error) {
	_ = b.refreshAccounts(ctx)
	b.mu.Lock()
	targets := slices.Clone(b.accounts)
	b.mu.Unlock()
	if requestedLauncher != "" {
		for _, t := range targets {
			if t.launcher == requestedLauncher {
				return t, nil
			}
		}
		return accountTarget{}, &RPCError{Code: "launcher_missing", Message: "unknown club assistant launcher: " + requestedLauncher}
	}
	if len(targets) == 0 {
		return accountTarget{}, &RPCError{Code: "launcher_missing", Message: "no club account enabled as Persona assistant backend"}
	}
	return targets[0], nil
}

func (b *Bridge) resolveBackingSession(ctx context.Context, acctID string) (string, error) {
	pick := func() (string, error) {
		listed, err := b.opts.Client.Sessions(ctx)
		if err != nil {
			return "", err
		}
		fallback := ""
		for _, s := range listed.Sessions {
			if s.AccountID != acctID || !s.CanSend {
				continue
			}
			if s.Kind == protocol.KindMain {
				return s.SessionKey, nil
			}
			if fallback == "" {
				fallback = s.SessionKey
			}
		}
		return fallback, nil
	}
	key, err := pick()
	if err != nil || key != "" {
		return key, err
	}
	_ = b.opts.Client.SyncAccount(ctx, acctID)
	return pick()
}

func (b *Bridge) handleSessionStart(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Cwd                     string `json:"cwd"`
		Prompt                  string `json:"prompt"`
		Assistant               bool   `json:"assistant"`
		Launcher                string `json:"launcher"`
		SessionID               string `json:"session_id"`
		SupervisionInstructions string `json:"supervision_instructions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: "bad session.start params"}
	}
	if strings.TrimSpace(p.SupervisionInstructions) != "" {
		return nil, &RPCError{
			Code:    "assistant_supervision_runtime",
			Message: "club runtime does not declare caps.supervision_policy",
		}
	}
	target, err := b.pickAccountTarget(ctx, strings.TrimSpace(p.Launcher))
	if err != nil {
		return nil, err
	}

	b.mu.Lock()
	sid := strings.TrimSpace(p.SessionID)
	if !ValidRuntimeSessionID(sid) || b.sessions[sid] != nil {
		for {
			sid = newAssistantSessionID()
			if b.sessions[sid] == nil {
				break
			}
		}
	}
	b.mu.Unlock()

	backingKey, err := b.resolveBackingSession(ctx, target.account.AccountID)
	if err != nil {
		return nil, &RPCError{Code: "upstream_error", Message: err.Error()}
	}
	title := target.account.DisplayName
	if title == "" {
		title = target.account.Provider
	}
	now := b.now().UTC()
	st := &sessionState{
		binding: SessionBinding{
			SessionID:  sid,
			AccountID:  target.account.AccountID,
			Provider:   target.account.Provider,
			SessionKey: backingKey,
			Launcher:   target.launcher,
			Cwd:        p.Cwd,
			Title:      title,
			Assistant:  p.Assistant,
			CreatedAt:  now,
			UpdatedAt:  now,
		},
		status:   "done",
		itemByID: map[string]Item{},
		sentIDs:  map[string]sentRecord{},
	}

	b.mu.Lock()
	b.sessions[sid] = st
	row := b.rowLocked(st)
	b.persistStateLocked()
	b.mu.Unlock()

	b.syncAssistantSessionsBestEffort(ctx)
	b.pushSessions()

	if strings.TrimSpace(p.Prompt) != "" {
		sendRaw, _ := json.Marshal(map[string]any{
			"session_id": sid,
			"text":       p.Prompt,
			"assistant":  p.Assistant,
		})
		if _, err := b.handleSessionSend(ctx, sendRaw, false); err != nil {
			return nil, err
		}
		b.mu.Lock()
		if cur := b.sessions[sid]; cur != nil {
			row = b.rowLocked(cur)
		}
		b.mu.Unlock()
	}

	return map[string]any{"session": row}, nil
}

func (b *Bridge) handleSessionSend(ctx context.Context, params json.RawMessage, unseenRetry bool) (any, error) {
	var p struct {
		SessionID               string `json:"session_id"`
		Text                    string `json:"text"`
		ClientMsgID             string `json:"client_msg_id"`
		Assistant               bool   `json:"assistant"`
		Context                 string `json:"context"`
		SupervisionInstructions string `json:"supervision_instructions"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: "bad session.send params"}
	}
	if strings.TrimSpace(p.SupervisionInstructions) != "" {
		return nil, &RPCError{
			Code:    "assistant_supervision_runtime",
			Message: "club runtime does not declare caps.supervision_policy",
		}
	}

	cleanText := strings.TrimSpace(StripPersonaContext(p.Text))
	if cleanText == "" {
		return nil, &RPCError{Code: "invalid_params", Message: "session.send requires non-empty text"}
	}

	now := b.now().UTC()
	cmID := strings.TrimSpace(p.ClientMsgID)

	b.mu.Lock()
	s := b.sessions[p.SessionID]
	if s == nil {
		b.mu.Unlock()
		return nil, &RPCError{Code: "not_found", Message: "session not found"}
	}
	for k, rec := range s.sentIDs {
		if now.Sub(rec.at) > sendDedupeTTL {
			delete(s.sentIDs, k)
		}
	}
	if unseenRetry {
		rec, ok := s.sentIDs[cmID]
		if cmID == "" || !ok {
			b.mu.Unlock()
			return nil, &RPCError{Code: "echo_unconfirmed", Message: "no unconfirmed send record found"}
		}
		if rec.status == "sent" {
			b.mu.Unlock()
			return map[string]any{"ok": true}, nil
		}
		if rec.status != "unconfirmed" {
			b.mu.Unlock()
			return nil, &RPCError{Code: "echo_unconfirmed", Message: "prior send did not end unconfirmed"}
		}
	} else if cmID != "" {
		if rec, ok := s.sentIDs[cmID]; ok && rec.status == "sent" {
			b.mu.Unlock()
			return map[string]any{"ok": true}, nil
		}
	}

	// Supersede any previous active turn on this session cleanly.
	if s.turnCancel != nil {
		s.turnCancel()
		s.turnCancel = nil
	}
	s.turnEpoch++
	epoch := s.turnEpoch
	parentCtx := b.rootCtx
	if parentCtx == nil {
		parentCtx = ctx
	}
	tctx, tcancel := context.WithCancel(parentCtx)
	s.turnCancel = tcancel
	acctID := s.binding.AccountID
	backingKey := s.binding.SessionKey
	bootstrapSent := s.binding.BootstrapSent
	s.turnError, s.turnErrorKind, s.turnErrorCode, s.turnErrorReset = "", "", "", nil
	s.status = "working"
	s.binding.UpdatedAt = now
	var userItem Item
	if !unseenRetry {
		userItem = b.appendItemLocked(s, "user", "text", cleanText, cmID, "", "", nil, false)
	}
	shouldUseToolCaller := false
	if tc, ok := b.opts.Client.(toolCapableClient); ok && tc.SupportsTools(ctx, acctID) {
		shouldUseToolCaller = true
	}
	b.mu.Unlock()

	if userItem.ID != "" {
		b.pushNewItems(p.SessionID, []Item{userItem})
	}
	b.pushSessions()

	if shouldUseToolCaller {
		b.mu.Lock()
		if s.turnEpoch == epoch && cmID != "" {
			s.sentIDs[cmID] = sentRecord{status: "sent", at: b.now().UTC()}
		}
		turns := b.buildToolTurnsLocked(s, p.Context)
		s.binding.BootstrapSent = true
		b.persistStateLocked()
		b.mu.Unlock()

		b.turnWG.Add(1)
		go func() {
			defer b.turnWG.Done()
			defer tcancel()
			b.runToolCallerTurn(tctx, s, epoch, turns)
		}()
		return map[string]any{"ok": true}, nil
	}

	// Build outbound message with delta <persona-context> (capped on first turn) and compact tool catalog.
	var prefixParts []string
	if ctxText := strings.TrimSpace(p.Context); ctxText != "" {
		if !bootstrapSent {
			ctxText = CapBootstrap(ctxText, defaultBootstrapB)
		}
		prefixParts = append(prefixParts, "<persona-context>\n"+ctxText+"\n</persona-context>")
	}
	if b.opts.ToolRelay && b.hubConnectedSnapshot() {
		if mcpTools := b.fetchMCPTools(ctx, p.SessionID, p.Assistant); len(mcpTools) > 0 {
			if cat := FormatToolCatalog(mcpTools, maxToolRelayBytes); cat != "" {
				prefixParts = append(prefixParts, cat)
			}
		}
	}
	outboundText := FitMaxText(strings.Join(prefixParts, "\n\n"), cleanText)

	var knownExtIDs map[string]bool
	var outboxID string
	var sendRes protocol.Message
	var sendErr error

	if backingKey == "" {
		var started protocol.Session
		started, sendErr = b.opts.Client.StartSession(ctx, protocol.StartSessionParams{
			AccountID: acctID,
			Title:     p.SessionID,
			Text:      outboundText,
		})
		if sendErr == nil {
			backingKey = started.SessionKey
			b.mu.Lock()
			if s.binding.SessionKey == "" {
				s.binding.SessionKey = backingKey
				b.persistStateLocked()
			}
			b.mu.Unlock()
			b.syncAssistantSessionsBestEffort(ctx)
			knownExtIDs = map[string]bool{}
			sendRes = protocol.Message{Local: &protocol.Local{Status: protocol.LocalSent}}
		}
	} else {
		knownExtIDs = b.snapshotKnownMessages(ctx, backingKey)
		seed := fmt.Sprintf("%s:%s:%d", p.SessionID, cmID, epoch)
		if cmID != "" && !unseenRetry && uuidShapeRe.MatchString(cmID) {
			outboxID = cmID
		} else {
			outboxID = DeterministicUUID(seed)
		}
		b.mu.Lock()
		if s.turnEpoch == epoch {
			s.pendingOutboxID = outboxID
		}
		b.mu.Unlock()
		sendRes, sendErr = b.opts.Client.Send(ctx, protocol.SendParams{
			SessionKey: backingKey,
			ClientID:   outboxID,
			Text:       outboundText,
		})
	}

	b.mu.Lock()
	if s.pendingOutboxID == outboxID {
		s.pendingOutboxID = ""
	}
	b.mu.Unlock()

	if sendErr != nil {
		b.finishTurnError(s, epoch, "", "", sendErr.Error(), nil)
		return nil, &RPCError{Code: "send_failed", Message: sendErr.Error()}
	}

	localStatus := protocol.LocalSent
	localErr := ""
	if sendRes.Local != nil {
		localStatus = sendRes.Local.Status
		localErr = sendRes.Local.Error
	}

	switch localStatus {
	case protocol.LocalUnconfirmed:
		tcancel()
		b.mu.Lock()
		if cmID != "" {
			s.sentIDs[cmID] = sentRecord{status: "unconfirmed", at: b.now().UTC()}
		}
		if s.turnEpoch == epoch {
			s.status = "done"
			s.binding.UpdatedAt = b.now().UTC()
			b.persistStateLocked()
		}
		b.mu.Unlock()
		b.pushSessions()
		return nil, &RPCError{
			Code:    "send_unconfirmed",
			Message: "send outcome is unconfirmed",
			Data:    json.RawMessage(`{"delivered":"unknown"}`),
		}
	case protocol.LocalFailed:
		tcancel()
		kind, code, resetAt := b.classifySendFailure(ctx, acctID, localErr)
		b.finishTurnError(s, epoch, kind, code, localErr, resetAt)
		b.emitUsageWindows(b.currentPeer())
		return nil, &RPCError{Code: "send_failed", Message: localErr}
	}

	b.mu.Lock()
	if cmID != "" {
		s.sentIDs[cmID] = sentRecord{status: "sent", at: b.now().UTC()}
	}
	s.binding.BootstrapSent = true
	b.persistStateLocked()
	b.mu.Unlock()

	b.turnWG.Add(1)
	go func() {
		defer b.turnWG.Done()
		defer tcancel()
		b.runWatchTurn(tctx, s, epoch, backingKey, knownExtIDs, cleanText)
	}()
	return map[string]any{"ok": true}, nil
}

func (b *Bridge) classifySendFailure(ctx context.Context, acctID, localErr string) (kind, code string, resetAt *time.Time) {
	if localErr != provider.CodeRateLimited && !strings.Contains(strings.ToUpper(localErr), "RATE") {
		return "transient", "", nil
	}
	kind, code = "usage_limit", "rate_limited"
	st, err := b.opts.Client.Status(ctx)
	if err != nil {
		return kind, code, nil
	}
	for _, a := range st.Accounts {
		if a.AccountID == acctID && a.Quota != nil && a.Quota.ResetsAt > 0 {
			t := time.UnixMilli(a.Quota.ResetsAt).UTC()
			return kind, code, &t
		}
	}
	return kind, code, nil
}

func (b *Bridge) snapshotKnownMessages(ctx context.Context, sessionKey string) map[string]bool {
	known := map[string]bool{}
	page, err := b.opts.Client.Messages(ctx, protocol.MessagesParams{SessionKey: sessionKey, Limit: 100})
	if err != nil {
		return known
	}
	for _, m := range page.Messages {
		if strings.HasPrefix(m.ExternalID, "local:") {
			continue
		}
		known[m.ExternalID] = true
	}
	return known
}

func (b *Bridge) runWatchTurn(ctx context.Context, s *sessionState, epoch uint64, backingKey string, knownExtIDs map[string]bool, sentUserText string) {
	evCh, stopSub := b.opts.Client.Subscribe(ctx)
	defer stopSub()

	deadline := time.NewTimer(b.opts.MaxTurnWait)
	defer deadline.Stop()

	interval := b.opts.PollInterval
	pollTimer := time.NewTimer(interval)
	defer pollTimer.Stop()

	step := 0
	lastUserEchoed := strings.TrimSpace(sentUserText)
	var lastReplyText string
	sawAssistantReply := false
	sawRunning := false
	var idleSince time.Time
	startedAt := b.now()

	checkOnce := func(syncFirst bool) (done bool) {
		if ctx.Err() != nil || b.sessionEpoch(s) != epoch {
			return true
		}
		if syncFirst {
			_ = b.opts.Client.SyncAccount(ctx, s.binding.AccountID)
		}
		page, err := b.opts.Client.Messages(ctx, protocol.MessagesParams{SessionKey: backingKey, Limit: 100})
		hasActiveStatusBlock := false
		if err == nil {
			var emitted []Item
			b.mu.Lock()
			if s.turnEpoch != epoch {
				b.mu.Unlock()
				return true
			}
			for _, m := range page.Messages {
				if strings.HasPrefix(m.ExternalID, "local:") {
					continue
				}
				if knownExtIDs[m.ExternalID] {
					continue
				}
				knownExtIDs[m.ExternalID] = true
				role, kind, text := mapClubMessage(m)
				if text == "" && kind == "" {
					continue
				}
				if role == "system" && m.Role == protocol.RoleSystem && len(m.Blocks) > 0 && (m.Blocks[0].Status == "running" || m.Blocks[0].Status == "step") {
					hasActiveStatusBlock = true
				}
				if m.Role == protocol.RoleUser {
					stripped := strings.TrimSpace(StripPersonaContext(text))
					if stripped == lastUserEchoed {
						// Avoid duplicating the user prompt we already appended locally.
						continue
					}
					if stripped != "" {
						text = stripped
					} else {
						continue
					}
				}
				if m.Role == protocol.RoleAssistant && text != "" {
					lastReplyText = text
					sawAssistantReply = true
				}
				emitted = append(emitted, b.appendItemLocked(s, role, kind, text, "", "", "", nil, false))
			}
			b.mu.Unlock()
			if len(emitted) > 0 {
				b.pushNewItems(s.binding.SessionID, emitted)
				b.pushSessions()
			}
		}

		live, lErr := b.opts.Client.Live(ctx, backingKey)
		if lErr == nil && live.LastTurnError != "" {
			b.finishTurnError(s, epoch, "transient", "", live.LastTurnError, nil)
			return true
		}

		now := b.now()
		settled := false
		if lErr == nil && live.Supported {
			if live.Running || live.Composing {
				sawRunning = true
				idleSince = time.Time{}
			} else if sawAssistantReply || (sawRunning && !live.Running && !live.Composing) {
				if idleSince.IsZero() {
					idleSince = now
				}
				if now.Sub(idleSince) >= b.opts.IdleDebounce {
					settled = true
				}
			}
		} else if sawAssistantReply && !hasActiveStatusBlock {
			settled = true
		}

		if !settled {
			return false
		}

		if b.opts.ToolRelay && step < maxToolSteps && b.hubConnectedSnapshot() {
			if name, rawArgs, ok := ParseToolCall(lastReplyText); ok {
				step++
				res, callErr := b.executeRelayedTool(ctx, s, epoch, name, rawArgs)
				if ctx.Err() != nil || b.sessionEpoch(s) != epoch {
					return true
				}
				toolResp := formatToolRelayResult(name, res, callErr)
				if sent, nextID := b.sendRelayFollowup(ctx, s, epoch, backingKey, toolResp, step); sent {
					if nextID != "" {
						_ = nextID
					}
					lastUserEchoed = strings.TrimSpace(toolResp)
					lastReplyText = ""
					sawAssistantReply = false
					sawRunning = false
					idleSince = time.Time{}
					interval = b.opts.PollInterval
					return false
				}
			}
		}

		b.finishTurnDone(s, epoch)
		b.emitUsageWindows(b.currentPeer())
		return true
	}

	// Check immediately in case a synchronous provider (echo / groq) already stored the reply during Send.
	if checkOnce(false) {
		return
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			if ctx.Err() == nil && b.sessionEpoch(s) == epoch {
				b.finishTurnDone(s, epoch)
			}
			return
		case ev, ok := <-evCh:
			if !ok {
				return
			}
			if len(ev.SessionKeys) == 0 || slices.Contains(ev.SessionKeys, backingKey) {
				if checkOnce(false) {
					return
				}
			}
		case <-pollTimer.C:
			if checkOnce(true) {
				return
			}
			if b.now().Sub(startedAt) > 15*time.Second {
				interval = min(interval*2, maxPoll)
			}
			pollTimer.Reset(interval)
		}
	}
}

func (b *Bridge) sendRelayFollowup(ctx context.Context, s *sessionState, epoch uint64, backingKey, toolResp string, step int) (bool, string) {
	outboxID := DeterministicUUID(fmt.Sprintf("%s:relay:%d:%d", s.binding.SessionID, epoch, step))
	msg, err := b.opts.Client.Send(ctx, protocol.SendParams{
		SessionKey: backingKey,
		ClientID:   outboxID,
		Text:       FitMaxText("", toolResp),
	})
	if err != nil {
		return false, ""
	}
	if msg.Local != nil && msg.Local.Status != protocol.LocalSent {
		return false, ""
	}
	return true, outboxID
}

func formatToolRelayResult(name string, res MCPCallResult, err error) string {
	var text string
	if err != nil {
		text = "error: " + err.Error()
	} else {
		var parts []string
		for _, c := range res.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
		text = strings.Join(parts, "\n")
		if res.IsError && text == "" {
			text = "tool returned an error"
		}
	}
	return fmt.Sprintf("<persona-tool-result name=%q is_error=%t>\n%s\n</persona-tool-result>", name, res.IsError || err != nil, text)
}

func mapClubMessage(m protocol.Message) (role, kind, text string) {
	role = "assistant"
	switch m.Role {
	case protocol.RoleUser:
		role = "user"
	case protocol.RoleTool:
		role = "assistant"
	case protocol.RoleSystem:
		role = "assistant"
	}
	kind = "text"
	var parts []string
	for _, blk := range m.Blocks {
		switch blk.Type {
		case "text":
			if blk.Text != "" {
				parts = append(parts, blk.Text)
			}
		case "tool-call":
			label := blk.Name
			if label == "" {
				label = "tool"
			}
			if blk.Summary != "" {
				parts = append(parts, "["+label+"] "+blk.Summary)
			}
		case "tool-result":
			if blk.Text != "" {
				parts = append(parts, blk.Text)
			}
		case "error":
			kind = "notice"
			if blk.Text != "" {
				parts = append(parts, blk.Text)
			}
		case "status":
			kind = "notice"
			if blk.Text != "" {
				parts = append(parts, blk.Text)
			}
		default:
			if blk.Text != "" {
				parts = append(parts, blk.Text)
			} else if blk.Summary != "" {
				parts = append(parts, blk.Summary)
			}
		}
	}
	return role, kind, strings.TrimSpace(strings.Join(parts, "\n"))
}

func (b *Bridge) runToolCallerTurn(ctx context.Context, s *sessionState, epoch uint64, turns []provider.ToolTurn) {
	var mcpTools []MCPTool
	if b.hubConnectedSnapshot() {
		b.mu.Lock()
		sid, asst := s.binding.SessionID, s.binding.Assistant
		b.mu.Unlock()
		mcpTools = b.fetchMCPTools(ctx, sid, asst)
	}
	defs := make([]provider.ToolDef, 0, len(mcpTools))
	for _, t := range mcpTools {
		defs = append(defs, provider.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}

	b.mu.Lock()
	acctID := s.binding.AccountID
	sid := s.binding.SessionID
	b.mu.Unlock()

	for step := range maxToolSteps {
		if ctx.Err() != nil || b.sessionEpoch(s) != epoch {
			return
		}
		res, _, err := b.opts.Client.CallTools(ctx, acctID, turns, defs)
		if ctx.Err() != nil || b.sessionEpoch(s) != epoch {
			return
		}
		if err != nil {
			kind, code := "", ""
			if provider.CodeOf(err) == provider.CodeRateLimited {
				kind, code = "usage_limit", "rate_limited"
			} else {
				kind = "transient"
			}
			b.finishTurnError(s, epoch, kind, code, err.Error(), nil)
			_ = b.refreshAccounts(context.WithoutCancel(ctx))
			b.emitUsageWindows(b.currentPeer())
			return
		}

		if len(res.ToolCalls) == 0 {
			text := strings.TrimSpace(res.Content)
			if text == "" {
				text = "(empty response)"
			}
			b.mu.Lock()
			if s.turnEpoch != epoch {
				b.mu.Unlock()
				return
			}
			it := b.appendItemLocked(s, "assistant", "text", text, "", "", "", nil, false)
			b.mu.Unlock()
			b.pushNewItems(sid, []Item{it})
			b.finishTurnDone(s, epoch)
			_ = b.refreshAccounts(context.WithoutCancel(ctx))
			b.emitUsageWindows(b.currentPeer())
			return
		}

		turns = append(turns, provider.ToolTurn{
			Role:      protocol.RoleAssistant,
			Content:   res.Content,
			ToolCalls: res.ToolCalls,
		})
		for _, tc := range res.ToolCalls {
			if ctx.Err() != nil || b.sessionEpoch(s) != epoch {
				return
			}
			toolRes, callErr := b.executeRelayedToolWithID(ctx, s, epoch, tc.ID, tc.Name, tc.Args)
			if ctx.Err() != nil || b.sessionEpoch(s) != epoch {
				return
			}
			var parts []string
			if callErr != nil {
				parts = append(parts, "error: "+callErr.Error())
			} else {
				for _, c := range toolRes.Content {
					if c.Text != "" {
						parts = append(parts, c.Text)
					}
				}
			}
			outText := strings.Join(parts, "\n")
			turns = append(turns, provider.ToolTurn{
				Role:       protocol.RoleTool,
				ToolCallID: tc.ID,
				ToolName:   tc.Name,
				Content:    outText,
			})
		}

		if step == maxToolSteps-1 {
			b.mu.Lock()
			if s.turnEpoch != epoch {
				b.mu.Unlock()
				return
			}
			it := b.appendItemLocked(s, "assistant", "notice", fmt.Sprintf("Stopped after %d tool steps.", maxToolSteps), "", "", "", nil, false)
			b.mu.Unlock()
			b.pushNewItems(sid, []Item{it})
			b.finishTurnDone(s, epoch)
			return
		}
	}
}

func (b *Bridge) buildToolTurnsLocked(s *sessionState, contextBlock string) []provider.ToolTurn {
	var turns []provider.ToolTurn
	if ctxText := strings.TrimSpace(contextBlock); ctxText != "" {
		if !s.binding.BootstrapSent {
			ctxText = CapBootstrap(ctxText, defaultBootstrapB)
		}
		turns = append(turns, provider.ToolTurn{Role: protocol.RoleSystem, Content: ctxText})
	}
	const maxHist = 20
	var hist []provider.ToolTurn
	for _, it := range s.items {
		if it.Kind != "text" || strings.TrimSpace(it.Text) == "" {
			continue
		}
		role := protocol.RoleUser
		if it.Role == "assistant" {
			role = protocol.RoleAssistant
		}
		hist = append(hist, provider.ToolTurn{Role: role, Content: it.Text})
	}
	if len(hist) > maxHist {
		hist = hist[len(hist)-maxHist:]
	}
	return append(turns, hist...)
}

func (b *Bridge) executeRelayedTool(ctx context.Context, s *sessionState, epoch uint64, name string, args json.RawMessage) (MCPCallResult, error) {
	return b.executeRelayedToolWithID(ctx, s, epoch, fmt.Sprintf("call_%d_%d", epoch, s.nextSeq+1), name, args)
}

func (b *Bridge) executeRelayedToolWithID(ctx context.Context, s *sessionState, epoch uint64, callID, name string, args json.RawMessage) (MCPCallResult, error) {
	b.mu.Lock()
	if s.turnEpoch != epoch {
		b.mu.Unlock()
		return MCPCallResult{}, context.Canceled
	}
	sid := s.binding.SessionID
	asst := s.binding.Assistant
	useItem := b.appendItemLocked(s, "assistant", "tool_use", "", "", callID, name, args, false)
	b.mu.Unlock()
	b.pushNewItems(sid, []Item{useItem})

	res, callErr := b.callMCPTool(ctx, sid, name, args, asst)
	if ctx.Err() != nil || b.sessionEpoch(s) != epoch {
		return res, context.Canceled
	}

	var parts []string
	if callErr != nil {
		parts = append(parts, "error: "+callErr.Error())
	} else {
		for _, c := range res.Content {
			if c.Text != "" {
				parts = append(parts, c.Text)
			}
		}
	}
	resText := strings.Join(parts, "\n")

	b.mu.Lock()
	if s.turnEpoch != epoch {
		b.mu.Unlock()
		return res, context.Canceled
	}
	resItem := b.appendItemLocked(s, "user", "tool_result", resText, "", callID, name, nil, res.IsError || callErr != nil)
	b.mu.Unlock()
	b.pushNewItems(sid, []Item{resItem})

	return res, callErr
}

func (b *Bridge) hubConnectedSnapshot() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hubConnected && b.peer != nil
}

func (b *Bridge) fetchMCPTools(ctx context.Context, sessionID string, assistant bool) []MCPTool {
	p := b.currentPeer()
	if p == nil {
		return nil
	}
	tctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var res struct {
		Tools []MCPTool `json:"tools"`
	}
	if err := p.Call(tctx, "mcp.tools", map[string]any{
		"session_id": sessionID,
		"assistant":  assistant,
	}, &res); err != nil {
		return nil
	}
	return res.Tools
}

func (b *Bridge) callMCPTool(ctx context.Context, sessionID, name string, args json.RawMessage, assistant bool) (MCPCallResult, error) {
	p := b.currentPeer()
	if p == nil {
		return MCPCallResult{}, errors.New("Persona node disconnected")
	}
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var res MCPCallResult
	err := p.Call(ctx, "mcp.call", map[string]any{
		"name":       name,
		"args":       args,
		"session_id": sessionID,
		"assistant":  assistant,
	}, &res)
	return res, err
}

func (b *Bridge) sessionEpoch(s *sessionState) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return s.turnEpoch
}

func (b *Bridge) finishTurnDone(s *sessionState, epoch uint64) {
	b.mu.Lock()
	if s.turnEpoch != epoch {
		b.mu.Unlock()
		return
	}
	s.status = "done"
	s.turnCancel = nil
	s.binding.UpdatedAt = b.now().UTC()
	b.persistStateLocked()
	b.mu.Unlock()
	b.pushSessions()
}

func (b *Bridge) finishTurnError(s *sessionState, epoch uint64, kind, code, msg string, resetAt *time.Time) {
	b.mu.Lock()
	if s.turnEpoch != epoch {
		b.mu.Unlock()
		return
	}
	s.status = "done"
	s.turnCancel = nil
	s.turnError = msg
	s.turnErrorKind = kind
	s.turnErrorCode = code
	s.turnErrorReset = resetAt
	s.binding.UpdatedAt = b.now().UTC()
	b.persistStateLocked()
	b.mu.Unlock()
	b.pushSessions()
}

func (b *Bridge) appendItemLocked(s *sessionState, role, kind, text, clientMsgID, toolUseID, toolName string, toolInput json.RawMessage, isError bool) Item {
	s.nextSeq++
	s.rev++
	now := b.now().UTC()
	it := Item{
		ID:          fmt.Sprintf("%s-%d", s.binding.SessionID, s.nextSeq),
		Seq:         s.nextSeq,
		TS:          now,
		Role:        role,
		Kind:        kind,
		Text:        text,
		ClientMsgID: clientMsgID,
		ToolUseID:   toolUseID,
		ToolName:    toolName,
		ToolInput:   toolInput,
		IsError:     isError,
	}
	s.items = append(s.items, it)
	s.itemByID[it.ID] = it
	if role == "assistant" && kind == "text" && strings.TrimSpace(text) != "" {
		s.lastText = prefix16Units(strings.TrimSpace(text), 200)
	}
	s.binding.UpdatedAt = now
	return it
}

func (b *Bridge) pushNewItems(sessionID string, items []Item) {
	if len(items) == 0 {
		return
	}
	b.mu.Lock()
	s := b.sessions[sessionID]
	watched := s != nil && s.watched
	p := b.peer
	b.mu.Unlock()
	if !watched || p == nil {
		return
	}
	fromSeq := max(0, items[0].Seq-1)
	toSeq := items[len(items)-1].Seq
	_ = p.Notify("session.items", map[string]any{
		"session_id": sessionID,
		"items":      applyLight(items),
		"history":    defaultHistoryName,
		"from_seq":   fromSeq,
		"to_seq":     toSeq,
	})
}

func (b *Bridge) handleSessionInterrupt(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: "bad session.interrupt params"}
	}

	b.mu.Lock()
	s := b.sessions[p.SessionID]
	if s == nil {
		b.mu.Unlock()
		return nil, &RPCError{Code: "not_found", Message: "session not found"}
	}
	s.turnEpoch++
	cancel := s.turnCancel
	s.turnCancel = nil
	outboxID := s.pendingOutboxID
	s.pendingOutboxID = ""
	backingKey := s.binding.SessionKey
	wasWorking := s.status == "working"
	s.status = "idle"
	s.binding.UpdatedAt = b.now().UTC()
	b.persistStateLocked()
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if backingKey != "" && outboxID != "" {
		_ = b.opts.Client.DeleteMessage(ctx, protocol.DeleteMessageParams{
			SessionKey: backingKey,
			ClientID:   outboxID,
		})
	}
	settled := !wasWorking
	if backingKey != "" {
		if err := b.opts.Client.InterruptSession(ctx, backingKey); err == nil {
			settled = true
		}
	}
	b.pushSessions()
	if settled {
		return map[string]any{"ok": true, "settled": true}, nil
	}
	return map[string]any{"ok": true}, nil
}

func (b *Bridge) handleSessionItems(params json.RawMessage) (any, error) {
	var req ItemsRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: "bad session.items params"}
	}
	b.mu.Lock()
	s := b.sessions[req.SessionID]
	if s == nil {
		b.mu.Unlock()
		return nil, &RPCError{Code: "not_found", Message: "session not found"}
	}
	snapshot := slices.Clone(s.items)
	b.mu.Unlock()
	return PageItems(snapshot, req, defaultHistoryName), nil
}

func (b *Bridge) handleSessionItem(params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"session_id"`
		ItemID    string `json:"item_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: "bad session.item params"}
	}
	b.mu.Lock()
	s := b.sessions[p.SessionID]
	if s == nil {
		b.mu.Unlock()
		return nil, &RPCError{Code: "not_found", Message: "session not found"}
	}
	it, ok := s.itemByID[p.ItemID]
	b.mu.Unlock()
	if !ok {
		return nil, &RPCError{Code: "not_found", Message: "item not found"}
	}
	return it, nil
}

func (b *Bridge) handleSessionWatch(params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"session_id"`
		SinceSeq  int    `json:"since_seq"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: "bad session.watch params"}
	}
	b.mu.Lock()
	s := b.sessions[p.SessionID]
	if s == nil {
		b.mu.Unlock()
		return nil, &RPCError{Code: "not_found", Message: "session not found"}
	}
	s.watched = true
	var replay []Item
	for _, it := range s.items {
		if it.Seq > p.SinceSeq {
			replay = append(replay, it)
		}
	}
	peer := b.peer
	b.mu.Unlock()

	if peer != nil && len(replay) > 0 {
		for pageIdx := 0; pageIdx < rewatchMaxPages && len(replay) > 0; pageIdx++ {
			n := min(len(replay), rewatchPageSize)
			chunk := replay[:n]
			replay = replay[n:]
			moreAfter := pageIdx == rewatchMaxPages-1 && len(replay) > 0
			fromSeq := chunk[0].Seq - 1
			if p.SinceSeq > 0 && pageIdx == 0 {
				fromSeq = p.SinceSeq
			}
			ev := map[string]any{
				"session_id": p.SessionID,
				"items":      applyLight(chunk),
				"history":    defaultHistoryName,
				"from_seq":   fromSeq,
				"to_seq":     chunk[len(chunk)-1].Seq,
			}
			if moreAfter {
				ev["more_after"] = true
			}
			_ = peer.Notify("session.items", ev)
		}
	}
	return map[string]any{"ok": true}, nil
}

func (b *Bridge) handleSessionUnwatch(params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(params, &p)
	b.mu.Lock()
	if s := b.sessions[p.SessionID]; s != nil {
		s.watched = false
	}
	b.mu.Unlock()
	return map[string]any{"ok": true}, nil
}

func (b *Bridge) handleSessionClose(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: "bad session.close params"}
	}
	b.mu.Lock()
	s := b.sessions[p.SessionID]
	if s == nil {
		b.mu.Unlock()
		return map[string]any{"ok": true}, nil
	}
	s.turnEpoch++
	cancel := s.turnCancel
	delete(b.sessions, p.SessionID)
	b.persistStateLocked()
	b.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	b.syncAssistantSessionsBestEffort(ctx)
	b.pushSessions()
	return map[string]any{"ok": true}, nil
}
