package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/nwverse/ensemble/internal/protocol"
)

const (
	pageTextBytes      = 24 * 1024
	pageOutputBytes    = 4 * 1024
	defaultPageLimit   = 80
	maxPageLimit       = 500
	maxTurnsLimit      = 10
	defaultBootstrapB  = 8192
	maxToolRelayBytes  = 2048
	sessionPrefix      = "pac-asst-"
	defaultHistoryName = "pac-v1"
)

var (
	runtimeIDRe        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	launcherNameRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	uuidShapeRe        = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
	personaContextRe   = regexp.MustCompile(`(?is)<persona-context>\s*.*?\s*</persona-context>`)
	personaToolsRe     = regexp.MustCompile(`(?is)<persona-tools>\s*.*?\s*</persona-tools>`)
	personaToolCallRe  = regexp.MustCompile(`(?is)<persona-tool-call>\s*(\{.*?\})\s*</persona-tool-call>`)
	badLauncherCharsRe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)
)

// RuntimeCaps declares runtime capabilities on runtime.hello.
type RuntimeCaps struct {
	Start             bool `json:"start"`
	Send              bool `json:"send"`
	Interrupt         bool `json:"interrupt"`
	Close             bool `json:"close"`
	Assistant         bool `json:"assistant"`
	RemoteAgent       bool `json:"remote_agent"`
	SupervisionPolicy bool `json:"supervision_policy"`
}

// Launcher describes a startable assistant launcher in runtime.hello / runtime.launchers.
type Launcher struct {
	Name        string `json:"name"`
	Model       string `json:"model,omitempty"`
	Agent       string `json:"agent,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// SessionRow is one row pushed in runtime.sessions or answered by session.start.
type SessionRow struct {
	SessionID         string     `json:"session_id"`
	Title             string     `json:"title"`
	Cwd               string     `json:"cwd"`
	Status            string     `json:"status"`
	Agent             string     `json:"agent,omitempty"`
	Launcher          string     `json:"launcher,omitempty"`
	Model             string     `json:"model,omitempty"`
	UpdatedAt         time.Time  `json:"updated_at"`
	Rev               int64      `json:"rev,omitempty"`
	LastText          string     `json:"last_text,omitempty"`
	SpaceID           string     `json:"space_id,omitempty"`
	SpaceName         string     `json:"space_name,omitempty"`
	TurnError         string     `json:"turn_error,omitempty"`
	TurnErrorKind     string     `json:"turn_error_kind,omitempty"`
	TurnErrorCode     string     `json:"turn_error_code,omitempty"`
	TurnErrorResetsAt *time.Time `json:"turn_error_resets_at,omitempty"`
}

// Item is one transcript entry matching Persona's proto.Item.
type Item struct {
	ID          string          `json:"id"`
	Seq         int             `json:"seq"`
	TS          time.Time       `json:"ts"`
	Role        string          `json:"role"`
	Kind        string          `json:"kind"`
	Text        string          `json:"text,omitempty"`
	ToolUseID   string          `json:"tool_use_id,omitempty"`
	ToolName    string          `json:"tool_name,omitempty"`
	ToolInput   json.RawMessage `json:"tool_input,omitempty"`
	IsError     bool            `json:"is_error,omitempty"`
	ClientMsgID string          `json:"client_msg_id,omitempty"`
	FullSize    int             `json:"full_size,omitempty"`
	Model       string          `json:"model,omitempty"`
}

// UsageWindow matches Persona's proto.UsageWindow. UsedPercent must omit `omitempty` so 0 is serialized.
type UsageWindow struct {
	Label         string     `json:"label"`
	WindowMinutes int        `json:"window_minutes"`
	UsedPercent   float64    `json:"used_percent"`
	ResetsAt      *time.Time `json:"resets_at,omitempty"`
	Model         string     `json:"model,omitempty"`
}

// UsageAccountEvent matches Persona's usage.windows event payload.
type UsageAccountEvent struct {
	Provider  string        `json:"provider"`
	Name      string        `json:"name"`
	Plan      string        `json:"plan,omitempty"`
	SampledAt time.Time     `json:"sampled_at"`
	Windows   []UsageWindow `json:"windows"`
}

// MCPTool is one tool entry returned by Persona's mcp.tools RPC.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Shell       string          `json:"shell,omitempty"`
}

// MCPContent is one block inside MCPCallResult.
type MCPContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// MCPCallResult matches Persona's proto.ToolResult returned by mcp.call.
type MCPCallResult struct {
	Content []MCPContent `json:"content"`
	IsError bool         `json:"is_error,omitempty"`
}

// ItemsRequest decodes Persona's session.items call parameters.
type ItemsRequest struct {
	SessionID  string `json:"session_id"`
	BeforeSeq  int    `json:"before_seq,omitempty"`
	AfterSeq   *int   `json:"after_seq,omitempty"`
	FromSeq    int    `json:"from_seq,omitempty"`
	ThroughSeq int    `json:"through_seq,omitempty"`
	Turns      int    `json:"turns,omitempty"`
	Limit      int    `json:"limit,omitempty"`
}

// ItemsResult matches Persona's session.items response shape.
type ItemsResult struct {
	Items     []Item `json:"items"`
	HasMore   bool   `json:"has_more,omitempty"`
	MoreAfter bool   `json:"more_after,omitempty"`
	More      bool   `json:"more,omitempty"`
	History   string `json:"history,omitempty"`
	FromSeq   *int   `json:"from_seq,omitempty"`
	ToSeq     int    `json:"to_seq,omitempty"`
}

// StripPersonaContext removes attribute-less <persona-context> and <persona-tools> wrapper blocks from text.
func StripPersonaContext(s string) string {
	out := personaContextRe.ReplaceAllString(s, "")
	out = personaToolsRe.ReplaceAllString(out, "")
	return strings.TrimSpace(out)
}

// CapBootstrap keeps inline first-turn instructions <= maxBytes (default 8192 bytes) cleanly on rune boundaries.
func CapBootstrap(s string, maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = defaultBootstrapB
	}
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return s
	}
	const notice = "\n[truncated]"
	cut := maxBytes - len(notice)
	if cut <= 0 {
		return clipUTF8Bytes(s, maxBytes)
	}
	head := clipUTF8Bytes(s, cut)
	if nl := strings.LastIndexByte(head, '\n'); nl > cut/2 {
		head = head[:nl]
	}
	return strings.TrimRight(head, " \t\n") + notice
}

func clipUTF8Bytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// FitMaxText combines prefix and body so the combined UTF-16 code unit count never exceeds protocol.MaxText.
func FitMaxText(prefix, body string) string {
	prefix = strings.TrimSpace(prefix)
	body = strings.TrimSpace(body)
	if prefix == "" {
		return prefix16Units(body, protocol.MaxText)
	}
	if body == "" {
		return prefix16Units(prefix, protocol.MaxText)
	}
	combined := prefix + "\n\n" + body
	if len16(combined) <= protocol.MaxText {
		return combined
	}
	bodyLen := len16(body)
	const sep = 2 // "\n\n"
	if bodyLen+sep >= protocol.MaxText {
		return prefix16Units(body, protocol.MaxText)
	}
	avail := protocol.MaxText - bodyLen - sep
	trimmed := strings.TrimSpace(prefix16Units(prefix, avail))
	if trimmed == "" {
		return body
	}
	return trimmed + "\n\n" + body
}

func len16(s string) int { return len(utf16.Encode([]rune(s))) }

func prefix16Units(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	if len(s) <= n {
		return s
	}
	u := utf16.Encode([]rune(s))
	if len(u) <= n {
		return s
	}
	// Avoid splitting a surrogate pair right at boundary n.
	if n > 0 && u[n-1] >= 0xD800 && u[n-1] <= 0xDBFF {
		n--
	}
	return string(utf16.Decode(u[:n]))
}

// lightItem clips oversized item text for list/push responses while recording full_size.
func lightItem(it Item) Item {
	limit := pageTextBytes
	if it.Kind == "tool_result" {
		limit = pageOutputBytes
	}
	if len(it.Text) <= limit {
		return it
	}
	full := len(it.Text)
	head := clipUTF8Bytes(it.Text, limit)
	if nl := strings.LastIndexByte(head, '\n'); nl > 0 && len(head)-nl < 512 {
		head = head[:nl]
	}
	it.FullSize = full
	it.Text = head + fmt.Sprintf("\n…（已截断，共 %d 字节）", full)
	return it
}

func applyLight(items []Item) []Item {
	if len(items) == 0 {
		return []Item{}
	}
	out := make([]Item, len(items))
	for i, it := range items {
		out[i] = lightItem(it)
	}
	return out
}

// PageItems slices all (sorted by Seq ascending) according to Persona's session.items paging rules.
func PageItems(all []Item, req ItemsRequest, history string) ItemsResult {
	toSeq := 0
	if len(all) > 0 {
		toSeq = all[len(all)-1].Seq
	}
	if req.ThroughSeq > 0 {
		from := req.FromSeq
		if from <= 0 {
			from = 1
		}
		var out []Item
		for _, it := range all {
			if it.Seq >= from && it.Seq <= req.ThroughSeq {
				out = append(out, it)
			}
		}
		return ItemsResult{Items: applyLight(out), More: false}
	}
	if req.AfterSeq != nil {
		afterSeq := *req.AfterSeq
		var after []Item
		for _, it := range all {
			if it.Seq > afterSeq {
				after = append(after, it)
			}
		}
		out := after
		to := toSeq
		cut := false
		if len(after) > maxPageLimit {
			n := maxPageLimit
			for n < len(after) && after[n].Seq == after[n-1].Seq {
				n++
			}
			out = after[:n]
			cut = n < len(after)
			if cut && len(out) > 0 {
				to = out[len(out)-1].Seq
			}
		}
		return ItemsResult{
			Items:     applyLight(out),
			HasMore:   false,
			MoreAfter: cut,
			History:   history,
			FromSeq:   &afterSeq,
			ToSeq:     to,
		}
	}

	before := all
	if req.BeforeSeq > 0 {
		end := 0
		for end < len(all) && all[end].Seq < req.BeforeSeq {
			end++
		}
		before = all[:end]
	}

	var start int
	if req.Turns > 0 {
		wantTurns := min(req.Turns, maxTurnsLimit)
		turns := 0
		start = 0
		for i := len(before) - 1; i >= 0; i-- {
			it := before[i]
			if it.Role == "user" && (it.Kind == "text" || it.Kind == "image") {
				if i > 0 && before[i-1].Role == "user" && before[i-1].Seq == it.Seq {
					continue
				}
				turns++
				if turns == wantTurns {
					start = i
					break
				}
			}
		}
	} else {
		limit := req.Limit
		if limit <= 0 {
			limit = defaultPageLimit
		}
		limit = min(limit, maxPageLimit)
		start = max(0, len(before)-limit)
		for start > 0 && before[start].Seq == before[start-1].Seq {
			start--
		}
	}

	out := before[start:]
	outToSeq := toSeq
	if req.BeforeSeq > 0 {
		if len(out) > 0 {
			outToSeq = out[len(out)-1].Seq
		} else {
			outToSeq = 0
		}
	}
	return ItemsResult{
		Items:   applyLight(out),
		HasMore: start > 0,
		History: history,
		ToSeq:   outToSeq,
	}
}

// ParseToolCall extracts one structured <persona-tool-call>{"name":...,"args":...}</persona-tool-call> invocation
// from a chat-only assistant message.
func ParseToolCall(text string) (name string, args json.RawMessage, ok bool) {
	m := personaToolCallRe.FindStringSubmatch(text)
	if len(m) < 2 {
		return "", nil, false
	}
	var payload struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal([]byte(m[1]), &payload); err != nil {
		return "", nil, false
	}
	name = strings.TrimSpace(payload.Name)
	if name == "" {
		return "", nil, false
	}
	rawArgs := strings.TrimSpace(string(payload.Args))
	if rawArgs == "" || rawArgs == "null" || !json.Valid([]byte(rawArgs)) {
		rawArgs = "{}"
	}
	return name, json.RawMessage(rawArgs), true
}

// FormatToolCatalog formats Persona MCP tools into a compact instruction block suited for chat-only providers.
func FormatToolCatalog(tools []MCPTool, maxBytes int) string {
	if len(tools) == 0 {
		return ""
	}
	if maxBytes <= 0 {
		maxBytes = maxToolRelayBytes
	}
	var b strings.Builder
	b.WriteString("<persona-tools>\n")
	b.WriteString("Available Persona tools (call ONE at a time by replying ONLY with <persona-tool-call>{\"name\":\"TOOL\",\"args\":{...}}</persona-tool-call>):\n")
	const footer = "</persona-tools>"
	listed := 0
	for _, t := range tools {
		desc := strings.Join(strings.Fields(t.Description), " ")
		if len([]rune(desc)) > 72 {
			desc = string([]rune(desc)[:72]) + "…"
		}
		line := "- " + t.Name
		if desc != "" {
			line += ": " + desc
		}
		line += "\n"
		if b.Len()+len(line)+len(footer)+40 > maxBytes {
			break
		}
		b.WriteString(line)
		listed++
	}
	if listed < len(tools) {
		fmt.Fprintf(&b, "(+%d more omitted)\n", len(tools)-listed)
	}
	b.WriteString(footer)
	return b.String()
}

// LauncherName builds a Persona shellcmd-safe launcher name from provider and accountID.
func LauncherName(prov, accountID string) string {
	raw := "club-" + strings.TrimSpace(prov) + "-" + strings.TrimSpace(accountID)
	clean := strings.Trim(badLauncherCharsRe.ReplaceAllString(raw, "-"), "-")
	if clean == "" {
		return "club"
	}
	if !launcherNameRe.MatchString(clean) {
		clean = "club-" + clean
	}
	return clean
}

// SanitizeLauncher checks and cleans an explicit launcher override into a valid Persona launcher name.
func SanitizeLauncher(name string) string {
	clean := strings.TrimSpace(name)
	if launcherNameRe.MatchString(clean) {
		return clean
	}
	clean = strings.Trim(badLauncherCharsRe.ReplaceAllString(clean, "-"), "-")
	if clean == "" || !launcherNameRe.MatchString(clean) {
		return ""
	}
	return clean
}

// ValidRuntimeSessionID says whether id is accepted by Persona node as a runtime session ID (non-empty, matches
// runtimeIDRe, and NOT a bare UUID).
func ValidRuntimeSessionID(id string) bool {
	return id != "" && !uuidShapeRe.MatchString(id) && runtimeIDRe.MatchString(id)
}

// DeterministicUUID builds a valid UUIDv4-formatted ID from an arbitrary seed string for club outbox idempotency.
func DeterministicUUID(seed string) string {
	sum := sha256.Sum256([]byte("pac-runtime:" + seed))
	b := sum[:16]
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// QuotaToUsageWindows converts a club protocol.Quota snapshot into Persona usage.windows entries.
func QuotaToUsageWindows(prov, name string, q *protocol.Quota, now time.Time) (UsageAccountEvent, bool) {
	if q == nil {
		return UsageAccountEvent{}, false
	}
	if strings.TrimSpace(prov) == "" {
		prov = "club"
	}
	if strings.TrimSpace(name) == "" {
		name = prov
	}
	all := append([]protocol.Quota{*q}, q.Details...)
	var windows []UsageWindow
	for _, item := range all {
		var used float64
		switch {
		case item.State == protocol.QuotaExhausted:
			used = 100
		case item.Remaining != nil:
			used = math.Max(0, math.Min(100, (1-*item.Remaining)*100))
		default:
			continue
		}
		used = math.Round(used*10) / 10
		mins := windowMinutesOf(item.Label, item.Kind)
		w := UsageWindow{
			Label:         windowLabel(mins),
			WindowMinutes: mins,
			UsedPercent:   used,
		}
		if item.ResetsAt > 0 {
			t := time.UnixMilli(item.ResetsAt).UTC()
			if !t.After(now) {
				continue
			}
			w.ResetsAt = &t
		}
		windows = append(windows, w)
		if len(windows) >= 32 {
			break
		}
	}
	if len(windows) == 0 {
		return UsageAccountEvent{}, false
	}
	sampled := now.UTC()
	if q.CheckedAt > 0 {
		t := time.UnixMilli(q.CheckedAt).UTC()
		if now.Sub(t) < 2*time.Hour && !t.After(now.Add(time.Minute)) {
			sampled = t
		}
	}
	return UsageAccountEvent{
		Provider:  prov,
		Name:      name,
		SampledAt: sampled,
		Windows:   windows,
	}, true
}

func windowMinutesOf(label, kind string) int {
	lower := strings.ToLower(label)
	switch {
	case strings.Contains(lower, "minute"), strings.Contains(lower, "min"):
		return 1
	case strings.Contains(lower, "day"), strings.Contains(lower, "24h"):
		return 1440
	case strings.Contains(lower, "week"), strings.Contains(lower, "7d"):
		return 10080
	case strings.Contains(lower, "5h"):
		return 300
	case strings.Contains(lower, "hour"), kind == protocol.QuotaKindWindow:
		return 60
	default:
		return 60
	}
}

func windowLabel(minutes int) string {
	switch {
	case minutes <= 0:
		return "1h"
	case minutes%(24*60) == 0:
		return strconv.Itoa(minutes/(24*60)) + "d"
	case minutes%60 == 0:
		return strconv.Itoa(minutes/60) + "h"
	default:
		return strconv.Itoa(minutes) + "m"
	}
}
