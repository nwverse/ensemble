package datadir

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nwverse/ensemble/internal/protocol"
	"github.com/nwverse/ensemble/internal/store"
)

func TestOneOwnerAndDataThatLasts(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "data")
	d, err := Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root, ""); !errors.Is(err, ErrInUse) {
		t.Fatalf("a second owner: %v", err)
	}
	st, v, err := d.OpenSpace("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAccount(ctx, store.Account{AccountID: "a000000000001", Provider: "groq", DisplayName: "Groq",
		Status: protocol.AccountActive, CreatedAt: 1, UpdatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	if err := v.Put(ctx, "a000000000001", "fixture-key-not-real"); err != nil {
		t.Fatal(err)
	}
	ro, err := OpenReadOnly(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	if list, _ := ro.ListAccounts(ctx); len(list) != 1 {
		t.Fatalf("%+v", list)
	}
	if err := ro.InsertAccount(ctx, store.Account{AccountID: "a000000000002", Provider: "x", Status: "active"}); err == nil {
		t.Fatal("a read-only store wrote")
	}
	_ = ro.Close()
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{KeyPath(root), filepath.Join(root, "spaces", "default", "club.db"), filepath.Join(root, "spaces", "default", "credentials.enc")} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
	d, err = Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	st, v, err = d.OpenSpace("default")
	if err != nil {
		t.Fatal(err)
	}
	if a, ok, _ := st.Account(ctx, "a000000000001"); !ok || a.Provider != "groq" {
		t.Fatalf("%+v", a)
	}
	if c, ok, _ := v.Get(ctx, "a000000000001"); !ok || c != "fixture-key-not-real" {
		t.Fatal("the credential did not survive a restart")
	}
	if _, err := OpenReadOnly(root, "nope"); err == nil {
		t.Fatal("opened a missing space")
	}
}

func TestAKeyFileElsewhere(t *testing.T) {
	root, keyDir := t.TempDir(), t.TempDir()
	d, err := Open(root, filepath.Join(keyDir, "club.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := os.Stat(filepath.Join(keyDir, "club.key")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(KeyPath(root)); err == nil {
		t.Fatal("the default key was created as well")
	}
}

// listing is every file under root with its size, mode and modification time.
func listing(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		raw := []byte{}
		if !fi.IsDir() {
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

func TestReadingCreatesNothingAndWorksBesideTheOwner(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	d, err := Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	st, _, err := d.OpenSpace("default")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAccount(ctx, store.Account{AccountID: "a000000000001", Provider: "groq", Status: "active"}); err != nil {
		t.Fatal(err)
	}
	// beside a running owner
	ro, err := OpenReadOnly(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	if list, err := ro.ListAccounts(ctx); err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	_ = ro.Close()
	_ = d.Close()
	// with nobody there: nothing is created or touched
	ro, err = OpenReadOnly(root, "default")
	if err != nil {
		t.Fatal(err)
	}
	if list, err := ro.ListAccounts(ctx); err != nil || len(list) != 1 {
		t.Fatalf("%+v %v", list, err)
	}
	// an owner starting meanwhile waits for the reader
	opened := make(chan error, 1)
	go func() {
		d, err := Open(root, "")
		if err == nil {
			_ = d.Close()
		}
		opened <- err
	}()
	time.Sleep(50 * time.Millisecond)
	_ = ro.Close()
	if err := <-opened; err != nil {
		t.Fatalf("an owner after a reader: %v", err)
	}
	if spaces, err := Spaces(root); err != nil || len(spaces) != 1 || spaces[0] != "default" {
		t.Fatalf("spaces %v %v", spaces, err)
	}
	if spaces, err := Spaces(filepath.Join(root, "nope")); err != nil || len(spaces) != 0 {
		t.Fatalf("spaces of nothing %v %v", spaces, err)
	}
	// reading a second time, still nothing changes
	before := listing(t, root)
	ro, _ = OpenReadOnly(root, "default")
	_, _ = ro.ListAccounts(ctx)
	_ = ro.Close()
	if after := listing(t, root); !maps.Equal(before, after) {
		t.Fatalf("reading changed the directory:\n%v\n%v", before, after)
	}
}

func TestAWrongKeyIsRefusedAndLosesNothing(t *testing.T) {
	ctx := context.Background()
	root, other := t.TempDir(), filepath.Join(t.TempDir(), "other.key")
	d, err := Open(root, "")
	if err != nil {
		t.Fatal(err)
	}
	_, v, _ := d.OpenSpace("default")
	if err := v.Put(ctx, "a000000000001", "fixture-key-not-real"); err != nil {
		t.Fatal(err)
	}
	_ = d.Close()
	sealed, _ := os.ReadFile(filepath.Join(root, "spaces", "default", "credentials.enc"))
	d, err = Open(root, other)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.OpenSpace("default"); err == nil || !strings.Contains(err.Error(), "does not open with this key") {
		t.Fatalf("a wrong key: %v", err)
	}
	_ = d.Close()
	if now, _ := os.ReadFile(filepath.Join(root, "spaces", "default", "credentials.enc")); !bytes.Equal(now, sealed) {
		t.Fatal("the credentials changed")
	}
	d, _ = Open(root, "")
	defer d.Close()
	if _, v, err := d.OpenSpace("default"); err != nil {
		t.Fatal(err)
	} else if c, ok, _ := v.Get(ctx, "a000000000001"); !ok || c != "fixture-key-not-real" {
		t.Fatal("the credential is gone")
	}
}
