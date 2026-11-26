package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/nwverse/ensemble/internal/protocol"
)

// A version-2 store gains name_source and stays version 2: its rows read "" (who named them is settled on their next sync), new rows
// and patches keep theirs.
func TestNameSourceMigratesFromVersion2AndStaysOpenableByOlderPrograms(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "club.db")
	db, err := sql.Open("sqlite", "file:"+path)
	must(t, err)
	_, err = db.Exec(`CREATE TABLE agent_accounts (account_id text PRIMARY KEY NOT NULL, provider text NOT NULL,
  display_name text NOT NULL, status text NOT NULL, external_account_id text, bridge_token_hash text,
  last_synced_at integer, last_error text, created_at integer NOT NULL, updated_at integer NOT NULL,
  assistant_control text NOT NULL DEFAULT 'off', show_quota integer NOT NULL DEFAULT 0, quota_display text NOT NULL DEFAULT '');
INSERT INTO agent_accounts (account_id, provider, display_name, status, created_at, updated_at)
  VALUES ('a000000000001', 'grok_bot', 'Grok Bot', 'active', 1, 1);
PRAGMA user_version = 2;`)
	must(t, err)
	must(t, db.Close())
	s, err := OpenSQLite(path)
	must(t, err)
	defer s.Close()
	old, ok, err := s.Account(ctx, "a000000000001")
	if err != nil || !ok || old.DisplayName != "Grok Bot" || old.NameSource != "" {
		t.Fatalf("%+v %v %v", old, ok, err)
	}
	must(t, s.InsertAccount(ctx, Account{AccountID: "a000000000002", Provider: "cue", DisplayName: "Cue-Pro", NameSource: NameOwner,
		Status: protocol.AccountActive, CreatedAt: 2, UpdatedAt: 2}))
	auto := NameAuto
	must(t, s.UpdateAccount(ctx, "a000000000001", AccountPatch{NameSource: &auto}, 3))
	list, err := s.ListAccounts(ctx)
	must(t, err)
	if len(list) != 2 || list[0].NameSource != NameAuto || list[1].NameSource != NameOwner {
		t.Fatalf("%+v", list)
	}
	var v int
	must(t, s.db.QueryRow(`PRAGMA user_version`).Scan(&v))
	if v != 2 { // unchanged: 0.6.0 still opens the store
		t.Fatal(v)
	}
}

// What 0.6.0 does with a store this version opened: its own column list, schema check and writes still work.
func TestAnOlderProgramStillOpensAndWritesTheStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "club.db")
	s, err := OpenSQLite(path)
	must(t, err)
	must(t, s.InsertAccount(ctx, Account{AccountID: "a000000000001", Provider: "cue", DisplayName: "Cue-Pro", NameSource: NameOwner,
		Status: protocol.AccountActive, CreatedAt: 1, UpdatedAt: 1}))
	must(t, s.Close())
	db, err := sql.Open("sqlite", "file:"+path)
	must(t, err)
	defer db.Close()
	var v int
	must(t, db.QueryRow(`PRAGMA user_version`).Scan(&v))
	if v > 2 {
		t.Fatalf("user_version %d: 0.6.0 refuses a store above 2", v)
	}
	// 0.6.0's INSERT OR REPLACE and UPDATE name only the columns it knows.
	_, err = db.Exec(`INSERT OR REPLACE INTO agent_accounts (account_id, provider, display_name, status, external_account_id,
  last_synced_at, last_error, assistant_control, show_quota, quota_display, created_at, updated_at)
  VALUES ('a000000000002', 'groq', 'Groq', 'active', NULL, NULL, NULL, 'off', 0, '', 2, 2)`)
	must(t, err)
	_, err = db.Exec(`UPDATE agent_accounts SET display_name = 'Cue-Pro 2', updated_at = 3 WHERE account_id = 'a000000000001'`)
	must(t, err)
	must(t, db.Close())
	s, err = OpenSQLite(path)
	must(t, err)
	defer s.Close()
	list, err := s.ListAccounts(ctx)
	must(t, err)
	if len(list) != 2 || list[0].NameSource != NameOwner || list[0].DisplayName != "Cue-Pro 2" || list[1].NameSource != "" {
		t.Fatalf("%+v", list)
	}
}

// A store 0.6.0 wrote and nothing has migrated yet reads fine read-only (personal-agent-club accounts, import
// planning): every account's name source is "".
func TestReadOnlyOpenOfAnUnmigratedStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "club.db")
	db, err := sql.Open("sqlite", "file:"+path)
	must(t, err)
	_, err = db.Exec(`CREATE TABLE agent_accounts (account_id text PRIMARY KEY NOT NULL, provider text NOT NULL,
  display_name text NOT NULL, status text NOT NULL, external_account_id text, bridge_token_hash text,
  last_synced_at integer, last_error text, created_at integer NOT NULL, updated_at integer NOT NULL,
  assistant_control text NOT NULL DEFAULT 'off', show_quota integer NOT NULL DEFAULT 0, quota_display text NOT NULL DEFAULT '');
INSERT INTO agent_accounts (account_id, provider, display_name, status, created_at, updated_at)
  VALUES ('a000000000001', 'grok_bot', 'Grok Bot', 'active', 1, 1);
PRAGMA user_version = 2;`)
	must(t, err)
	must(t, db.Close())
	for _, immutable := range []bool{false, true} {
		ro, err := OpenSQLiteReadOnly(path, immutable)
		must(t, err)
		list, err := ro.ListAccounts(ctx)
		must(t, err)
		a, ok, err := ro.Account(ctx, "a000000000001")
		must(t, err)
		if len(list) != 1 || list[0].DisplayName != "Grok Bot" || list[0].NameSource != "" || !ok || a.NameSource != "" {
			t.Fatalf("immutable=%v: %+v %+v", immutable, list, a)
		}
		must(t, ro.Close())
	}
	// Migrated, the same read-only open reads the column.
	s, err := OpenSQLite(path)
	must(t, err)
	owner := NameOwner
	must(t, s.UpdateAccount(ctx, "a000000000001", AccountPatch{NameSource: &owner}, 2))
	must(t, s.Close())
	ro, err := OpenSQLiteReadOnly(path, false)
	must(t, err)
	defer ro.Close()
	if a, _, _ := ro.Account(ctx, "a000000000001"); a.NameSource != NameOwner {
		t.Fatalf("%+v", a)
	}
}

func TestUpdateAccountIfOnlyOntoTheRowAsRead(t *testing.T) {
	ctx := context.Background()
	for name, s := range map[string]Store{"memory": NewMemory(), "sqlite": func() Store {
		x, err := OpenSQLite(filepath.Join(t.TempDir(), "club.db"))
		must(t, err)
		t.Cleanup(func() { _ = x.Close() })
		return x
	}()} {
		must(t, s.InsertAccount(ctx, Account{AccountID: "a000000000001", Provider: "grok_bot", DisplayName: "Grok Bot",
			Status: protocol.AccountActive, CreatedAt: 1, UpdatedAt: 1}))
		renamed, auto := "Grok Bot · Ada", NameAuto
		patch := AccountPatch{DisplayName: &renamed, NameSource: &auto}
		if ok, err := s.UpdateAccountIf(ctx, "a000000000001", AccountMatch{DisplayName: "Other", NameSource: ""}, patch, 2); ok || err != nil {
			t.Fatalf("%s: applied onto a changed row: %v %v", name, ok, err)
		}
		if ok, err := s.UpdateAccountIf(ctx, "a000000000002", AccountMatch{}, patch, 2); ok || err != nil {
			t.Fatalf("%s: no such row: %v %v", name, ok, err)
		}
		if ok, err := s.UpdateAccountIf(ctx, "a000000000001", AccountMatch{DisplayName: "Grok Bot", NameSource: ""}, patch, 2); !ok || err != nil {
			t.Fatalf("%s: %v %v", name, ok, err)
		}
		a, _, _ := s.Account(ctx, "a000000000001")
		if a.DisplayName != renamed || a.NameSource != NameAuto {
			t.Fatalf("%s: %+v", name, a)
		}
	}
}
