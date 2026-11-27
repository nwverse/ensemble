package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/nwverse/ensemble/internal/core"
	"github.com/nwverse/ensemble/internal/datadir"
)

// accountRow is one account as `accounts -json` prints it. There is no credential here: the vault is never opened.
type accountRow struct {
	AccountID        string `json:"accountId"`
	Provider         string `json:"provider"`
	DisplayName      string `json:"displayName"`
	Status           string `json:"status"`
	LastSyncedAt     int64  `json:"lastSyncedAt"`
	LastError        string `json:"lastError,omitempty"`
	Sessions         int    `json:"sessions"`
	Messages         int    `json:"messages"`
	LatestMessageMs  int64  `json:"latestMessageMs"`
	AssistantControl string `json:"assistantControl"`
	ShowQuota        bool   `json:"showQuota"`
}

func runAccounts(args []string, d deps) int {
	fs := flag.NewFlagSet("accounts", flag.ContinueOnError)
	fs.SetOutput(d.stderr)
	data := fs.String("data", defaultData(), "data directory")
	space := fs.String("space", core.DefaultSpace, "space to list")
	asJSON := fs.Bool("json", false, "print the accounts as JSON")
	if code, ok := parse(fs, args); !ok {
		return code
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(d.stderr, "accounts: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if !core.ValidSpace(*space) {
		fmt.Fprintf(d.stderr, "accounts: invalid space name %q\n", *space)
		return 2
	}
	rows, err := listAccounts(*data, *space)
	if err != nil {
		fmt.Fprintln(d.stderr, "accounts:", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(d.stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(map[string]any{"accounts": rows}); err != nil {
			return 1
		}
		return 0
	}
	printAccounts(d.stdout, rows)
	return 0
}

// listAccounts reads the space through the read-only store: no lock, nothing created, the vault never opened.
func listAccounts(data, space string) ([]accountRow, error) {
	if !datadir.HasSpace(data, space) {
		return nil, fmt.Errorf("no space %q in %s (nothing paired or imported there yet)", space, data)
	}
	ctx := context.Background()
	ro, err := datadir.OpenReadOnly(data, space)
	if err != nil {
		return nil, err
	}
	defer ro.Close()
	list, err := ro.ListAccounts(ctx)
	if err != nil {
		return nil, err
	}
	rows := make([]accountRow, 0, len(list))
	for _, a := range list {
		c, err := ro.Counts(ctx, a.AccountID)
		if err != nil {
			return nil, err
		}
		control, _ := core.ParseControl(a.AssistantControl)
		rows = append(rows, accountRow{AccountID: a.AccountID, Provider: a.Provider, DisplayName: a.DisplayName,
			Status: a.Status, LastSyncedAt: a.LastSyncedAt, LastError: a.LastError, Sessions: c.Sessions,
			Messages: c.Messages, LatestMessageMs: c.LatestMessageMs, AssistantControl: string(control), ShowQuota: a.ShowQuota})
	}
	return rows, nil
}

func printAccounts(w io.Writer, rows []accountRow) {
	if len(rows) == 0 {
		fmt.Fprintln(w, "no accounts")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ACCOUNT\tPROVIDER\tNAME\tSTATUS\tLAST SYNCED\tLAST ERROR\tSESSIONS\tMESSAGES\tASSISTANT\tSHOW QUOTA")
	for _, r := range rows {
		show := "no"
		if r.ShowQuota {
			show = "yes"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n", r.AccountID, r.Provider, cell(r.DisplayName), r.Status,
			when(r.LastSyncedAt), cell(r.LastError), r.Sessions, r.Messages, r.AssistantControl, show)
	}
	_ = tw.Flush()
}
