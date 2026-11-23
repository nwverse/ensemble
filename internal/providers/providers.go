// Package providers builds the registry of the API providers club ships: cue, grok_bot and groq, and muse2api when
// its server's address is configured, all through one HTTP client and its proxy. The browser providers (muse, dots,
// claude_duet) are not in this version. Both shells build their registry here.
package providers

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/nwverse/personal-agent-club/internal/provider"
	"github.com/nwverse/personal-agent-club/internal/provider/cue"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
	"github.com/nwverse/personal-agent-club/internal/provider/grokbot"
	"github.com/nwverse/personal-agent-club/internal/provider/groq"
	"github.com/nwverse/personal-agent-club/internal/provider/muse2api"
	"github.com/nwverse/personal-agent-club/internal/provider/web"
)

// Environment variables Config reads.
const (
	EnvProxy         = "PERSONAL_AGENT_CLUB_PROXY"
	EnvGroqModel     = "PERSONAL_AGENT_CLUB_GROQ_MODEL"
	EnvMuse2APIURL   = "PERSONAL_AGENT_CLUB_MUSE2API_URL"
	EnvMuse2APIModel = "PERSONAL_AGENT_CLUB_MUSE2API_MODEL"
)

// Config configures the providers.
type Config struct {
	// Proxy every provider request goes through (socks5://, socks5h://, http:// or https://); "" uses HTTP_PROXY,
	// HTTPS_PROXY and NO_PROXY from the environment.
	Proxy string
	// GroqModel is the model Groq accounts ask; "" is groq.DefaultModel.
	GroqModel string
	// Muse2APIURL is the address of the owner's muse2api server; "" leaves muse2api out (listed as unavailable).
	Muse2APIURL string
	// Muse2APIModel is the model muse2api accounts ask; "" is muse2api.DefaultModel.
	Muse2APIModel string
	// Client, when set, is the HTTP client every provider request goes through instead of one built for Proxy (the
	// Persona shell hands the hub's, which goes out through the sandbox's egress proxy). Redirects are refused either
	// way. Proxy must then be "".
	Client *http.Client
	// DemoEcho adds the in-memory echo provider (demos and tests only).
	DemoEcho bool
}

// FromEnv is the configuration in the environment.
func FromEnv() Config {
	return Config{Proxy: strings.TrimSpace(os.Getenv(EnvProxy)), GroqModel: strings.TrimSpace(os.Getenv(EnvGroqModel)),
		Muse2APIURL: strings.TrimSpace(os.Getenv(EnvMuse2APIURL)), Muse2APIModel: strings.TrimSpace(os.Getenv(EnvMuse2APIModel))}
}

// Registry is a registry with the API providers. A muse2api address that is not an http(s) URL is an error.
func Registry(c Config) (*provider.Registry, error) {
	var client *http.Client
	switch {
	case c.Client != nil && c.Proxy != "":
		return nil, errors.New("providers: a client and a proxy were both given")
	case c.Client != nil:
		client = web.UseClient(c.Client)
	default:
		var err error
		if client, err = web.NewClient(c.Proxy); err != nil {
			return nil, err
		}
	}
	r := provider.NewRegistry(
		cue.New(cue.Options{Client: client}),
		grokbot.New(grokbot.Options{Client: client}),
		groq.New(groq.Options{Client: client, Model: c.GroqModel}),
	)
	if c.Muse2APIURL != "" {
		m, err := muse2api.New(muse2api.Options{Client: client, URL: c.Muse2APIURL, Model: c.Muse2APIModel})
		if err != nil {
			return nil, err
		}
		r.Register(m)
	}
	if c.DemoEcho {
		r.Register(echo.New())
	}
	return r, nil
}
