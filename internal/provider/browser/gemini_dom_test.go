package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/net/html"
)

// The pages in testdata/gemini were recorded from gemini.google.com (2026-11-27, the 对话 / 任务 composer, the
// Hydrogen model) in a test conversation, with Angular's generated attributes and icons left out:
//
//	new-chat.html              /app, a new conversation: the composer with its 对话 / 任务 switch
//	conversation-idle.html     a conversation, nothing typed
//	conversation-typed.html    the same with text in the composer: the send button
//	conversation-answering.html  just after sending: the new turn, and the stop button while the answer is written
//	conversation-answered.html once the answer is written
//
// Each selector the scripts use must find what they take it for on these pages; a page Google changes is recorded
// again and the test says which selector no longer holds.

func loadGeminiPage(t *testing.T, name string) *html.Node {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", "gemini", name))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	doc, err := html.Parse(f)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestGeminiSelectorsHoldOnRecordedPages(t *testing.T) {
	pages := []string{"new-chat.html", "conversation-idle.html", "conversation-typed.html", "conversation-answering.html", "conversation-answered.html", "gemini-spark.html"}
	for _, tc := range []struct {
		sel  string
		name string
		want map[string]int // page → matches; pages not named: 0
	}{
		{geminiComposerSel, "composer", map[string]int{"new-chat.html": 1, "conversation-idle.html": 1, "conversation-typed.html": 1, "conversation-answering.html": 1, "conversation-answered.html": 1}},
		{geminiSendSel, "send button", map[string]int{"conversation-typed.html": 1}},
		{geminiStopSel, "stop button", map[string]int{"conversation-answering.html": 1}},
		{geminiChatModeSel, "对话 mode", map[string]int{"new-chat.html": 1}},
		{geminiTaskModeSel, "任务 mode", map[string]int{"new-chat.html": 1}},
		{geminiQuerySel, "user turns", map[string]int{"conversation-idle.html": 2, "conversation-typed.html": 2, "conversation-answering.html": 2, "conversation-answered.html": 2}},
		{geminiAgentSettingsSel, "gemini-spark settings root", map[string]int{"gemini-spark.html": 1}},
		{geminiDisableAgentButtonSel, "gemini-spark disable task-mode button", map[string]int{"gemini-spark.html": 1}},
		{geminiInternetSwitchSel, "gemini-spark remote computer network switch", map[string]int{"gemini-spark.html": 1}},
	} {
		for _, p := range pages {
			if got := len(querySelectorAll(loadGeminiPage(t, p), tc.sel)); got != tc.want[p] {
				t.Errorf("%s (%s) on %s: %d matches, want %d", tc.name, tc.sel, p, got, tc.want[p])
			}
		}
	}

	// The 对话 switch is the pressed one on a new conversation's page, and 任务 (data-value=40) is alongside it.
	mode := querySelectorAll(loadGeminiPage(t, "new-chat.html"), geminiChatModeSel)
	if len(mode) != 1 || strings.TrimSpace(nodeText(mode[0])) != "对话" || attr(mode[0], "aria-pressed") != "true" {
		t.Errorf("对话 mode: %d matches", len(mode))
	}
	taskMode := querySelectorAll(loadGeminiPage(t, "new-chat.html"), geminiTaskModeSel)
	if len(taskMode) != 1 || strings.TrimSpace(nodeText(taskMode[0])) != "任务" || attr(taskMode[0], "aria-pressed") != "false" {
		t.Errorf("任务 mode: %d matches", len(taskMode))
	}
	sparkSwitch := querySelectorAll(loadGeminiPage(t, "gemini-spark.html"), geminiInternetSwitchSel)
	if len(sparkSwitch) != 1 || attr(sparkSwitch[0], "aria-checked") != "true" || attr(sparkSwitch[0], "aria-label") != "远程计算机的网络访问权限" {
		t.Errorf("gemini-spark network switch: %d matches", len(sparkSwitch))
	}

	// The newest turn's text is its lines, without the screen reader's "你说" label the turn also carries (and which
	// Gemini cuts short with … for a long message).
	doc := loadGeminiPage(t, "conversation-answering.html")
	turns := querySelectorAll(doc, geminiQuerySel)
	last := turns[len(turns)-1]
	var lines []string
	for _, l := range querySelectorAll(last, geminiQueryTextSel) {
		lines = append(lines, strings.TrimSpace(nodeText(l)))
	}
	if got := strings.Join(lines, "\n"); got != "[Ensemble 连通性测试 15] 只回复：好的" {
		t.Errorf("newest turn's text = %q", got)
	}
	if !strings.Contains(nodeText(last), "你说") {
		t.Errorf("the recorded turn no longer carries its label: the text selector may be moot")
	}
	// The composer holds the typed text on the typed page, and is empty once sent.
	if c := querySelectorAll(loadGeminiPage(t, "conversation-typed.html"), geminiComposerSel); len(c) != 1 || !strings.Contains(nodeText(c[0]), "连通性测试 15") {
		t.Errorf("typed composer: %d", len(c))
	}
	if c := querySelectorAll(doc, geminiComposerSel); len(c) != 1 || strings.TrimSpace(nodeText(c[0])) != "" {
		t.Errorf("composer after sending is not empty")
	}
}

// --- a small CSS selector matcher, for the selectors the scripts use: type, .class, [attr], [attr="v"],
// [attr="v" i], :not(...), descendant combinators and selector lists.

func querySelectorAll(root *html.Node, sel string) []*html.Node {
	var out []*html.Node
	seen := map[*html.Node]bool{}
	var walk func(n *html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type == html.ElementNode {
				for _, complex := range splitTop(sel, ',') {
					if !seen[c] && matchComplex(c, root, splitTop(strings.TrimSpace(complex), ' ')) {
						seen[c] = true
						out = append(out, c)
					}
				}
			}
			walk(c)
		}
	}
	walk(root)
	return out
}

// splitTop splits s at sep outside brackets, parentheses and quotes, dropping empty parts.
func splitTop(s string, sep rune) []string {
	var out []string
	depth, quote, start := 0, rune(0), 0
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '[' || r == '(':
			depth++
		case r == ']' || r == ')':
			depth--
		case r == sep && depth == 0:
			if p := strings.TrimSpace(s[start:i]); p != "" {
				out = append(out, p)
			}
			start = i + 1
		}
	}
	if p := strings.TrimSpace(s[start:]); p != "" {
		out = append(out, p)
	}
	return out
}

// matchComplex: n matches the last compound, and some ancestor (below root's parent) the rest, in order.
func matchComplex(n, root *html.Node, parts []string) bool {
	if len(parts) == 0 {
		return true
	}
	if !matchCompound(n, parts[len(parts)-1]) {
		return false
	}
	for a := n.Parent; a != nil && a != root.Parent; a = a.Parent {
		if a.Type == html.ElementNode && matchComplex(a, root, parts[:len(parts)-1]) {
			return true
		}
	}
	return len(parts) == 1
}

func matchCompound(n *html.Node, c string) bool {
	i := 0
	for i < len(c) && c[i] != '.' && c[i] != '[' && c[i] != ':' {
		i++
	}
	if tag := c[:i]; tag != "" && tag != n.Data {
		return false
	}
	for i < len(c) {
		switch c[i] {
		case '.':
			j := i + 1
			for j < len(c) && c[j] != '.' && c[j] != '[' && c[j] != ':' {
				j++
			}
			if !hasClass(n, c[i+1:j]) {
				return false
			}
			i = j
		case '[':
			j := strings.IndexByte(c[i:], ']') + i
			if !matchAttr(n, c[i+1:j]) {
				return false
			}
			i = j + 1
		case ':':
			if !strings.HasPrefix(c[i:], ":not(") {
				return false
			}
			depth, j := 0, i+4
			for ; j < len(c); j++ {
				if c[j] == '(' {
					depth++
				} else if c[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if matchCompound(n, c[i+5:j]) {
				return false
			}
			i = j + 1
		default:
			return false
		}
	}
	return true
}

func matchAttr(n *html.Node, spec string) bool {
	name, value, hasValue := strings.Cut(spec, "=")
	name = strings.TrimSpace(name)
	op := byte(0) // *= (contains) and ^= (starts with), besides =
	if l := len(name); l > 0 && (name[l-1] == '*' || name[l-1] == '^') {
		op, name = name[l-1], name[:l-1]
	}
	v, ok := attrOK(n, name)
	if !hasValue {
		return ok
	}
	value = strings.TrimSpace(value)
	if op != 0 {
		value = strings.Trim(value, `"'`)
		if op == '*' {
			return ok && strings.Contains(v, value)
		}
		return ok && strings.HasPrefix(v, value)
	}
	fold := false
	if strings.HasSuffix(value, " i") {
		fold, value = true, strings.TrimSpace(strings.TrimSuffix(value, " i"))
	}
	value = strings.Trim(value, `"'`)
	if fold {
		return ok && strings.EqualFold(v, value)
	}
	return ok && v == value
}

func attrOK(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func attr(n *html.Node, name string) string {
	v, _ := attrOK(n, name)
	return v
}

func hasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}
