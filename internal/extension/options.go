package extension

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// An AI account's reply often ends with the choices it wants the owner to make, one to a line:
//
//	A：run it now
//	B：fix the cause
//	leave it for now
//
// parseOptions finds that list so the Inbox can show it as buttons (inbox.go draws them, each one confirmed before it
// is sent). It reads only the shape of the text and never runs or follows anything in it.

// Limits of a list of options.
const (
	maxOptions      = 6   // buttons on one row
	maxOptionLabel  = 28  // characters of a button's title
	maxOptionReply  = 200 // characters sent back for a choice
	maxExtraRunes   = 20  // an unmarked closing line this short is an option too ("leave it for now")
	maxExtraLines   = 2   // at most this many unmarked closing lines
	maxClosingLines = 3   // more prose than this after the list and it is not what the message ends on
)

// option is one choice: Label is the button's title, Reply what is sent back to the chat when it is picked.
type option struct {
	Marker string `json:"marker,omitempty"`
	Label  string `json:"label"`
	Reply  string `json:"reply"`
}

// markerLine reads "A：text", "A. text", "(B) text", "选项C：text", "1、text" (after any list bullet or bold marks).
var markerLine = regexp.MustCompile(`^(?:[-*•·]\s*)?(?:(?:选项|方案|Option|option)\s*)?[（(\[【]?([A-Fa-f]|[1-9])[）)\]】]?\s*(?:[：:、]\s*|[.．](?:\s+|$))(\S.*)$`)

// markerBare is the same with the delimiter inside brackets only: "(A) text", "A) text".
var markerBare = regexp.MustCompile(`^(?:[-*•·]\s*)?[（(\[【]([A-Fa-f]|[1-9])[）)\]】]\s*(\S.*)$|^(?:[-*•·]\s*)?([A-Fa-f]|[1-9])[）)]\s*(\S.*)$`)

// cueWords say the line before a list asks for a choice. Only that one line is read: words further away, or after the
// list, say nothing about it.
var cueWords = []string{"选", "哪个", "哪一", "哪种", "决定", "要不要", "是否要", "你想", "你看", "请回复", "怎么办", "怎么做", "接下来", "办法", "做法", "方案", "选项",
	"choose", "which one", "which do", "which would", "which option", "prefer", "pick", "option", "next step", "how would you like", "how should", "what should", "what do you want"}

type markedLine struct {
	idx    int
	marker string // upper-case letter or digit
	text   string // the line without bullet and emphasis marks
	body   string // the text after the marker
}

var bullet = regexp.MustCompile(`^[-*•·]\s+`)

// cleanLine is a line without its list bullet and emphasis marks (bold, italics, code).
func cleanLine(s string) string {
	s = bullet.ReplaceAllString(strings.TrimSpace(s), "")
	return strings.TrimSpace(strings.Trim(s, "*_` \t"))
}

func readMarker(line string) (marker, body string, ok bool) {
	if m := markerLine.FindStringSubmatch(line); m != nil {
		return strings.ToUpper(m[1]), strings.TrimSpace(strings.Trim(m[2], "*_` ")), true
	}
	if m := markerBare.FindStringSubmatch(line); m != nil {
		if m[1] != "" {
			return strings.ToUpper(m[1]), strings.TrimSpace(strings.Trim(m[2], "*_` ")), true
		}
		return strings.ToUpper(m[3]), strings.TrimSpace(strings.Trim(m[4], "*_` ")), true
	}
	return "", "", false
}

// parseOptions is the choices at the end of text; nil when there are none (fewer than two, out of order, or a
// numbered list that does not ask for a choice).
func parseOptions(text string) []option {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := range lines {
		lines[i] = cleanLine(lines[i])
	}
	// the last run of consecutive marked lines
	var run []markedLine
	end := -1
	for i := len(lines) - 1; i >= 0; i-- {
		marker, body, ok := readMarker(lines[i])
		if ok {
			if end < 0 {
				end = i
			}
			run = append([]markedLine{{idx: i, marker: marker, text: lines[i], body: body}}, run...)
			continue
		}
		if end >= 0 {
			break // the run is over: this line is not marked
		}
	}
	if end >= 0 && closingLines(lines[end+1:]) > maxClosingLines {
		return nil // prose follows the list: it is not what the message ends on
	}
	if len(run) < 2 || len(run) > maxOptions {
		return nil
	}
	// the markers go A, B, C… or 1, 2, 3… from the start
	digits := run[0].marker[0] >= '1' && run[0].marker[0] <= '9'
	first := byte('A')
	if digits {
		first = '1'
	}
	for i, m := range run {
		if m.marker[0] != first+byte(i) || (digits != (m.marker[0] >= '1' && m.marker[0] <= '9')) {
			return nil
		}
	}
	if !asksForChoice(lines, run[0].idx) {
		return nil
	}
	var out []option
	for _, m := range run {
		out = append(out, option{Marker: m.marker, Label: clip(m.marker+"："+m.body, maxOptionLabel), Reply: clip(m.marker+"："+m.body, maxOptionReply)})
	}
	// short unmarked closing lines are choices too ("leave it for now"); a question ends the list
	extras := 0
	for _, l := range lines[end+1:] {
		if l == "" {
			continue
		}
		if extras >= maxExtraLines || utf8.RuneCountInString(l) > maxExtraRunes || endsSentence(l) || isSignOff(l) || len(out) >= maxOptions {
			break
		}
		out = append(out, option{Label: clip(l, maxOptionLabel), Reply: clip(l, maxOptionReply)})
		extras++
	}
	return out
}

func closingLines(lines []string) int {
	n := 0
	for _, l := range lines {
		if l != "" {
			n++
		}
	}
	return n
}

// leadOf is the last non-empty line before the list.
func leadOf(lines []string, from int) string {
	for i := from - 1; i >= 0; i-- {
		if lines[i] != "" {
			return lines[i]
		}
	}
	return ""
}

// asksForChoice says the list is a choice the owner is meant to make: the line right before it asks (ends in a
// question mark) or uses a choosing word. A colon alone is not enough ("sample dialogue:", "results:").
func asksForChoice(lines []string, from int) bool {
	lead := strings.ToLower(leadOf(lines, from))
	if strings.HasSuffix(lead, "?") || strings.HasSuffix(lead, "？") {
		return true
	}
	for _, w := range cueWords {
		if strings.Contains(lead, w) {
			return true
		}
	}
	return false
}

// signOffs are closing lines that are not choices.
var signOffs = []string{"cheers", "thanks", "thank you", "regards", "best", "谢谢", "感谢", "以上", "祝好", "辛苦"}

func isSignOff(l string) bool {
	l = strings.ToLower(strings.Trim(l, " .!,~，。！"))
	for _, w := range signOffs {
		if strings.HasPrefix(l, w) {
			return true
		}
	}
	return false
}

func endsSentence(s string) bool {
	return strings.HasSuffix(s, "。") || strings.HasSuffix(s, "？") || strings.HasSuffix(s, "?") ||
		strings.HasSuffix(s, "！") || strings.HasSuffix(s, "!") || strings.HasSuffix(s, ".") || strings.HasSuffix(s, "：") ||
		strings.HasSuffix(s, ":")
}

// clip cuts s to n characters, with an ellipsis when it was longer.
func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
