package extension

import (
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// An AI account sometimes stops and asks the owner to sign in or approve an authorization link (a device login, an
// OAuth page, "click this link to approve"). That is the owner's to do: the row is "needs you" and is pushed to the
// phone. asksForAuth reads only the shape of the text. It never opens, fetches or follows anything in it; it is
// conservative, because a wrong guess is a phone alert for nothing.

// Limits.
const (
	maxAuthPlainRunes = 700  // a longer answer with a "please log in" in it is an explanation, not a request
	maxAuthRunes      = 4000 // even with a link
)

var (
	fencedCode = regexp.MustCompile("(?s)```.*?```")
	urlPattern = regexp.MustCompile(`https?://[^\s<>"'）)】」》]+`)
	codePair   = regexp.MustCompile(`\b[A-Z0-9]{4}-[A-Z0-9]{4}\b`)

	// verbs that ask the owner to do something at a link
	linkVerbs = regexp.MustCompile(`(?i)(点击|点一下|点开|点这|打开|访问|前往|批准|授权|登录|登入|同意|确认|验证|\b(click|tap|open|visit|follow|go to|approve|authori[sz]e|sign ?in|log ?in|grant|confirm|verify|enter)\b)`)
	enterCode = regexp.MustCompile(`(?i)(输入|填入|填写|\benter\b|\btype\b|\binput\b)`)

	// requests in words, with no link needed
	plainAsks = []*regexp.Regexp{
		regexp.MustCompile(`(请|麻烦|需要|得|要)\s*(你|您)?\s*(先|重新|再|来|去)*\s*(完成)?\s*(重新)?(登录|登入|授权|批准)`),
		regexp.MustCompile(`(你|您)\s*(点|点击|点一下|点开|打开|去)[^。\n]{0,20}(链接|网址|页面|网页)[^。\n]{0,24}(批准|授权|登录|登入|确认)`),
		regexp.MustCompile(`(登录|登入|授权)\s*(已经?|都)?\s*(过期|失效|超时)`),
		regexp.MustCompile(`等(你|您)\s*(来)?\s*(批准|授权|登录|登入)`),
		regexp.MustCompile(`(?i)\bplease\s+(re-?\s?)?(log\s?in|sign\s?in|authori[sz]e|approve|re-?authenticate)\b`),
		regexp.MustCompile(`(?i)\byou\s+(?:will\s+|'ll\s+)?(?:need|have|must)\s+to\s+(?:re-?\s?)?(?:log\s?in|sign\s?in|authori[sz]e|approve)\b`),
		regexp.MustCompile(`(?i)\b(?:click|open|visit|follow)\s+(?:the|this|that)\s+(?:link|url|page)\b[^.\n]{0,60}\b(?:approve|authori[sz]e|log\s?in|sign\s?in|confirm)`),
		regexp.MustCompile(`(?i)\b(?:login|sign-?in)\s+(?:is\s+)?(?:required|expired|needed)\b`),
		regexp.MustCompile(`(?i)\bwaiting\s+(?:for|on)\s+(?:your\s+)?(?:approval|authori[sz]ation|login)\b`),
	}
	// "no need to log in", "if you log in later": the words just before a match that turn it into something else
	negations = regexp.MustCompile(`(?i)(不|无需|不用|不必|没有必要|无须|如果|假如|若|如需|if |unless |no need to |don't need to |do not need to |not )\s*$`)
)

// authHosts are sign-in pages: a link there, with a verb asking the owner to act, is an authorization request.
var authHosts = map[string]bool{
	"login.tailscale.com": true, "accounts.google.com": true, "login.microsoftonline.com": true, "login.live.com": true,
	"appleid.apple.com": true, "auth.openai.com": true, "id.atlassian.com": true,
}

// authLink says a link is a page to sign in or approve at: a known sign-in host, or a path that is one by the usual
// names (OAuth, device activation).
func authLink(raw string) bool {
	u, err := url.Parse(strings.TrimRight(raw, ".,;:!?，。；：！？"))
	if err != nil || u.Host == "" {
		return false
	}
	host, path := strings.ToLower(u.Hostname()), strings.ToLower(u.Path)
	if authHosts[host] {
		return true
	}
	switch {
	case host == "github.com" && (strings.HasPrefix(path, "/login/device") || strings.HasPrefix(path, "/login/oauth")):
		return true
	case strings.HasSuffix(host, ".auth0.com"), strings.HasSuffix(host, ".okta.com"), strings.HasSuffix(host, ".b2clogin.com"):
		return true
	}
	for _, p := range []string{"/oauth", "/authorize", "/device/activate", "/activate", "/login/device", "/device"} {
		if strings.Contains(path, p) && (path == p || strings.HasPrefix(path, p+"/") || strings.Contains(path, p+"/") || strings.HasSuffix(path, p)) {
			return true
		}
	}
	return false
}

// withoutQuotes is text with code blocks and quoted lines taken out: an example in a code block or a quoted passage is
// not the AI asking.
func withoutQuotes(text string) string {
	text = fencedCode.ReplaceAllString(text, "\n")
	var keep []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); !strings.HasPrefix(t, ">") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// asksForAuth says the reply asks the owner to sign in or approve an authorization.
func asksForAuth(text string) bool {
	text = withoutQuotes(text)
	n := utf8.RuneCountInString(text)
	if n == 0 || n > maxAuthRunes {
		return false
	}
	links := urlPattern.FindAllString(text, -1)
	authLinks := 0
	for _, l := range links {
		if authLink(l) {
			authLinks++
		}
	}
	// a sign-in link, and words asking the owner to act on it
	if authLinks > 0 && linkVerbs.MatchString(urlPattern.ReplaceAllString(text, " ")) {
		return true
	}
	// a device code to type at a link
	if len(links) > 0 && codePair.MatchString(text) && enterCode.MatchString(text) {
		return true
	}
	// the request in words alone: short messages only
	if n > maxAuthPlainRunes {
		return false
	}
	for _, re := range plainAsks {
		for _, loc := range re.FindAllStringIndex(text, -1) {
			before := text[:loc[0]]
			if i := strings.LastIndexAny(before, "。.!?！？\n"); i >= 0 {
				before = before[i+1:]
			}
			if r := []rune(before); len(r) > 12 {
				before = string(r[len(r)-12:])
			}
			if !negations.MatchString(before) {
				return true
			}
		}
	}
	return false
}
