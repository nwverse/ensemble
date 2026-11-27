package extension

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/store"
)

// A row that waits for the owner (the reply ends in choices, or asks them to sign in or approve an authorization) is
// "needs you" in the Inbox, but the Inbox does not push a phone alert for a connector's row by itself: the only thing
// it sends is the count. So the extension sends the alert itself, through the hub's notify route (scope `notify`),
// once for each such reply.

const (
	pushWindow  = 6 * time.Hour // a reply older than this is not worth an alert (after a restart, or a switch turned on)
	pushMaxEach = 5             // alerts tried in one round
	pushKeep    = 500           // replies remembered
	pushTimeout = 15 * time.Second
	pushRetry   = time.Minute // when an alert failed or the round's cap left some waiting
	pushLogKey  = "inboxPushed"
	notifyPath  = "/api/v1/notify"
)

// notification is one phone alert. It names the account and its service only: no words of the reply and not the
// chat's title either (a service often writes the title from the conversation), so nothing of it shows on a lock screen.
type notification struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Level     string `json:"level,omitempty"`      // active or time-sensitive
	PushLevel string `json:"push_level,omitempty"` // P0: the hub's push grading lets it through
}

// pushFunc sends a notification; an error says it did not go and may be tried again, except errNoHubAccess.
type pushFunc func(ctx context.Context, n notification) error

// errNoHubAccess says alerts cannot be sent in this run at all: the hub gave no address or token for its notify route
// (the owner did not grant the scope). Trying again changes nothing.
var errNoHubAccess = errors.New("no hub address or token to send an alert with")

// hubAPI is how the program reaches the hub's HTTP API: the address and the token this run's hub gave it in
// initialize (its own, with only the scopes it asked for).
type hubAPI struct{ URL, Token string }

// hubPush sends through POST /api/v1/notify with the token the hub gave this program.
func hubPush(client *http.Client, api func() hubAPI) pushFunc {
	return func(ctx context.Context, n notification) error {
		a := api()
		if a.URL == "" || a.Token == "" {
			return errNoHubAccess
		}
		body, err := json.Marshal(n)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, pushTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(a.URL, "/")+notifyPath, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+a.Token)
		req.Header.Set("Content-Type", "application/json")
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("alert not sent: %s", urlErrorKind(err)) // the error text can carry the address
		}
		defer res.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
		if res.StatusCode/100 != 2 {
			return fmt.Errorf("alert not sent: the hub answered %d", res.StatusCode)
		}
		return nil
	}
}

func urlErrorKind(err error) string {
	switch {
	case err == context.DeadlineExceeded, strings.Contains(err.Error(), "deadline"):
		return "timed out"
	case strings.Contains(err.Error(), "refused"):
		return "connection refused"
	}
	return "request failed"
}

// pushLog remembers which replies were alerted, so more rounds or a restart never alert one twice. Kept in the
// space's settings, with the time of each (to forget the old ones).
type pushLog struct {
	mu     sync.Mutex
	loaded bool
	fresh  bool // nothing was stored yet: the log starts with this run
	m      map[string]int64
}

// load reads the stored log once. An error leaves it unloaded: nothing is alerted until it can be read, as an alert
// sent without knowing what was sent before could be a repeat.
func (l *pushLog) load(ctx context.Context, st store.Store) error {
	if l.loaded {
		return nil
	}
	raw, ok, err := st.Setting(ctx, pushLogKey)
	if err != nil {
		return err
	}
	l.m = map[string]int64{}
	if ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &l.m)
	}
	l.loaded, l.fresh = true, !ok
	return nil
}

func (l *pushLog) save(ctx context.Context, st store.Store) error {
	if len(l.m) > pushKeep {
		keys := make([]string, 0, len(l.m))
		for k := range l.m {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int { return cmp.Compare(l.m[a], l.m[b]) })
		for _, k := range keys[:len(keys)-pushKeep] {
			delete(l.m, k)
		}
	}
	b, _ := json.Marshal(l.m)
	return st.PutSetting(ctx, pushLogKey, string(b))
}

func pushKey(r cachedRow) string { return r.thing.Thread + "\n" + r.thing.Last }

// pushID is the alert's id (the hub replaces an earlier alert with the same one): one a reply, at most 56 bytes.
func pushID(thread, last string) string {
	sum := sha256.Sum256([]byte("club-inbox-alert:" + thread + "\n" + last))
	return "ensemble-" + hex.EncodeToString(sum[:12])
}

// alertFor is the phone alert for a row waiting for the owner.
func alertFor(r cachedRow) notification {
	n := notification{ID: pushID(r.thing.Thread, r.thing.Last), PushLevel: "P0",
		Body: clip(r.thing.Label+"：打开 Persona 收件箱处理", 120)}
	switch r.attn {
	case attnAuth:
		n.Title, n.Level = clip(r.thing.Sender+" 请你登录或批准授权", 120), "time-sensitive"
	default:
		n.Title, n.Level = clip(r.thing.Sender+" 在等你选", 120), "active"
	}
	return n
}

// owed says a row is owed an alert now: something waits for the owner, unread, unanswered and recent.
func owed(r cachedRow, now time.Time) bool {
	return r.attn != "" && !r.thing.Read && r.thing.Done == "" && now.Sub(r.thing.At) <= pushWindow
}

// pushAttention is one round of alerts: at most pushMaxEach, each for a row that is owed one when its turn comes (the
// rows are read again before every send, so a row the owner read or answered meanwhile is not alerted). An alert is
// remembered once the hub took it; one that failed is not, and more says a later round has work left (a failure, or
// rows past the cap).
//
// The first round of a log that never existed (the first run with alerts, an upgrade) remembers the rows waiting then
// without alerting them: they were in the Inbox already, and a burst of alerts for old news helps nobody.
func (n *notifier) pushAttention(ctx context.Context) (more bool) {
	cfg := n.in.cfg()
	if !cfg.On || !cfg.Push || n.push == nil {
		return false
	}
	space, err := n.in.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return false
	}
	st := space.Store()
	l := &n.log
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, err := n.seedLocked(ctx, st); err != nil {
		return true // not known what was alerted before: try again before alerting anything
	}
	failed := map[string]bool{}
	for tries := 0; ; tries++ {
		if ctx.Err() != nil {
			return false
		}
		rows, err := n.rows(ctx)
		if err != nil {
			return true
		}
		now := n.in.club.Now()
		var next *cachedRow
		for i := range rows {
			if _, done := l.m[pushKey(rows[i])]; owed(rows[i], now) && !done && !failed[pushKey(rows[i])] {
				next = &rows[i]
				break
			}
		}
		if next == nil {
			return len(failed) > 0
		}
		if tries >= pushMaxEach { // sends and failures alike: a hub that hangs costs one round a bounded time
			return true
		}
		if err := n.push(ctx, alertFor(*next)); err != nil {
			if errors.Is(err, errNoHubAccess) {
				return false // nothing can be sent in this run: no retry
			}
			failed[pushKey(*next)] = true
			continue
		}
		l.m[pushKey(*next)] = now.UnixMilli() // remembered for this run even if the write below fails
		_ = l.save(ctx, st)
	}
}

// seed starts the log of alerts when there is none yet, remembering the rows that wait now without alerting them.
// Called when the program starts, before anything can change, and again by a round if that failed.
func (n *notifier) seed(ctx context.Context) {
	cfg := n.in.cfg()
	if !cfg.On || !cfg.Push || n.push == nil {
		return
	}
	space, err := n.in.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return
	}
	n.log.mu.Lock()
	defer n.log.mu.Unlock()
	if n.startAt.IsZero() {
		n.startAt = n.in.club.Now()
	}
	_, _ = n.seedLocked(ctx, space.Store())
}

// seedLocked loads the log and, if it never existed, seeds and stores it; seeded says it did. n.log.mu is held.
func (n *notifier) seedLocked(ctx context.Context, st store.Store) (seeded bool, err error) {
	l := &n.log
	if err := l.load(ctx, st); err != nil {
		return false, err
	}
	if !l.fresh {
		return false, nil
	}
	rows, err := n.rows(ctx)
	if err != nil {
		return false, err
	}
	now := n.in.club.Now()
	if n.startAt.IsZero() {
		n.startAt = now
	}
	for _, r := range rows {
		// only what waited when the program started: if this is a later round making up for a seed that failed
		// then, a reply that came since is news and is alerted
		if owed(r, now) && !r.thing.At.After(n.startAt) {
			l.m[pushKey(r)] = now.UnixMilli()
		}
	}
	if err := l.save(ctx, st); err != nil {
		return false, err
	}
	l.fresh = false
	return true, nil
}

// run sends the alerts until ctx ends: one round after each kick, and again after pushRetry while a round left work.
// One goroutine, so rounds never overlap and never pile up behind a slow hub.
func (n *notifier) run(ctx context.Context) {
	defer close(n.done)
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-n.kick:
		case <-retry:
		}
		retry = nil
		if n.pushAttention(ctx) {
			retry = time.After(n.retry)
		}
		if n.onRound != nil {
			n.onRound()
		}
	}
}
