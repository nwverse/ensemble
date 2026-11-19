package core

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
)

// agyScripted is the scripted provider speaking as antigravity's code sign-in: CompleteLogin takes the pasted code
// and answers the next credential.
type agyScripted struct {
	*scripted
	complete []any
}

func (p *agyScripted) CompleteLogin(_ context.Context, flow provider.LoginFlow, code string) (string, error) {
	if code == "" || flow.State == nil {
		return "", &provider.Error{Code: "REQUEST_FAILED"}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.complete) == 0 {
		return "", &provider.Error{Code: "AUTH_FAILED"}
	}
	next := p.complete[0]
	p.complete = p.complete[1:]
	if err, ok := next.(error); ok {
		return "", err
	}
	return next.(string), nil
}

func newAgyEnv(t *testing.T) (*env, *agyScripted) {
	t.Helper()
	p := &agyScripted{scripted: newScripted("antigravity")}
	clk := &clock{t: time.UnixMilli(1_800_000_000_000)}
	club := New(Options{Version: "test", Registry: provider.NewRegistry(p), Now: clk.Now, FollowUps: []time.Duration{}, WatchLimit: time.Second})
	t.Cleanup(club.Close)
	space, err := club.Space(context.Background(), DefaultSpace)
	if err != nil {
		t.Fatal(err)
	}
	return &env{club: club, space: space, p: p.scripted, clock: clk}, p
}

func fixtureCredential(access, refresh, email string) string {
	b, _ := json.Marshal(map[string]string{"accessToken": access, "refreshToken": refresh, "email": email,
		"expiry": "2026-03-01T01:00:00Z"})
	return string(b)
}

func TestAntigravityLogin(t *testing.T) {
	e, p := newAgyEnv(t)
	ctx := context.Background()
	cred := fixtureCredential("fixture-agy-access-0001", "fixture-agy-refresh-0001", "ada@example.com")
	p.complete = []any{cred}

	start, err := e.space.StartAntigravityLogin(ctx, protocol.AntigravityLoginStartParams{})
	if err != nil || !strings.Contains(start.LoginURL, "challenge=") || !strings.HasPrefix(start.LoginID, "g") {
		t.Fatalf("%+v %v", start, err)
	}
	if _, err := e.space.StartAntigravityLogin(ctx, protocol.AntigravityLoginStartParams{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.space.StartAntigravityLogin(ctx, protocol.AntigravityLoginStartParams{}); reason(err) != "LOGIN_LIMIT" {
		t.Fatalf("a third waiting sign-in: %v", err)
	}
	if _, err := e.space.CompleteAntigravityLogin(ctx, protocol.AntigravityLoginCompleteParams{LoginID: start.LoginID}); reason(err) != "INVALID_AGENT_BRIDGE_INPUT" {
		t.Fatalf("an empty code: %v", err)
	}
	done, err := e.space.CompleteAntigravityLogin(ctx, protocol.AntigravityLoginCompleteParams{LoginID: start.LoginID, Code: "fixture-code", DisplayName: "Ada"})
	if err != nil || done.Status != "done" || done.Account.DisplayName != "Ada" || done.Account.Provider != "antigravity" {
		t.Fatalf("%+v %v", done, err)
	}
	if b, _ := json.Marshal(done); strings.Contains(string(b), "fixture-agy-access") || strings.Contains(string(b), "fixture-code") {
		t.Fatalf("a token came back: %s", b)
	}
	if c, _, _ := e.space.vault.Get(ctx, done.Account.AccountID); c != cred {
		t.Fatal("the credential was not stored")
	}
	if _, err := e.space.CompleteAntigravityLogin(ctx, protocol.AntigravityLoginCompleteParams{LoginID: start.LoginID, Code: "x"}); reason(err) != "LOGIN_NOT_FOUND" {
		t.Fatal(err)
	}

	// a refused code drops the sign-in; signing in again replaces the credential of the same account
	e.advance(11 * time.Minute)
	refused, err := e.space.StartAntigravityLogin(ctx, protocol.AntigravityLoginStartParams{AccountID: done.Account.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	p.complete = []any{&provider.Error{Code: "AUTH_FAILED"}}
	if _, err := e.space.CompleteAntigravityLogin(ctx, protocol.AntigravityLoginCompleteParams{LoginID: refused.LoginID, Code: "nope"}); reason(err) != "AUTH_FAILED" {
		t.Fatal(err)
	}
	if _, err := e.space.CompleteAntigravityLogin(ctx, protocol.AntigravityLoginCompleteParams{LoginID: refused.LoginID, Code: "nope"}); reason(err) != "LOGIN_NOT_FOUND" {
		t.Fatalf("a refused sign-in is dropped: %v", err)
	}
	again, err := e.space.StartAntigravityLogin(ctx, protocol.AntigravityLoginStartParams{AccountID: done.Account.AccountID})
	if err != nil {
		t.Fatal(err)
	}
	fresh := fixtureCredential("fixture-agy-access-0002", "fixture-agy-refresh-0002", "ada@example.com")
	p.complete = []any{fresh}
	redone, err := e.space.CompleteAntigravityLogin(ctx, protocol.AntigravityLoginCompleteParams{LoginID: again.LoginID, Code: "fixture-code-2"})
	if err != nil || redone.Account.AccountID != done.Account.AccountID {
		t.Fatalf("signing in again pairs the same account: %+v %v", redone, err)
	}
	if c, _, _ := e.space.vault.Get(ctx, done.Account.AccountID); c != fresh {
		t.Fatal("the credential was not replaced")
	}

	// a refresh swaps the stored access token; the core keeps whatever the provider returns
	e.p.refresh = func(c string) (string, error) {
		if !strings.Contains(c, "fixture-agy-refresh-0002") {
			return "", &provider.Error{Code: "AUTH_FAILED"}
		}
		return fixtureCredential("fixture-agy-access-0003", "fixture-agy-refresh-0002", "ada@example.com"), nil
	}
	adapter, _ := e.club.Registry().Get("antigravity")
	if got := e.space.refresh(ctx, done.Account.AccountID, adapter, fresh); !strings.Contains(got, "fixture-agy-access-0003") {
		t.Fatalf("refresh: %q", got)
	}
	if c, _, _ := e.space.vault.Get(ctx, done.Account.AccountID); !strings.Contains(c, "fixture-agy-access-0003") {
		t.Fatal("the refreshed credential was not stored")
	}
}

func TestInstallAntigravityWritesTheAgyFile(t *testing.T) {
	e, p := newAgyEnv(t)
	ctx := context.Background()
	cred := fixtureCredential("fixture-agy-access-0001", "fixture-agy-refresh-0001", "ada@example.com")
	p.complete = []any{cred}
	start, err := e.space.StartAntigravityLogin(ctx, protocol.AntigravityLoginStartParams{})
	if err != nil {
		t.Fatal(err)
	}
	done, err := e.space.CompleteAntigravityLogin(ctx, protocol.AntigravityLoginCompleteParams{LoginID: start.LoginID, Code: "fixture-code"})
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	got, err := e.space.InstallAntigravity(ctx, protocol.AntigravityInstallParams{AccountID: done.Account.AccountID, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".gemini", "antigravity-cli", "antigravity-oauth-token")
	if got.Path != want || got.Email != "ada@example.com" {
		t.Fatalf("%+v", got)
	}
	raw, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(want); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
	var file map[string]any
	if json.Unmarshal(raw, &file) != nil {
		t.Fatal("not the agy file")
	}
	token, _ := file["token"].(map[string]any)
	if file["auth_method"] != "consumer" || file["id_token_email"] != "ada@example.com" ||
		token["token_type"] != "Bearer" || token["access_token"] != "fixture-agy-access-0001" || token["refresh_token"] != "fixture-agy-refresh-0001" {
		t.Fatalf("shape: %v", file)
	}
	// a sign-in that is already there is left untouched
	if _, err := e.space.InstallAntigravity(ctx, protocol.AntigravityInstallParams{AccountID: done.Account.AccountID, Home: home}); reason(err) != "ACCOUNT_ALREADY_PAIRED" {
		t.Fatalf("an existing file: %v", err)
	}
	if again, _ := os.ReadFile(want); string(again) != string(raw) {
		t.Fatal("the existing file was changed")
	}
}
