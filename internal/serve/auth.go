package serve

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/nwverse/ensemble/internal/protocol"
)

// Level is what a token may do.
type Level int

const (
	// LevelNone is no access.
	LevelNone Level = iota
	// LevelRead reads: status, sessions, messages, search, events.
	LevelRead
	// LevelWrite also acts in sessions: send, delete_message, mark_read.
	LevelWrite
	// LevelAdmin also manages club: every club.* method.
	LevelAdmin
)

func (l Level) String() string {
	switch l {
	case LevelRead:
		return "read"
	case LevelWrite:
		return "write"
	case LevelAdmin:
		return "admin"
	}
	return "none"
}

// minToken is the shortest token accepted.
const minToken = 32

// Tokens are the tokens a TCP listener accepts, by level. Only their hashes are kept.
type Tokens struct {
	entries []tokenEntry
}

type tokenEntry struct {
	hash  [32]byte
	level Level
}

// TokensFile is the tokens file: each level's list of tokens.
type TokensFile struct {
	Read  []string `json:"read,omitempty"`
	Write []string `json:"write,omitempty"`
	Admin []string `json:"admin,omitempty"`
}

// NewTokens checks and hashes f's tokens: at least one, each 32 characters or more, none at two levels.
func NewTokens(f TokensFile) (*Tokens, error) {
	t := &Tokens{}
	seen := map[[32]byte]bool{}
	for _, group := range []struct {
		level  Level
		tokens []string
	}{{LevelRead, f.Read}, {LevelWrite, f.Write}, {LevelAdmin, f.Admin}} {
		for _, tok := range group.tokens {
			tok = strings.TrimSpace(tok)
			if len(tok) < minToken {
				return nil, fmt.Errorf("a %s token is shorter than %d characters", group.level, minToken)
			}
			h := sha256.Sum256([]byte(tok))
			if seen[h] {
				return nil, fmt.Errorf("a token is listed twice")
			}
			seen[h] = true
			t.entries = append(t.entries, tokenEntry{hash: h, level: group.level})
		}
	}
	if len(t.entries) == 0 {
		return nil, fmt.Errorf("no tokens")
	}
	return t, nil
}

// LoadTokens reads a tokens file (JSON: {"read": [...], "write": [...], "admin": [...]}).
func LoadTokens(path string) (*Tokens, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f TokensFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("tokens file: %w", err)
	}
	return NewTokens(f)
}

// Level is the level of token; LevelNone when it is not one.
func (t *Tokens) Level(token string) Level {
	if t == nil || token == "" {
		return LevelNone
	}
	h := sha256.Sum256([]byte(token))
	level := LevelNone
	for _, e := range t.entries {
		if subtle.ConstantTimeCompare(h[:], e.hash[:]) == 1 {
			level = e.level
		}
	}
	return level
}

// Required is the level a method needs. mark_read needs write because it changes unread state, as the Lark gateway's
// read-only agent token cannot mark sessions read either.
func Required(method string) Level {
	switch {
	case strings.HasPrefix(method, protocol.ClubNamespace+"."):
		return LevelAdmin
	case method == protocol.AgentSourceNamespace+"."+protocol.MethodSend,
		method == protocol.AgentSourceNamespace+"."+protocol.MethodDeleteMessage,
		method == protocol.AgentSourceNamespace+"."+protocol.MethodMarkRead:
		return LevelWrite
	}
	return LevelRead
}
