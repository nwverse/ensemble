package store

import (
	"encoding/json"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"github.com/nwverse/ensemble/internal/protocol"
)

const (
	// indexTextLimit is how many characters of a message its CJK tokens are cut from.
	indexTextLimit = 20_000
	// substringScan is how many of the most recent messages a substring search reads.
	substringScan = 5000
	// echoWindowMs is how far before a send a stored user message may be to count as the agent's echo of it.
	echoWindowMs = 60_000
)

// BlocksText is the searchable text of blocks, one block per line: text, tool-result and error texts and
// attachment names, as the Lark gateway indexes them.
func BlocksText(blocks []protocol.Block) string {
	var parts []string
	for _, b := range blocks {
		var t string
		switch b.Type {
		case "text", "tool-result", "error":
			t = b.Text
		case "attachment":
			t = b.Name
		}
		if t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func isCJK(r rune) bool { return r >= 0x3400 && r <= 0x9fff }

func cjkRunes(s string) []rune {
	var out []rune
	for _, r := range norm.NFKC.String(s) {
		if isCJK(r) {
			out = append(out, r)
		}
	}
	return out
}

func indexText(text string) string {
	n := 0
	for i := range text {
		if n == indexTextLimit {
			return text[:i]
		}
		n++
	}
	return text
}

// CJKTokens are the 2- and 3-grams of a message's CJK characters (cjk_tokens), from its first 20,000 characters,
// so a Chinese substring matches without a dictionary.
func CJKTokens(text string) string {
	cjk := cjkRunes(indexText(text))
	var tokens []string
	for size := 2; size <= 3; size++ {
		for i := 0; i+size <= len(cjk); i++ {
			tokens = append(tokens, string(cjk[i:i+size]))
		}
	}
	return strings.Join(tokens, " ")
}

// queryCJK are the n-grams of a query's CJK characters: 3-grams when it has three or more, else 2-grams.
func queryCJK(q string) []string {
	cjk := cjkRunes(q)
	size := 2
	if len(cjk) >= 3 {
		size = 3
	}
	var out []string
	for i := 0; i+size <= len(cjk); i++ {
		out = append(out, string(cjk[i:i+size]))
	}
	return out
}

func tokenRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) ||
		unicode.Is(unicode.Co, r)
}

// words splits text into lower-case words as SQLite's unicode61 tokenizer does.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !tokenRune(r) })
}

// query is a search: the words of q, its CJK n-grams, and q itself for the substring match.
type query struct {
	words []string
	cjk   []string
	text  string
}

func parseQuery(q string) query {
	q = strings.TrimSpace(q)
	return query{words: words(q), cjk: queryCJK(q), text: q}
}

// fts is the query as FTS5 syntax: all its words, or all its CJK n-grams; "" when it has neither.
func (q query) fts() string {
	quote := func(ts []string) string {
		parts := make([]string, len(ts))
		for i, t := range ts {
			parts[i] = `"` + strings.ReplaceAll(t, `"`, `""`) + `"`
		}
		return "(" + strings.Join(parts, " AND ") + ")"
	}
	var alts []string
	if len(q.words) > 0 {
		alts = append(alts, quote(q.words))
	}
	if len(q.cjk) > 0 {
		alts = append(alts, quote(q.cjk))
	}
	return strings.Join(alts, " OR ")
}

// matches is what fts() matches, for the in-memory store: a message's index holds the words of its text and its
// CJK n-grams.
func (q query) matches(text string) bool {
	if len(q.words) == 0 && len(q.cjk) == 0 {
		return false
	}
	index := map[string]bool{}
	for _, w := range words(text) {
		index[w] = true
	}
	for _, w := range words(CJKTokens(text)) {
		index[w] = true
	}
	all := func(ts []string) bool {
		for _, t := range ts {
			if !index[strings.ToLower(t)] {
				return false
			}
		}
		return len(ts) > 0
	}
	return all(q.words) || all(q.cjk)
}

// asciiLower lower-cases ASCII letters only, as SQLite's lower() does.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
}

// containsText is the substring match: q in text, ASCII case-insensitively, % and _ taken literally.
func (q query) containsText(text string) bool {
	return q.text != "" && strings.Contains(asciiLower(text), asciiLower(q.text))
}

// echoText is a text as an echo is compared: spaces trimmed at both ends.
func echoText(s string) string { return strings.Trim(s, " ") }

// blocksJSON is blocks as stored (blocks_json).
func blocksJSON(b []protocol.Block) string {
	if b == nil {
		return "[]"
	}
	out, err := json.Marshal(b)
	if err != nil {
		return "[]"
	}
	return string(out)
}
