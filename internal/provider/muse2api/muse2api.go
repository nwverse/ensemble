// Package muse2api is a muse2api server (github.com/czg86389-hub/muse2api) as a personal chat bot. muse2api is a
// self-hosted bridge that drives muse.ai's web chat with the owner's own Muse accounts and answers an OpenAI-compatible
// API, so this adapter talks to it the way the groq adapter talks to Groq: the server keeps no conversation, club
// stores the transcript and sends the latest turns with each message. The server's address is configuration (it is
// the owner's own deployment); the credential is the server's API key (MUSE2API_KEY), only ever sent to that server.
package muse2api

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/provider/groq"
	"github.com/nwverse/ensemble/internal/provider/web"
)

// Name is the provider's name.
const Name = "muse2api"

// DefaultModel is muse2api's chat model; the server routes every chat to muse.ai's one assistant whatever the model.
const DefaultModel = "muse-spark"

// SessionID is the one session an account has.
const SessionID = "chat"

// AnswerTimeout is how long an answer may take: muse2api drives a browser and answers in one piece, which takes
// longer than an inference API, but stays under the core's two-minute send limit.
const AnswerTimeout = 110 * time.Second

var modelID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/:-]{0,127}$`)

// Model is v when it is a model id, else DefaultModel. Any id muse2api knows (its models or one of its aliases) works.
func Model(v string) string {
	v = strings.TrimSpace(v)
	if modelID.MatchString(v) {
		return v
	}
	return DefaultModel
}

// Origin is the API root of the server at v: an http or https URL with a host and no user info, query or fragment.
// Its path is kept, and /v1 is added unless it already ends so: "http://192.0.2.7:18610" and
// "https://muse.example.com/v1" are both accepted.
func Origin(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", errors.New("muse2api: no server address")
	}
	u, err := url.Parse(v)
	if err != nil || u.Host == "" || u.Opaque != "" {
		return "", errors.New("muse2api: the server address must be an http:// or https:// URL with a host")
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return "", errors.New("muse2api: the server address must be an http:// or https:// URL with a host")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return "", errors.New("muse2api: the server address takes no user info, query or fragment (the key is the credential)")
	}
	path := strings.TrimRight(u.EscapedPath(), "/")
	if !strings.HasSuffix(path, "/v1") {
		path += "/v1"
	}
	return strings.ToLower(u.Scheme) + "://" + u.Host + path, nil
}

// Options configure the adapter.
type Options struct {
	Client *http.Client
	// URL is the server's address (see Origin).
	URL string
	// Model is the model asked; "" or an invalid id is DefaultModel.
	Model string
	Now   func() time.Time
}

// Adapter is the muse2api provider.
type Adapter struct {
	client *http.Client
	origin string
	model  string
	chat   *groq.Adapter
}

// New is the adapter, or an error when the server's address is not one.
func New(o Options) (*Adapter, error) {
	origin, err := Origin(o.URL)
	if err != nil {
		return nil, err
	}
	if o.Client == nil {
		o.Client, _ = web.NewClient("")
	}
	model := Model(o.Model)
	return &Adapter{client: o.Client, origin: origin, model: model,
		chat: groq.New(groq.Options{Client: o.Client, Origin: origin, Model: model, Timeout: AnswerTimeout, Now: o.Now})}, nil
}

var (
	_ provider.Adapter       = (*Adapter)(nil)
	_ provider.HistoryWanter = (*Adapter)(nil)
)

func (a *Adapter) Name() string       { return Name }
func (a *Adapter) Connection() string { return protocol.ConnectionServer }
func (a *Adapter) WantsHistory() bool { return true }

// ModelName is the model the adapter asks.
func (a *Adapter) ModelName() string { return a.model }

// OriginURL is the API root the adapter calls.
func (a *Adapter) OriginURL() string { return a.origin }

// Verify checks the key against the server's model list. The server is the account: one key per deployment, so a
// rotated key can replace the old one.
func (a *Adapter) Verify(ctx context.Context, key string) (provider.RemoteAccount, error) {
	r := web.Request(ctx, a.client, http.MethodGet, a.origin+"/models",
		map[string]string{"authorization": "Bearer " + key}, nil, 0)
	if !r.OK {
		return provider.RemoteAccount{}, web.ReadError(r)
	}
	if !web.IsArray(web.Record(r.Data)["data"]) {
		// Not a model list: the address is something other than a muse2api (or OpenAI-compatible) server.
		return provider.RemoteAccount{}, &provider.Error{Code: provider.CodeRequestFailed, HTTPStatus: r.Status}
	}
	return provider.RemoteAccount{ExternalAccountID: Name}, nil
}

// ListSessions is the one chat.
func (a *Adapter) ListSessions(context.Context, string) ([]provider.RemoteSession, error) {
	return []provider.RemoteSession{{ExternalID: SessionID, Title: "Muse", Kind: protocol.KindMain, CanSend: true,
		Ref: map[string]string{"model": a.model}}}, nil
}

// ListMessages has nothing to read: the store keeps the transcript.
func (a *Adapter) ListMessages(context.Context, string, provider.SessionRef, provider.ListOptions) (provider.RemotePage, error) {
	return provider.RemotePage{}, nil
}

// Send asks for an answer with the recent turns (the newest 20, at most 24k characters) and returns both sides of
// the exchange. A lost answer leaves the send unconfirmed: muse2api cannot tell whether it reached muse.ai.
func (a *Adapter) Send(ctx context.Context, key string, s provider.SessionRef, msg provider.Outgoing, sc provider.SendContext) (provider.SendResult, error) {
	return a.chat.Send(ctx, key, s, msg, sc)
}
