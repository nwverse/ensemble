package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/names"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
)

// dotsSite drives ChatGPT's Dot (chatgpt.com/dots/<id>). It reads through the backend-api the signed-in page itself
// uses (the page's own session token never leaves the browser) and sends through the page's composer, then reads
// the message back to confirm it arrived.
//
// What the page answers (2026-11):
//   - /api/auth/session: {user: {id, name, email}, account: {id, planType}, accessToken}; {} when signed out.
//   - /backend-api/tbo?limit=N: {items: [{id, display_name, messaging_room_id, status, updated_at, avatar_url,
//     avatar_type, avatar_manifest: {snapshot: {asset_pointer, content_sha256, width, height}}}]}, one per Dot.
//     avatar_url is the Dot's own picture when its owner gave it one (a 512x512 PNG for avatar_type "codex-pet"),
//     null otherwise; it is a signed oaiusercontent.com address that expires within minutes (se=…&sig=…), which the
//     hub, without the owner's ChatGPT session, could not load later (dotsAvatarJS).
//   - /backend-api/files/download/<file id>: {download_url}, a signed address of the file asset_pointer names
//     ("sediment://file_…"), read only when a Dot's avatar_url is missing.
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
	// Avatar is the Dot's own picture as the page made it (dotsAvatarJS): a public https address, or a data URL of
	// the picture scaled down in the page; "" for none, or when AvatarKey names a picture the Adapter already has.
	Avatar    string `json:"avatar"`
	AvatarKey string `json:"avatar_key"`
}

// dotsAvatarMax bounds one Dot's picture as a data URL (names.ownPictureMax is 64 KB; a 128x128 WebP is about 9 KB;
// the url rides on each of its rows in agentsrc.sessions) and dotsAvatarBudget all the pictures one list makes:
// browser.run answers at most 60000 bytes. The pictures the
// budget leaves out are made by the next syncs, as the ones made are kept (pictureCache).
const (
	dotsAvatarMax    = 16 * 1024
	dotsAvatarBudget = 36 * 1024
)

// dotsAvatarJS defines __dotAvatar(s, d, have, max) inside the page: the Dot d's own picture as {key, url}.
//   - None (no avatar_url and no file in its manifest): {}. The Dot then shows its product's icon (names.Icon).
//   - An https address that needs nothing of the owner's (no signature, not chatgpt.com's own, not oaiusercontent's,
//     whose addresses are all signed and short-lived): that address, which the apps load themselves.
//   - Else a picture only the signed-in page can load: its key (the file its manifest names, else the address without
//     its query, which names the file), and, unless have holds the key, the picture fetched here, fitted into
//     128x128 (smaller when needed) and encoded as WebP (PNG where the browser has no WebP), as a data URL of at most
//     max characters. url is "" when it cannot be made: the Dot then shows its product's icon until a later sync.
const dotsAvatarJS = `
const __signed = (u) => {
  let x;
  try { x = new URL(u, location.href); } catch (e) { return true; }
  if (x.protocol !== "https:" || x.origin === location.origin || /(^|\.)oaiusercontent\.com$/i.test(x.hostname)) return true;
  for (const k of x.searchParams.keys()) {
    if (/^(sig|se|signature|expires|token|policy|key-pair-id|x-amz-signature|x-amz-credential|x-goog-signature)$/i.test(k)) return true;
  }
  return false;
};
const __shrink = async (blob, max) => {
  const bmp = await createImageBitmap(blob);
  try {
    for (const [px, q] of [[128, 0.85], [128, 0.6], [96, 0.6], [64, 0.5]]) {
      const c = new OffscreenCanvas(px, px);
      const g = c.getContext("2d");
      const k = Math.min(px / bmp.width, px / bmp.height);
      const w = Math.max(1, Math.round(bmp.width * k)), h = Math.max(1, Math.round(bmp.height * k));
      g.drawImage(bmp, Math.floor((px - w) / 2), Math.floor((px - h) / 2), w, h);
      let b = await c.convertToBlob({ type: "image/webp", quality: q });
      if (b.type !== "image/webp") b = await c.convertToBlob({ type: "image/png" });
      const url = await new Promise((ok, no) => { const r = new FileReader(); r.onload = () => ok(String(r.result)); r.onerror = () => no(r.error); r.readAsDataURL(b); });
      if (url.length <= max) return url;
    }
  } finally {
    if (bmp.close) bmp.close();
  }
  return "";
};
const __dotAvatar = async (s, d, have, max) => {
  const snap = d.avatar_manifest && d.avatar_manifest.snapshot;
  const pointer = snap && typeof snap.asset_pointer === "string" ? snap.asset_pointer : "";
  let src = typeof d.avatar_url === "string" ? d.avatar_url.trim() : "";
  if (src.startsWith("data:image/")) return src.length <= max ? { url: src } : {};
  if (src && !__signed(src)) return src.length <= 2048 ? { url: src } : {};
  let key = pointer;
  if (!key && src) { try { const x = new URL(src, location.href); key = x.origin + x.pathname; } catch (e) {} }
  if (!key) return {};
  if (have.has(key)) return { key };
  if (max < 4096) return { key, url: "" };
  try {
    if (!src) {
      const m = /^[a-z-]+:\/\/(file[-_][A-Za-z0-9_-]+)$/.exec(pointer);
      if (!m) return { key, url: "" };
      src = (await __api(s, "/backend-api/files/download/" + encodeURIComponent(m[1]))).download_url || "";
      if (!src) return { key, url: "" };
    }
    const at = new URL(src, location.href);
    const init = { signal: AbortSignal.timeout(10000), credentials: "omit" };
    if (at.origin === location.origin) {
      init.credentials = "include";
      init.headers = { Authorization: "Bearer " + s.accessToken };
      if (s.account && s.account.id) init.headers["ChatGPT-Account-Id"] = s.account.id;
    }
    const r = await fetch(at.href, init);
    const type = r.headers.get("content-type") || "";
    if (!r.ok || !type.startsWith("image/")) return { key, url: "" };
    return { key, url: await __shrink(await r.blob(), max) };
  } catch (e) {
    return { key, url: "" };
  }
};
`

func (dotsSite) listSessions(ctx context.Context, a *Adapter, credential string) ([]provider.RemoteSession, error) {
	var r struct {
		Error string    `json:"error"`
		Items []dotItem `json:"items"`
	}
	have := a.pictures.keys()
	script := dotsScript(dotsOrigin+"/", map[string]any{"have": have, "max": dotsAvatarMax, "budget": dotsAvatarBudget},
		dotsAvatarJS+`
    const s = await __session();
    const t = await __api(s, "/backend-api/tbo?limit=25&include_room_preview=false");
    const have = new Set(ARGS.have || []);
    let budget = ARGS.budget;
    const items = [];
    for (const d of (t.items || [])) {
      const item = { id: d.id, display_name: d.display_name || "", messaging_room_id: d.messaging_room_id || "", status: d.status || "", updated_at: d.updated_at || "", avatar: "", avatar_key: "" };
      if (item.id && item.messaging_room_id && (!item.status || item.status === "active")) {
        const p = await __dotAvatar(s, d, have, Math.min(ARGS.max, budget));
        item.avatar_key = p.key || "";
        item.avatar = p.url || "";
        if (item.avatar.startsWith("data:")) budget -= item.avatar.length;
        if (budget <= 0) budget = 0;
        if (item.avatar_key) have.add(item.avatar_key);
      }
      items.push(item);
    }
    return { items };
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
		avatar := a.dotAvatar(d)
		pending := avatar == "" && d.AvatarKey != "" && d.Avatar == ""
		out = append(out, provider.RemoteSession{
			ExternalID:     d.ID,
			Title:          title,
			Kind:           kind,
			LastActivityMs: parseTimeMs(d.UpdatedAt),
			CanSend:        true,
			AvatarURL:      avatar,
			AvatarPending:  pending,
			Ref:            map[string]string{"url": dotsOrigin + "/dots/" + d.ID, "room": d.Room},
		})
	}
	return out, nil
}

// dotAvatar is a listed Dot's own picture: an https address as the page gave it, or the data URL the page made, kept
// under its key, or the one kept under its key when the page skipped it. "" when it has none the apps could draw:
// it then shows its product's icon (names.Avatar), unless the page named its key without a picture (the budget left
// it out, or its fetch failed), which listSessions reports as AvatarPending so the club keeps the one it has.
func (a *Adapter) dotAvatar(d dotItem) string {
	v := strings.TrimSpace(d.Avatar)
	if strings.HasPrefix(v, "data:") {
		if names.DataType(v) == "" {
			return ""
		}
		a.pictures.put(d.AvatarKey, v)
		return v
	}
	if v != "" {
		if names.HTTPS(v) {
			return v
		}
		return ""
	}
	if d.AvatarKey != "" {
		v, _ = a.pictures.get(d.AvatarKey)
	}
	return v
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

// readQuota reads ChatGPT's rolling rate-limit windows from /backend-api/wham/usage inside the already-open tab.
// Best-effort: returns nil when the endpoint is unavailable, while still propagating 429s.
func (dotsSite) readQuota(ctx context.Context, a *Adapter, credential string) ([]provider.QuotaReading, error) {
	script := dotsScript(dotsOrigin+"/", nil, `
    const s = await __session();
    const headers = { Authorization: "Bearer " + s.accessToken };
    if (s.account && s.account.id) headers["ChatGPT-Account-Id"] = s.account.id;
    const r = await fetch("/backend-api/wham/usage", { credentials: "include", headers });
    if (r.status === 429) throw new Error("http 429");
    if (!r.ok) return { unavailable: true };
    const data = await r.json().catch(() => null);
    if (!data || !data.rate_limit) return { unavailable: true };
    return { rate_limit: data.rate_limit };
`)
	var r struct {
		Error       string `json:"error"`
		Unavailable bool   `json:"unavailable"`
		RateLimit   *struct {
			Allowed         *bool            `json:"allowed"`
			LimitReached    bool             `json:"limit_reached"`
			PrimaryWindow   *whamUsageWindow `json:"primary_window"`
			SecondaryWindow *whamUsageWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := a.run(ctx, credential, script, &r); err != nil {
		return nil, nil
	}
	if r.Error == "http 429" {
		return nil, dotsError(r.Error)
	}
	if r.Error != "" || r.Unavailable || r.RateLimit == nil {
		return nil, nil
	}
	nowMs := time.Now().UnixMilli()
	var out []provider.QuotaReading
	appendWindow := func(w *whamUsageWindow, fallbackWindow string, forceExhausted bool) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		used := *w.UsedPercent
		if used < 0 {
			used = 0
		}
		if used > 100 {
			used = 100
		}
		rem := 100 - used
		if forceExhausted {
			rem = 0
		}
		var resetsAt int64
		if w.ResetAt >= 1e11 {
			resetsAt = int64(w.ResetAt)
		} else if w.ResetAt > 0 {
			resetsAt = int64(w.ResetAt * 1000)
		}
		if resetsAt <= 0 && w.ResetAfterSeconds > 0 {
			resetsAt = nowMs + int64(w.ResetAfterSeconds*1000)
		}
		out = append(out, provider.QuotaReading{
			Kind:       protocol.QuotaKindWindow,
			Window:     whamWindowName(int64(w.LimitWindowSeconds), fallbackWindow),
			Limit:      100,
			Remaining:  rem,
			ResetsAt:   resetsAt,
			Exhausted:  forceExhausted || rem <= 0,
			SourceNote: "dots:wham/usage",
		})
	}
	limited := r.RateLimit.LimitReached || (r.RateLimit.Allowed != nil && !*r.RateLimit.Allowed)
	appendWindow(r.RateLimit.PrimaryWindow, "5h", limited && r.RateLimit.PrimaryWindow != nil && (r.RateLimit.SecondaryWindow == nil || (r.RateLimit.PrimaryWindow.UsedPercent != nil && *r.RateLimit.PrimaryWindow.UsedPercent >= 100)))
	appendWindow(r.RateLimit.SecondaryWindow, "week", limited && r.RateLimit.SecondaryWindow != nil && r.RateLimit.SecondaryWindow.UsedPercent != nil && *r.RateLimit.SecondaryWindow.UsedPercent >= 100)
	if limited {
		if len(out) == 0 {
			out = append(out, provider.QuotaReading{
				Kind:       protocol.QuotaKindWindow,
				Window:     "5h",
				Limit:      100,
				Remaining:  0,
				Exhausted:  true,
				SourceNote: "dots:wham/usage",
			})
		} else {
			anyExhausted := false
			for _, q := range out {
				if q.Exhausted || q.Remaining <= 0 {
					anyExhausted = true
					break
				}
			}
			if !anyExhausted {
				out[0].Exhausted = true
				out[0].Remaining = 0
			}
		}
	}
	return out, nil
}

type whamUsageWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds float64  `json:"limit_window_seconds"`
	ResetAfterSeconds  float64  `json:"reset_after_seconds"`
	ResetAt            float64  `json:"reset_at"`
}

func whamWindowName(seconds int64, fallback string) string {
	switch {
	case seconds <= 0:
		return fallback
	case seconds == 3600:
		return "hour"
	case seconds == 10800:
		return "3h"
	case seconds == 18000:
		return "5h"
	case seconds == 86400:
		return "day"
	case seconds == 604800:
		return "week"
	case seconds%86400 == 0:
		return fmt.Sprintf("%dd", seconds/86400)
	case seconds%3600 == 0:
		return fmt.Sprintf("%dh", seconds/3600)
	default:
		return fallback
	}
}
