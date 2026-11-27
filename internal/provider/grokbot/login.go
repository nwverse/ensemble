package grokbot

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"

	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/web"
)

// LoginOrigin is where the user approves a sign-in.
const LoginOrigin = "https://cursor.com"

// loginState is a sign-in's secret half; it never leaves club.
type loginState struct {
	uuid     string
	verifier string
}

// StartLogin is a browser sign-in as grok-bot-sdk's docs/guide/auth.md documents it: the user approves in their own
// browser at the URL, which carries only the challenge, and club polls for the token with the verifier.
func (a *Adapter) StartLogin() (provider.LoginFlow, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return provider.LoginFlow{}, err
	}
	st := loginState{uuid: newUUID(), verifier: base64.RawURLEncoding.EncodeToString(b[:])}
	sum := sha256.Sum256([]byte(st.verifier))
	q := "challenge=" + url.QueryEscape(base64.RawURLEncoding.EncodeToString(sum[:])) + "&uuid=" + url.QueryEscape(st.uuid) +
		"&mode=login&redirectTarget=sand&supportsSelectedTeamLogin=true"
	return provider.LoginFlow{URL: a.loginOrigin + "/loginDeepControl?" + q, State: st}, nil
}

// PollLogin asks whether the user approved: 404 is still pending, 403 a refusal (LOGIN_REFUSED).
func (a *Adapter) PollLogin(ctx context.Context, flow provider.LoginFlow) (string, error) {
	st, ok := flow.State.(loginState)
	if !ok {
		return "", &provider.Error{Code: provider.CodeRequestFailed}
	}
	q := url.Values{"uuid": {st.uuid}, "verifier": {st.verifier}}
	r := web.Request(ctx, a.client, http.MethodGet, a.origin+"/auth/poll?"+q.Encode(), map[string]string{}, nil, 0)
	switch {
	case !r.OK && r.Status == http.StatusNotFound:
		return "", nil
	case !r.OK && r.Status == http.StatusForbidden:
		return "", &provider.Error{Code: "LOGIN_REFUSED", HTTPStatus: http.StatusForbidden}
	case !r.OK:
		return "", web.ReadError(r)
	}
	body := web.Record(r.Data)
	access := web.Text(body["accessToken"])
	if access == "" {
		return "", &provider.Error{Code: provider.CodeRequestFailed}
	}
	return Credential(access, web.Text(body["refreshToken"])), nil
}
