package takeover

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseReport(t *testing.T) {
	r, err := ParseReport("name=grok node=ready pair=missing ssh=missing already=no")
	if err != nil || r.Node != "grok" || !r.NodeReady || r.Paired || r.Phase() != PhasePair {
		t.Fatalf("%+v %v", r, err)
	}
	if _, err := ParseReport("name=grok node=ready pair=ready ssh=ready already=no token=secret"); err == nil {
		t.Fatal("an unknown field was accepted")
	}
	for _, line := range []string{
		"name=grok BEGIN OPENSSH PRIVATE KEY",
		"name=grok database_url=postgres://x",
		`name=grok "psk"=abc`,
	} {
		if _, err := ParseReport(line); err == nil {
			t.Fatalf("a secret was accepted: %s", line)
		}
	}
	already, err := ParseReport("name=grok node=ready pair=ready ssh=ready already=yes")
	if err != nil || already.Phase() != PhaseLinked {
		t.Fatalf("already: %+v %v", already, err)
	}
}

func TestStepNamesNoSecret(t *testing.T) {
	for _, phase := range []string{PhaseNode, PhasePair, PhaseSSH} {
		text, err := Step(phase, "grok")
		if err != nil {
			t.Fatal(err)
		}
		low := strings.ToLower(text)
		for _, bad := range []string{"private key", "database_url", "password", "10.", "192.168", "BEGIN "} {
			if strings.Contains(low, bad) {
				t.Fatalf("%s step contains %q:\n%s", phase, bad, text)
			}
		}
	}
	if _, err := Step(PhasePlan, "grok"); err == nil {
		t.Fatal("a plan is not a step for the box")
	}
	if _, err := Step(PhaseNode, "Bad Name"); err == nil {
		t.Fatal("a bad node name was accepted")
	}
}

func TestPrepareAndApplyAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	orig := []byte("{\n  \"hub\": \"kept\",\n  \"machines\": [{\"name\": \"other\", \"kind\": \"ssh\", \"ssh_host\": \"other\"}]\n}\n")
	if err := os.WriteFile(path, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(path, "grok", "/home/box/.persona/bin/persona-node")
	if err != nil || plan.AlreadyLinked || plan.Kind != "ssh" || plan.SSHHost != "grok" {
		t.Fatalf("prepare: %+v %v", plan, err)
	}
	after, _ := os.ReadFile(path)
	if string(after) != string(orig) {
		t.Fatal("prepare wrote the file")
	}

	backup, err := Apply(path, plan, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := os.ReadFile(backup)
	if string(saved) != string(orig) {
		t.Fatalf("backup is not the original:\n%s", saved)
	}
	wrote, _ := os.ReadFile(path)
	if !strings.Contains(string(wrote), `"name": "grok"`) || !strings.Contains(string(wrote), `"hub": "kept"`) {
		t.Fatalf("written:\n%s", wrote)
	}
	if _, err := Apply(path, plan, time.Now()); err == nil {
		t.Fatal("a second apply overwrote the entry")
	}
	again, err := Prepare(path, "grok", "/home/box/.persona/bin/persona-node")
	if err != nil || !again.AlreadyLinked {
		t.Fatalf("second prepare: %+v %v", again, err)
	}
	if _, err := Apply(path, again, time.Now()); err == nil {
		t.Fatal("an already-linked plan was written")
	}
}

func TestApplyRefusesAPartialPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	os.WriteFile(path, []byte(`{"machines":[]}`), 0o600)
	if _, err := Apply(path, Plan{Node: "grok"}, time.Now()); err == nil {
		t.Fatal("a plan without a kind was written")
	}
}

// TestApplyPreservesExistingMachineFieldsAndSetsID guards against truncating existing Persona MachineConfig entries
// (id, trust, address, icon, launchers, claude_start_dir, tailcat_addr, etc.), verifies the appended entry sets the
// required Persona id, and verifies Plan.Summary does not promise a tailcat fallback address.
func TestApplyPreservesExistingMachineFieldsAndSetsID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manager.json")
	before := `{"token":"t","machines":[{"id":"m-opc","name":"opc","trust":"work","kind":"service","address":"10.0.0.2:7000","icon":"laptop","launchers":[{"name":"cc"}],"claude_start_dir":"~/.claude-work","tailcat_addr":"tc-opc"}]}`
	if err := os.WriteFile(path, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	plan, err := Prepare(path, "grok", "~/.persona/bin/persona-node")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan.Summary, "no tailcat fallback") || strings.Contains(plan.Summary, "keeping its tailcat") {
		t.Fatalf("summary does not match ssh-only write: %q", plan.Summary)
	}
	if _, err := Apply(path, plan, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Token    string           `json:"token"`
		Machines []map[string]any `json:"machines"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal written manager.json: %v\n%s", err, raw)
	}
	if got.Token != "t" {
		t.Fatalf("token = %q, want t", got.Token)
	}
	if len(got.Machines) != 2 {
		t.Fatalf("machines len = %d, want 2:\n%s", len(got.Machines), raw)
	}

	wantExisting := map[string]any{
		"id":               "m-opc",
		"name":             "opc",
		"trust":            "work",
		"kind":             "service",
		"address":          "10.0.0.2:7000",
		"icon":             "laptop",
		"launchers":        []any{map[string]any{"name": "cc"}},
		"claude_start_dir": "~/.claude-work",
		"tailcat_addr":     "tc-opc",
	}
	if !reflect.DeepEqual(got.Machines[0], wantExisting) {
		t.Fatalf("existing machine entry altered:\n got: %#v\nwant: %#v", got.Machines[0], wantExisting)
	}

	wantAdded := map[string]any{
		"id":         "grok",
		"name":       "grok",
		"kind":       "ssh",
		"ssh_host":   "grok",
		"remote_bin": "~/.persona/bin/persona-node",
	}
	if !reflect.DeepEqual(got.Machines[1], wantAdded) {
		t.Fatalf("added machine entry mismatch:\n got: %#v\nwant: %#v", got.Machines[1], wantAdded)
	}

	// Matching by id even when name differs must report AlreadyLinked and leave the file untouched.
	byIDPath := filepath.Join(dir, "by-id.json")
	byIDOrig := []byte(`{"machines":[{"id":"grok","name":"Grok Box","kind":"ssh","ssh_host":"grok"}]}`)
	if err := os.WriteFile(byIDPath, byIDOrig, 0o600); err != nil {
		t.Fatal(err)
	}
	linked, err := Prepare(byIDPath, "grok", "~/.persona/bin/persona-node")
	if err != nil || !linked.AlreadyLinked {
		t.Fatalf("prepare by id: %+v %v", linked, err)
	}
	if _, err := Apply(byIDPath, Plan{Node: "grok", Kind: "ssh", SSHHost: "grok"}, time.Unix(0, 0)); err == nil {
		t.Fatal("apply wrote duplicate id")
	}
	if after, _ := os.ReadFile(byIDPath); string(after) != string(byIDOrig) {
		t.Fatalf("file changed on duplicate id:\n%s", after)
	}
}
