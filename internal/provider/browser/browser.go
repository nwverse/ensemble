// Package browser provides a pluggable browser-backed provider.Adapter for web personal agents (Muse, Claude Duet,
// Gemini Tasks, etc.) without bundling a headless browser or exporting cookies (#16).
//
// Any browser automation transport implements Driver:
//   - PersonaHubDriver: calls Persona's cross-machine browser.machines / browser.run host RPCs (ego lite, future CDP)
//   - LocalEgoDriver: drives a local ego-browser CLI directly (standalone mode & E2E tests)
//   - Future CDPDriver / PlaywrightDriver: implement Driver and register the same sites unchanged.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// Machine describes a computer exposing a browser backend to this Driver.
type Machine struct {
	MachineID string `json:"machine_id"`
	Name      string `json:"name"`
	Backend   string `json:"backend"`
	Online    bool   `json:"online"`
	Ready     bool   `json:"ready"`
}

// RunRequest is one isolated script execution on a Driver.
type RunRequest struct {
	MachineID string `json:"machine_id"`
	Backend   string `json:"backend,omitempty"`
	Slot      string `json:"slot"`
	Script    string `json:"script"`
	TimeoutMS int    `json:"timeout_ms,omitempty"`
}

// RunResult is the Driver's answer to a RunRequest.
type RunResult struct {
	Backend string `json:"backend"`
	Output  string `json:"output"`
	Error   string `json:"error,omitempty"`
}

// Driver is the Go extension point for browser automation transports (ego lite over Persona Hub, local ego-browser,
// future CDP, etc.).
type Driver interface {
	Name() string
	Machines(ctx context.Context) ([]Machine, error)
	Run(ctx context.Context, req RunRequest) (RunResult, error)
}

// Credential is the non-secret target binding stored for a browser-backed account. Raw cookies never leave the
// browser.
type Credential struct {
	MachineID string `json:"machine_id"`
	Backend   string `json:"backend,omitempty"`
}

// EncodeCredential formats a Credential for the vault.
func EncodeCredential(c Credential) string {
	b, _ := json.Marshal(c)
	return string(b)
}

// ParseCredential parses either JSON {"machine_id":"...","backend":"..."} or a plain "machine_id" string.
func ParseCredential(raw string) (Credential, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Credential{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	if strings.HasPrefix(s, "{") {
		var c Credential
		if err := json.Unmarshal([]byte(s), &c); err != nil {
			return Credential{}, &provider.Error{Code: provider.CodeAuthFailed}
		}
		c.MachineID = strings.TrimSpace(c.MachineID)
		c.Backend = strings.TrimSpace(c.Backend)
		if c.MachineID == "" {
			return Credential{}, &provider.Error{Code: provider.CodeAuthFailed}
		}
		return c, nil
	}
	return Credential{MachineID: s}, nil
}

// Site describes one web personal-agent service driven through a browser.Driver.
type Site struct {
	Provider       string // "muse", "claude_duet", "gemini_tasks"
	Label          string // human-readable name
	AppURL         string // landing URL
	Slot           string // isolated task space slot name (e.g. "pac-muse")
	SingleSession  bool   // true when the app has one main chat at AppURL
	ComposerSel    string // CSS selector for the message composer
	SendButtonSel  string // CSS selector for send button (Enter fallback when empty)
	MessageLogSel  string // CSS selector for message container / rows
	SignedOutHosts []string
	// impl replaces the generic DOM scripts for a site that needs its own (dots.go, sites.go); nil for the generic
	// ones.
	impl siteImpl
}

// siteImpl is a site's own Verify, ListSessions, ListMessages and Send, run through the Adapter's driver.
type siteImpl interface {
	verify(ctx context.Context, a *Adapter, credential string) (provider.RemoteAccount, error)
	listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error)
	listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error)
	send(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, msg provider.Outgoing) (provider.SendResult, error)
}

// Built-in site presets.
var (
	SiteMuse = Site{
		Provider:       "muse",
		Label:          "Muse",
		AppURL:         "https://muse.ai/",
		Slot:           "pac-muse",
		SingleSession:  true,
		ComposerSel:    museComposerSel,
		SendButtonSel:  museSendSel,
		MessageLogSel:  `[data-message-item][data-message-id]`,
		SignedOutHosts: []string{"login", "auth"},
		impl:           museSite{},
	}
	SiteClaudeDuet = Site{
		Provider:       "claude_duet",
		Label:          "Claude Duet",
		AppURL:         "https://claude.ai/duet",
		Slot:           "pac-duet",
		SingleSession:  false,
		ComposerSel:    duetComposerSel,
		SendButtonSel:  duetSendSel,
		MessageLogSel:  `[role="feed"] [role="article"]`,
		SignedOutHosts: []string{"login"},
		impl:           duetSite{},
	}
	SiteGeminiTasks = Site{
		Provider:       "gemini_tasks",
		Label:          "Gemini Tasks",
		AppURL:         "https://gemini.google.com/app",
		Slot:           "pac-gemini",
		SingleSession:  false,
		ComposerSel:    `div.ql-editor[contenteditable="true"], rich-textarea [contenteditable="true"], textarea`,
		SendButtonSel:  `button[aria-label*="Send" i], button.send-button`,
		MessageLogSel:  `user-query, model-response, message-content`,
		SignedOutHosts: []string{"accounts.google.com"},
	}
	// SiteDots is ChatGPT's Dot (chatgpt.com/dots/<id>): one session per Dot, read through the page's own
	// backend-api and sent through its composer (dots.go).
	SiteDots = Site{
		Provider:       "dots",
		Label:          "Dots",
		AppURL:         "https://chatgpt.com/",
		Slot:           "pac-dots",
		SingleSession:  false,
		ComposerSel:    dotsComposerSel,
		SendButtonSel:  dotsSendSel,
		MessageLogSel:  `article[data-message-id]`,
		SignedOutHosts: []string{"auth/login"},
		impl:           dotsSite{},
	}
)

// Adapter implements provider.Adapter for a Site using any browser.Driver.
type Adapter struct {
	site    Site
	driver  Driver
	timeout time.Duration
}

// New creates a browser-backed provider.Adapter for site using driver.
func New(site Site, driver Driver) *Adapter {
	return &Adapter{site: site, driver: driver, timeout: 45 * time.Second}
}

var _ provider.Adapter = (*Adapter)(nil)

func (a *Adapter) Name() string       { return a.site.Provider }
func (a *Adapter) Connection() string { return protocol.ConnectionBrowser }
func (a *Adapter) Site() Site         { return a.site }
func (a *Adapter) Driver() Driver     { return a.driver }

func (a *Adapter) run(ctx context.Context, rawCred, script string, out any) error {
	if a.driver == nil {
		return &provider.Error{Code: provider.CodeUpstreamError}
	}
	cred, err := ParseCredential(rawCred)
	if err != nil {
		return err
	}
	res, err := a.driver.Run(ctx, RunRequest{
		MachineID: cred.MachineID,
		Backend:   cred.Backend,
		Slot:      a.site.Slot,
		Script:    script,
		TimeoutMS: int(a.timeout / time.Millisecond),
	})
	if err != nil {
		return &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
	}
	if strings.TrimSpace(res.Error) != "" {
		return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	payload := scriptValue(res.Output)
	if payload == "" {
		return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	if err := json.Unmarshal([]byte(payload), out); err != nil {
		return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
	return nil
}

// scriptValue is what a site script returned. Persona's browser.run (and LocalEgoDriver, which wraps scripts the
// same way) answers {"value": <the script's return value>, "error", "url", "title", "console"}; a driver that only
// relays what the script printed answers it as the last JSON line.
func scriptValue(output string) string {
	var hub struct {
		Value   json.RawMessage `json:"value"`
		Console string          `json:"console"`
	}
	if json.Unmarshal([]byte(strings.TrimSpace(output)), &hub) == nil {
		if v := strings.TrimSpace(string(hub.Value)); strings.HasPrefix(v, "{") {
			return v
		}
		if line := extractJSONLine(hub.Console); line != "" {
			return line
		}
	}
	return extractJSONLine(output)
}

func jsonArray(v []string) string {
	if v == nil {
		v = []string{}
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// extractJSONLine finds the last JSON object line in driver stdout (ignoring notice/log lines).
func extractJSONLine(raw string) string {
	lines := strings.Split(strings.TrimSpace(raw), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			return line
		}
	}
	return ""
}

func (a *Adapter) Verify(ctx context.Context, credential string) (provider.RemoteAccount, error) {
	if a.site.impl != nil {
		return a.site.impl.verify(ctx, a, credential)
	}
	script := fmt.Sprintf(`
await page.goto(%q);
await page.waitForLoadState("domcontentloaded").catch(() => {});
await new Promise(r => setTimeout(r, 1500));
const composer = await page.waitForSelector("loc=css:" + %q, { state: "visible", timeout: 10000 }).then(() => true).catch(() => false);
const url = await page.url();
const title = await page.title();
const signedOut = /\/login\b|accounts\.google\.com/i.test(url) || %s.some(h => url.includes(h));
return { url, title, signedOut, composer };
`, a.site.AppURL, a.site.ComposerSel, jsonArray(a.site.SignedOutHosts))
	var r struct {
		URL       string `json:"url"`
		Title     string `json:"title"`
		SignedOut bool   `json:"signedOut"`
		Composer  *bool  `json:"composer"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemoteAccount{}, err
	}
	// A site that does not redirect a signed-out visitor (muse.ai, gemini.google.com show a landing page) is told
	// apart by its composer: only a signed-in page has one. An answer without the field is a signed-out one too.
	if r.SignedOut || r.Composer == nil || !*r.Composer {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	cred, _ := ParseCredential(credential)
	name := strings.TrimSpace(r.Title)
	if name == "" {
		name = a.site.Label
	}
	return provider.RemoteAccount{
		ExternalAccountID: a.site.Provider + ":" + cred.MachineID,
		DisplayName:       name,
	}, nil
}

func (a *Adapter) ListSessions(ctx context.Context, credential string) ([]provider.RemoteSession, error) {
	if a.site.impl != nil {
		return a.site.impl.listSessions(ctx, a, credential)
	}
	if a.site.SingleSession {
		return []provider.RemoteSession{{
			ExternalID:     "main",
			Title:          a.site.Label,
			Kind:           protocol.KindMain,
			LastActivityMs: time.Now().UnixMilli(),
			CanSend:        true,
			Ref:            map[string]string{"url": a.site.AppURL},
		}}, nil
	}
	script := fmt.Sprintf(`
await page.goto(%q);
await page.waitForLoadState("domcontentloaded").catch(() => {});
await new Promise(r => setTimeout(r, 1500));
const items = await page.evaluate(() => {
  const seen = new Set();
  const out = [];
  for (const el of document.querySelectorAll('a[href]')) {
    const href = el.getAttribute('href') || '';
    const m = href.match(/\/(?:duet|messages|app|chat|thread|c)\/([A-Za-z0-9_-]{4,})/);
    if (!m || seen.has(m[1])) continue;
    seen.add(m[1]);
    out.push({ id: m[1], title: (el.innerText || '').trim().split('\n')[0] || m[1], url: el.href });
    if (out.length >= 20) break;
  }
  return out;
});
return { sessions: items };
`, a.site.AppURL)
	var r struct {
		Sessions []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
			URL   string `json:"url"`
		} `json:"sessions"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, err
	}
	now := time.Now().UnixMilli()
	out := make([]provider.RemoteSession, 0, len(r.Sessions)+1)
	if len(r.Sessions) == 0 {
		out = append(out, provider.RemoteSession{
			ExternalID:     "main",
			Title:          a.site.Label,
			Kind:           protocol.KindMain,
			LastActivityMs: now,
			CanSend:        true,
			Ref:            map[string]string{"url": a.site.AppURL},
		})
		return out, nil
	}
	for i, s := range r.Sessions {
		kind := protocol.KindSide
		if i == 0 {
			kind = protocol.KindMain
		}
		out = append(out, provider.RemoteSession{
			ExternalID:     s.ID,
			Title:          s.Title,
			Kind:           kind,
			LastActivityMs: now,
			CanSend:        true,
			Ref:            map[string]string{"url": s.URL},
		})
	}
	return out, nil
}

func (a *Adapter) ListMessages(ctx context.Context, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	if a.site.impl != nil {
		return a.site.impl.listMessages(ctx, a, credential, session, opts)
	}
	targetURL := a.site.AppURL
	if u := strings.TrimSpace(session.Ref["url"]); u != "" {
		targetURL = u
	}
	limit := opts.Limit
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	script := fmt.Sprintf(`
await page.goto(%q);
await page.waitForLoadState("domcontentloaded").catch(() => {});
await new Promise(r => setTimeout(r, 1500));
const msgs = await page.evaluate(({ sel, limit }) => {
  const nodes = Array.from(document.querySelectorAll(sel));
  const out = [];
  for (const n of nodes.slice(-limit)) {
    const text = (n.innerText || '').trim();
    if (!text) continue;
    const aria = (n.getAttribute('aria-label') || '').toLowerCase();
    const roleAttr = (n.getAttribute('data-message-author-role') || n.getAttribute('data-message-role') || '').toLowerCase();
    const isUser = aria === 'you' || roleAttr === 'user' || n.matches('[data-testid="user-message"], user-query');
    out.push({ id: n.getAttribute('data-message-id') || '', role: isUser ? 'user' : 'assistant', text: text.slice(0, 8000) });
  }
  return out;
}, { sel: %q, limit: %d });
return { messages: msgs };
`, targetURL, a.site.MessageLogSel, limit)
	var r struct {
		Messages []struct {
			ID   string `json:"id"`
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemotePage{}, err
	}
	out := make([]provider.RemoteMessage, 0, len(r.Messages))
	for i, m := range r.Messages {
		id := m.ID
		if id == "" {
			id = fmt.Sprintf("b%d", i+1)
		}
		role := protocol.RoleAssistant
		if m.Role == "user" {
			role = protocol.RoleUser
		}
		out = append(out, provider.RemoteMessage{
			ExternalID: id,
			Role:       role,
			Blocks:     []protocol.Block{{Type: "text", Text: m.Text}},
			Position:   int64(i + 1),
		})
	}
	return provider.RemotePage{Messages: out}, nil
}

func (a *Adapter) Send(ctx context.Context, credential string, session provider.SessionRef, msg provider.Outgoing, _ provider.SendContext) (provider.SendResult, error) {
	if a.site.impl != nil {
		return a.site.impl.send(ctx, a, credential, session, msg)
	}
	if strings.TrimSpace(msg.Text) == "" {
		return provider.SendResult{}, errors.New("empty message")
	}
	targetURL := a.site.AppURL
	if u := strings.TrimSpace(session.Ref["url"]); u != "" {
		targetURL = u
	}
	script := fmt.Sprintf(`
await page.goto(%q);
await page.waitForLoadState("domcontentloaded").catch(() => {});
await new Promise(r => setTimeout(r, 1500));
const sel = %q;
await page.waitForSelector("loc=css:" + sel, { state: "visible", timeout: 15000 });
await page.fill("loc=css:" + sel, %q);
const sendSel = %q;
const clicked = await page.click("loc=css:" + sendSel, { timeout: 2000 }).then(() => true).catch(() => false);
if (!clicked) {
  await page.press("loc=css:" + sel, "Enter");
}
return { sent: true };
`, targetURL, a.site.ComposerSel, msg.Text, a.site.SendButtonSel)
	var r struct {
		Sent bool `json:"sent"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.SendResult{Status: provider.Unconfirmed}, err
	}
	if !r.Sent {
		return provider.SendResult{Status: provider.Unconfirmed}, nil
	}
	return provider.SendResult{Status: provider.Sent}, nil
}
