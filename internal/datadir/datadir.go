// Package datadir is club's data directory on disk:
//
//	<data>/key                          the vault key (unless --key-file puts it elsewhere), 0600
//	<data>/club.lock                    held by the one process that owns the directory
//	<data>/spaces/<space>/club.db       the space's SQLite store (WAL)
//	<data>/spaces/<space>/credentials.enc  the space's credentials, AES-256-GCM under the key
//
// One process owns a data directory at a time (a serve, the Persona extension, or an import): Open takes an
// exclusive lock and fails with ErrInUse while another holds it.
package datadir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/nwverse/ensemble/internal/store"
	"github.com/nwverse/ensemble/internal/vault"
)

// ErrInUse is Open's error while another process owns the directory.
var ErrInUse = errors.New("the data directory is in use by another personal-agent-club process (serve or the Persona extension); stop it first")

// Dir is an open data directory.
type Dir struct {
	root string
	key  []byte
	lock *os.File

	mu     sync.Mutex
	stores []*store.SQLite
	closed bool
}

// KeyPath is the default key file of a data directory.
func KeyPath(root string) string { return filepath.Join(root, "key") }

// SpacePath is a space's directory.
func SpacePath(root, space string) string { return filepath.Join(root, "spaces", space) }

// Open opens (creating it, 0700) the data directory at root and takes its lock. keyFile "" is <root>/key; a missing
// key file is created with a fresh key. A reader (OpenReadOnly) may hold the lock shared for a moment: Open waits for
// it up to lockWait before it answers ErrInUse.
func Open(root, keyFile string) (*Dir, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(root, lockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(lockWait)
	for {
		err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if err != nil {
		_ = lock.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrInUse
		}
		return nil, fmt.Errorf("lock %s: %w", root, err)
	}
	if keyFile == "" {
		keyFile = KeyPath(root)
	}
	key, err := vault.LoadKey(keyFile, true)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	return &Dir{root: root, key: key, lock: lock}, nil
}

const (
	lockName = "club.lock"
	lockWait = time.Second
)

// Root is the directory's path.
func (d *Dir) Root() string { return d.root }

// OpenSpace opens a space's store and vault: the core.Options.OpenSpace of a club kept on disk.
func (d *Dir) OpenSpace(name string) (store.Store, vault.Vault, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, nil, errors.New("the data directory is closed")
	}
	dir := SpacePath(d.root, name)
	st, err := store.OpenSQLite(filepath.Join(dir, "club.db"))
	if err != nil {
		return nil, nil, err
	}
	v, err := vault.OpenFile(filepath.Join(dir, "credentials.enc"), d.key, name)
	if err == nil {
		// A key that does not open the credentials already there is refused now, not at the first send.
		err = v.Check()
	}
	if err != nil {
		_ = st.Close()
		return nil, nil, fmt.Errorf("space %s: %w", name, err)
	}
	d.stores = append(d.stores, st)
	return st, v, nil
}

// Close closes the stores and releases the lock. Close the club first.
func (d *Dir) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	var errs []error
	for _, s := range d.stores {
		errs = append(errs, s.Close())
	}
	errs = append(errs, d.lock.Close())
	return errors.Join(errs...)
}

// HasSpace says whether a space exists in the directory at root.
func HasSpace(root, space string) bool {
	_, err := os.Stat(filepath.Join(SpacePath(root, space), "club.db"))
	return err == nil
}

// Spaces are the spaces that exist in the directory at root, by name.
func Spaces(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "spaces"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && HasSpace(root, e.Name()) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// ReadOnly is a space's store opened for reading while another process may own the directory.
type ReadOnly struct {
	*store.SQLite
	lock *os.File
}

// Close closes the store and lets go of the directory.
func (r *ReadOnly) Close() error {
	err := r.SQLite.Close()
	if r.lock != nil {
		err = errors.Join(err, r.lock.Close())
	}
	return err
}

// OpenReadOnly opens a space's store read-only, without taking the directory: for reading while the owner runs. It
// creates and changes nothing in the directory. While no process owns it (the lock is free; it is then held shared
// until Close, so none starts meanwhile) and no -wal file is left over, the database file is read alone; while an
// owner runs, the -wal and -shm files it keeps are read alongside.
func OpenReadOnly(root, space string) (*ReadOnly, error) {
	if !HasSpace(root, space) {
		return nil, fmt.Errorf("no space %q in %s", space, root)
	}
	path := filepath.Join(SpacePath(root, space), "club.db")
	lock, err := os.Open(filepath.Join(root, lockName))
	if err == nil && syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB) != nil {
		_ = lock.Close()
		lock = nil
	} else if err != nil {
		lock = nil
	}
	_, walErr := os.Stat(path + "-wal")
	immutable := lock != nil && errors.Is(walErr, fs.ErrNotExist)
	st, err := store.OpenSQLiteReadOnly(path, immutable)
	if err != nil {
		if lock != nil {
			_ = lock.Close()
		}
		return nil, err
	}
	return &ReadOnly{SQLite: st, lock: lock}, nil
}
