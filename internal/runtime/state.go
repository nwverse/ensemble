package runtime

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const stateFileVersion = 1

// SessionBinding maps a Persona assistant runtime session_id onto a paired club account and backing club session.
type SessionBinding struct {
	SessionID     string    `json:"session_id"`
	AccountID     string    `json:"account_id"`
	Provider      string    `json:"provider"`
	SessionKey    string    `json:"session_key,omitempty"`
	Launcher      string    `json:"launcher"`
	Cwd           string    `json:"cwd"`
	Title         string    `json:"title"`
	Assistant     bool      `json:"assistant"`
	BootstrapSent bool      `json:"bootstrap_sent,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type stateEnvelope struct {
	Version  int              `json:"version"`
	Bindings []SessionBinding `json:"bindings"`
}

func loadState(path string) ([]SessionBinding, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var env stateEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if env.Version != stateFileVersion {
		return nil, nil
	}
	return env.Bindings, nil
}

func saveState(path string, bindings []SessionBinding) error {
	if path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	sorted := slices.Clone(bindings)
	slices.SortFunc(sorted, func(a, b SessionBinding) int {
		if a.SessionID < b.SessionID {
			return -1
		}
		if a.SessionID > b.SessionID {
			return 1
		}
		return 0
	})
	raw, err := json.MarshalIndent(stateEnvelope{Version: stateFileVersion, Bindings: sorted}, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
