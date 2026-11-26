// Package groq is Groq (groq.com) as a personal chat bot. Groq is a stateless, OpenAI-compatible inference API: it
// keeps no conversation, so club stores the transcript and sends the latest turns with each message. The API key is
// the owner's own and is only sent to Groq.
package groq

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/web"
)

// Name is the provider's name.
const Name = "groq"

// Origin is Groq's OpenAI-compatible API.
const Origin = "https://api.groq.com/openai/v1"

// DefaultModel is a production model in Groq's list.
const DefaultModel = "openai/gpt-oss-120b"

// SessionID is the one session a Groq account has.
const SessionID = "chat"

const (
	historyTurns  = 20
	historyChars  = 24_000
	answerTimeout = 60 * time.Second
)

var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,127}$`)

// Model is v when it is a model id, else DefaultModel.
func Model(v string) string {
	v = strings.TrimSpace(v)
	if modelID.MatchString(v) {
		return v
	}
	return DefaultModel
}

// History is the newest turns that fit the budget (20 turns, 24k characters), oldest first.
func History(turns []provider.Turn) []provider.Turn {
	var kept []provider.Turn
	chars := 0
	for i := len(turns) - 1; i >= 0; i-- {
		t := turns[i]
		if strings.TrimSpace(t.Text) == "" {
			continue
		}
		n := len(utf16.Encode([]rune(t.Text)))
		if len(kept) >= historyTurns || chars+n > historyChars {
			break
		}
		kept = append([]provider.Turn{t}, kept...)
		chars += n
	}
	return kept
}

// Options configure the adapter.
type Options struct {
	Client *http.Client
	Origin string
	// Model is the model asked; "" or an invalid id is DefaultModel.
	Model string
	// Timeout is how long an answer may take; 0 is 60 s.
	Timeout time.Duration
	Now     func() time.Time
}

// Adapter is the Groq provider.
type Adapter struct {
	client  *http.Client
	origin  string
	model   string
	timeout time.Duration
	now     func() time.Time
}

// New is the adapter.
func New(o Options) *Adapter {
	if o.Client == nil {
		o.Client, _ = web.NewClient("")
	}
	if o.Origin == "" {
		o.Origin = Origin
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Timeout <= 0 {
		o.Timeout = answerTimeout
	}
	return &Adapter{client: o.Client, origin: strings.TrimRight(o.Origin, "/"), model: Model(o.Model), timeout: o.Timeout,
		now: o.Now}
}

var (
	_ provider.Adapter       = (*Adapter)(nil)
	_ provider.HistoryWanter = (*Adapter)(nil)
	_ provider.ToolCaller    = (*Adapter)(nil)
)

func (a *Adapter) Name() string       { return Name }
func (a *Adapter) Connection() string { return protocol.ConnectionServer }
func (a *Adapter) WantsHistory() bool { return true }

// ModelName is the model the adapter asks.
func (a *Adapter) ModelName() string { return a.model }

func headers(key string) map[string]string {
	return map[string]string{"content-type": "application/json", "authorization": "Bearer " + key}
}

// Verify checks the key against the model list. Groq names no account, so any key of the owner's can replace the
// old one when it is rotated.
func (a *Adapter) Verify(ctx context.Context, key string) (provider.RemoteAccount, error) {
	r := web.Request(ctx, a.client, http.MethodGet, a.origin+"/models", headers(key), nil, 0)
	if !r.OK {
		return provider.RemoteAccount{}, web.ReadError(r)
	}
	for _, row := range web.Records(web.Record(r.Data)["data"]) {
		if web.Text(row["id"]) == a.model {
			return provider.RemoteAccount{ExternalAccountID: "groq", Quota: Quota(r.Header, a.now())}, nil
		}
	}
	return provider.RemoteAccount{}, &provider.Error{Code: "MODEL_UNAVAILABLE"}
}

// ListSessions is the one chat.
func (a *Adapter) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: SessionID, Title: "Groq", Kind: protocol.KindMain, CanSend: true,
		Ref: map[string]string{"model": a.model}}}, nil
}

// ListMessages has nothing to read: the store keeps the transcript.
func (a *Adapter) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}

// Send asks for an answer with the recent turns and returns both sides of the exchange.
func (a *Adapter) Send(ctx context.Context, key string, _ provider.SessionRef, msg provider.Outgoing, sc provider.SendContext) (provider.SendResult, error) {
	sentAt := a.now().UnixMilli()
	turns := History(append(append([]provider.Turn(nil), sc.History...), provider.Turn{Role: protocol.RoleUser, Text: msg.Text}))
	messages := make([]map[string]string, len(turns))
	for i, t := range turns {
		messages[i] = map[string]string{"role": t.Role, "content": t.Text}
	}
	r := web.Request(ctx, a.client, http.MethodPost, a.origin+"/chat/completions", headers(key),
		map[string]any{"model": a.model, "messages": messages}, a.timeout)
	if !r.OK && r.Transport {
		return provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, nil
	}
	if !r.OK {
		return provider.SendResult{}, web.ReadError(r)
	}
	var choice map[string]any
	if c := web.Records(web.Record(r.Data)["choices"]); len(c) > 0 {
		choice = c[0]
	}
	answer := strings.TrimSpace(web.Text(web.Record(choice["message"])["content"]))
	answeredAt := max(a.now().UnixMilli(), sentAt+1)
	reply := provider.RemoteMessage{ExternalID: "a:" + msg.ClientID, Role: protocol.RoleAssistant,
		Blocks: []protocol.Block{protocol.TextBlock(answer)}, CreateTimeMs: answeredAt}
	if answer == "" {
		reply.Role, reply.Blocks = protocol.RoleSystem, []protocol.Block{{Type: "status", Status: "empty_answer"}}
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: "u:" + msg.ClientID, Messages: []provider.RemoteMessage{
		{ExternalID: "u:" + msg.ClientID, Role: protocol.RoleUser, Blocks: []protocol.Block{protocol.TextBlock(msg.Text)},
			CreateTimeMs: sentAt},
		reply,
	}, Quota: Quota(r.Header, a.now())}, nil
}

// CallTools runs one OpenAI-compatible chat completion step with function calling tools.
func (a *Adapter) CallTools(ctx context.Context, key string, turns []provider.ToolTurn, tools []provider.ToolDef) (provider.ToolStepResult, error) {
	messages := make([]map[string]any, 0, len(turns))
	for _, t := range turns {
		m := map[string]any{"role": t.Role}
		switch t.Role {
		case protocol.RoleTool:
			m["tool_call_id"] = t.ToolCallID
			if t.ToolName != "" {
				m["name"] = t.ToolName
			}
			m["content"] = t.Content
		case protocol.RoleAssistant:
			m["content"] = t.Content
			if len(t.ToolCalls) > 0 {
				tcs := make([]map[string]any, 0, len(t.ToolCalls))
				for _, tc := range t.ToolCalls {
					args := strings.TrimSpace(string(tc.Args))
					if args == "" {
						args = "{}"
					}
					tcs = append(tcs, map[string]any{
						"id":   tc.ID,
						"type": "function",
						"function": map[string]any{
							"name":      tc.Name,
							"arguments": args,
						},
					})
				}
				m["tool_calls"] = tcs
			}
		default:
			m["content"] = t.Content
		}
		messages = append(messages, m)
	}
	payload := map[string]any{
		"model":    a.model,
		"messages": messages,
	}
	if len(tools) > 0 {
		encoded := make([]map[string]any, 0, len(tools))
		for _, td := range tools {
			schema := bytes.TrimSpace(td.InputSchema)
			if len(schema) == 0 || !json.Valid(schema) {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			encoded = append(encoded, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        td.Name,
					"description": td.Description,
					"parameters":  schema,
				},
			})
		}
		payload["tools"] = encoded
	}
	r := web.Request(ctx, a.client, http.MethodPost, a.origin+"/chat/completions", headers(key), payload, a.timeout)
	if !r.OK {
		return provider.ToolStepResult{}, web.ReadError(r)
	}
	var choice map[string]any
	if c := web.Records(web.Record(r.Data)["choices"]); len(c) > 0 {
		choice = c[0]
	}
	msg := web.Record(choice["message"])
	var toolCalls []provider.ToolCall
	for i, rawTC := range web.Records(msg["tool_calls"]) {
		fn := web.Record(rawTC["function"])
		name := strings.TrimSpace(web.Text(fn["name"]))
		if name == "" {
			continue
		}
		id := strings.TrimSpace(web.Text(rawTC["id"]))
		if id == "" {
			id = "call_" + strconv.Itoa(i+1)
		}
		argsStr := strings.TrimSpace(web.Text(fn["arguments"]))
		var args json.RawMessage
		if argsStr != "" && json.Valid([]byte(argsStr)) {
			args = json.RawMessage(argsStr)
		} else {
			args = json.RawMessage(`{}`)
		}
		toolCalls = append(toolCalls, provider.ToolCall{ID: id, Name: name, Args: args})
	}
	return provider.ToolStepResult{
		Content:   strings.TrimSpace(web.Text(msg["content"])),
		ToolCalls: toolCalls,
		Quota:     Quota(r.Header, a.now()),
	}, nil
}

// Quota reads Groq's rate limit headers: requests per day and tokens per minute, each with its limit, what is left
// and when it resets. A dimension whose headers are missing or unreadable is left out.
func Quota(h http.Header, now time.Time) []provider.QuotaReading {
	var out []provider.QuotaReading
	for _, d := range []struct{ suffix, kind, window string }{
		{"requests", protocol.QuotaKindRequests, "day"},
		{"tokens", protocol.QuotaKindTokens, "minute"},
	} {
		limit, err1 := strconv.ParseFloat(h.Get("x-ratelimit-limit-"+d.suffix), 64)
		left, err2 := strconv.ParseFloat(h.Get("x-ratelimit-remaining-"+d.suffix), 64)
		if err1 != nil || err2 != nil || limit <= 0 || left < 0 {
			continue
		}
		q := provider.QuotaReading{Kind: d.kind, Limit: limit, Remaining: left, Window: d.window}
		if reset, err := time.ParseDuration(h.Get("x-ratelimit-reset-" + d.suffix)); err == nil && reset >= 0 {
			q.ResetsAt = now.Add(reset).UnixMilli()
		}
		out = append(out, q)
	}
	return out
}
