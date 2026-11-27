package muse2api

import (
	"context"
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
	ctx     = context.Background()
	session = provider.SessionRef{ExternalID: SessionID, Ref: map[string]string{"model": DefaultModel}}
	message = provider.Outgoing{ClientID: "1c5e1c2a-1f0e-4d6b-8c3a-2e4f5a6b7c8d", Text: "写一句诗"}
)

func adapter(t *testing.T, url string, o Options) *Adapter {
	t.Helper()
	o.URL = url
	a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestOriginTakesTheServerRootOrItsV1(t *testing.T) {
	for in, want := range map[string]string{
		"http://192.0.2.7:18610":              "http://192.0.2.7:18610/v1",
		" http://192.0.2.7:18610/ ":           "http://192.0.2.7:18610/v1",
		"HTTPS://muse.example.com/v1":         "https://muse.example.com/v1",
		"https://muse.example.com/v1/":        "https://muse.example.com/v1",
		"https://example.com/muse2api":        "https://example.com/muse2api/v1",
		"http://host.persona.internal:18610/": "http://host.persona.internal:18610/v1",
	} {
		if got, err := Origin(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, in := range []string{"", "muse.example.com", "ftp://muse.example.com", "http://", "mailto:me@example.com",
		"https://user:pass@muse.example.com", "https://muse.example.com/?key=x", "https://muse.example.com/#x"} {
		if got, err := Origin(in); err == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
	if _, err := New(Options{}); err == nil {
		t.Fatal("an adapter without an address")
	}
}

func TestModel(t *testing.T) {
	if Model("") != DefaultModel || Model("bad model") != DefaultModel || Model("gpt-4o") != "gpt-4o" {
		t.Fatal(Model(""), Model("bad model"), Model("gpt-4o"))
	}
}

func TestVerifiesTheKeyAgainstTheServersModelList(t *testing.T) {
	up := providertest.Serve(t, func(c providertest.Call) providertest.Answer {
		if c.Header.Get("authorization") != "Bearer "+key {
			return providertest.Answer{Status: 401, Body: map[string]any{"detail": "API key 无效"}}
		}
		return providertest.Answer{Body: map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "muse-spark", "object": "model", "owned_by": "muse"},
			map[string]any{"id": "muse-image", "object": "model", "owned_by": "muse"},
		}}}
	})
	a := adapter(t, up.URL+"/", Options{})
	if a.Name() != Name || a.Connection() != protocol.ConnectionServer || !a.WantsHistory() || a.ModelName() != DefaultModel {
		t.Fatalf("%s %s %v %s", a.Name(), a.Connection(), a.WantsHistory(), a.ModelName())
	}
	acct, err := a.Verify(ctx, key)
	if err != nil || !reflect.DeepEqual(acct, provider.RemoteAccount{ExternalAccountID: Name}) {
		t.Fatalf("%+v %v", acct, err)
	}
	if c := up.Calls()[0]; c.Path != "/v1/models" || c.Method != "GET" {
		t.Fatalf("%+v", c)
	}
	if _, err := a.Verify(ctx, "wrong-key"); provider.CodeOf(err) != provider.CodeAuthFailed {
		t.Fatal(err)
	}
	// An address that answers something else is not a muse2api server.
	other := providertest.Serve(t, func(providertest.Call) providertest.Answer {
		return providertest.Answer{Body: map[string]any{"status": "ok"}}
	})
	if _, err := adapter(t, other.URL, Options{}).Verify(ctx, key); provider.CodeOf(err) != provider.CodeRequestFailed {
		t.Fatal(err)
	}
}

func TestAnswersWithTheRecentTurnsAndReturnsBothSides(t *testing.T) {
	up := providertest.Serve(t, func(providertest.Call) providertest.Answer {
		return providertest.Answer{Body: map[string]any{"id": "chatcmpl-1", "object": "chat.completion", "model": "muse-spark",
			"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": " 霓虹夜雨落长街。 "}}}}}
	})
	now := time.UnixMilli(1_000)
	a := adapter(t, up.URL+"/v1", Options{Model: "gpt-4o", Now: func() time.Time { return now }})
	res, err := a.Send(ctx, key, session, message, provider.SendContext{History: []provider.Turn{
		{Role: "user", Text: "你好"}, {Role: "assistant", Text: "你好！"}}})
	if err != nil {
		t.Fatal(err)
	}
	calls := up.Calls()
	if len(calls) != 1 || calls[0].Path != "/v1/chat/completions" || calls[0].Header.Get("authorization") != "Bearer "+key {
		t.Fatalf("%+v", calls)
	}
	if got := string(calls[0].Body); got != `{"messages":[{"content":"你好","role":"user"},{"content":"你好！","role":"assistant"},{"content":"写一句诗","role":"user"}],"model":"gpt-4o"}` {
		t.Fatal(got)
	}
	want := provider.SendResult{Status: provider.Sent, ExternalID: "u:" + message.ClientID, Messages: []provider.RemoteMessage{
		{ExternalID: "u:" + message.ClientID, Role: "user", Blocks: []protocol.Block{protocol.TextBlock(message.Text)}, CreateTimeMs: 1000},
		{ExternalID: "a:" + message.ClientID, Role: "assistant", Blocks: []protocol.Block{protocol.TextBlock("霓虹夜雨落长街。")}, CreateTimeMs: 1001},
	}}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("%+v", res)
	}
	sessions, _ := a.ListSessions(ctx, key)
	if len(sessions) != 1 || sessions[0].ExternalID != SessionID || !sessions[0].CanSend || sessions[0].Ref["model"] != "gpt-4o" {
		t.Fatalf("%+v", sessions)
	}
}

func TestReportsALostAnswerAsUnconfirmedAndRefusalsAsErrors(t *testing.T) {
	lost := providertest.Serve(t, func(providertest.Call) providertest.Answer { return providertest.Answer{Drop: true} })
	res, err := adapter(t, lost.URL, Options{}).Send(ctx, key, session, message, provider.SendContext{})
	if err != nil || !reflect.DeepEqual(res, provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}) {
		t.Fatalf("%+v %v", res, err)
	}
	// muse2api answers 400 when it has no Muse account left to use.
	none := providertest.Serve(t, func(providertest.Call) providertest.Answer {
		return providertest.Answer{Status: 400, Body: map[string]any{"detail": "没有可用账号，请先在管理页面导入 cookie"}}
	})
	_, err = adapter(t, none.URL, Options{}).Send(ctx, key, session, message, provider.SendContext{})
	if provider.CodeOf(err) != provider.CodeRequestFailed {
		t.Fatal(err)
	}
	if strings.Contains(err.Error(), key) {
		t.Fatal("an error carries the key")
	}
}
