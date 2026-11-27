// Command personal-agent-club runs club: started by a Persona hub as an extension (no arguments; the hub speaks the
// extension protocol on stdin and stdout), or on its own with `serve` for the Lark gateway, with `import` and
// `accounts` as the tools around a serve's data directory.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/nwverse/persona/sdk/go/ext"

	pac "github.com/nwverse/ensemble"
	"github.com/nwverse/ensemble/internal/extension"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/providers"
)

// version may be set at build time (-ldflags "-X main.version=..."); by default it is the manifest's.
var version = ""

const usage = `personal-agent-club: personal agents as one source, for Persona and the Lark gateway.

Usage:
  personal-agent-club                       run as a Persona extension (the hub starts it this way)
  personal-agent-club serve [flags]         serve the protocol over HTTP on a unix socket (and optionally TCP)
  personal-agent-club runtime [flags]       bridge club accounts into Persona's runtime.sock as an assistant runtime
  personal-agent-club import [flags] FILE   import a bundle exported from the Lark gateway into a data directory
  personal-agent-club accounts [flags]      list a space's accounts, read-only (works while serve runs)
  personal-agent-club version               print the version
  personal-agent-club manifest              print the Persona extension manifest

Run "personal-agent-club <command> -h" for a command's flags. Provider requests go through
$PERSONAL_AGENT_CLUB_PROXY when it is set (socks5://, socks5h://, http:// or https://), else the usual
HTTP_PROXY / HTTPS_PROXY / NO_PROXY; $PERSONAL_AGENT_CLUB_GROQ_MODEL picks the model Groq accounts ask;
$PERSONAL_AGENT_CLUB_MUSE2API_URL (and _MODEL) add muse2api, your own muse2api server.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// deps are what the commands take from outside the program; tests replace them.
type deps struct {
	stdout, stderr io.Writer
	// ctx ends serve (as SIGINT and SIGTERM do).
	ctx context.Context
	// registry builds the providers; demoEcho adds the echo provider.
	registry func(demoEcho bool) (*provider.Registry, error)
	// listening, when set, is told serve's addresses once it listens.
	listening func([]net.Addr)
}

func defaultDeps(stdout, stderr io.Writer) deps {
	return deps{stdout: stdout, stderr: stderr, ctx: context.Background(), registry: func(demoEcho bool) (*provider.Registry, error) {
		c := providers.FromEnv()
		c.DemoEcho = demoEcho
		return providers.Registry(c)
	}}
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWith(args, defaultDeps(stdout, stderr))
}

func runWith(args []string, d deps) int {
	if version == "" {
		version = pac.Version()
	}
	if len(args) == 0 || args[0] == "manifest" {
		if len(args) == 0 && isTerminal(os.Stdin) && ext.Env().ID == "" {
			fmt.Fprint(d.stderr, usage)
			return 2
		}
		club, _, err := extension.NewClub(version, logger(d.stderr))
		if err != nil {
			fmt.Fprintln(d.stderr, "personal-agent-club:", err)
			return 1
		}
		ext.Main(extension.Program(club, version))
		// The program's end closes the club too (and with it the data directory); Close waits for that.
		club.Close()
		return 0
	}
	switch args[0] {
	case "version", "--version", "-v":
		fmt.Fprintf(d.stdout, "personal-agent-club %s (protocol %d)\n", version, protocol.Version)
		return 0
	case "serve":
		return runServe(args[1:], d)
	case "runtime":
		return runRuntime(args[1:], d)
	case "import":
		return runImport(args[1:], d)
	case "accounts":
		return runAccounts(args[1:], d)
	case "help", "-h", "--help":
		fmt.Fprint(d.stdout, usage)
		return 0
	}
	fmt.Fprintf(d.stderr, "unknown command %q\n\n%s", args[0], usage)
	return 2
}

// defaultData is the data directory when -data is not given.
func defaultData() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".personal-agent-club")
}

// parse parses a command's flags: ok false with the exit code when the program should stop (-h is 0, a bad flag 2).
func parse(fs *flag.FlagSet, args []string) (code int, ok bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, false
		}
		return 2, false
	}
	return 0, true
}

// inUse is the message for datadir.ErrInUse.
func inUse(cmd, data string) string {
	return fmt.Sprintf("%s: %s is in use by another personal-agent-club process (a serve or the Persona extension); stop it first", cmd, data)
}

func logger(w io.Writer) *slog.Logger { return slog.New(slog.NewTextHandler(w, nil)) }

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}
