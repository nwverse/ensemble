package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// dotsSite drives ChatGPT's Dot (chatgpt.com/dots/<id>). It reads through the backend-api the signed-in page itself
// uses (the page's own session token never leaves the browser) and sends through the page's composer, then reads
// the message back to confirm it arrived.
//
// What the page answers (2026-11):
//   - /api/auth/session: {user: {id, name, email}, account: {id, planType}, accessToken}; {} when signed out.
//   - /backend-api/tbo?limit=N: {items: [{id, display_name, messaging_room_id, status, updated_at}]}, one per Dot.
//   - /backend-api/messaging/rooms/<room>/messages?limit=N: {items: [{id, created_at, role, account_user_id,
//     content: {text}, deleted_at}]}, oldest first. Every item has role "user": the owner's account_user_id starts
//     with their user id ("user-…__<dot id>"), the Dot's with "calpico-member-".
type dotsSite struct{}

const dotsOrigin = "https://chatgpt.com"

// dotsMaxPage is the most messages the room's messages endpoint answers in one read (a larger limit is a 422).
const dotsMaxPage = 32

// challengeJS defines __challenged() inside the page: whether Cloudflare's "Verify you are human" interstitial
// stands in front of the site instead of its app. Only a person passing it lets the browser through. It is asked
// only once the session did not answer (a signed-in page never is one), and it takes neither the
// challenge-platform script Cloudflare also puts on working pages, nor a title or a text ("Just a moment" may be a
// Dot's name), nor a page with the app's composer: only the interstitial's own form or Turnstile frame.
const challengeJS = `
const __challenged = () => {
  if (document.querySelector('[data-role="composer"], #prompt-textarea')) return false;
  return !!document.querySelector('#challenge-form, #challenge-stage, iframe[src*="challenges.cloudflare.com"]');
};
`

// dotsPrelude defines api(path) inside the page: the session, then a fetch with its token. It throws "human check"
// behind a human check, "auth" when signed out and "http <status>" on a failed request.
const dotsPrelude = challengeJS + `
const __session = async () => {
  const r = await fetch("/api/auth/session", { credentials: "include" }).catch(() => null);
  const s = r && r.ok ? await r.json().catch(() => ({})) : {};
  if (s && s.user && s.accessToken) return s;
  throw new Error(__challenged() ? "human check" : "auth");
};
const __api = async (s, path, init) => {
  const headers = { Authorization: "Bearer " + s.accessToken, ...(init && init.headers) };
  if (s.account && s.account.id) headers["ChatGPT-Account-Id"] = s.account.id;
  const r = await fetch(path, { credentials: "include", ...init, headers });
  if (r.status === 401 || r.status === 403) throw new Error("auth");
  if (!r.ok) throw new Error("http " + r.status);
  return r.json();
};
`

// dotsScript wraps body (an async function body of (ARGS) run inside the page) so its answer, or its failure, is
// printed as one JSON line. The reads only call the backend-api from the page, so a tab already on a loaded
// chatgpt.com page stays where it is (openJS): any of them answers, with the session read fresh every time.
func dotsScript(gotoURL string, args any, body string) string {
	a, _ := json.Marshal(args)
	return openJS(gotoURL, dotsStay) + fmt.Sprintf(`
const __out = await page.evaluate(async (ARGS) => {
%s
  try {
%s
  } catch (e) {
    return { error: String((e && e.message) || e) };
  }
}, %s);
return __out;
`, dotsPrelude, body, a)
}

// dotsStay is any chatgpt.com page (openJS).
const dotsStay = `^https://chatgpt\.com/`

// dotsError maps a site script's error string to the provider's error (Dots and the sites in sites.go).
func dotsError(msg string) error {
	switch {
	case msg == "":
		return nil
	case msg == "auth":
		return &provider.Error{Code: provider.CodeAuthFailed}
	case msg == "human check":
		// The site's human verification (Google's unusual-traffic page, Cloudflare's check), not a rate limit: it
		// does not pass by waiting, a person has to solve it in that browser.
		return &provider.Error{Code: provider.CodeHumanCheck}
	case msg == "http 404":
		return &provider.Error{Code: provider.CodeNotFound, HTTPStatus: 404}
	case msg == "http 429":
		return &provider.Error{Code: provider.CodeRateLimited, HTTPStatus: 429, Retryable: true}
	case msg == "no dot", msg == "no thread":
		return &provider.Error{Code: provider.CodeNotFound}
	case strings.HasPrefix(msg, "http "):
		return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	default:
		return &provider.Error{Code: provider.CodeUpstreamError, Retryable: true}
	}
}

func parseTimeMs(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func (dotsSite) verify(ctx context.Context, a *Adapter, credential string) (provider.RemoteAccount, error) {
	var r struct {
		Error     string `json:"error"`
		UserID    string `json:"userId"`
		AccountID string `json:"accountId"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		Dots      int    `json:"dots"`
	}
	script := dotsScript(dotsOrigin+"/", nil, `
    const s = await __session();
    const t = await __api(s, "/backend-api/tbo?limit=25&include_room_preview=false");
    const dots = (t.items || []).filter(d => !d.status || d.status === "active").length;
    return { userId: s.user.id, accountId: (s.account && s.account.id) || "", name: s.user.name || "", email: s.user.email || "", dots };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemoteAccount{}, err
	}
	if err := dotsError(r.Error); err != nil {
		return provider.RemoteAccount{}, err
	}
	if r.UserID == "" {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	if r.Dots == 0 {
		// Signed in, but this ChatGPT account (workspace) has no Dot to talk to.
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeNotFound}
	}
	id := "dots:" + r.UserID
	if r.AccountID != "" {
		id += ":" + r.AccountID
	}
	name := strings.TrimSpace(r.Name)
	if name == "" {
		name = strings.TrimSpace(r.Email)
	}
	return provider.RemoteAccount{ExternalAccountID: id, Alias: name}, nil
}

type dotItem struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Room        string `json:"messaging_room_id"`
	Status      string `json:"status"`
	UpdatedAt   string `json:"updated_at"`
}

func (dotsSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	var r struct {
		Error string    `json:"error"`
		Items []dotItem `json:"items"`
	}
	script := dotsScript(dotsOrigin+"/", nil, `
    const s = await __session();
    const t = await __api(s, "/backend-api/tbo?limit=25&include_room_preview=false");
    return { items: (t.items || []).map(d => ({ id: d.id, display_name: d.display_name || "", messaging_room_id: d.messaging_room_id || "", status: d.status || "", updated_at: d.updated_at || "" })) };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, err
	}
	if err := dotsError(r.Error); err != nil {
		return nil, err
	}
	out := make([]provider.RemoteSession, 0, len(r.Items))
	for _, d := range r.Items {
		if d.ID == "" || d.Room == "" || (d.Status != "" && d.Status != "active") {
			continue
		}
		kind := protocol.KindSide
		if len(out) == 0 {
			kind = protocol.KindMain
		}
		title := strings.TrimSpace(d.DisplayName)
		if title == "" {
			title = "Dot"
		}
		out = append(out, provider.RemoteSession{
			ExternalID:     d.ID,
			Title:          title,
			Kind:           kind,
			LastActivityMs: parseTimeMs(d.UpdatedAt),
			CanSend:        true,
			Ref:            map[string]string{"url": dotsOrigin + "/dots/" + d.ID, "room": d.Room},
		})
	}
	return out, nil
}

type dotMessage struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Owner     bool   `json:"owner"`
	Text      string `json:"text"`
}

// dotsMessagesJS reads the room's latest ARGS.limit messages into {messages: [dotMessage]}; s and room are in scope.
const dotsMessagesJS = `
    const page = await __api(s, "/backend-api/messaging/rooms/" + encodeURIComponent(room) + "/messages?limit=" + ARGS.limit);
    const me = s.user.id;
    const all = (page.items || []).filter(m => !m.deleted_at).map(m => {
      const who = String(m.account_user_id || "");
      return { id: m.id, created_at: m.created_at || "", owner: who === me || who.startsWith(me + "__") || who.startsWith("user-"), text: (m.content && m.content.text) || "" };
    });
    // Newest first within a byte budget: browser.run answers at most 60000 bytes, so older messages are left out.
    const enc = new TextEncoder();
    let budget = ARGS.budget || 40000;
    const messages = [];
    for (let i = all.length - 1; i >= 0; i--) {
      const n = enc.encode(JSON.stringify(all[i])).length;
      if (n > budget) {
        if (messages.length === 0) { all[i].text = all[i].text.slice(0, Math.floor(budget / 4)); messages.unshift(all[i]); }
        break;
      }
      budget -= n;
      messages.unshift(all[i]);
    }
`

// dotsRoomJS finds the room of ARGS.dot: from ARGS.room, or by reading the Dot.
const dotsRoomJS = `
    const s = await __session();
    let room = ARGS.room;
    if (!room) {
      const d = await __api(s, "/backend-api/tbo/" + encodeURIComponent(ARGS.dot));
      room = d.messaging_room_id;
    }
    if (!room) throw new Error("no dot");
`

func dotsTarget(session provider.SessionRef) (dot, room string) {
	return strings.TrimSpace(session.ExternalID), strings.TrimSpace(session.Ref["room"])
}

func (dotsSite) listMessages(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, opts provider.ListOptions) (provider.RemotePage, error) {
	dot, room := dotsTarget(session)
	if dot == "" || dot == "main" {
		return provider.RemotePage{}, &provider.Error{Code: provider.CodeNotFound}
	}
	limit := opts.Limit
	if limit <= 0 || limit > dotsMaxPage {
		limit = dotsMaxPage
	}
	var r struct {
		Error    string       `json:"error"`
		Messages []dotMessage `json:"messages"`
	}
	script := dotsScript(dotsOrigin+"/", map[string]any{"dot": dot, "room": room, "limit": limit},
		dotsRoomJS+dotsMessagesJS+`
    return { messages };
`)
	if err := a.run(ctx, credential, script, &r); err != nil {
		return provider.RemotePage{}, err
	}
	if err := dotsError(r.Error); err != nil {
		return provider.RemotePage{}, err
	}
	out := make([]provider.RemoteMessage, 0, len(r.Messages))
	for _, m := range r.Messages {
		if m.ID == "" || strings.TrimSpace(m.Text) == "" {
			continue
		}
		role := protocol.RoleAssistant
		if m.Owner {
			role = protocol.RoleUser
		}
		ms := parseTimeMs(m.CreatedAt)
		out = append(out, provider.RemoteMessage{
			ExternalID:   m.ID,
			Role:         role,
			Blocks:       []protocol.Block{{Type: "text", Text: m.Text}},
			CreateTimeMs: ms,
			Position:     ms,
		})
	}
	return provider.RemotePage{Messages: out}, nil
}

const dotsComposerSel = `[data-role="composer"] div.ProseMirror[contenteditable="true"]`
const dotsSendSel = `[data-role="composer"] button[aria-label="发送"], [data-role="composer"] button[aria-label="Send" i], [data-role="composer"] button[aria-label*="Send" i]`

func (dotsSite) send(ctx context.Context, a *Adapter, credential string, session provider.SessionRef, msg provider.Outgoing) (provider.SendResult, error) {
	text := strings.TrimSpace(msg.Text)
	if text == "" {
		return provider.SendResult{}, errors.New("empty message")
	}
	dot, room := dotsTarget(session)
	if dot == "" || dot == "main" {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeNotFound}
	}
	args, _ := json.Marshal(map[string]any{"dot": dot, "room": room, "limit": 10, "text": text})
	// Typed and sent in the page, then read back from the room: sent only once the owner's message with this text
	// shows up after the send started.
	script := fmt.Sprintf(`
const ARGS0 = %s;
await page.goto(%q);
await page.waitForLoadState("domcontentloaded").catch(() => {});
const signedIn = await page.evaluate(async () => {
%s
  const r = await fetch("/api/auth/session", { credentials: "include" }).catch(() => null);
  const s = r && r.ok ? await r.json().catch(() => ({})) : {};
  if (s && s.user) return true;
  return __challenged() ? "human check" : false;
});
if (signedIn === "human check") {
  return { error: "human check" };
} else if (!signedIn) {
  return { error: "auth" };
} else {
  const startedAt = Date.now();
  const sel = %q;
  await page.waitForSelector("loc=css:" + sel, { state: "visible", timeout: 20000 });
  await page.fill("loc=css:" + sel, ARGS0.text);
  await new Promise(r => setTimeout(r, 300));
  const clicked = await page.click("loc=css:" + %q, { timeout: 3000 }).then(() => true).catch(() => false);
  if (!clicked) await page.press("loc=css:" + sel, "Enter");
  let found = null;
  for (let i = 0; i < 20 && !found; i++) {
    await new Promise(r => setTimeout(r, 1000));
    found = await page.evaluate(async (ARGS) => {
%s
      try {
%s
%s
        const norm = t => String(t || "").replace(/\s+/g, " ").trim();
        const want = norm(ARGS.text);
        const hit = messages.filter(m => m.owner && norm(m.text) === want && Date.parse(m.created_at) >= ARGS.since - 60000).pop();
        return hit ? { id: hit.id, room } : null;
      } catch (e) {
        return { error: String((e && e.message) || e) };
      }
    }, { ...ARGS0, since: startedAt });
    if (found && found.error) break;
  }
  return found ? (found.error ? { clicked, error: found.error } : { sent: true, clicked, id: found.id }) : { sent: false, clicked };
}
`, args, dotsOrigin+"/dots/"+dot, challengeJS, dotsComposerSel, dotsSendSel, dotsPrelude, dotsRoomJS, dotsMessagesJS)
	var r struct {
		Error string `json:"error"`
		Sent  bool   `json:"sent"`
		ID    string `json:"id"`
	}
	if err := a.runSend(ctx, credential, script, &r); err != nil {
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Code == provider.CodeAuthFailed {
			return provider.SendResult{}, err
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: provider.CodeOf(err)}, nil
	}
	if r.Error == "auth" || r.Error == "human check" {
		return provider.SendResult{}, dotsError(r.Error)
	}
	if !r.Sent {
		code := provider.CodeUpstreamError
		if r.Error != "" {
			code = provider.CodeOf(dotsError(r.Error))
		}
		return provider.SendResult{Status: provider.Unconfirmed, Error: code}, nil
	}
	return provider.SendResult{Status: provider.Sent, ExternalID: r.ID}, nil
}
