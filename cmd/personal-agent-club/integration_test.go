package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/datadir"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider/providertest"
	"github.com/nwverse/ensemble/internal/store"
)

// These run the whole program as the Lark gateway meets it: the serve command on a data directory on disk (SQLite
// stores, the encrypted vault, the lock), HTTP over its unix socket, and the real provider adapters talking to a
// fake upstream that counts what it is asked.

type rpcClient struct {
	t    *testing.T
	http *http.Client
}

func (c *rpcClient) call(method string, params, out any) error {
	c.t.Helper()
	body, _ := json.Marshal(map[string]any{"method": method, "params": params})
	resp, err := c.http.Post("http://club/v1/spaces/default/rpc", "application/json", bytes.NewReader(body))
	if err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	defer resp.Body.Close()
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *protocol.Error `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
	if r.Error != nil {
		return r.Error
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (c *rpcClient) must(method string, params, out any) {
	c.t.Helper()
	if err := c.call(method, params, out); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
}

// events streams the space's agentsrc.updated events until the test ends.
func (c *rpcClient) events() <-chan protocol.UpdatedEvent {
	c.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.t.Cleanup(cancel)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://club/v1/spaces/default/events", nil)
	resp, err := c.http.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		c.t.Fatalf("events: %v %v", resp, err)
	}
	out := make(chan protocol.UpdatedEvent, 256)
	connected := make(chan struct{})
	go func() {
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		first := true
		for sc.Scan() {
			line := sc.Text()
			if first {
				first = false
				close(connected)
			}
			if data, ok := strings.CutPrefix(line, "data: "); ok {
				var e protocol.UpdatedEvent
				if json.Unmarshal([]byte(data), &e) == nil {
					out <- e
				}
			}
		}
	}()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		c.t.Fatal("the event stream did not open")
	}
	return out
}

// awaitEvent waits for an event naming key (or the whole list).
func awaitEvent(t *testing.T, events <-chan protocol.UpdatedEvent, key string) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case e := <-events:
			if e.SessionKeys == nil || slices.Contains(e.SessionKeys, key) {
				return
			}
		case <-deadline:
			t.Fatalf("no agentsrc.updated for %s", key)
		}
	}
}

func send(c *rpcClient, key, clientID, text string) protocol.Message {
	var m protocol.Message
	c.must("agentsrc.send", map[string]any{"session_key": key, "client_id": clientID, "text": text}, &m)
	return m
}

func groqAccount(t *testing.T, c *rpcClient) protocol.Account {
	t.Helper()
	var st protocol.StatusResult
	c.must("agentsrc.status", nil, &st)
	for _, a := range st.Accounts {
		if a.Provider == "groq" {
			return a
		}
	}
	t.Fatalf("no groq account: %+v", st)
	return protocol.Account{}
}

func messageTexts(t *testing.T, c *rpcClient, key string) (texts []string, local map[string]string) {
	t.Helper()
	var page protocol.MessagePage
	c.must("agentsrc.messages", map[string]any{"session_key": key}, &page)
	local = map[string]string{}
	for _, m := range page.Messages {
		for _, b := range m.Blocks {
			texts = append(texts, b.Text)
		}
		if m.Local != nil {
			local[m.Local.ClientID] = m.Local.Status
		}
	}
	return texts, local
}

const chatPath = "/openai/v1/chat/completions"

func TestEndToEndAndRestart(t *testing.T) {
	u := newUpstream(t)
	data := shortDir(t)
	sock := filepath.Join(data, "club.sock")
	stop := startServe(t, u, "-data", data, "-poll", "0")
	c := &rpcClient{t: t, http: serveHTTP(sock)}
	events := c.events()

	var info protocol.ClubInfo
	c.must("club.info", nil, &info)
	if info.ProtocolVersion != protocol.Version || !slices.ContainsFunc(info.Providers, func(p protocol.Provider) bool {
		return p.Provider == "groq" && p.Available
	}) {
		t.Fatalf("%+v", info)
	}

	// pairing: the key is checked against the model list, once
	var acct protocol.Account
	c.must("club.accounts.add", map[string]any{"provider": "groq", "credential": groqKey}, &acct)
	if acct.Status != "active" || acct.AssistantControl != "off" || acct.SessionCount != 1 || u.count("/openai/v1/models") != 1 {
		t.Fatalf("%+v", acct)
	}
	if err := c.call("club.accounts.add", map[string]any{"provider": "groq", "credential": "fixture-wrong-key"}, nil); protocol.ReasonOf(err) != "AUTH_FAILED" {
		t.Fatalf("a refused key: %v", err)
	}
	var sessions protocol.SessionsResult
	c.must("agentsrc.sessions", nil, &sessions)
	if len(sessions.Sessions) != 1 || !sessions.Sessions[0].CanSend {
		t.Fatalf("%+v", sessions)
	}
	key := sessions.Sessions[0].SessionKey

	// a send: Groq answers in the same call, the stream says so, the answer is stored and found
	m := send(c, key, "00000000-0000-4000-8000-0000000000e1", "明早有几班直飞？")
	if m.Local == nil || m.Local.Status != "sent" {
		t.Fatalf("%+v", m)
	}
	awaitEvent(t, events, key)
	if u.count(chatPath) != 1 {
		t.Fatalf("%d chat calls", u.count(chatPath))
	}
	var chat providertest.Call
	for _, call := range u.calls() {
		if call.Path == chatPath {
			chat = call
		}
	}
	if chat.Header.Get("Authorization") != "Bearer "+groqKey || !strings.Contains(string(chat.Body), "明早有几班直飞") {
		t.Fatalf("%+v", chat)
	}
	texts, _ := messageTexts(t, c, key)
	if !slices.Contains(texts, "明早 9 点有 3 班直飞。") || !slices.Contains(texts, "明早有几班直飞？") {
		t.Fatalf("%q", texts)
	}
	for _, q := range []string{"直飞", "班直飞", "明早"} {
		var hits protocol.SearchResult
		c.must("agentsrc.search", map[string]any{"q": q}, &hits)
		if len(hits.Hits) == 0 || hits.Hits[0].SessionKey != key {
			t.Fatalf("search %q: %+v", q, hits)
		}
	}
	c.must("agentsrc.mark_read", map[string]any{"session_key": key}, nil)
	c.must("agentsrc.sessions", nil, &sessions)
	if sessions.Sessions[0].UnreadCount != 0 {
		t.Fatalf("%+v", sessions.Sessions[0])
	}

	// quota from the answer's headers: 15% of the tokens is the tightest, the requests in the details
	a := groqAccount(t, c)
	if q := a.Quota; q == nil || q.State != "low" || q.Kind != "tokens" || q.Source != "provider" || len(q.Details) != 1 ||
		q.Details[0].Kind != "requests" || q.Details[0].State != "ok" {
		t.Fatalf("%+v", a.Quota)
	}
	if sessions.Sessions[0].Quota == nil || sessions.Sessions[0].Quota.State != "low" {
		t.Fatalf("the session's quota is its account's: %+v", sessions.Sessions[0].Quota)
	}

	// a 429 (with rate limit headers too): the send fails, the account is exhausted until Retry-After
	u.answer(func(call providertest.Call) (providertest.Answer, bool) {
		h := groqLimits("14400", "14398", "8000", "0")
		h.Set("Retry-After", "30")
		return providertest.Answer{Status: 429, Header: h, Body: map[string]any{"error": map[string]any{"code": "rate_limit_exceeded"}}},
			call.Path == chatPath
	})
	before := time.Now().UnixMilli()
	m = send(c, key, "00000000-0000-4000-8000-0000000000e2", "再问一次")
	if m.Local == nil || m.Local.Status != "failed" || m.Local.Error != "RATE_LIMITED" {
		t.Fatalf("%+v", m)
	}
	a = groqAccount(t, c)
	if q := a.Quota; q == nil || q.State != "exhausted" || q.Source != "response" || q.ResetsAt < before+29_000 || q.ResetsAt > time.Now().UnixMilli()+30_000 {
		t.Fatalf("%+v", a.Quota)
	}
	if u.count(chatPath) != 2 {
		t.Fatalf("%d chat calls", u.count(chatPath))
	}
	u.answer(nil)

	// a Cue account too, whose sync reads the upstream with its key
	var cue protocol.Account
	c.must("club.accounts.add", map[string]any{"provider": "cue", "credential": cueKey}, &cue)
	if code, errOut := stop(); code != 0 {
		t.Fatalf("serve: %d %s", code, errOut)
	}

	// While serve was down: one send claimed when the process died, one still pending.
	// While serve was down: a send that was claimed when the process died.
	claimed, pending := "00000000-0000-4000-8000-0000000000c1", "00000000-0000-4000-8000-0000000000c2"
	enqueue := func(clientID, text string, claim bool) {
		offline(t, data, func(st store.Store) {
			ctx := context.Background()
			row := store.Outbox{ClientID: clientID, Text: text, SessionKey: key, AccountID: a.AccountID, CreatedAt: time.Now().UnixMilli()}
			if _, ok, err := st.EnqueueOutbox(ctx, row); err != nil || !ok {
				t.Fatal(err)
			}
			if !claim {
				return
			}
			if _, ok, err := st.ClaimOutbox(ctx, clientID, time.Now().UnixMilli()); err != nil || !ok {
				t.Fatal(err)
			}
		})
	}
	enqueue(claimed, "发到一半", true)

	// a wrong key file: refused before listening, nothing lost
	if code, _, errOut := runCmd(t, u, "serve", "-data", data, "-key-file", filepath.Join(data, "other.key")); code != 1 ||
		!strings.Contains(errOut, "does not open with this key") {
		t.Fatalf("a wrong key: %d %s", code, errOut)
	}

	chats := u.count(chatPath)
	stop = startServe(t, u, "-data", data, "-poll", "0")
	c = &rpcClient{t: t, http: serveHTTP(sock)}

	// a second owner is refused while serve runs
	if d, err := datadir.Open(data, ""); !errors.Is(err, datadir.ErrInUse) {
		if d != nil {
			_ = d.Close()
		}
		t.Fatalf("a second owner: %v", err)
	}

	// everything is still there, read state included; quota is kept in memory only
	a = groqAccount(t, c)
	if a.AccountID != acct.AccountID || a.Quota != nil {
		t.Fatalf("%+v %+v", a, a.Quota)
	}
	texts, local := messageTexts(t, c, key)
	if !slices.Contains(texts, "明早 9 点有 3 班直飞。") || local["00000000-0000-4000-8000-0000000000e2"] != "failed" {
		t.Fatalf("%q %v", texts, local)
	}
	var hits protocol.SearchResult
	c.must("agentsrc.search", map[string]any{"q": "直飞"}, &hits)
	if len(hits.Hits) == 0 {
		t.Fatal("search lost its index")
	}
	c.must("agentsrc.sessions", nil, &sessions)
	for _, s := range sessions.Sessions {
		if s.SessionKey == key && s.UnreadCount != 0 {
			t.Fatalf("read state lost: %+v", s)
		}
	}
	// the claimed send is unconfirmed, and the upstream was not asked again
	if local[claimed] != "unconfirmed" || u.count(chatPath) != chats {
		t.Fatalf("%v, %d chat calls after the restart", local, u.count(chatPath)-chats)
	}

	// the credentials still decrypt: a sync reaches the upstream with the Cue key
	calls := len(u.calls())
	c.must("club.accounts.sync", map[string]any{"account_id": cue.AccountID}, &cue)
	reached := false
	for _, call := range u.calls()[calls:] {
		if call.Path == "/v2/agent.list" {
			reached = call.Header.Get("x-manus-api-key") == cueKey
		}
	}
	if !reached || cue.Status != "active" || cue.LastError != "" {
		t.Fatalf("%v %+v", reached, cue)
	}
	if code, errOut := stop(); code != 0 {
		t.Fatalf("serve: %d %s", code, errOut)
	}

	// A send still pending (it never reached a provider) goes out when the space opens again, once, as
	// Space.recover documents; the unconfirmed one is still not sent.
	enqueue(pending, "排队中", false)
	chats = u.count(chatPath)
	calls = len(u.calls())
	startServe(t, u, "-data", data, "-poll", "0")
	c = &rpcClient{t: t, http: serveHTTP(sock)}
	if n := u.count(chatPath) - chats; n != 1 {
		t.Fatalf("%d chat calls for the pending send", n)
	}
	for _, call := range u.calls()[calls:] {
		if call.Path == chatPath && !strings.Contains(string(call.Body), "排队中") {
			t.Fatalf("not the pending send: %s", call.Body)
		}
	}
	texts, local = messageTexts(t, c, key)
	// (a sent one drops out of the local echoes once Groq's own copy of it is stored)
	if status, shown := local[pending]; (shown && status != "sent") || !slices.Contains(texts, "排队中") || local[claimed] != "unconfirmed" {
		t.Fatalf("%v %q", local, texts)
	}
}

// offline opens the default space of data the way no serve is running, for f.
func offline(t *testing.T, data string, f func(store.Store)) {
	t.Helper()
	dir, err := datadir.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	st, _, err := dir.OpenSpace("default")
	if err != nil {
		t.Fatal(err)
	}
	f(st)
}

// An import into a directory a serve then owns: the imported accounts, sessions and messages are served, the
// claimed send is unconfirmed, the pending one failed, and none reached the upstream.
func TestImportThenServe(t *testing.T) {
	u := newUpstream(t)
	data := shortDir(t)
	if code, _, errOut := runCmd(t, u, "import", "-data", data, writeBundle(t, testBundle(), 0o600)); code != 0 {
		t.Fatal(errOut)
	}
	startServe(t, u, "-data", data, "-poll", "0")
	c := &rpcClient{t: t, http: serveHTTP(filepath.Join(data, "club.sock"))}
	var st protocol.StatusResult
	c.must("agentsrc.status", nil, &st)
	if len(st.Accounts) != 3 {
		t.Fatalf("%+v", st.Accounts)
	}
	_, local := messageTexts(t, c, "agent:groq:"+groqAcct+":chat")
	want := map[string]string{"00000000-0000-4000-8000-000000000001": "unconfirmed", "00000000-0000-4000-8000-000000000002": "failed"}
	if fmt.Sprint(local) != fmt.Sprint(want) {
		t.Fatalf("%v", local)
	}
	if u.count(chatPath) != 0 {
		t.Fatal("an imported send went out")
	}
	var hits protocol.SearchResult
	c.must("agentsrc.search", map[string]any{"q": "提醒"}, &hits)
	if len(hits.Hits) != 2 {
		t.Fatalf("%+v", hits)
	}
}
