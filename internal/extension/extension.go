// Package extension is the Persona shell: club as a Persona extension program. It contributes the agent source "pac"
// (the hub's agentsrc.* calls go to the club's default space through the same core.Club.Call the serve shell uses,
// and the space's changes go back as agentsrc.updated), the accounts page (ui "accounts", accounts.go) and the
// connector "club" carrying the assistant's club_agent_* tools (tools.go). Its data lives in the extension's data
// directory (storage.go).
package extension

import (
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nwverse/persona/sdk/go/ext"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/protocol"
	"github.com/nwverse/personal-agent-club/internal/provider/browser"
	"github.com/nwverse/personal-agent-club/internal/provider/echo"
	"github.com/nwverse/personal-agent-club/internal/provider/muse2api"
	"github.com/nwverse/personal-agent-club/internal/provider/web"
	"github.com/nwverse/personal-agent-club/internal/runtime"
)

// ID is the extension's id; Contribution is its agent_sources contribution, the prefix of its session ids in
// Persona (<contribution>:<sessionKey>), which must never change.
const (
	ID           = "personal-agent-club"
	Contribution = "pac"
)

// Config is the extension's configuration (persona-extension.json "configuration").
type Config struct {
	DemoEcho         bool  `json:"demo_echo"`
	BrowserProviders bool  `json:"browser_providers"`
	AssistantRuntime *bool `json:"assistant_runtime"`
	// Muse2APIURL is the address of the owner's muse2api server; set, the muse2api provider is added. The manifest's
	// "config:muse2api_url:http" lets the egress proxy through to that address only.
	Muse2APIURL   string `json:"muse2api_url"`
	Muse2APIModel string `json:"muse2api_model"`
}

func (c Config) assistantRuntimeEnabled() bool {
	return c.AssistantRuntime == nil || *c.AssistantRuntime
}

func resolveRuntimeSocket() string {
	if sock := strings.TrimSpace(os.Getenv("PERSONA_RUNTIME_SOCKET")); sock != "" {
		return sock
	}
	// Never dial the machine's live ~/.persona/runtime.sock from go test by default.
	if testing.Testing() {
		return ""
	}
	return runtime.DefaultPersonaSocketPath()
}

// Program is the extension program serving club. The club is closed when the program ends.
func Program(club *core.Club, version string) ext.Program {
	var hubPtr atomic.Pointer[ext.Hub]
	driver := browser.NewPersonaHubDriver(&hubPtr)
	return ext.Program{
		ID:       ID,
		Version:  version,
		Manifest: json.RawMessage(pac.Manifest),
		Init: func(ctx context.Context, init *ext.Init, hub *ext.Hub) (map[string]any, error) {
			hubPtr.Store(hub)
			var cfg Config
			if err := init.Config(&cfg); err != nil {
				return nil, err
			}
			if cfg.BrowserProviders {
				for _, site := range []browser.Site{browser.SiteMuse, browser.SiteClaudeDuet, browser.SiteGeminiTasks, browser.SiteDots} {
					if _, ok := club.Registry().Get(site.Provider); !ok {
						club.Registry().Register(browser.New(site, driver))
					}
				}
			}
			if strings.TrimSpace(cfg.Muse2APIURL) != "" {
				// Through the hub's egress proxy, like every provider (storage.go), but waiting as long for an answer's
				// headers as for the answer: muse2api answers in one piece once muse.ai has finished. A bad address
				// leaves muse2api out (listed as unavailable) rather than stopping the whole extension.
				client := HTTPClient()
				client.Transport.(*http.Transport).ResponseHeaderTimeout = muse2api.AnswerTimeout
				m, err := muse2api.New(muse2api.Options{Client: web.UseClient(client), URL: cfg.Muse2APIURL,
					Model: cfg.Muse2APIModel})
				if err != nil {
					slog.Warn("muse2api left out", "err", err)
				} else {
					club.Registry().Register(m)
				}
			}
			if cfg.DemoEcho {
				if _, ok := club.Registry().Get(echo.Name); !ok {
					club.Registry().Register(echo.New())
				}
			}
			if _, err := club.Space(ctx, core.DefaultSpace); err != nil {
				return nil, err
			}
			var bridgeDone chan struct{}
			if cfg.assistantRuntimeEnabled() {
				if sock := resolveRuntimeSocket(); sock != "" {
					dataDir := cmp.Or(init.Info().DataDir, ext.Env().Data)
					statePath := ""
					if dataDir != "" {
						statePath = filepath.Join(dataDir, "runtime-state.json")
					}
					bridge := runtime.New(runtime.Options{
						Client:     runtime.NewDirectClient(club, core.DefaultSpace),
						SocketPath: sock,
						StatePath:  statePath,
						Version:    version,
						ToolRelay:  true,
					})
					bridgeDone = make(chan struct{})
					go func() {
						defer close(bridgeDone)
						_ = bridge.Run(ctx)
					}()
				}
			}
			sub := club.Subscribe(core.DefaultSpace)
			go forward(ctx, sub, hub, club, bridgeDone)
			// The connector as the manifest has it: the hub wants the program to say it has it.
			var m struct {
				Contributes struct {
					Connectors json.RawMessage `json:"connectors"`
				} `json:"contributes"`
			}
			if err := json.Unmarshal(pac.Manifest, &m); err != nil {
				return nil, err
			}
			return map[string]any{"connectors": m.Contributes.Connectors}, nil
		},
		UI: map[string]ext.Page{AccountsUI: accountsPage(club, &hubPtr, driver)},
		Namespaces: map[string]ext.Handler{
			ext.ConnectorNamespace: toolsHandler(club),
			protocol.AgentSourceNamespace: func(ctx context.Context, method string, params json.RawMessage) (any, error) {
				if c := ext.Contribution(params); c != "" && c != Contribution {
					return nil, ext.NoContribution(c)
				}
				// The hub tells "no such method" (unknown_method) from "this source cannot" (unsupported).
				if !slices.Contains(protocol.AgentSourceMethods, method) {
					return nil, ext.UnknownMethod(protocol.AgentSourceNamespace + "." + method)
				}
				return club.Call(ctx, core.DefaultSpace, protocol.AgentSourceNamespace+"."+method, params)
			},
		},
	}
}

// forward sends the space's changes to the hub until the program ends, then closes the club. A change also redraws
// the accounts page where it is on screen (the hub coalesces these).
func forward(ctx context.Context, sub *core.Subscription, hub *ext.Hub, club *core.Club, bridgeDone <-chan struct{}) {
	defer club.Close()
	defer sub.Close()
	for {
		select {
		case <-ctx.Done():
			if bridgeDone != nil {
				<-bridgeDone
			}
			return
		case <-sub.Ready():
			for _, e := range sub.Take() {
				_ = hub.Notify(protocol.EventUpdated, protocol.UpdatedEvent{Contribution: Contribution, SessionKeys: e.SessionKeys})
			}
			_ = hub.Invalidate(AccountsUI)
		}
	}
}
