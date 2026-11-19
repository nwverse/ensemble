package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/provider"
)

// fake is one token endpoint: it records the form and answers the next scripted body.
type fake struct {
	srv    *http.Server
	forms  []url.Values
	next   []answer
	user   string
	emails int
}

type answer struct {
	status int
	body   string
}

func newFake(t *testing.T) *fake {
	t.Helper()
	f := &fake{user: `{"id":"acct-1","email":"ada@example.com"}`}
	mux := http.NewServeMux()
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		form, _ := url.ParseQuery(string(raw))
		// the public client secret is sent, and a test must be able to say so without printing it
		if form.Get("client_secret") != "" {
			form.Set("client_secret", "present")
		}
		f.forms = append(f.forms, form)
		a := answer{status: http.StatusInternalServerError, body: `{}`}
		if len(f.next) > 0 {
			a = f.next[0]
			f.next = f.next[1:]
		}
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(a.status)
		_, _ = w.Write([]byte(a.body))
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, r *http.Request) {
		f.emails++
		if !strings.HasPrefix(r.Header.Get("authorization"), "Bearer ") || len(r.Header.Get("authorization")) < 12 {
			http.Error(w, `{"error":"no"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(f.user))
	})
	f.srv = &http.Server{Addr: "127.0.0.1:0", Handler: mux}
	l, err := listen(t)
	if err != nil {
		t.Fatal(err)
	}
	f.srv.Addr = l.Addr().String()
	go f.srv.Serve(l)
	t.Cleanup(func() { _ = f.srv.Close() })
	return f
}

func (f *fake) adapter(now time.Time) *Adapter {
	base := "http://" + f.srv.Addr
	return New(Options{AuthURL: base + "/auth", TokenURL: base + "/token", UserInfoURL: base + "/userinfo",
		GenerateURL: base + "/generate", Now: func() time.Time { return now }})
}

func listen(t *testing.T) (net.Listener, error) {
	t.Helper()
	return net.Listen("tcp", "127.0.0.1:0")
}

func tokenJSON(access, refresh string, seconds int) string {
	b, _ := json.Marshal(map[string]any{"access_token": access, "refresh_token": refresh, "expires_in": seconds, "token_type": "Bearer"})
	return string(b)
}

func TestLoginStartCompleteAndRefresh(t *testing.T) {
	t.Setenv(EnvClientSecret, "test-client-secret")
	f := newFake(t)
	now := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	a := f.adapter(now)
	flow, err := a.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(flow.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	for _, k := range []string{"client_id", "redirect_uri", "code_challenge", "code_challenge_method", "access_type", "prompt", "scope"} {
		if q.Get(k) == "" {
			t.Fatalf("the authorization url has no %s: %s", k, flow.URL)
		}
	}
	if q.Get("client_id") != OAuthClientID || q.Get("redirect_uri") != RedirectURI || q.Get("code_challenge_method") != "S256" ||
		q.Get("access_type") != "offline" || q.Get("prompt") != "consent" || q.Get("response_type") != "code" {
		t.Fatalf("query: %v", q)
	}
	if strings.Contains(flow.URL, "client_secret") || strings.Contains(flow.URL, "code_verifier") {
		t.Fatal("the authorization url carries a secret")
	}
	for _, scope := range []string{"cloud-platform", "userinfo.email", "userinfo.profile", "cclog", "experimentsandconfigs", "aicode", "openid"} {
		if !strings.Contains(q.Get("scope"), scope) {
			t.Fatalf("scope %s missing: %s", scope, q.Get("scope"))
		}
	}

	f.next = []answer{{status: 200, body: tokenJSON("fixture-access-token-1", "fixture-refresh-token-1", 3600)}}
	cred, err := a.CompleteLogin(context.Background(), flow, "  fixture-auth-code  ")
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if json.Unmarshal([]byte(cred), &got) != nil || got["accessToken"] != "fixture-access-token-1" ||
		got["refreshToken"] != "fixture-refresh-token-1" || got["email"] != "ada@example.com" || got["expiry"] != "2026-03-01T01:00:00Z" {
		t.Fatalf("credential: %s", cred)
	}
	form := f.forms[0]
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "fixture-auth-code" ||
		form.Get("client_id") != OAuthClientID || form.Get("client_secret") != "present" || form.Get("code_verifier") == "" ||
		form.Get("redirect_uri") != RedirectURI {
		t.Fatalf("exchange form: %v", form)
	}

	// a refused code is an auth failure and stores nothing
	f.next = []answer{{status: 400, body: `{"error":"invalid_grant"}`}}
	if _, err := a.CompleteLogin(context.Background(), flow, "nope"); codeOf(err) != provider.CodeAuthFailed {
		t.Fatalf("a refused code: %v", err)
	}

	// refresh swaps the access token and keeps the refresh token; the email is read again
	f.next = []answer{{status: 200, body: tokenJSON("fixture-access-token-2", "", 1800)}}
	fresh, err := a.RefreshCredential(context.Background(), cred)
	if err != nil {
		t.Fatal(err)
	}
	var got2 map[string]string
	if json.Unmarshal([]byte(fresh), &got2) != nil || got2["accessToken"] != "fixture-access-token-2" ||
		got2["refreshToken"] != "fixture-refresh-token-1" || got2["email"] != "ada@example.com" || got2["expiry"] != "2026-03-01T00:30:00Z" {
		t.Fatalf("refreshed: %s", fresh)
	}
	rf := f.forms[len(f.forms)-1]
	if rf.Get("grant_type") != "refresh_token" || rf.Get("refresh_token") != "fixture-refresh-token-1" || rf.Get("client_secret") != "present" {
		t.Fatalf("refresh form: %v", rf)
	}

	// a pasted credential without a refresh token cannot be refreshed
	if again, err := a.RefreshCredential(context.Background(), `{"accessToken":"only"}`); again != "" || err != nil {
		t.Fatalf("no refresh token: %q %v", again, err)
	}
	// a refused refresh is an auth failure
	f.next = []answer{{status: 400, body: `{"error":"invalid_grant"}`}}
	if _, err := a.RefreshCredential(context.Background(), cred); codeOf(err) != provider.CodeAuthFailed {
		t.Fatalf("a refused refresh: %v", err)
	}
}

func TestMissingClientSecretIsRefused(t *testing.T) {
	f := newFake(t)
	a := f.adapter(time.Unix(0, 0).UTC())
	flow, err := a.StartLogin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.CompleteLogin(context.Background(), flow, "code"); codeOf(err) != "CLIENT_SECRET_MISSING" {
		t.Fatalf("complete: %v", err)
	}
	if _, err := a.RefreshCredential(context.Background(), `{"accessToken":"a","refreshToken":"r"}`); codeOf(err) != "CLIENT_SECRET_MISSING" {
		t.Fatalf("refresh: %v", err)
	}
	if len(f.forms) != 0 {
		t.Fatal("a request went out without a client secret")
	}
}

func TestVerifyNamesTheEmail(t *testing.T) {
	f := newFake(t)
	a := f.adapter(time.Unix(0, 0).UTC())
	got, err := a.Verify(context.Background(), `{"accessToken":"fixture-access-token-1","refreshToken":"r","email":"ada@example.com"}`)
	if err != nil || got.ExternalAccountID != "acct-1" || got.DisplayName != "ada@example.com" {
		t.Fatalf("%+v %v", got, err)
	}
	f.user = `{}`
	if _, err := a.Verify(context.Background(), `{"accessToken":"fixture-access-token-1"}`); codeOf(err) != provider.CodeAuthFailed {
		t.Fatalf("no identity: %v", err)
	}
}

func codeOf(err error) string { return provider.CodeOf(err) }
