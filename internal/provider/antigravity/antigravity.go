// Package antigravity is the Antigravity (agy) provider. Antigravity has no public API of its own: a sign-in is the
// CLI's own Google sign-in, the same public OAuth client agy 1.5.12 carries (client id
// 1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com, redirect
// https://antigravity.google/oauth-callback, PKCE S256). The user approves in their own browser and pastes the
// authorization code the callback page shows; club exchanges it and keeps the refresh token, and a refused access
// token is swapped once through Google's token endpoint. The token endpoint requires the public client secret as well as the code verifier.
//
// The provider keeps no conversation of its own: club stores the turns and sends the latest ones with each message,
// which goes to the Code Assist generateContent endpoint the CLI uses.
package antigravity

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/web"
)

// Name is the provider's name.
const Name = "antigravity"

// OAuthClientID is the Antigravity CLI's public OAuth client id (agy 1.5.12).
const OAuthClientID = "1071006060591-tmhssin2h21lcre235vtolojh4g403ep.apps.googleusercontent.com"

// clientSecret is the public OAuth client's secret. agy 1.5.12 does not carry it as text, and the token
// endpoint refuses the exchange without it ("client_secret is missing"); with the secret published for this
// client, a bad code is "invalid_grant" instead. It is not an account credential and is not kept in this repo
// (a host's secret scanning rejects it). Set PERSONAL_AGENT_CLUB_ANTIGRAVITY_CLIENT_SECRET. It is sent with the
// exchange and never stored, logged or answered.
func clientSecret() string { return strings.TrimSpace(os.Getenv(EnvClientSecret)) }

// EnvClientSecret is where the public client secret is read from.
const EnvClientSecret = "PERSONAL_AGENT_CLUB_ANTIGRAVITY_CLIENT_SECRET"

// RedirectURI is the page that shows the authorization code for the user to copy.
const RedirectURI = "https://antigravity.google/oauth-callback"

// AuthURL is the Google authorization endpoint the CLI opens.
const AuthURL = "https://accounts.google.com/o/oauth2/v2/auth"

// TokenURL is Google's token endpoint, where the code is exchanged and the refresh token swapped.
const TokenURL = "https://oauth2.googleapis.com/token"

// UserInfoURL is where the signed-in email is read.
const UserInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

// GenerateURL is the Code Assist endpoint one turn is sent to.
const GenerateURL = "https://cloudcode-pa.googleapis.com/v1internal:generateContent"

// scopes are the ones agy 1.5.12 requests, in its order.
const scopes = "https://www.googleapis.com/auth/cloud-platform " +
	"https://www.googleapis.com/auth/userinfo.email " +
	"https://www.googleapis.com/auth/userinfo.profile " +
	"https://www.googleapis.com/auth/cclog " +
	"https://www.googleapis.com/auth/experimentsandconfigs " +
	"https://www.googleapis.com/auth/aicode openid"

const (
	historyTurns = 20
	historyRunes = 24000
	model        = "gemini-2.5-pro"
)

// Options configure the adapter.
type Options struct {
	Client      *http.Client
	AuthURL     string
	TokenURL    string
	UserInfoURL string
	GenerateURL string
	Now         func() time.Time
}

// Adapter is the Antigravity provider.
type Adapter struct {
	client      *http.Client
	authURL     string
	tokenURL    string
	userInfoURL string
	generateURL string
	now         func() time.Time
}

// New is the adapter.
func New(o Options) *Adapter {
	if o.Client == nil {
		o.Client, _ = web.NewClient("")
	}
	if o.AuthURL == "" {
		o.AuthURL = AuthURL
	}
	if o.TokenURL == "" {
		o.TokenURL = TokenURL
	}
	if o.UserInfoURL == "" {
		o.UserInfoURL = UserInfoURL
	}
	if o.GenerateURL == "" {
		o.GenerateURL = GenerateURL
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Adapter{client: o.Client, authURL: o.AuthURL, tokenURL: o.TokenURL, userInfoURL: o.UserInfoURL,
		generateURL: o.GenerateURL, now: o.Now}
}

var (
	_ provider.Adapter             = (*Adapter)(nil)
	_ provider.HistoryWanter       = (*Adapter)(nil)
	_ provider.CredentialRefresher = (*Adapter)(nil)
	_ provider.CodeLogin           = (*Adapter)(nil)
)

func (a *Adapter) Name() string       { return Name }
func (a *Adapter) Connection() string { return protocol.ConnectionServer }
func (a *Adapter) WantsHistory() bool { return true }

// tokens is a stored credential: both tokens, when they expire, and the email the sign-in read.
type tokens struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	Expiry       string `json:"expiry,omitempty"`
	Email        string `json:"email,omitempty"`
}

// Credential is the stored credential. expiry is when the access token stops being valid.
func Credential(accessToken, refreshToken, email string, expiry time.Time) string {
	b, _ := json.Marshal(tokens{AccessToken: accessToken, RefreshToken: refreshToken, Email: email,
		Expiry: expiry.UTC().Format(time.RFC3339)})
	return string(b)
}

// parse reads a stored credential.
func parse(credential string) tokens {
	var t tokens
	_ = json.Unmarshal([]byte(credential), &t)
	return t
}

// loginState is a sign-in's secret half; it never leaves club.
type loginState struct {
	verifier string
}

// StartLogin is the CLI's sign-in: the URL carries only the PKCE challenge, and the verifier stays here until the
// user pastes the authorization code.
func (a *Adapter) StartLogin() (provider.LoginFlow, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return provider.LoginFlow{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"client_id":             {OAuthClientID},
		"redirect_uri":          {RedirectURI},
		"response_type":         {"code"},
		"scope":                 {scopes},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
	}
	return provider.LoginFlow{URL: a.authURL + "?" + q.Encode(), State: loginState{verifier: verifier}}, nil
}

// CompleteLogin exchanges the authorization code the user pasted. The code verifier and the public client secret
// are both sent; neither is kept.
func (a *Adapter) CompleteLogin(ctx context.Context, flow provider.LoginFlow, code string) (string, error) {
	if clientSecret() == "" {
		return "", &provider.Error{Code: "CLIENT_SECRET_MISSING"}
	}
	st, ok := flow.State.(loginState)
	if !ok || st.verifier == "" {
		return "", &provider.Error{Code: provider.CodeRequestFailed}
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", &provider.Error{Code: provider.CodeRequestFailed}
	}
	body, err := a.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"client_id":     {OAuthClientID},
		"client_secret": {clientSecret()},
		"redirect_uri":  {RedirectURI},
		"code":          {code},
		"code_verifier": {st.verifier},
	})
	if err != nil {
		return "", err
	}
	access := web.Text(body["access_token"])
	refresh := web.Text(body["refresh_token"])
	if access == "" || refresh == "" {
		return "", &provider.Error{Code: provider.CodeAuthFailed}
	}
	email := a.email(ctx, access)
	return Credential(access, refresh, email, a.expiry(body)), nil
}

// RefreshCredential swaps the access token through the token endpoint. The endpoint may not rotate the refresh
// token; the stored one stays.
func (a *Adapter) RefreshCredential(ctx context.Context, credential string) (string, error) {
	if clientSecret() == "" {
		return "", &provider.Error{Code: "CLIENT_SECRET_MISSING"}
	}
	t := parse(credential)
	if t.RefreshToken == "" {
		return "", nil
	}
	body, err := a.token(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {OAuthClientID},
		"client_secret": {clientSecret()},
		"refresh_token": {t.RefreshToken},
	})
	if err != nil {
		return "", err
	}
	access := web.Text(body["access_token"])
	if access == "" {
		return "", &provider.Error{Code: provider.CodeAuthFailed}
	}
	return Credential(access, or(web.Text(body["refresh_token"]), t.RefreshToken), or(a.email(ctx, access), t.Email), a.expiry(body)), nil
}

// token posts a form to the token endpoint and reads its JSON object. web.Request always sends JSON, and
// this endpoint wants a form, so the body is written here; a redirect is still the client's to refuse.
func (a *Adapter) token(ctx context.Context, form url.Values) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(ctx, web.DefaultTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
	}
	req.Header.Set("content-type", "application/x-www-form-urlencoded")
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, &provider.Error{Code: provider.CodeNetworkError, Retryable: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, web.MaxResponse+1))
	if err != nil || len(raw) > web.MaxResponse {
		return nil, &provider.Error{Code: provider.CodeUpstreamError, HTTPStatus: resp.StatusCode, Retryable: true}
	}
	var data any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&data) != nil {
		return nil, &provider.Error{Code: provider.CodeBadResponse, HTTPStatus: resp.StatusCode}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusUnauthorized {
			return nil, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: resp.StatusCode}
		}
		return nil, web.ReadError(web.Result{Status: resp.StatusCode, Header: resp.Header})
	}
	return web.Record(data), nil
}

func (a *Adapter) expiry(body map[string]any) time.Time {
	n, _ := body["expires_in"].(json.Number)
	seconds, _ := n.Int64()
	if seconds <= 0 {
		seconds = 3600
	}
	return a.now().Add(time.Duration(seconds) * time.Second)
}

// email reads the signed-in address; "" when it cannot be read (the tokens are still kept).
func (a *Adapter) email(ctx context.Context, access string) string {
	r := web.Request(ctx, a.client, http.MethodGet, a.userInfoURL, map[string]string{"authorization": "Bearer " + access}, nil, 0)
	if !r.OK {
		return ""
	}
	return web.Text(web.Record(r.Data)["email"])
}

// Verify reads who the token belongs to.
func (a *Adapter) Verify(ctx context.Context, credential string) (provider.RemoteAccount, error) {
	t := parse(credential)
	if t.AccessToken == "" {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	r := web.Request(ctx, a.client, http.MethodGet, a.userInfoURL,
		map[string]string{"authorization": "Bearer " + t.AccessToken}, nil, 0)
	if !r.OK {
		return provider.RemoteAccount{}, web.ReadError(r)
	}
	body := web.Record(r.Data)
	email := web.Text(body["email"])
	id := web.Text(body["id"])
	if id == "" {
		id = email
	}
	if id == "" {
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeAuthFailed, HTTPStatus: r.Status}
	}
	return provider.RemoteAccount{ExternalAccountID: id, DisplayName: or(email, "Antigravity")}, nil
}

// ListSessions is the one local session: Antigravity keeps no conversation club can read.
func (a *Adapter) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: "main", Title: "Antigravity", Kind: protocol.KindMain, CanSend: true}}, nil
}

// ListMessages reads nothing: the transcript is the one club stores.
func (a *Adapter) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}

// Send asks the model, with the stored turns, and answers in the same call.
func (a *Adapter) Send(ctx context.Context, credential string, _ provider.SessionRef, msg provider.Outgoing, sc provider.SendContext) (provider.SendResult, error) {
	t := parse(credential)
	if t.AccessToken == "" {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeAuthFailed}
	}
	var contents []map[string]any
	for _, turn := range tail(sc.History) {
		role := "user"
		if turn.Role == protocol.RoleAssistant {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": []map[string]string{{"text": turn.Text}}})
	}
	contents = append(contents, map[string]any{"role": "user", "parts": []map[string]string{{"text": msg.Text}}})
	r := web.Request(ctx, a.client, http.MethodPost, a.generateURL,
		map[string]string{"authorization": "Bearer " + t.AccessToken, "content-type": "application/json"},
		map[string]any{"model": model, "contents": contents}, 0)
	if !r.OK {
		return provider.SendResult{}, web.ReadError(r)
	}
	text := answerText(web.Record(r.Data))
	if text == "" {
		return provider.SendResult{}, &provider.Error{Code: provider.CodeBadResponse}
	}
	return provider.SendResult{Status: provider.Sent, Messages: []provider.RemoteMessage{{
		ExternalID: "r-" + msg.ClientID, Role: protocol.RoleAssistant, Blocks: []protocol.Block{protocol.TextBlock(text)},
	}}}, nil
}

func answerText(body map[string]any) string {
	candidates := web.Records(body["candidates"])
	if len(candidates) == 0 {
		return ""
	}
	content := web.Record(candidates[0]["content"])
	var b strings.Builder
	for _, part := range web.Records(content["parts"]) {
		b.WriteString(web.Text(part["text"]))
	}
	return b.String()
}

// tail keeps the latest turns, within the rune budget.
func tail(turns []provider.Turn) []provider.Turn {
	if len(turns) > historyTurns {
		turns = turns[len(turns)-historyTurns:]
	}
	n := 0
	for i := len(turns) - 1; i >= 0; i-- {
		n += len([]rune(turns[i].Text))
		if n > historyRunes {
			return turns[i+1:]
		}
	}
	return turns
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
