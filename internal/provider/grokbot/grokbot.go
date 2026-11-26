// Package grokbot is the Grok Bot (xAI) provider. Grok Bot has no public API: this speaks the desktop client's
// Connect JSON protocol as the community SDK grok-bot-sdk (RongleCat, MIT, v0.78.1) documents it: the roster,
// sessions, transcript, transcript watch, send, send-status and agent-status RPCs only. A sign-in keeps the refresh
// token next to the access token, and a refused access token is swapped once through the desktop client's token
// endpoint; a pasted access token cannot be refreshed. A send whose answer is lost is reported as unconfirmed and
// never sent again.
package grokbot

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/web"
)

// Name is the provider's name.
const Name = "grok_bot"

// Origin is the desktop client's API.
const Origin = "https://api2.cursor.sh"

// ClientVersion is the desktop client version the protocol is spoken as.
const ClientVersion = "0.78.1"

// OAuthClientID is the desktop client's public OAuth client id, as the open-source Cursor login in can1357/oh-my-pi
// (packages/ai/src/utils/oauth/cursor.ts) sends it to the same /oauth/token endpoint the sign-in's tokens come from.
const OAuthClientID = "KbZUR41cY7W6zRSdpSUJ7I7mLYBKOCmB"

const (
	maxAttachments = 20
	maxFrame       = 1 << 20
)

var (
	botID         = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	rejectionCode = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
	userKind      = regexp.MustCompile(`(?i)user`)
)

// Options configure the adapter.
type Options struct {
	Client      *http.Client
	Origin      string
	LoginOrigin string
	Version     string
	Now         func() time.Time
}

// Adapter is the Grok Bot provider.
type Adapter struct {
	client      *http.Client
	origin      string
	loginOrigin string
	version     string
	now         func() time.Time
}

// New is the adapter.
func New(o Options) *Adapter {
	if o.Client == nil {
		o.Client, _ = web.NewClient("")
	}
	if o.Origin == "" {
		o.Origin = Origin
	}
	if o.LoginOrigin == "" {
		o.LoginOrigin = LoginOrigin
	}
	if o.Version == "" {
		o.Version = ClientVersion
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Adapter{client: o.Client, origin: strings.TrimRight(o.Origin, "/"), loginOrigin: strings.TrimRight(o.LoginOrigin, "/"),
		version: o.Version, now: o.Now}
}

var (
	_ provider.Adapter             = (*Adapter)(nil)
	_ provider.SendConfirmer       = (*Adapter)(nil)
	_ provider.LiveStater          = (*Adapter)(nil)
	_ provider.CredentialRefresher = (*Adapter)(nil)
	_ provider.Watcher             = (*Adapter)(nil)
	_ provider.DeviceLogin         = (*Adapter)(nil)
)

func (a *Adapter) Name() string       { return Name }
func (a *Adapter) Connection() string { return protocol.ConnectionServer }

// Credential is the stored credential: the access token alone when it was pasted, or both tokens as JSON when it
// came from the sign-in.
func Credential(accessToken, refreshToken string) string {
	if refreshToken == "" {
		return accessToken
	}
	b, _ := json.Marshal(struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}{accessToken, refreshToken})
	return string(b)
}

// Tokens reads a stored credential.
func Tokens(credential string) (accessToken, refreshToken string) {
	if strings.HasPrefix(credential, "{") {
		var v map[string]any
		if json.Unmarshal([]byte(credential), &v) == nil {
			if access := web.Text(v["accessToken"]); access != "" {
				return access, web.Text(v["refreshToken"])
			}
		}
	}
	return credential, ""
}

func newUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func (a *Adapter) headers(credential string) map[string]string {
	access, _ := Tokens(credential)
	return map[string]string{
		"content-type":             "application/json",
		"connect-protocol-version": "1",
		"authorization":            "Bearer " + access,
		"x-cursor-client-type":     "sand",
		"x-cursor-client-version":  a.version,
		"x-cursor-client-source":   "sand-desktop",
		"x-cursor-client-os":       "darwin",
		"x-sand-box-namespace":     "prod",
		"x-ghost-mode":             "true",
		"x-request-id":             newUUID(),
	}
}

func (a *Adapter) rpc(ctx context.Context, credential, service, method string, body any) web.Result {
	return web.Request(ctx, a.client, http.MethodPost, a.origin+"/aiserver.v1."+service+"/"+method,
		a.headers(credential), body, 0)
}

func (a *Adapter) data(ctx context.Context, credential, method string, body any) (map[string]any, error) {
	r := a.rpc(ctx, credential, "GrokBotService", method, body)
	if !r.OK {
		return nil, web.ReadError(r)
	}
	return web.Record(r.Data), nil
}

// Verify reads who the token belongs to, in one request: its alias is the email GetMe answers (grok-bot-sdk 0.78.1's
// shape: {authId, userId, email}). Pairing asks PairingAlias for a better one.
func (a *Adapter) Verify(ctx context.Context, credential string) (provider.RemoteAccount, error) {
	r := a.rpc(ctx, credential, "DashboardService", "GetMe", map[string]any{})
	if !r.OK {
		return provider.RemoteAccount{}, web.ReadError(r)
	}
	me := web.Record(r.Data)
	id := web.Text(me["authId"])
	if id == "" {
		if n, ok := me["userId"].(json.Number); ok {
			id = n.String()
		} else {
			id = web.Text(me["userId"])
		}
	}
	if id == "" {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: r.Status}
	}
	return provider.RemoteAccount{ExternalAccountID: id, Alias: web.Text(me["email"])}, nil
}

// PairingAlias is the name of the account's only bot of its own, which tells two Grok Bot accounts apart better than
// an email: the roster is asked without teammates' bots (includeTeamAgents false) and rooms are not bots. "" when the
// account has none, several, or the roster cannot be read (pairing does not fail for a name).
func (a *Adapter) PairingAlias(ctx context.Context, credential string) string {
	roster, err := a.data(ctx, credential, "ListGrokBotAgents", map[string]any{"includeTeamAgents": false})
	if err != nil {
		return ""
	}
	var names []string
	for _, bot := range web.Records(roster["agents"]) {
		if botID.MatchString(web.Text(bot["agentId"])) && !roomKinds[web.String(bot["kind"])] {
			names = append(names, or(web.Text(bot["name"]), web.Text(bot["title"])))
		}
	}
	if len(names) != 1 {
		return ""
	}
	return names[0]
}

var roomKinds = map[string]bool{"2": true, "ROOM": true, "GROK_BOT_AGENT_KIND_ROOM": true}

// ListSessions is every bot's main session (the viewer's) and its other sessions.
func (a *Adapter) ListSessions(ctx context.Context, credential string) ([]provider.RemoteSession, error) {
	roster, err := a.data(ctx, credential, "ListGrokBotAgents", map[string]any{"includeTeamAgents": true})
	if err != nil {
		return nil, err
	}
	if !web.IsArray(roster["agents"]) {
		return nil, &provider.Error{Code: "INVALID_ROSTER_RESPONSE"}
	}
	var out []provider.RemoteSession
	for _, bot := range web.Records(roster["agents"]) {
		id := web.Text(bot["agentId"])
		if !botID.MatchString(id) {
			continue
		}
		name := or(web.Text(bot["name"]), web.Text(bot["title"]), "Grok Bot")
		viewer := web.Text(bot["viewerSessionId"])
		room := roomKinds[web.String(bot["kind"])]
		avatar := ""
		if !room {
			avatar = AvatarURL(bot)
		}
		listed, err := a.data(ctx, credential, "ListGrokBotAgentSessions", map[string]any{"agentId": id})
		if err != nil {
			var pe *provider.Error
			if errors.As(err, &pe) && pe.Code == provider.CodeRequestFailed &&
				(pe.HTTPStatus == http.StatusNotFound || pe.HTTPStatus == http.StatusBadRequest) {
				listed = nil
			} else {
				return nil, err
			}
		}
		mainID := id + "/" + viewer
		seen := map[string]bool{}
		add := func(sessionID string, activity int64) {
			externalID := id + "/" + sessionID
			if seen[externalID] {
				return
			}
			seen[externalID] = true
			main := sessionID == viewer
			s := provider.RemoteSession{ExternalID: externalID, Title: name, Kind: protocol.KindMain,
				LastActivityMs: activity, CanSend: true, AvatarURL: avatar,
				Ref: map[string]string{"botId": id, "sessionId": sessionID}}
			if !main {
				s.Title, s.Kind, s.ParentExternalID = name+" · "+prefix16(sessionID, 8), protocol.KindSide, mainID
			}
			if room {
				s.Ref["room"] = "1"
			}
			out = append(out, s)
		}
		add(viewer, web.Millis(bot["updatedAtMs"]))
		for _, s := range web.Records(listed["sessions"]) {
			add(web.Text(s["sessionId"]), firstMillis(s["lastActivityAtMs"], s["updatedAtMs"], s["createdAtMs"]))
		}
	}
	return out, nil
}

func firstMillis(vs ...any) int64 {
	for _, v := range vs {
		if n := web.Millis(v); n > 0 {
			return n
		}
	}
	return 0
}

// ListMessages reads the newest transcript entries, in sequence order.
func (a *Adapter) ListMessages(ctx context.Context, credential string, s provider.SessionRef, o provider.ListOptions) (provider.RemotePage, error) {
	body, err := a.data(ctx, credential, "ListGrokBotTranscriptEntries", map[string]any{"agentId": s.Ref["botId"],
		"sessionId": s.Ref["sessionId"], "limit": min(200, max(1, o.Limit))})
	if err != nil {
		return provider.RemotePage{}, err
	}
	if raw, ok := body["entries"]; ok && raw != nil && !web.IsArray(raw) {
		return provider.RemotePage{}, &provider.Error{Code: provider.CodeRequestFailed}
	}
	var msgs []provider.RemoteMessage
	for _, e := range web.Records(body["entries"]) {
		if m, ok := Message(e); ok {
			msgs = append(msgs, m)
		}
	}
	slices.SortStableFunc(msgs, func(x, y provider.RemoteMessage) int { return cmp.Compare(x.Position, y.Position) })
	return provider.RemotePage{Messages: msgs, Raw: []any{body}}, nil
}

// Send posts the message with the client id as its message id, so the server reports a repeat as a duplicate
// rather than a second message.
func (a *Adapter) Send(ctx context.Context, credential string, s provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	bot := s.Ref["botId"]
	if !botID.MatchString(bot) {
		return provider.SendResult{}, &provider.Error{Code: "BOT_ID_REQUIRED"}
	}
	r := a.rpc(ctx, credential, "GrokBotService", "SendGrokBotUserMessage", map[string]any{
		"agentId": bot, "text": msg.Text, "sessionId": s.Ref["sessionId"], "messageId": msg.ClientID,
		"attachmentPaths": []string{}, "attachmentNames": []string{},
		"sentAtMs": strconv.FormatInt(a.now().UnixMilli(), 10), "isFork": false, "source": 1, "initiator": 1,
		"sendSource": 1,
	})
	if !r.OK {
		switch r.Status {
		case http.StatusUnauthorized, http.StatusForbidden:
			return provider.SendResult{}, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: r.Status}
		case http.StatusTooManyRequests:
			// A rate limiter's refusal: the message was not taken (and its message id makes a later try safe).
			return provider.SendResult{}, web.ReadError(r)
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: "SEND_UNCONFIRMED"}, nil
	}
	if !web.Is(web.Record(r.Data)["delivery"], 1, 2, 3, "GROK_BOT_USER_MESSAGE_DELIVERY_ACCEPTED_BOX",
		"GROK_BOT_USER_MESSAGE_DELIVERY_ACCEPTED_TEMPORAL", "GROK_BOT_USER_MESSAGE_DELIVERY_DUPLICATE") {
		return provider.SendResult{}, &provider.Error{Code: "SEND_REFUSED", HTTPStatus: r.Status}
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: msg.ClientID}, nil
}

// ConfirmSend reads an unconfirmed send's status by its message id. Not found, pending and unknown durability can
// still turn up later, so they are unknown.
func (a *Adapter) ConfirmSend(ctx context.Context, credential string, s provider.SessionRef, clientID string) (provider.Confirmation, error) {
	bot := s.Ref["botId"]
	if !botID.MatchString(bot) {
		return provider.Confirmation{Status: provider.ConfirmUnknown}, nil
	}
	st, err := a.data(ctx, credential, "GetGrokBotSendStatus", map[string]any{"agentId": bot, "messageId": clientID,
		"sessionId": s.Ref["sessionId"]})
	if err != nil {
		return provider.Confirmation{}, err
	}
	switch {
	case web.Is(st["status"], 2, "GROK_BOT_SEND_STATUS_ACCEPTED"):
		return provider.Confirmation{Status: provider.ConfirmSent}, nil
	case web.Is(st["status"], 3, "GROK_BOT_SEND_STATUS_REJECTED"):
		code := web.Text(st["rejectionCode"])
		if !rejectionCode.MatchString(code) {
			code = "SEND_REJECTED"
		}
		return provider.Confirmation{Status: provider.ConfirmFailed, Error: code}, nil
	}
	return provider.Confirmation{Status: provider.ConfirmUnknown}, nil
}

// LiveState reads whether the bot is working in this session. With a live record only this session's run counts;
// without one the agent-wide flag is all there is.
func (a *Adapter) LiveState(ctx context.Context, credential string, s provider.SessionRef) (provider.LiveState, error) {
	bot := s.Ref["botId"]
	if !botID.MatchString(bot) {
		return provider.LiveState{}, nil
	}
	body, err := a.data(ctx, credential, "ListGrokBotAgentStatuses", map[string]any{"agentIds": []string{bot}})
	if err != nil {
		return provider.LiveState{}, err
	}
	var row map[string]any
	for _, st := range web.Records(body["statuses"]) {
		if web.Text(st["agentId"]) == bot {
			row = st
			break
		}
	}
	live := web.Record(row["live"])
	if len(live) == 0 {
		return provider.LiveState{Running: row["isRunning"] == true}, nil
	}
	here := web.Text(live["sessionId"]) == s.Ref["sessionId"]
	return provider.LiveState{Running: here && live["isRunning"] == true, Composing: here && live["isComposingMessage"] == true}, nil
}

// RefreshCredential swaps a signed-in credential's access token through the token endpoint; a pasted token cannot
// be refreshed. The endpoint may not rotate the refresh token; the stored one stays.
func (a *Adapter) RefreshCredential(ctx context.Context, credential string) (string, error) {
	_, refresh := Tokens(credential)
	if refresh == "" {
		return "", nil
	}
	r := web.Request(ctx, a.client, http.MethodPost, a.origin+"/oauth/token", map[string]string{"content-type": "application/json"},
		map[string]any{"grant_type": "refresh_token", "client_id": OAuthClientID, "refresh_token": refresh}, 0)
	if !r.OK {
		if r.Status == http.StatusBadRequest {
			return "", &provider.Error{Code: provider.CodeAuthFailed}
		}
		return "", web.ReadError(r)
	}
	body := web.Record(r.Data)
	access := web.Text(body["access_token"])
	if body["shouldLogout"] == true || access == "" {
		return "", &provider.Error{Code: provider.CodeAuthFailed}
	}
	return Credential(access, or(web.Text(body["refresh_token"]), refresh)), nil
}

func or(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// prefix16 is the first n UTF-16 code units of s, as JavaScript's slice counts them.
func prefix16(s string, n int) string {
	u := utf16.Encode([]rune(s))
	if len(u) <= n {
		return s
	}
	return string(utf16.Decode(u[:n]))
}

func decodeBody(v any) map[string]any {
	s, ok := v.(string)
	if !ok || s == "" {
		return nil
	}
	var raw []byte
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil {
			raw = b
			break
		}
	}
	if raw == nil || !utf8.Valid(raw) {
		return nil
	}
	var body any
	if json.Unmarshal(raw, &body) != nil {
		return nil
	}
	m, _ := body.(map[string]any)
	return m
}

func bodyText(body map[string]any) string {
	message := web.Record(body["message"])
	return or(web.Text(message["content"]), web.Text(body["text"]), web.Text(body["content"]), web.Text(message["text"]))
}

// attachmentBlocks are the files an entry names, by name and type only: the paths are references into the bot's
// storage that club never fetches.
func attachmentBlocks(body map[string]any) []protocol.Block {
	message := web.Record(body["message"])
	type file struct{ name, kind string }
	var files []file
	for _, n := range append(web.Strings(body["attachmentNames"]), web.Strings(message["attachmentNames"])...) {
		files = append(files, file{name: n})
	}
	for _, item := range append(web.Records(body["attachments"]), web.Records(message["attachments"])...) {
		files = append(files, file{name: or(web.Text(item["name"]), web.Text(item["fileName"])),
			kind: or(web.Text(item["contentType"]), web.Text(item["mimeType"]))})
	}
	var out []protocol.Block
	for _, f := range files {
		if strings.TrimSpace(f.name) == "" {
			continue
		}
		if len(out) == maxAttachments {
			break
		}
		out = append(out, protocol.Block{Type: "attachment", Name: prefix16(f.name, 200), Kind: prefix16(f.kind, 100)})
	}
	return out
}

func seqOf(v any) float64 {
	switch x := v.(type) {
	case string:
		if strings.TrimSpace(x) == "" {
			return 0
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		if err != nil {
			return math.NaN()
		}
		return f
	case json.Number:
		f, _ := x.Float64()
		return f
	case float64:
		return x
	}
	return math.NaN()
}

// Message is one transcript entry. A text send-message is the bot's reply, as the SDK reads it; a user-sent entry
// carries "user" in its kind or role. Every other kind stays visible as a folded step. ok is false for an entry
// without id or sequence.
func Message(entry map[string]any) (provider.RemoteMessage, bool) {
	seq := seqOf(entry["seq"])
	finite := !math.IsNaN(seq) && !math.IsInf(seq, 0)
	id := web.Text(entry["entryId"])
	if id == "" && finite {
		id = "seq-" + strconv.FormatFloat(seq, 'f', -1, 64)
	}
	if id == "" {
		return provider.RemoteMessage{}, false
	}
	body := decodeBody(entry["body"])
	kind := web.Text(entry["entryKind"])
	if kind == "" && body != nil {
		kind = web.Text(body["kind"])
	}
	m := provider.RemoteMessage{ExternalID: id}
	if finite {
		m.Position = int64(seq)
	}
	if body == nil {
		m.Role = protocol.RoleSystem
		m.Blocks = []protocol.Block{{Type: "status", Status: "unavailable", Text: kind}}
		return m, true
	}
	m.CreateTimeMs = firstMillis(body["sentAtMs"], body["createdAtMs"], body["timestampMs"])
	content := bodyText(body)
	files := attachmentBlocks(body)
	switch {
	case userKind.MatchString(kind) || body["role"] == "user":
		m.Role = protocol.RoleUser
		if content != "" {
			m.Blocks = append(m.Blocks, protocol.TextBlock(content))
		}
		m.Blocks = append(m.Blocks, files...)
		if m.Blocks == nil {
			m.Blocks = []protocol.Block{}
		}
	case kind == "send-message" && web.Record(body["message"])["type"] == "text":
		m.Role = protocol.RoleAssistant
		m.Blocks = append([]protocol.Block{protocol.TextBlock(content)}, files...)
	default:
		m.Role = protocol.RoleTool
		m.Blocks = []protocol.Block{{Type: "tool-call", Name: or(kind, "entry"), Summary: content}}
	}
	return m, true
}

func generationOf(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case json.Number:
		return x.String()
	}
	return "0"
}

func newestSeq(entries any) string {
	newest := new(big.Int)
	for _, e := range web.Records(entries) {
		raw := e["updatedSeq"]
		if raw == nil {
			raw = e["seq"]
		}
		var s string
		switch x := raw.(type) {
		case string:
			s = strings.TrimSpace(x)
		case json.Number:
			s = x.String()
		default:
			continue
		}
		if n, ok := new(big.Int).SetString(s, 10); ok && n.Cmp(newest) > 0 {
			newest = n
		}
	}
	return newest.String()
}
