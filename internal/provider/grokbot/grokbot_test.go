package grokbot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/providertest"
)

const key = "fixture-key-not-real"

var (
	ctx      = context.Background()
	session  = provider.SessionRef{ExternalID: "bot_alpha/", Ref: map[string]string{"botId": "bot_alpha", "sessionId": ""}}
	clientID = "9b5e1c2a-1f0e-4d6b-8c3a-2e4f5a6b7c8d"
)

func method(c providertest.Call) string { return c.Path[strings.LastIndex(c.Path, "/")+1:] }

func routes(t *testing.T) func(providertest.Call) providertest.Answer {
	grok := providertest.Fixture(t, "grok-bot-0.78.1.json")
	return func(c providertest.Call) providertest.Answer {
		body := map[string]any{}
		_ = json.Unmarshal(c.Body, &body)
		switch m := method(c); m {
		case "ListGrokBotAgentSessions":
			return providertest.Answer{Body: grok["ListGrokBotAgentSessions:"+body["agentId"].(string)]}
		case "ListGrokBotTranscriptEntries":
			return providertest.Answer{Body: grok["ListGrokBotTranscriptEntries:"+body["agentId"].(string)+":"+body["sessionId"].(string)]}
		default:
			return providertest.Answer{Body: grok[m]}
		}
	}
}

func answer(body any) func(providertest.Call) providertest.Answer {
	return func(providertest.Call) providertest.Answer { return providertest.Answer{Body: body} }
}

func TestVerifiesWithGetMeAndListsMainAndSideSessions(t *testing.T) {
	up := providertest.Serve(t, routes(t))
	a := New(Options{Origin: up.URL})
	acct, err := a.Verify(ctx, key)
	if err != nil || !reflect.DeepEqual(acct, provider.RemoteAccount{ExternalAccountID: "auth0|fixture", Alias: "fixture@example.com"}) {
		t.Fatalf("%+v %v", acct, err)
	}
	c := up.Calls()[0]
	if c.Path != "/aiserver.v1.DashboardService/GetMe" || c.Header.Get("authorization") != "Bearer "+key ||
		c.Header.Get("connect-protocol-version") != "1" || c.Header.Get("x-request-id") == "" {
		t.Fatalf("%+v", c)
	}
	sessions, err := a.ListSessions(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	pic := "https://images.example.com/bot_alpha.png"
	want := []provider.RemoteSession{
		{ExternalID: "bot_alpha/", Title: "Grok 秘书", Kind: protocol.KindMain, LastActivityMs: 1761802000000, CanSend: true,
			AvatarURL: pic, Ref: map[string]string{"botId": "bot_alpha", "sessionId": ""}},
		{ExternalID: "bot_alpha/sess-side-42", Title: "Grok 秘书 · sess-sid", Kind: protocol.KindSide, ParentExternalID: "bot_alpha/",
			LastActivityMs: 1761790000000, CanSend: true, AvatarURL: pic, Ref: map[string]string{"botId": "bot_alpha", "sessionId": "sess-side-42"}},
	}
	if !reflect.DeepEqual(sessions, want) {
		t.Fatalf("%+v", sessions)
	}
}

func TestPairingAliasIsTheSoleBot(t *testing.T) {
	base := routes(t)
	withRoster := func(answer providertest.Answer) func(providertest.Call) providertest.Answer {
		return func(c providertest.Call) providertest.Answer {
			if method(c) == "ListGrokBotAgents" {
				// Only the account's own bots: a teammate's is not "the only bot".
				if !strings.Contains(string(c.Body), `"includeTeamAgents":false`) {
					return providertest.Answer{Status: 400, Body: map[string]any{"code": "want own bots"}}
				}
				return answer
			}
			return base(c)
		}
	}
	withRoom := providertest.Answer{Body: map[string]any{"agents": []any{
		map[string]any{"agentId": "bot_alpha", "name": "Grok 秘书"}, map[string]any{"agentId": "bot_room", "name": "Team room", "kind": "GROK_BOT_AGENT_KIND_ROOM"}}}}
	two := providertest.Answer{Body: map[string]any{"agents": []any{
		map[string]any{"agentId": "bot_alpha", "name": "Grok 秘书"}, map[string]any{"agentId": "bot_beta", "name": "Grok 2"}}}}
	none := providertest.Answer{Body: map[string]any{"agents": []any{}}}
	untitled := providertest.Answer{Body: map[string]any{"agents": []any{map[string]any{"agentId": "bot_alpha", "title": "Helper"}}}}
	broken := providertest.Answer{Status: 500, Body: map[string]any{"code": "internal"}}
	for _, c := range []struct {
		roster providertest.Answer
		want   string
	}{{providertest.Answer{Body: providertest.Fixture(t, "grok-bot-0.78.1.json")["ListGrokBotAgents"]}, "Grok 秘书"},
		{two, ""}, {none, ""}, {untitled, "Helper"}, {broken, ""}, {withRoom, "Grok 秘书"}} {
		if got := New(Options{Origin: providertest.Serve(t, withRoster(c.roster)).URL}).PairingAlias(ctx, key); got != c.want {
			t.Fatalf("%+v: %q, want %q", c.roster.Body, got, c.want)
		}
	}
}

func TestGivesRoomsNoAvatarAndBotsWithoutAPictureAGeneratedOne(t *testing.T) {
	base := routes(t)
	roster := func(agents ...any) func(providertest.Call) providertest.Answer {
		return func(c providertest.Call) providertest.Answer {
			if method(c) == "ListGrokBotAgents" {
				return providertest.Answer{Body: map[string]any{"agents": agents, "truncated": false}}
			}
			return base(c)
		}
	}
	bot := map[string]any{"agentId": "bot_alpha", "name": "Grok 秘书", "viewerSessionId": ""}
	solo, err := New(Options{Origin: providertest.Serve(t, roster(bot)).URL}).ListSessions(ctx, key)
	if err != nil || !strings.HasPrefix(solo[0].AvatarURL, "data:image/svg+xml;base64,") {
		t.Fatalf("%+v %v", solo, err)
	}
	room := map[string]any{"agentId": "bot_alpha", "name": "Grok 秘书", "viewerSessionId": "", "kind": "2"}
	rooms, err := New(Options{Origin: providertest.Serve(t, roster(room)).URL}).ListSessions(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range rooms {
		if s.AvatarURL != "" || s.Ref["room"] != "1" {
			t.Fatalf("%+v", s)
		}
	}
	bad := func(providertest.Call) providertest.Answer {
		return providertest.Answer{Body: map[string]any{"agents": "x"}}
	}
	if _, err := New(Options{Origin: providertest.Serve(t, bad).URL}).ListSessions(ctx, key); provider.CodeOf(err) != "INVALID_ROSTER_RESPONSE" {
		t.Fatal(err)
	}
}

func TestAvatarMatchesTheOfficialDerivation(t *testing.T) {
	for id, want := range map[string][2]string{"bot_alpha": {"blob", "magenta"}, "x": {"cloud", "green"},
		"雅典娜": {"hex", "violet"}, "agent-123456": {"teardrop", "green"}, "Z": {"cloud", "yellow"}} {
		if shape, color := Character(id); shape != want[0] || color != want[1] {
			t.Errorf("%s: %s %s", id, shape, color)
		}
	}
	got := AvatarURL(map[string]any{"agentId": "bot_alpha", "avatarColor": "black", "avatarShape": "hex"})
	if got != "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCAxMDAgMTAwIj48cGF0aCBkPSJNNTAgNkw4OCAyOFY3Mkw1MCA5NEwxMiA3MlYyOHoiIGZpbGw9IiMwMDAwMDAiLz48L3N2Zz4=" {
		t.Fatal(got)
	}
	for picture, kept := range map[string]bool{"http://example.com/a.png": false, "data:image/png;base64,AAAA": true,
		"data:image/svg+xml;base64,AAAA": false, "https://example.com/" + strings.Repeat("a", 2048): false} {
		got := AvatarURL(map[string]any{"agentId": "b", "avatarUrl": picture})
		if (got == picture) != kept {
			t.Errorf("%.40s: %.60s", picture, got)
		}
	}
	if AvatarURL(map[string]any{}) != "" {
		t.Fatal("avatar without an agent id")
	}
}

func TestDecodesTranscriptBodiesInSequenceOrder(t *testing.T) {
	up := providertest.Serve(t, routes(t))
	page, err := New(Options{Origin: up.URL}).ListMessages(ctx, key, session, provider.ListOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		id, role string
		pos      int64
	}
	var got []row
	for _, m := range page.Messages {
		got = append(got, row{m.ExternalID, m.Role, m.Position})
	}
	if !reflect.DeepEqual(got, []row{{"e-omitted", "system", 9}, {"e-user", "user", 10}, {"e-tool", "tool", 11}, {"e-reply", "assistant", 12}}) {
		t.Fatalf("%+v", got)
	}
	if m := page.Messages[1]; m.CreateTimeMs != 1761801990000 || !reflect.DeepEqual(m.Blocks, []protocol.Block{protocol.TextBlock("明天早上提醒我开会")}) {
		t.Fatalf("%+v", m)
	}
	if !reflect.DeepEqual(page.Messages[3].Blocks, []protocol.Block{protocol.TextBlock("明早 9 点提醒你开会。")}) {
		t.Fatalf("%+v", page.Messages[3])
	}
	if !reflect.DeepEqual(page.Messages[0].Blocks, []protocol.Block{{Type: "status", Status: "unavailable", Text: "send-message"}}) {
		t.Fatalf("%+v", page.Messages[0])
	}
	if !reflect.DeepEqual(page.Messages[2].Blocks, []protocol.Block{{Type: "tool-call", Name: "tool-call", Summary: "calendar.create"}}) {
		t.Fatalf("%+v", page.Messages[2])
	}
	var body map[string]any
	_ = json.Unmarshal(up.Calls()[0].Body, &body)
	if body["limit"] != 50.0 || body["agentId"] != "bot_alpha" || body["sessionId"] != "" {
		t.Fatalf("%v", body)
	}
}

func TestSendsWithTheClientIDAsMessageIDAndNeverResends(t *testing.T) {
	grok := providertest.Fixture(t, "grok-bot-0.78.1.json")
	msg := provider.Outgoing{ClientID: clientID, Text: "hi"}
	ok := providertest.Serve(t, answer(grok["SendGrokBotUserMessage"]))
	res, err := New(Options{Origin: ok.URL}).Send(ctx, key, session, msg, provider.SendContext{})
	if err != nil || !reflect.DeepEqual(res, provider.SendResult{Status: provider.Sent, ExternalID: clientID}) {
		t.Fatalf("%+v %v", res, err)
	}
	body := ok.Calls()[0].JSON(t)
	if body["agentId"] != "bot_alpha" || body["text"] != "hi" || body["messageId"] != clientID || body["isFork"] != false {
		t.Fatalf("%v", body)
	}
	for _, tc := range []struct {
		name   string
		answer providertest.Answer
		want   provider.SendResult
		code   string
	}{
		{"lost", providertest.Answer{Drop: true}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"busy", providertest.Answer{Status: 503}, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, ""},
		{"refused", providertest.Answer{Body: grok["SendGrokBotUserMessage.refused"]}, provider.SendResult{}, "SEND_REFUSED"},
		{"expired", providertest.Answer{Status: 401}, provider.SendResult{}, "AUTH_FAILED"},
		{"rate limited", providertest.Answer{Status: 429, Header: http.Header{"Retry-After": {"12"}}}, provider.SendResult{}, "RATE_LIMITED"},
	} {
		up := providertest.Serve(t, func(providertest.Call) providertest.Answer { return tc.answer })
		res, err := New(Options{Origin: up.URL}).Send(ctx, key, session, msg, provider.SendContext{})
		if !reflect.DeepEqual(res, tc.want) || (tc.code != "" && provider.CodeOf(err) != tc.code) || (tc.code == "" && err != nil) {
			t.Errorf("%s: %+v %v", tc.name, res, err)
		}
		if n := len(up.Calls()); n != 1 {
			t.Errorf("%s: %d calls", tc.name, n)
		}
		if e, ok := err.(*provider.Error); tc.code == "RATE_LIMITED" && (!ok || e.HTTPStatus != 429 || e.RetryAfter != 12*time.Second) {
			t.Errorf("%s: %#v", tc.name, err)
		}
	}
	if _, err := New(Options{Origin: ok.URL}).Send(ctx, key, provider.SessionRef{Ref: map[string]string{"botId": "bad id"}}, msg,
		provider.SendContext{}); provider.CodeOf(err) != "BOT_ID_REQUIRED" {
		t.Fatal(err)
	}
}

func TestReadsAnUnconfirmedSendsStatusByItsMessageID(t *testing.T) {
	grok := providertest.Fixture(t, "grok-bot-0.78.1.json")
	for name, want := range map[string]provider.Confirmation{
		"GetGrokBotSendStatus":          {Status: provider.ConfirmSent},
		"GetGrokBotSendStatus.rejected": {Status: provider.ConfirmFailed, Error: "BOX_ASLEEP"},
		"GetGrokBotSendStatus.pending":  {Status: provider.ConfirmUnknown},
	} {
		up := providertest.Serve(t, answer(grok[name]))
		got, err := New(Options{Origin: up.URL}).ConfirmSend(ctx, key, session, clientID)
		if err != nil || got != want {
			t.Errorf("%s: %+v %v", name, got, err)
		}
		calls := up.Calls()
		if len(calls) != 1 || calls[0].Path != "/aiserver.v1.GrokBotService/GetGrokBotSendStatus" ||
			string(calls[0].Body) != `{"agentId":"bot_alpha","messageId":"`+clientID+`","sessionId":""}` {
			t.Errorf("%s: %+v", name, calls)
		}
	}
}

func TestReadsWhetherTheBotIsWorkingInThisSession(t *testing.T) {
	grok := providertest.Fixture(t, "grok-bot-0.78.1.json")
	up := providertest.Serve(t, answer(grok["ListGrokBotAgentStatuses"]))
	a := New(Options{Origin: up.URL})
	if got, err := a.LiveState(ctx, key, session); err != nil || got != (provider.LiveState{Running: true, Composing: true}) {
		t.Fatalf("%+v %v", got, err)
	}
	if string(up.Calls()[0].Body) != `{"agentIds":["bot_alpha"]}` {
		t.Fatalf("%s", up.Calls()[0].Body)
	}
	side := provider.SessionRef{ExternalID: "bot_alpha/sess-side-42", Ref: map[string]string{"botId": "bot_alpha", "sessionId": "sess-side-42"}}
	if got, _ := a.LiveState(ctx, key, side); got != (provider.LiveState{}) {
		t.Fatalf("%+v", got)
	}
	flag := providertest.Serve(t, answer(map[string]any{"statuses": []any{map[string]any{"agentId": "bot_alpha", "isRunning": true}}}))
	if got, _ := New(Options{Origin: flag.URL}).LiveState(ctx, key, side); got != (provider.LiveState{Running: true}) {
		t.Fatalf("%+v", got)
	}
	expired := providertest.Serve(t, func(providertest.Call) providertest.Answer { return providertest.Answer{Status: 401} })
	if _, err := New(Options{Origin: expired.URL}).LiveState(ctx, key, session); provider.CodeOf(err) != "AUTH_FAILED" {
		t.Fatal(err)
	}
}

func entryBody(v any) string {
	b, _ := json.Marshal(v)
	return base64.StdEncoding.EncodeToString(b)
}

func TestShowsTheFilesAnEntryNamesByNameAndTypeOnly(t *testing.T) {
	user, ok := Message(map[string]any{"entryId": "e-file", "seq": "20", "entryKind": "user-message", "body": entryBody(map[string]any{
		"kind": "user-message", "text": "看看这两个文件", "attachmentNames": []any{"report.pdf", ""},
		"attachmentPaths": []any{"uploads/u1/report.pdf"}})})
	if !ok || !reflect.DeepEqual(user.Blocks, []protocol.Block{protocol.TextBlock("看看这两个文件"), {Type: "attachment", Name: "report.pdf"}}) {
		t.Fatalf("%+v", user)
	}
	if b, _ := json.Marshal(user); bytes.Contains(b, []byte("uploads/u1")) {
		t.Fatal("a storage path leaked")
	}
	reply, _ := Message(map[string]any{"entryId": "e-chart", "seq": "21", "body": entryBody(map[string]any{
		"kind": "send-message", "message": map[string]any{"type": "text", "content": "图表在这里",
			"attachments": []any{map[string]any{"name": "chart.png", "contentType": "image/png"}}}})})
	if reply.Role != "assistant" || !reflect.DeepEqual(reply.Blocks, []protocol.Block{protocol.TextBlock("图表在这里"),
		{Type: "attachment", Name: "chart.png", Kind: "image/png"}}) {
		t.Fatalf("%+v", reply)
	}
	if _, ok := Message(map[string]any{"seq": "x"}); ok {
		t.Fatal("an entry without id or sequence")
	}
	if m, ok := Message(map[string]any{"seq": json.Number("7")}); !ok || m.ExternalID != "seq-7" || m.Position != 7 {
		t.Fatalf("%+v", m)
	}
}

func endFrame(body any) []byte {
	f := ConnectFrame(body)
	f[0] = 2
	return f
}

type chunked struct {
	data []byte
	n    int
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(len(p), c.n, len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

func TestReadsConnectFramesSplitAcrossChunksAndStopsAtTheEndFrame(t *testing.T) {
	stream := bytes.Join([][]byte{ConnectFrame(map[string]any{"heartbeat": map[string]any{"serverTimeMs": "1"}}),
		ConnectFrame(map[string]any{"rows": map[string]any{"agentId": "bot_alpha"}}), endFrame(map[string]any{}),
		ConnectFrame(map[string]any{"after": "end"})}, nil)
	var got []string
	err := ReadConnectFrames(ctx, &chunked{data: stream, n: 7}, func(m map[string]any) bool {
		b, _ := json.Marshal(m)
		got = append(got, string(b))
		return true
	})
	if err != nil || !reflect.DeepEqual(got, []string{`{"heartbeat":{"serverTimeMs":"1"}}`, `{"rows":{"agentId":"bot_alpha"}}`}) {
		t.Fatalf("%v %v", got, err)
	}
	if err := ReadConnectFrames(ctx, &chunked{data: endFrame(map[string]any{"error": map[string]any{}}), n: 7},
		func(map[string]any) bool { return true }); provider.CodeOf(err) != "UPSTREAM_ERROR" {
		t.Fatal(err)
	}
	compressed := ConnectFrame(map[string]any{})
	compressed[0] = 1
	if err := ReadConnectFrames(ctx, bytes.NewReader(compressed), func(map[string]any) bool { return true }); provider.CodeOf(err) != "REQUEST_FAILED" {
		t.Fatal(err)
	}
}

func TestWatchesTheTranscriptAfterItsNewestEntryUntilTheBotGoesIdle(t *testing.T) {
	signed := Credential("fixture-access-1", "fixture-refresh-1")
	stream := bytes.Join([][]byte{
		ConnectFrame(map[string]any{"connected": map[string]any{"serverTimeMs": "1"}}),
		ConnectFrame(map[string]any{"rows": map[string]any{"agentId": "bot_alpha", "sessionId": "sess-other", "entries": []any{map[string]any{"seq": "7"}}}}),
		ConnectFrame(map[string]any{"rows": map[string]any{"agentId": "bot_alpha", "sessionId": "", "entries": []any{map[string]any{"seq": "42"}}}}),
		ConnectFrame(map[string]any{"agentState": map[string]any{"live": []any{map[string]any{"agentId": "bot_alpha", "sessionId": "", "isRunning": true}}}}),
		ConnectFrame(map[string]any{"agentState": map[string]any{"live": []any{map[string]any{"agentId": "bot_alpha", "sessionId": "", "isRunning": false}}}}),
		endFrame(map[string]any{}),
	}, nil)
	up := providertest.Serve(t, func(c providertest.Call) providertest.Answer {
		if method(c) == "ListGrokBotTranscriptEntries" {
			return providertest.Answer{Body: map[string]any{"generation": "3", "entries": []any{map[string]any{"seq": "40", "updatedSeq": "41"}}}}
		}
		return providertest.Answer{Body: stream, Header: map[string][]string{"Content-Type": {"application/connect+json"}}}
	})
	var events []provider.WatchEvent
	err := New(Options{Origin: up.URL}).Watch(ctx, signed, session, func(e provider.WatchEvent) bool {
		events = append(events, e)
		return true
	})
	if err != nil || !reflect.DeepEqual(events, []provider.WatchEvent{provider.WatchChanged, provider.WatchIdle}) {
		t.Fatalf("%v %v", events, err)
	}
	w := up.Calls()[1]
	if w.Path != "/aiserver.v1.GrokBotService/WatchGrokBotTranscripts" || w.Header.Get("content-type") != "application/connect+json" ||
		w.Header.Get("authorization") != "Bearer fixture-access-1" || w.Body[0] != 0 {
		t.Fatalf("%+v", w)
	}
	var sent map[string]any
	_ = json.Unmarshal(w.Body[5:], &sent)
	cursor := sent["cursors"].([]any)[0].(map[string]any)
	if cursor["agentId"] != "bot_alpha" || cursor["sessionId"] != "" || cursor["generation"] != "3" || cursor["afterUpdatedSeq"] != "41" {
		t.Fatalf("%v", sent)
	}
}

func TestRefusesAWatchTheServerAnswersWithoutAStream(t *testing.T) {
	up := providertest.Serve(t, func(c providertest.Call) providertest.Answer {
		if method(c) == "ListGrokBotTranscriptEntries" {
			return providertest.Answer{Body: map[string]any{"entries": []any{}}}
		}
		return providertest.Answer{Status: 401}
	})
	err := New(Options{Origin: up.URL}).Watch(ctx, key, session, func(provider.WatchEvent) bool { return true })
	if provider.CodeOf(err) != "AUTH_FAILED" {
		t.Fatal(err)
	}
	plain := providertest.Serve(t, answer(map[string]any{"entries": []any{}}))
	err = New(Options{Origin: plain.URL}).Watch(ctx, key, session, func(provider.WatchEvent) bool { return true })
	if provider.CodeOf(err) != "REQUEST_FAILED" {
		t.Fatal(err)
	}
}

func TestRefreshesASignedInCredentialAndKeepsTheRefreshToken(t *testing.T) {
	signed := Credential("fixture-access-1", "fixture-refresh-1")
	up := providertest.Serve(t, answer(map[string]any{"access_token": "fixture-access-2"}))
	a := New(Options{Origin: up.URL})
	fresh, err := a.RefreshCredential(ctx, signed)
	if err != nil || fresh != Credential("fixture-access-2", "fixture-refresh-1") {
		t.Fatalf("%q %v", fresh, err)
	}
	c := up.Calls()[0]
	if c.Path != "/oauth/token" || c.Header.Get("authorization") != "" ||
		string(c.Body) != `{"client_id":"`+OAuthClientID+`","grant_type":"refresh_token","refresh_token":"fixture-refresh-1"}` {
		t.Fatalf("%+v %s", c, c.Body)
	}
	_, _ = a.Verify(ctx, fresh)
	if got := up.Calls()[1].Header.Get("authorization"); got != "Bearer fixture-access-2" {
		t.Fatal(got)
	}
	if fresh, err := a.RefreshCredential(ctx, "pasted-token"); fresh != "" || err != nil || len(up.Calls()) != 2 {
		t.Fatalf("%q %v", fresh, err)
	}
	for _, ans := range []providertest.Answer{{Status: 400}, {Status: 401}, {Body: map[string]any{"access_token": "x", "shouldLogout": true}}} {
		up := providertest.Serve(t, func(providertest.Call) providertest.Answer { return ans })
		if _, err := New(Options{Origin: up.URL}).RefreshCredential(ctx, signed); provider.CodeOf(err) != "AUTH_FAILED" {
			t.Errorf("%+v: %v", ans, err)
		}
	}
}

func TestSignInBuildsAnApprovalURLAndPollsWithTheVerifier(t *testing.T) {
	answers := []providertest.Answer{{Status: 404}, {Body: map[string]any{"accessToken": "fixture-token-not-real", "refreshToken": "r"}}, {Status: 403}}
	up := providertest.Serve(t, func(providertest.Call) providertest.Answer {
		a := answers[0]
		answers = answers[1:]
		return a
	})
	a := New(Options{Origin: up.URL})
	flow, err := a.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(flow.URL)
	st := flow.State.(loginState)
	sum := sha256.Sum256([]byte(st.verifier))
	q := u.Query()
	if u.Scheme+"://"+u.Host+u.Path != "https://cursor.com/loginDeepControl" || q.Get("uuid") != st.uuid || q.Get("mode") != "login" ||
		q.Get("redirectTarget") != "sand" || q.Get("challenge") != base64.RawURLEncoding.EncodeToString(sum[:]) ||
		strings.Contains(flow.URL, st.verifier) {
		t.Fatal(flow.URL)
	}
	if cred, err := a.PollLogin(ctx, flow); cred != "" || err != nil {
		t.Fatalf("%q %v", cred, err)
	}
	poll := up.Calls()[0]
	if poll.Path != "/auth/poll" || poll.Query["uuid"][0] != st.uuid || poll.Query["verifier"][0] != st.verifier {
		t.Fatalf("%+v", poll)
	}
	if cred, err := a.PollLogin(ctx, flow); err != nil || cred != Credential("fixture-token-not-real", "r") {
		t.Fatalf("%q %v", cred, err)
	}
	if _, err := a.PollLogin(ctx, flow); provider.CodeOf(err) != "LOGIN_REFUSED" {
		t.Fatal(err)
	}
}

func TestTokens(t *testing.T) {
	if a, r := Tokens(Credential("a", "b")); a != "a" || r != "b" {
		t.Fatal(a, r)
	}
	if a, r := Tokens("{not json"); a != "{not json" || r != "" {
		t.Fatal(a, r)
	}
	if Credential("a", "") != "a" {
		t.Fatal("pasted token")
	}
}

func TestListMessagesAllowsOmittedEntriesFieldAndRejectsMalformed(t *testing.T) {
	empty := providertest.Serve(t, func(providertest.Call) providertest.Answer {
		return providertest.Answer{Body: map[string]any{}}
	})
	page, err := New(Options{Origin: empty.URL}).ListMessages(ctx, key, session, provider.ListOptions{Limit: 50})
	if err != nil || len(page.Messages) != 0 {
		t.Fatalf("empty entries: page=%+v err=%v", page, err)
	}

	malformed := providertest.Serve(t, func(providertest.Call) providertest.Answer {
		return providertest.Answer{Body: map[string]any{"entries": "bad"}}
	})
	if _, err := New(Options{Origin: malformed.URL}).ListMessages(ctx, key, session, provider.ListOptions{Limit: 50}); provider.CodeOf(err) != provider.CodeRequestFailed {
		t.Fatalf("malformed entries: %v", err)
	}
}

func TestListSessionsSkipsNotFoundSideSessionList(t *testing.T) {
	base := routes(t)
	roster := func(c providertest.Call) providertest.Answer {
		if method(c) == "ListGrokBotAgents" {
			return providertest.Answer{Body: map[string]any{
				"agents": []any{
					map[string]any{"agentId": "bot_alpha", "name": "Grok 秘书", "viewerSessionId": ""},
					map[string]any{"agentId": "bot_stale", "name": "Stale Bot", "viewerSessionId": "v1"},
				},
			}}
		}
		if method(c) == "ListGrokBotAgentSessions" && string(c.JSON(t)["agentId"].(string)) == "bot_stale" {
			return providertest.Answer{Status: http.StatusNotFound}
		}
		return base(c)
	}
	sessions, err := New(Options{Origin: providertest.Serve(t, roster).URL}).ListSessions(ctx, key)
	if err != nil {
		t.Fatalf("expected stale side-session 404 to keep roster: %v", err)
	}
	var ids []string
	for _, s := range sessions {
		ids = append(ids, s.ExternalID)
	}
	if !reflect.DeepEqual(ids, []string{"bot_alpha/", "bot_alpha/sess-side-42", "bot_stale/v1"}) {
		t.Fatalf("sessions = %v", ids)
	}

	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		errSrv := providertest.Serve(t, func(c providertest.Call) providertest.Answer {
			if method(c) == "ListGrokBotAgentSessions" {
				return providertest.Answer{Status: status}
			}
			return base(c)
		})
		if _, err := New(Options{Origin: errSrv.URL}).ListSessions(ctx, key); err == nil {
			t.Fatalf("status %d should propagate", status)
		}
	}
}
