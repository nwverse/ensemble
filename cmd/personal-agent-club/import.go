package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/datadir"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider"
	"github.com/nwverse/ensemble/internal/store"
)

// exitUnverified is import's exit code when the import was written but at least one imported account did not
// verify (its credential was refused, missing, or belongs to another remote account).
const exitUnverified = 3

// listFlag is a flag that may be given more than once, each time one value or several separated by commas.
type listFlag []string

func (l *listFlag) String() string { return strings.Join(*l, ",") }

func (l *listFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*l = append(*l, p)
		}
	}
	return nil
}

func runImport(args []string, d deps) int {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	fs.Usage = func() {
		fmt.Fprint(d.stderr, `Usage: personal-agent-club import [flags] BUNDLE.json

Imports a bundle exported from the Lark gateway (docs/import-bundle.md) into a space of a data directory: accounts,
sessions, messages, read state, the outbox and the credentials, keeping every id. Accounts of providers this build
cannot reach (the browser ones: muse, dots, claude_duet) are skipped. Nothing is ever sent; each imported account is
then verified read-only with its provider. The directory must not be in use (stop serve first); -dry-run writes
nothing and works beside a running serve.

Exit codes: 0 imported (or checked), 1 error, 2 usage, 3 imported but an imported account did not verify.

Flags:
`)
		fs.PrintDefaults()
	}
	data := fs.String("data", defaultData(), "data directory")
	keyFile := fs.String("key-file", "", "the credentials' key file (default <data>/key, created when missing)")
	space := fs.String("space", core.DefaultSpace, "space to import into")
	var only listFlag
	fs.Var(&only, "only-provider", "import only this provider's accounts (repeatable, or comma-separated)")
	dryRun := fs.Bool("dry-run", false, "check the bundle and show what would be imported; write nothing, verify nothing")
	asJSON := fs.Bool("json", false, "print the result as JSON (club.import's answer)")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	if !core.ValidSpace(*space) {
		fmt.Fprintf(d.stderr, "import: invalid space name %q\n", *space)
		return 2
	}
	path, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		fmt.Fprintln(d.stderr, "import:", err)
		return 1
	}
	reg, err := d.registry(false)
	if err != nil {
		fmt.Fprintln(d.stderr, "import:", err)
		return 1
	}
	opts := core.ImportOptions{OnlyProviders: only}
	// The plan first, without the lock and writing nothing: a bundle that would be refused never creates or touches
	// the data directory.
	plan, err := planImport(*data, *space, path, reg, opts)
	if err != nil {
		fmt.Fprintln(d.stderr, "import:", err)
		return 1
	}
	res := plan
	if !*dryRun {
		if res, err = importBundle(d.ctx, *data, *keyFile, *space, path, reg, opts, logger(d.stderr)); err != nil {
			if errors.Is(err, datadir.ErrInUse) {
				fmt.Fprintln(d.stderr, inUse("import", *data)+" (or use -dry-run, which works beside it)")
			} else {
				fmt.Fprintln(d.stderr, "import:", err)
			}
			return 1
		}
	}
	if *asJSON {
		enc := json.NewEncoder(d.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(res); err != nil {
			return 1
		}
	} else {
		printImport(d.stdout, res)
	}
	for _, a := range res.Accounts {
		if !res.DryRun && !a.Verified {
			return exitUnverified
		}
	}
	return 0
}

// planImport is a dry run against the space as it is: its accounts read through the read-only store when it exists
// (no lock, so a running serve is fine), none otherwise. Nothing is created.
func planImport(data, space, path string, reg *provider.Registry, opts core.ImportOptions) (protocol.ImportResult, error) {
	var existing []store.Account
	if datadir.HasSpace(data, space) {
		ro, err := datadir.OpenReadOnly(data, space)
		if err != nil {
			return protocol.ImportResult{}, err
		}
		existing, err = ro.ListAccounts(context.Background())
		_ = ro.Close()
		if err != nil {
			return protocol.ImportResult{}, err
		}
	}
	return core.PlanImport(path, reg, existing, opts)
}

// importBundle owns the data directory for the import: a club that never sends (no outbox recovery, no follow-ups,
// no watches, no polling) writes the rows and credentials, then verifies each account read-only.
func importBundle(ctx context.Context, data, keyFile, space, path string, reg *provider.Registry, opts core.ImportOptions,
	log *slog.Logger) (protocol.ImportResult, error) {
	dir, err := datadir.Open(data, keyFile)
	if err != nil {
		return protocol.ImportResult{}, err
	}
	defer dir.Close()
	club := core.New(core.Options{Version: version, Registry: reg, OpenSpace: dir.OpenSpace, NoRecovery: true,
		FollowUps: []time.Duration{}, WatchLimit: -1, Log: log})
	defer club.Close()
	s, err := club.Space(ctx, space)
	if err != nil {
		return protocol.ImportResult{}, err
	}
	return s.Import(ctx, path, opts)
}

func printImport(w io.Writer, res protocol.ImportResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tPROVIDER\tNAME\tACTION\tSESSIONS\tMESSAGES\tLATEST MESSAGE\tVERIFIED")
	action := "imported"
	if res.DryRun {
		action = "would import"
	}
	var sessions, messages, unverified int
	for _, a := range res.Accounts {
		verified := "yes"
		switch {
		case res.DryRun:
			verified = "not checked (dry run)"
			if !a.HasCredential {
				verified += ", no credential"
			}
		case !a.Verified:
			verified = "no: " + a.Error
			unverified++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", a.AccountID, a.Provider, cell(a.DisplayName), action,
			a.Sessions, a.Messages, when(a.LatestMessageMs), verified)
		sessions += a.Sessions
		messages += a.Messages
	}
	for _, s := range res.Skipped {
		fmt.Fprintf(tw, "%s\t%s\t%s\tskipped: %s\t-\t-\t-\t-\n", s.AccountID, s.Provider, cell(s.DisplayName), s.Reason)
	}
	_ = tw.Flush()
	verb := "imported"
	if res.DryRun {
		verb = "would be imported (dry run: nothing written)"
	}
	fmt.Fprintf(w, "\n%d account(s) %s with %d session(s) and %d message(s); %d skipped", len(res.Accounts), verb,
		sessions, messages, len(res.Skipped))
	if !res.DryRun {
		fmt.Fprintf(w, "; %d not verified", unverified)
	}
	fmt.Fprintln(w, ".")
}

// cell is a table cell: one line, never empty.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return "-"
	}
	return s
}

func when(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339)
}
