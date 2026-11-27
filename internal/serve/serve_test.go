package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/conformance"
	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/protocol"
)

// client is the serve shell as a caller sees it: HTTP over a unix socket (or TCP), with an optional token.
type client struct {
	t      *testing.T
	http   *http.Client
	base   string
	token  string
	events chan protocol.UpdatedEvent
}

func (c *client) Call(method string, params, out any) error {
	_, err := c.call(method, params, out)
	return err
}

func (c *client) call(method string, params, out any) (int, error) {
	body, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return 0, err
	}
	req, _ := http.NewRequest(http.MethodPost, c.base+"/v1/spaces/"+core.DefaultSpace+"/rpc", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *protocol.Error `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return resp.StatusCode, err
	}
	if r.Error != nil {
		if want := statusOf(r.Error.Code); r.Error.Code != "unauthorized" && r.Error.Code != "forbidden" && resp.StatusCode != want {
			c.t.Errorf("%s: code %s came with HTTP %d, want %d", method, r.Error.Code, resp.StatusCode, want)
		}
		return resp.StatusCode, r.Error
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out != nil {
		return resp.StatusCode, json.Unmarshal(r.Result, out)
	}
	return resp.StatusCode, nil
}

func (c *client) Event(d time.Duration) (protocol.UpdatedEvent, bool) {
	select {
	case e := <-c.events:
		return e, true
	case <-time.After(d):
		return protocol.UpdatedEvent{}, false
	}
}

// listen streams the space's events into c.events.
func (c *client) listen(ctx context.Context) {
	c.t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/v1/spaces/"+core.DefaultSpace+"/events", nil)
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		c.t.Fatalf("events: HTTP %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	c.events = make(chan protocol.UpdatedEvent, 256)
	connected := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		var name string
		first := true
		for sc.Scan() {
			line := sc.Text()
			if first && strings.HasPrefix(line, ":") {
				first = false
				close(connected)
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				if name != protocol.EventUpdated {
					c.t.Errorf("unexpected event %q", name)
				}
				var e protocol.UpdatedEvent
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &e); err != nil {
					c.t.Errorf("event data: %v", err)
				}
				if e.Contribution != "" {
					c.t.Errorf("serve events carry no contribution: %+v", e)
				}
				c.events <- e
			}
		}
	}()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		c.t.Fatal("event stream did not open")
	}
}

func shortSocket(t *testing.T) string {
	// Unix socket paths are limited to about 100 bytes; t.TempDir can be longer on macOS.
	dir, err := os.MkdirTemp("", "pac")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "club.sock")
}

func TestConformanceUnixSocket(t *testing.T) {
	conformance.Run(t, func(t *testing.T, club *core.Club) conformance.Shell {
		sock := shortSocket(t)
		ctx, cancel := context.WithCancel(context.Background())
		addrs, done, err := Run(ctx, club, Config{Socket: sock})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cancel(); <-done })
		if len(addrs) != 1 || addrs[0].Network() != "unix" {
			t.Fatalf("listening on %v", addrs)
		}
		fi, err := os.Stat(sock)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("socket mode %v %v", fi.Mode(), err)
		}
		hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		}}}
		c := &client{t: t, http: hc, base: "http://club"}
		c.listen(ctx)
		return c
	})
}

func TestTCPNeedsTokens(t *testing.T) {
	if err := (Config{Addr: "127.0.0.1:0"}).Check(); err == nil {
		t.Fatal("TCP without tokens was accepted")
	}
	if _, err := Listen(Config{Addr: "127.0.0.1:0"}); err == nil {
		t.Fatal("Listen opened TCP without tokens")
	}
	if _, _, err := Run(context.Background(), core.New(core.Options{}), Config{Addr: "127.0.0.1:0"}); err == nil {
		t.Fatal("Run started TCP without tokens")
	}
	if err := (Config{}).Check(); err == nil {
		t.Fatal("no listener was accepted")
	}
	if _, err := NewTokens(TokensFile{Read: []string{"short"}}); err == nil {
		t.Fatal("a short token was accepted")
	}
	if _, err := NewTokens(TokensFile{}); err == nil {
		t.Fatal("an empty tokens file was accepted")
	}
	same := strings.Repeat("s", 40)
	if _, err := NewTokens(TokensFile{Read: []string{same}, Admin: []string{same}}); err == nil {
		t.Fatal("one token at two levels was accepted")
	}
}

func TestTokenLevels(t *testing.T) {
	read, write, admin := strings.Repeat("r", 40), strings.Repeat("w", 40), strings.Repeat("a", 40)
	path := filepath.Join(t.TempDir(), "tokens.json")
	b, _ := json.Marshal(TokensFile{Read: []string{read}, Write: []string{write}, Admin: []string{admin}})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	tokens, err := LoadTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	club, _ := conformance.NewClub(t)
	ctx := context.Background()
	space, _ := club.Space(ctx, core.DefaultSpace)
	acct, err := space.AddAccount(ctx, protocol.AddAccountParams{Provider: "echo", Credential: "echo-synthetic-1"})
	if err != nil {
		t.Fatal(err)
	}
	main := core.SessionKey("echo", acct.AccountID, "main")
	srv := httptest.NewServer(New(club, tokens).Handler())
	defer srv.Close()

	calls := []struct {
		method string
		params any
		need   Level
	}{
		{"agentsrc.status", nil, LevelRead},
		{"agentsrc.sessions", nil, LevelRead},
		{"agentsrc.messages", protocol.MessagesParams{SessionKey: main, Limit: 10}, LevelRead},
		{"agentsrc.search", protocol.SearchParams{Q: "x"}, LevelRead},
		{"agentsrc.send", protocol.SendParams{SessionKey: main, ClientID: "77777777-7777-4777-8777-777777777777", Text: "hi"}, LevelWrite},
		{"agentsrc.mark_read", protocol.MarkReadParams{SessionKey: main}, LevelWrite},
		{"agentsrc.delete_message", protocol.DeleteMessageParams{SessionKey: main, ClientID: "77777777-7777-4777-8777-777777777777"}, LevelWrite},
		{"club.info", nil, LevelAdmin},
		{"club.accounts.sync", protocol.AccountParams{AccountID: acct.AccountID}, LevelAdmin},
		{"club.accounts.update", map[string]any{}, LevelAdmin},
	}
	for _, tok := range []struct {
		token string
		level Level
	}{{"", LevelNone}, {strings.Repeat("x", 40), LevelNone}, {read, LevelRead}, {write, LevelWrite}, {admin, LevelAdmin}} {
		c := &client{t: t, http: srv.Client(), base: srv.URL, token: tok.token}
		for _, call := range calls {
			status, err := c.call(call.method, call.params, nil)
			switch {
			case tok.level == LevelNone:
				if status != http.StatusUnauthorized {
					t.Errorf("%s without a valid token: HTTP %d %v", call.method, status, err)
				}
			case tok.level < call.need:
				if status != http.StatusForbidden {
					t.Errorf("%s with a %s token: HTTP %d %v", call.method, tok.level, status, err)
				}
			default:
				if status == http.StatusUnauthorized || status == http.StatusForbidden {
					t.Errorf("%s with a %s token was refused: %v", call.method, tok.level, err)
				}
			}
		}
	}
	for _, tok := range []string{"", read} {
		req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/spaces/default/events", nil)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := srv.Client().Do(req.WithContext(ctx))
		if err != nil {
			t.Fatal(err)
		}
		want := http.StatusOK
		if tok == "" {
			want = http.StatusUnauthorized
		}
		if resp.StatusCode != want {
			t.Errorf("events with token %v: HTTP %d", tok != "", resp.StatusCode)
		}
		cancel()
		resp.Body.Close()
	}
}

func TestRPCShapes(t *testing.T) {
	club, _ := conformance.NewClub(t)
	srv := httptest.NewServer(New(club, nil).Handler())
	defer srv.Close()
	post := func(path, body string) (int, string) {
		resp, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return resp.StatusCode, strings.TrimSpace(buf.String())
	}
	if status, body := post("/v1/spaces/default/rpc", `{"method":"club.info"}`); status != 200 ||
		!strings.Contains(body, `"protocolVersion":1`) || !strings.Contains(body, `"browser":false`) {
		t.Fatalf("club.info: %d %s", status, body)
	}
	if status, body := post("/v1/spaces/default/rpc", `not json`); status != 400 || !strings.Contains(body, `"code":"invalid"`) {
		t.Fatalf("bad body: %d %s", status, body)
	}
	if status, body := post("/v1/spaces/..%2Fx/rpc", `{"method":"agentsrc.status"}`); status != 400 {
		t.Fatalf("bad space: %d %s", status, body)
	}
	if status, body := post("/v1/spaces/default/rpc", `{"method":"club.logins.browser.start","params":{}}`); status != 501 ||
		!strings.Contains(body, `"code":"unsupported"`) {
		t.Fatalf("stub: %d %s", status, body)
	}
	if status, body := post("/v1/spaces/other/rpc", `{"method":"agentsrc.status"}`); status != 200 || !strings.Contains(body, `"setup":"no_accounts"`) {
		t.Fatalf("second space: %d %s", status, body)
	}
}
