package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nwverse/persona/sdk/go/exttest"

	pac "github.com/nwverse/personal-agent-club"
	"github.com/nwverse/personal-agent-club/internal/protocol"
)

func TestVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"version"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "protocol 1") {
		t.Fatalf("version: %d %q %q", code, out.String(), errOut.String())
	}
}

func TestServeRefusesTCPWithoutTokens(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "-socket", "none", "-listen", "127.0.0.1:0"},
		{"serve", "-socket", t.TempDir() + "/c.sock", "-listen", "127.0.0.1:0"},
	} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 1 || !strings.Contains(errOut.String(), "tokens") {
			t.Fatalf("%v: %d %q", args, code, errOut.String())
		}
	}
}

func TestUnknownCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"bogus"}, &out, &errOut); code != 2 {
		t.Fatalf("bogus: %d", code)
	}
}

// The built program, started the way the hub starts it, holds to the extension protocol.
func TestBinaryAsExtension(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the program")
	}
	bin := filepath.Join(t.TempDir(), "personal-agent-club")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	res := exttest.Check(t, pac.Manifest, exttest.Program(t, bin))
	if res.Version != pac.Version() {
		t.Fatalf("initialize answered version %q", res.Version)
	}
}

func TestServeFlags(t *testing.T) {
	u := newUpstream(t)
	data := filepath.Join(t.TempDir(), "data")
	for _, poll := range []string{"5s", "-1s"} {
		if code, _, errOut := runCmd(t, u, "serve", "-data", data, "-poll", poll); code != 2 || !strings.Contains(errOut, "-poll") {
			t.Fatalf("-poll %s: %d %s", poll, code, errOut)
		}
	}
	if _, err := os.Stat(data); err == nil {
		t.Fatal("a refused serve created the data directory")
	}
	if code, _, _ := runCmd(t, u, "serve", "-data", data, "extra"); code != 2 {
		t.Fatal("an argument was accepted")
	}
}

func TestServeOwnsItsDataDirectory(t *testing.T) {
	u := newUpstream(t)
	data := shortDir(t)
	sock := filepath.Join(data, "club.sock")
	stop := startServe(t, u, "-data", data, "-poll", "0", "-demo-echo")
	var info struct {
		Result protocol.ClubInfo `json:"result"`
	}
	resp, err := serveHTTP(sock).Post("http://club/v1/spaces/default/rpc", "application/json", strings.NewReader(`{"method":"club.info"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = json.NewDecoder(resp.Body).Decode(&info)
	resp.Body.Close()
	available := map[string]bool{}
	for _, p := range info.Result.Providers {
		available[p.Provider] = p.Available
	}
	if !available["groq"] || !available["cue"] || !available["grok_bot"] || !available["echo"] || available["muse"] {
		t.Fatalf("%+v", info.Result)
	}
	for _, f := range []string{"club.lock", "key"} {
		if _, err := os.Stat(filepath.Join(data, f)); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	// a second serve, and an import, on the same directory are refused
	code, _, errOut := runCmd(t, u, "serve", "-data", data, "-socket", filepath.Join(data, "other.sock"))
	if code != 1 || !strings.Contains(errOut, "in use") {
		t.Fatalf("a second serve: %d %s", code, errOut)
	}
	if code, errOut := stop(); code != 0 {
		t.Fatalf("serve ended with %d: %s", code, errOut)
	}
	if _, err := os.Stat(sock); err == nil {
		t.Fatal("the socket was left behind")
	}
}

func TestServeRefusesAWrongKey(t *testing.T) {
	u := newUpstream(t)
	data := shortDir(t)
	if code, _, errOut := runCmd(t, u, "import", "-data", data, writeBundle(t, testBundle(), 0o600)); code != 0 {
		t.Fatal(errOut)
	}
	sealed, _ := os.ReadFile(filepath.Join(data, "spaces", "default", "credentials.enc"))
	code, _, errOut := runCmd(t, u, "serve", "-data", data, "-poll", "0", "-key-file", filepath.Join(data, "other.key"))
	if code != 1 || !strings.Contains(errOut, "does not open with this key") {
		t.Fatalf("%d %s", code, errOut)
	}
	if now, _ := os.ReadFile(filepath.Join(data, "spaces", "default", "credentials.enc")); !bytes.Equal(now, sealed) {
		t.Fatal("the credentials changed")
	}
	if _, err := os.Stat(filepath.Join(data, "club.sock")); err == nil {
		t.Fatal("serve listened")
	}
}
