package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nwverse/ensemble/internal/datadir"
	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/provider/providertest"
)

// snapshot is every file under root: mode, size, modification time and contents' hash. A missing root is nil.
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		raw := []byte{}
		if fi.Mode().IsRegular() {
			raw, _ = os.ReadFile(p)
		}
		out[p] = fmt.Sprintf("%v %d %d %x", fi.Mode(), fi.Size(), fi.ModTime().UnixNano(), sha256.Sum256(raw))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func runCmd(t *testing.T, u *upstream, args ...string) (int, string, string) {
	t.Helper()
	d, out, errOut := testDeps(u)
	code := runWith(args, d)
	noSecrets(t, out.String(), errOut.String())
	return code, out.String(), errOut.String()
}

func importResult(t *testing.T, out string) protocol.ImportResult {
	t.Helper()
	var r protocol.ImportResult
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return r
}

func TestImportDryRunWritesNothing(t *testing.T) {
	u := newUpstream(t)
	data := filepath.Join(t.TempDir(), "data")
	bundle := writeBundle(t, testBundle(), 0o600)

	// into a directory that does not exist: it is not created, nothing is asked of a provider
	code, out, errOut := runCmd(t, u, "import", "-data", data, "-dry-run", bundle)
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if _, err := os.Stat(data); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a dry run created the data directory: %v", err)
	}
	if len(u.calls()) != 0 {
		t.Fatalf("a dry run called a provider: %+v", u.calls())
	}
	for _, want := range []string{grokAcct, groqAcct, cueAcct, "would import", "skipped: PROVIDER_UNSUPPORTED", "Cue · 小助手",
		"3 account(s) would be imported", "3 session(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output lacks %q:\n%s", want, out)
		}
	}

	// into an existing directory: byte for byte untouched
	if code, _, errOut := runCmd(t, u, "import", "-data", data, bundle); code != 0 {
		t.Fatalf("import: %d %s", code, errOut)
	}
	before := snapshot(t, data)
	calls := len(u.calls())
	code, out, errOut = runCmd(t, u, "import", "-data", data, "-dry-run", "-json", "-only-provider", "groq,cue", bundle)
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	if after := snapshot(t, data); !maps.Equal(before, after) {
		t.Fatalf("a dry run changed the directory:\n%v\n%v", before, after)
	}
	if len(u.calls()) != calls {
		t.Fatal("a dry run called a provider")
	}
	r := importResult(t, out)
	if !r.DryRun || len(r.Accounts) != 2 || len(r.Skipped) != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestImportIsIdempotentSendsNothingAndSkipsBrowserAccounts(t *testing.T) {
	u := newUpstream(t)
	data := t.TempDir()
	bundle := writeBundle(t, testBundle(), 0o600)
	code, out, errOut := runCmd(t, u, "import", "-data", data, "-json", bundle)
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errOut)
	}
	first := importResult(t, out)
	if len(first.Accounts) != 3 || len(first.Skipped) != 1 || first.Skipped[0].AccountID != museAcct ||
		first.Skipped[0].Reason != "PROVIDER_UNSUPPORTED" {
		t.Fatalf("%+v", first)
	}
	for _, a := range first.Accounts {
		if !a.Verified || !a.HasCredential {
			t.Errorf("%+v", a)
		}
	}
	code, out, _ = runCmd(t, u, "import", "-data", data, "-json", bundle)
	if again := importResult(t, out); code != 0 || !slices.Equal(again.Accounts, first.Accounts) {
		t.Fatalf("a second import differs: %+v", again)
	}
	// verification only: one read per account and import, no send, no sync
	for _, c := range u.calls() {
		if c.Path != "/openai/v1/models" && c.Path != "/aiserver.v1.DashboardService/GetMe" && c.Path != "/v2/agent.list" {
			t.Errorf("an import called %s %s", c.Method, c.Path)
		}
	}
	if n := len(u.calls()); n != 6 {
		t.Errorf("%d provider calls for two imports of three accounts", n)
	}

	code, out, errOut = runCmd(t, u, "accounts", "-data", data, "-json")
	if code != 0 {
		t.Fatalf("%d %s", code, errOut)
	}
	var listed struct{ Accounts []accountRow }
	if err := json.Unmarshal([]byte(out), &listed); err != nil || len(listed.Accounts) != 3 {
		t.Fatalf("%s %v", out, err)
	}
	for _, a := range listed.Accounts {
		if a.AssistantControl != "off" || a.ShowQuota || a.Provider == "muse" {
			t.Errorf("%+v", a)
		}
		if a.AccountID == grokAcct && (a.Sessions != 1 || a.Messages != 2 || a.LastSyncedAt != 1761900000000) {
			t.Errorf("%+v", a)
		}
		if a.AccountID == cueAcct && (a.Status != "error" || a.LastError != "AUTH_FAILED") {
			t.Errorf("%+v", a)
		}
	}
	code, out, _ = runCmd(t, u, "accounts", "-data", data)
	for _, want := range []string{"ACCOUNT", "SHOW QUOTA", grokAcct, "grok_bot", "Cue · 小助手", "AUTH_FAILED", "2025-10-31T"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Errorf("accounts lacks %q:\n%s", want, out)
		}
	}

	// the outbox: nothing that could go out; the skipped account's send is not there
	dir, err := datadir.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	st, v, err := dir.OpenSpace("default")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for id, want := range map[string]string{"1": "unconfirmed", "2": "failed", "3": ""} {
		o, ok, _ := st.Outbox(ctx, "00000000-0000-4000-8000-00000000000"+id)
		if o.Status != want || ok != (want != "") {
			t.Errorf("send %s: %+v", id, o)
		}
	}
	if _, ok, _ := v.Get(ctx, museAcct); ok {
		t.Error("the skipped account's credential was stored")
	}
	if c, ok, _ := v.Get(ctx, groqAcct); !ok || c != groqKey {
		t.Error("the groq key was not stored")
	}
	_ = dir.Close()
}

func TestImportRefusesAndReports(t *testing.T) {
	u := newUpstream(t)
	data := filepath.Join(t.TempDir(), "data")

	// a bundle others can read: refused before the directory is created
	code, _, errOut := runCmd(t, u, "import", "-data", data, writeBundle(t, testBundle(), 0o644))
	if code != 1 || !strings.Contains(errOut, "0600") {
		t.Fatalf("%d %s", code, errOut)
	}
	if _, err := os.Stat(data); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a refused bundle created the data directory")
	}
	if code, _, _ := runCmd(t, u, "import", "-data", data); code != 2 {
		t.Fatalf("no bundle: %d", code)
	}

	// a refused credential: imported, exit 3, the reason in the table
	u.answer(func(c providertest.Call) (providertest.Answer, bool) {
		return providertest.Answer{Status: 401}, c.Path == "/v2/agent.list"
	})
	bundle := writeBundle(t, testBundle(), 0o600)
	code, out, _ := runCmd(t, u, "import", "-data", data, bundle)
	if code != exitUnverified || !strings.Contains(out, "no: AUTH_FAILED") || !strings.Contains(out, "1 not verified") {
		t.Fatalf("%d\n%s", code, out)
	}
	u.answer(nil)

	// while another process owns the directory: refused; a dry run still works
	dir, err := datadir.Open(data, "")
	if err != nil {
		t.Fatal(err)
	}
	code, _, errOut = runCmd(t, u, "import", "-data", data, bundle)
	if code != 1 || !strings.Contains(errOut, "in use") || !strings.Contains(errOut, "-dry-run") {
		t.Fatalf("%d %s", code, errOut)
	}
	if code, _, errOut := runCmd(t, u, "import", "-data", data, "-dry-run", bundle); code != 0 {
		t.Fatalf("a dry run beside the owner: %d %s", code, errOut)
	}
	if code, out, errOut := runCmd(t, u, "accounts", "-data", data); code != 0 || !strings.Contains(out, groqAcct) {
		t.Fatalf("accounts beside the owner: %d %s %s", code, out, errOut)
	}
	_ = dir.Close()
}

func TestAccountsCreatesNothing(t *testing.T) {
	u := newUpstream(t)
	data := filepath.Join(t.TempDir(), "data")
	code, _, errOut := runCmd(t, u, "accounts", "-data", data)
	if code != 1 || !strings.Contains(errOut, "no space") {
		t.Fatalf("%d %s", code, errOut)
	}
	if _, err := os.Stat(data); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("accounts created the data directory")
	}
	if code, _, _ := runCmd(t, u, "import", "-data", data, writeBundle(t, testBundle(), 0o600)); code != 0 {
		t.Fatal(code)
	}
	before := snapshot(t, data)
	if code, _, _ := runCmd(t, u, "accounts", "-data", data, "-json"); code != 0 {
		t.Fatal(code)
	}
	if after := snapshot(t, data); !maps.Equal(before, after) {
		t.Fatal("accounts changed the directory")
	}
	if code, _, _ := runCmd(t, u, "accounts", "-data", data, "-space", "../x"); code != 2 {
		t.Fatal("an invalid space name was accepted")
	}
}

func TestOnlyProviderFlag(t *testing.T) {
	var l listFlag
	for _, v := range []string{"groq", " cue , grok_bot", ""} {
		_ = l.Set(v)
	}
	if !slices.Equal(l, listFlag{"groq", "cue", "grok_bot"}) {
		t.Fatalf("%v", l)
	}
}
