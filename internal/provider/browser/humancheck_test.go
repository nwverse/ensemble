package browser

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/provider"
)

// runPageJS runs prelude and then expr inside a stand-in page (location, document, window and fetch as page says)
// under Node, and answers what expr gave. Skipped where there is no Node.
func runPageJS(t *testing.T, prelude string, page map[string]any, expr string) string {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node")
	}
	p, _ := json.Marshal(page)
	src := `
const P = ` + string(p) + `;
const u = new URL(P.url);
globalThis.location = { hostname: u.hostname, pathname: u.pathname, href: P.url };
globalThis.window = globalThis;
if (P.wiz) window.WIZ_global_data = P.wiz;
globalThis.document = {
  title: P.title || "",
  body: { innerText: P.text || "" },
  documentElement: { lang: "en" },
  querySelector: sel => (P.selectors || []).some(s => sel.split(",").map(x => x.trim()).includes(s)) ? {} : null,
};
globalThis.fetch = async () => ({ status: P.status || 200, ok: (P.status || 200) < 400, url: P.fetchURL || P.url,
  text: async () => P.fetchBody || "", json: async () => JSON.parse(P.fetchBody || "{}") });
` + prelude + `
(async () => { let v; try { v = await (` + expr + `); } catch (e) { v = "throws " + e.message; } console.log(JSON.stringify(v)); })();
`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, node, "-e", src).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return strings.TrimSpace(string(out))
}

// Google's unusual-traffic check, as the page scripts see it, is "human check": on /sorry, behind a reCAPTCHA, or
// worded so on a page that is not Gemini's own; a signed-in Gemini page never is, whatever its conversations say.
func TestGeminiPageTellsHumanCheckApart(t *testing.T) {
	signedIn := map[string]any{"SNlM0e": "tok"}
	for _, tc := range []struct {
		name string
		page map[string]any
		want string
	}{
		{"sorry page", map[string]any{"url": "https://www.google.com/sorry/index?continue=https://gemini.google.com/app"}, "true"},
		{"recaptcha form", map[string]any{"url": "https://www.google.com/", "selectors": []string{"#captcha-form"}}, "true"},
		{"unusual traffic words", map[string]any{"url": "https://www.google.com/x", "text": "Our systems have detected unusual traffic from your computer network."}, "true"},
		{"chinese words", map[string]any{"url": "https://www.google.com/x", "text": "我们的系统检测到您的计算机网络中存在异常流量。"}, "true"},
		{"signed-in gemini talking about unusual traffic", map[string]any{"url": "https://gemini.google.com/app/1", "wiz": signedIn,
			"text": "Why do I see unusual traffic? I'm not a robot", "selectors": []string{".g-recaptcha"}}, "false"},
		{"signed-out gemini landing", map[string]any{"url": "https://gemini.google.com/", "text": "Sign in"}, "false"},
	} {
		if got := runPageJS(t, geminiHumanCheckJS, tc.page, "__humanCheck()"); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	// Through the RPC helper: the page, then the RPC's own answer.
	for _, tc := range []struct {
		name string
		page map[string]any
		want string
	}{
		{"sorry page", map[string]any{"url": "https://www.google.com/sorry/index"}, `"throws human check"`},
		{"signed out", map[string]any{"url": "https://gemini.google.com/"}, `"throws auth"`},
		{"bare 429", map[string]any{"url": "https://gemini.google.com/app", "wiz": signedIn, "status": 429}, `"throws http 429"`},
		{"429 carrying the sorry page", map[string]any{"url": "https://gemini.google.com/app", "wiz": signedIn, "status": 429,
			"fetchBody": "<html>Our systems have detected unusual traffic … recaptcha</html>"}, `"throws human check"`},
		{"rpc redirected to sorry", map[string]any{"url": "https://gemini.google.com/app", "wiz": signedIn, "status": 200,
			"fetchURL": "https://www.google.com/sorry/index?continue=x"}, `"throws human check"`},
	} {
		if got := runPageJS(t, geminiPrelude, tc.page, `__rpc("MaZiqc", [])`); got != tc.want {
			t.Errorf("rpc, %s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Cloudflare's "Verify you are human" in front of chatgpt.com is "human check" too, before the session is asked.
func TestDotsPageTellsCloudflareCheckApart(t *testing.T) {
	session := `{"user":{"id":"u"},"accessToken":"t"}`
	cfBlocked := map[string]any{"status": 403, "fetchBody": "<html>Just a moment...</html>"}
	page := func(base map[string]any, more map[string]any) map[string]any {
		out := map[string]any{"url": "https://chatgpt.com/dots/1"}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range more {
			out[k] = v
		}
		return out
	}
	for _, tc := range []struct {
		name string
		page map[string]any
		want string
	}{
		{"interstitial form", page(cfBlocked, map[string]any{"selectors": []string{"#challenge-form"}}), `"throws human check"`},
		{"turnstile frame", page(cfBlocked, map[string]any{"selectors": []string{`iframe[src*="challenges.cloudflare.com"]`}}), `"throws human check"`},
		// The challenge-platform script alone, on a page whose session does not answer: not a check, signed out.
		{"script tag alone", page(cfBlocked, map[string]any{"selectors": []string{`script[src*="/cdn-cgi/challenge-platform/"]`}}), `"throws auth"`},
		// A normal signed-in page carrying Cloudflare's script, and even a Turnstile frame: read as it is.
		{"signed in with the script", page(map[string]any{"status": 200, "fetchBody": session},
			map[string]any{"selectors": []string{`script[src*="/cdn-cgi/challenge-platform/"]`, `iframe[src*="challenges.cloudflare.com"]`}}), session},
		// A Dot named like the challenge page, in the title and the text: read as it is.
		{"dot named Just a moment", page(map[string]any{"status": 200, "fetchBody": session},
			map[string]any{"title": "Just a moment...", "text": "Verify you are human"}), session},
		// The app's composer on the page: not an interstitial, whatever else is there.
		{"composer and a frame, signed out", page(cfBlocked, map[string]any{"selectors": []string{`[data-role="composer"]`, `iframe[src*="challenges.cloudflare.com"]`}}), `"throws auth"`},
		{"signed out", page(map[string]any{"status": 200, "fetchBody": "{}"}, nil), `"throws auth"`},
	} {
		if got := runPageJS(t, dotsPrelude, tc.page, `__session()`); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
}

// The scripts' "human check" is its own code at pairing, sync and send, never a rate limit; a real 429 still is.
func TestHumanCheckIsItsOwnCode(t *testing.T) {
	ctx := context.Background()
	cred := EncodeCredential(Credential{MachineID: "mac-1", Backend: "ego"})
	answer := func(v any) *fakeDriver {
		return &fakeDriver{answer: func(RunRequest) (RunResult, error) { return hubOutput(t, v), nil }}
	}
	for _, tc := range []struct {
		value any
		code  string
	}{
		{map[string]any{"error": "human check"}, provider.CodeHumanCheck},
		{map[string]any{"error": "http 429"}, provider.CodeRateLimited},
	} {
		for _, site := range []Site{SiteGeminiTasks, SiteDots} {
			a := New(site, answer(tc.value))
			if _, err := a.Verify(ctx, cred); provider.CodeOf(err) != tc.code {
				t.Errorf("%s verify %v: %v", site.Provider, tc.value, err)
			}
			if _, err := a.ListSessions(ctx, cred); provider.CodeOf(err) != tc.code {
				t.Errorf("%s list %v: %v", site.Provider, tc.value, err)
			}
			if _, err := a.ListMessages(ctx, cred, provider.SessionRef{ExternalID: "x", Ref: map[string]string{"room": "r"}}, provider.ListOptions{}); provider.CodeOf(err) != tc.code {
				t.Errorf("%s messages %v: %v", site.Provider, tc.value, err)
			}
		}
	}
	for _, site := range []Site{SiteGeminiTasks, SiteDots} {
		res, err := New(site, answer(map[string]any{"error": "human check"})).Send(ctx, cred, provider.SessionRef{ExternalID: "x"}, provider.Outgoing{Text: "hi"}, provider.SendContext{})
		if provider.CodeOf(err) != provider.CodeHumanCheck {
			t.Errorf("%s send: %+v, %v", site.Provider, res, err)
		}
	}
	if id, backend := New(SiteGeminiTasks, nil).Machine(cred); id != "mac-1" || backend != "ego" {
		t.Errorf("machine = %q %q", id, backend)
	}
}
