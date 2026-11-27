package extension

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	pushWindow   = 6 * time.Hour // a reply older than this is not worth an alert (after a restart, or a switch turned on)
	pushMaxEach  = 5             // alerts in one round
	pushKeep     = 500           // replies remembered
	pushTimeout  = 15 * time.Second
	pushLogKey   = "inboxPushed"
	notifyPath   = "/api/v1/notify"
	pushBodyRune = 80
)

// notification is one phone alert. It carries no words of the AI's reply, only whose chat it is, so nothing of the
// conversation shows on a lock screen.
type notification struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Level     string `json:"level,omitempty"`      // active or time-sensitive
	PushLevel string `json:"push_level,omitempty"` // P0: the hub's push grading lets it through
}

// pushFunc sends a notification; an error says it did not go and may be tried again.
type pushFunc func(ctx context.Context, n notification) error

// hubAPI is how the program reaches the hub's HTTP API: the address and the token this run's hub gave it in
// initialize (its own, with only the scopes it asked for).
type hubAPI struct{ URL, Token string }

// hubPush sends through POST /api/v1/notify with the token the hub gave this program.
func hubPush(client *http.Client, api func() hubAPI) pushFunc {
	return func(ctx context.Context, n notification) error {
		a := api()
		if a.URL == "" || a.Token == "" {
			return fmt.Errorf("no hub address or token to send an alert with")
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

// pushLog remembers which replies were alerted, so a restart or a repeated round never alerts one twice. Kept in the
// space's settings, with the time of each (to forget the old ones).
type pushLog struct {
	mu     sync.Mutex
	loaded bool
	m      map[string]int64
}

func (l *pushLog) load(ctx context.Context, st store.Store) {
	if l.loaded {
		return
	}
	l.m = map[string]int64{}
	if raw, ok, err := st.Setting(ctx, pushLogKey); err == nil && ok {
		_ = json.Unmarshal([]byte(raw), &l.m)
	}
	l.loaded = true
}

func (l *pushLog) has(ctx context.Context, st store.Store, key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load(ctx, st)
	_, ok := l.m[key]
	return ok
}

func (l *pushLog) add(ctx context.Context, st store.Store, key string, now int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.load(ctx, st)
	l.m[key] = now
	if len(l.m) > pushKeep {
		keys := make([]string, 0, len(l.m))
		for k := range l.m {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, func(a, b string) int { return int(l.m[a] - l.m[b]) })
		for _, k := range keys[:len(keys)-pushKeep] {
			delete(l.m, k)
		}
	}
	b, _ := json.Marshal(l.m)
	_ = st.PutSetting(ctx, pushLogKey, string(b))
}

// pushID is the alert's id (the hub replaces an earlier alert with the same one): one a reply, at most 56 bytes.
func pushID(thread, last string) string {
	sum := sha256.Sum256([]byte("club-inbox-alert:" + thread + "\n" + last))
	return "ensemble-" + hex.EncodeToString(sum[:12])
}

// alertFor is the phone alert for a row waiting for the owner.
func alertFor(r cachedRow) notification {
	chat := ""
	for _, t := range r.thing.Tags {
		if t.Key == "chat" {
			chat = t.Name
		}
	}
	n := notification{ID: pushID(r.thing.Thread, r.thing.Last), PushLevel: "P0",
		Body: clip(oneLine(chat, pushBodyRune)+"：点开收件箱处理", 120)}
	switch r.attn {
	case attnAuth:
		n.Title, n.Level = clip(r.thing.Sender+" 请你登录或批准授权", 120), "time-sensitive"
	default:
		n.Title, n.Level = clip(r.thing.Sender+" 在等你选", 120), "active"
	}
	return n
}

// pushAttention sends the alerts the rows are owed, and remembers them. A failed one is not remembered: the next round
// tries it again while it is still within the window.
func (n *notifier) pushAttention(ctx context.Context, rows []cachedRow) {
	cfg := n.in.cfg()
	if !cfg.On || !cfg.Push || n.push == nil {
		return
	}
	space, err := n.in.club.Space(ctx, core.DefaultSpace)
	if err != nil {
		return
	}
	st := space.Store()
	now := n.in.club.Now()
	sent := 0
	for _, r := range rows {
		if r.attn == "" || r.thing.Read || r.thing.Done != "" || now.Sub(r.thing.At) > pushWindow || sent >= pushMaxEach {
			continue
		}
		key := r.thing.Thread + "\n" + r.thing.Last
		if n.log.has(ctx, st, key) {
			continue
		}
		if err := n.push(ctx, alertFor(r)); err != nil {
			continue
		}
		n.log.add(ctx, st, key, now.UnixMilli())
		sent++
	}
}
