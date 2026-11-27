package main

import (
	"errors"
	"flag"
	"fmt"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/datadir"
	"github.com/nwverse/ensemble/internal/serve"
)

func runServe(args []string, d deps) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	data := fs.String("data", defaultData(), "data directory: the spaces' stores and credentials, the lock and the default key and socket")
	keyFile := fs.String("key-file", "", "the credentials' key file (default <data>/key, created on first start)")
	socket := fs.String("socket", "", "unix socket path (default <data>/club.sock; \"none\" to listen on TCP only)")
	listen := fs.String("listen", "", "TCP address to listen on as well, e.g. 127.0.0.1:7655 (needs -tokens)")
	tokensFile := fs.String("tokens", "", `tokens file: {"read": [...], "write": [...], "admin": [...]}; required with -listen`)
	demoEcho := fs.Bool("demo-echo", false, "register the in-memory echo provider (for demos and tests only)")
	poll := fs.Duration("poll", core.DefaultPollInterval, fmt.Sprintf("how often every account is synced; 0 turns polling off (at least %v)", core.MinPollInterval))
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(d.stderr, "serve: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *poll < 0 || (*poll > 0 && *poll < core.MinPollInterval) {
		fmt.Fprintf(d.stderr, "serve: -poll must be 0 (off) or at least %v\n", core.MinPollInterval)
		return 2
	}
	cfg := serve.Config{Socket: *socket, Addr: *listen}
	switch cfg.Socket {
	case "":
		cfg.Socket = filepath.Join(*data, "club.sock")
	case "none":
		cfg.Socket = ""
	}
	if *tokensFile != "" {
		t, err := serve.LoadTokens(*tokensFile)
		if err != nil {
			fmt.Fprintln(d.stderr, "serve:", err)
			return 1
		}
		cfg.Tokens = t
	}
	if err := cfg.Check(); err != nil {
		fmt.Fprintln(d.stderr, "serve:", err)
		return 1
	}
	reg, err := d.registry(*demoEcho)
	if err != nil {
		fmt.Fprintln(d.stderr, "serve:", err)
		return 1
	}
	dir, err := datadir.Open(*data, *keyFile)
	if errors.Is(err, datadir.ErrInUse) {
		fmt.Fprintln(d.stderr, inUse("serve", *data))
		return 1
	}
	if err != nil {
		fmt.Fprintln(d.stderr, "serve:", err)
		return 1
	}
	defer dir.Close()
	log := logger(d.stderr)
	club := core.New(core.Options{Version: version, Registry: reg, OpenSpace: dir.OpenSpace, PollInterval: *poll, Log: log})
	// Deferred after dir.Close, so it runs first: the club stops using the stores before they close.
	defer club.Close()
	ctx, stop := signal.NotifyContext(d.ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	// Open the spaces there are now: their outboxes recover, their polling starts, and a key that does not open
	// their credentials stops serve here rather than at the first send.
	spaces, err := datadir.Spaces(*data)
	if err != nil {
		fmt.Fprintln(d.stderr, "serve:", err)
		return 1
	}
	for _, name := range spaces {
		if !core.ValidSpace(name) {
			continue
		}
		if _, err := club.Space(ctx, name); err != nil {
			fmt.Fprintln(d.stderr, "serve:", err)
			return 1
		}
	}
	addrs, done, err := serve.Run(ctx, club, cfg)
	if err != nil {
		fmt.Fprintln(d.stderr, "serve:", err)
		return 1
	}
	for _, a := range addrs {
		log.Info("listening", "network", a.Network(), "address", a.String(), "tokens", cfg.Tokens != nil,
			"data", *data, "spaces", len(spaces), "poll", poll.String())
	}
	if d.listening != nil {
		d.listening(addrs)
	}
	if err := <-done; err != nil {
		fmt.Fprintln(d.stderr, "serve:", err)
		return 1
	}
	return 0
}
