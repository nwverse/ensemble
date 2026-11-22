package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/nwverse/personal-agent-club/internal/core"
	"github.com/nwverse/personal-agent-club/internal/datadir"
	"github.com/nwverse/personal-agent-club/internal/runtime"
)

func runRuntime(args []string, d deps) int {
	fs := flag.NewFlagSet("runtime", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	data := fs.String("data", defaultData(), "data directory (stores state and default club.sock; in -standalone also holds accounts and credentials)")
	keyFile := fs.String("key-file", "", "credentials key file (-standalone only, default <data>/key)")
	socket := fs.String("socket", "", "serve unix socket path (default <data>/club.sock)")
	clubURL := fs.String("club-url", "", "serve HTTP(S) base URL instead of unix socket, e.g. http://127.0.0.1:7655")
	token := fs.String("token", "", "bearer token for serve HTTP API (or $PERSONAL_AGENT_CLUB_TOKEN)")
	standalone := fs.Bool("standalone", false, "open <data> in-process instead of dialing a running serve instance")
	demoEcho := fs.Bool("demo-echo", false, "register the in-memory echo provider (-standalone only)")
	space := fs.String("space", core.DefaultSpace, "club space name to bridge")
	personaSock := fs.String("persona-socket", runtime.DefaultPersonaSocketPath(), "Persona runtime unix socket path (or $PERSONA_RUNTIME_SOCKET)")
	runtimeID := fs.String("runtime-id", "personal-agent-club", "runtime ID advertised to Persona")
	accountID := fs.String("account", "", "expose only this paired account ID as an assistant backend")
	allAccounts := fs.Bool("all-accounts", false, "expose all non-paused accounts even if assistant_backend is not enabled")
	launcher := fs.String("launcher", "", "override the primary launcher name offered to Persona")
	toolRelay := fs.Bool("tool-relay", true, "enable <persona-tool-call> relay for chat-only providers")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(d.stderr, "runtime: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if !core.ValidSpace(*space) {
		fmt.Fprintf(d.stderr, "runtime: invalid space name %q\n", *space)
		return 2
	}
	if *demoEcho && !*standalone {
		fmt.Fprintln(d.stderr, "runtime: -demo-echo requires -standalone")
		return 2
	}

	log := logger(d.stderr)
	ctx, stop := signal.NotifyContext(d.ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var client runtime.ClubClient
	if *standalone {
		reg, err := d.registry(*demoEcho)
		if err != nil {
			fmt.Fprintln(d.stderr, "runtime:", err)
			return 1
		}
		dir, err := datadir.Open(*data, *keyFile)
		if errors.Is(err, datadir.ErrInUse) {
			fmt.Fprintln(d.stderr, inUse("runtime", *data))
			return 1
		}
		if err != nil {
			fmt.Fprintln(d.stderr, "runtime:", err)
			return 1
		}
		defer dir.Close()
		club := core.New(core.Options{
			Version:      version,
			Registry:     reg,
			OpenSpace:    dir.OpenSpace,
			PollInterval: core.DefaultPollInterval,
			Log:          log,
		})
		defer club.Close()
		spaces, err := datadir.Spaces(*data)
		if err != nil {
			fmt.Fprintln(d.stderr, "runtime:", err)
			return 1
		}
		for _, name := range spaces {
			if !core.ValidSpace(name) {
				continue
			}
			if _, err := club.Space(ctx, name); err != nil {
				fmt.Fprintln(d.stderr, "runtime:", err)
				return 1
			}
		}
		if _, err := club.Space(ctx, *space); err != nil {
			fmt.Fprintln(d.stderr, "runtime:", err)
			return 1
		}
		client = runtime.NewDirectClient(club, *space)
	} else {
		target := strings.TrimSpace(*clubURL)
		if target == "" {
			target = strings.TrimSpace(*socket)
		}
		if target == "" {
			target = filepath.Join(*data, "club.sock")
		}
		tok := strings.TrimSpace(*token)
		if tok == "" {
			tok = strings.TrimSpace(os.Getenv("PERSONAL_AGENT_CLUB_TOKEN"))
		}
		sc, err := runtime.NewServeClient(target, *space, tok)
		if err != nil {
			fmt.Fprintln(d.stderr, "runtime:", err)
			return 1
		}
		client = sc
	}

	bridge := runtime.New(runtime.Options{
		Client:      client,
		SocketPath:  *personaSock,
		StatePath:   filepath.Join(*data, "runtime-state.json"),
		RuntimeID:   *runtimeID,
		Version:     version,
		AccountID:   strings.TrimSpace(*accountID),
		Launcher:    strings.TrimSpace(*launcher),
		AllAccounts: *allAccounts,
		ToolRelay:   *toolRelay,
		Log:         log,
	})
	log.Info("bridging club runtime", "runtime", *runtimeID, "persona_socket", *personaSock, "space", *space, "standalone", *standalone)
	if err := bridge.Run(ctx); err != nil {
		fmt.Fprintln(d.stderr, "runtime:", err)
		return 1
	}
	return 0
}
