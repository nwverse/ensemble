package extension

import (
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// An AI account sometimes stops and asks the owner to sign in or approve an authorization link (a device login, an
// OAuth page, "click this link to approve"). That is the owner's to do: the row is "needs you" and the phone is
// alerted. asksForAuth reads only the shape of the text. It never opens, fetches or follows anything in it. It is
// conservative, because a wrong guess is a phone alert for nothing: the request must be put to the owner ("请", "你",
// "please", "you", an instruction), not merely mention signing in.

// Limits.
const (
	maxAuthPlainRunes = 700  // a longer answer with a "please log in" in it is an explanation, not a request
	maxAuthRunes      = 4000 // even with a link
	maxAuthSentence   = 300  // the sentence that holds the link and the ask
)

const linkMark = "\x00L" // stands where a link was, followed by its number and \x00

var (
	fencedCode = regexp.MustCompile("(?s)```.*?```")
	// what someone else said, not the AI asking: "…", “…”, 「…」, `…`
	inlineQuote = regexp.MustCompile("\"[^\"\\n]{1,300}\"|“[^”\\n]{1,300}”|「[^」\\n]{1,300}」|`[^`\\n]{1,300}`")
	// ASCII only, so text written right after a link in Chinese is not taken into it; no brackets (markdown links)
	urlPattern  = regexp.MustCompile(`https?://[A-Za-z0-9\-._~:/?#@!$&'*+,;=%]+`)
	linkRef     = regexp.MustCompile("\x00L(\\d+)\x00")
	sentenceEnd = regexp.MustCompile(`[。！？!?\n]+|\.(?:\s+|$)`)

	// the owner is addressed or instructed
	cueZh = regexp.MustCompile(`请|麻烦|你|您|告诉我`)
	cueEn = regexp.MustCompile(`(?i)\b(please|you|your)\b`)
	// an instruction in English: the verb opens the sentence or a clause
	imperativeEn = regexp.MustCompile(`(?i)(?:^|[,;:]\s*|\b(?:then|and|now|just|to)\s+)(?:open|visit|click|tap|follow|go to|head to|sign in|log in|approve|authori[sz]e|use)\b`)
	// what the owner is asked to do at the link
	actVerb = regexp.MustCompile(`(?i)(点击|点一下|点开|点这|打开|访问|前往|批准|授权|登录|登入|同意|验证|\b(click|tap|open|visit|follow|go to|head to|approve|authori[sz]e|sign ?in|log ?in|grant|verify)\b)`)
	// it is already done, or the AI did it itself
	doneZh = regexp.MustCompile(`(已经?|刚刚?|成功)[^，。,]{0,30}(登录|登入|授权|批准|验证)|(登录|登入|授权|批准|验证)(好了|完了|完成了|成功|过了)`)
	doneEn = regexp.MustCompile(`(?i)\b(i|i've|i have|we|we've)\s+(?:have\s+|had\s+|already\s+|just\s+|successfully\s+)*(logged|signed|authori[sz]ed|approved|verified)\b|\b(logged|signed) in (successfully|already)\b`)

	// a device code to type: the code next to the word for it
	deviceCode = regexp.MustCompile(`(?i)(code|验证码|设备码|授权码|代码)[^\n]{0,12}\b[A-Z0-9]{4}-[A-Z0-9]{4}\b|\b[A-Z0-9]{4}-[A-Z0-9]{4}\b[^\n]{0,8}(code|验证码|设备码|授权码)`)
	enterCode  = regexp.MustCompile(`(?i)(输入|填入|填写|\benter\b|\btype\b|\binput\b|\bpaste\b)`)

	// requests in words, with no link needed: each one addresses the owner
	plainAsks = []*regexp.Regexp{
		regexp.MustCompile(`(请|麻烦)\s*(你|您)?\s*(先|重新|再|来|去)*\s*(完成)?(重新)?(登录|登入|授权)`),
		regexp.MustCompile(`(需要|得|要|等)\s*(你|您)\s*(先|重新|再|来|去|亲自)*\s*(完成)?(重新)?(登录|登入|授权|批准授权|批准登录)`),
		regexp.MustCompile(`(你|您)\s*(点|点击|点一下|点开|打开|去)[^。\n]{0,20}(链接|网址|页面|网页)[^。\n]{0,24}(批准|授权|登录|登入)`),
		regexp.MustCompile(`(请|麻烦)\s*(你|您)?[^。\n]{0,8}扫[^。\n]{0,6}码[^。\n]{0,8}(登录|登入|授权)`),
		regexp.MustCompile(`(登录|登入|授权)\s*(状态)?\s*(已经?|都)?\s*(过期|失效)[^。\n]{0,24}((没法|无法|不能)继续|请|麻烦|需要你|需要您|你来|你重新|您重新)`),
		regexp.MustCompile(`(?i)\bplease\s+(?:re-?\s?)?(?:log|sign)\s?(?:back\s+)?in\b|\bplease\s+(?:re-?\s?)?(?:authori[sz]e|authenticate)\b`),
		regexp.MustCompile(`(?i)\byou\s+(?:will\s+|'ll\s+)?(?:need|have|must)\s+to\s+(?:re-?\s?)?(?:(?:log|sign)\s?(?:back\s+)?in|authori[sz]e|authenticate)\b`),
		regexp.MustCompile(`(?i)\b(?:i\s+need|waiting\s+for|waiting\s+on)\s+you\s+to\s+(?:re-?\s?)?(?:(?:log|sign)\s?(?:back\s+)?in|authori[sz]e|authenticate|re-?auth)\b`),
		regexp.MustCompile(`(?i)\b(?:could|can|would)\s+you\s+(?:please\s+)?(?:re-?\s?)?(?:(?:log|sign)\s?(?:back\s+)?in|authori[sz]e|authenticate)\b`),
		regexp.MustCompile(`(?i)\b(?:click|open|visit|follow)\s+(?:the|this|that)\s+(?:link|url|page)\b[^.\n]{0,60}\b(?:approve|authori[sz]e|log\s?in|sign\s?in)\b`),
		regexp.MustCompile(`(?i)(?:^|\n)\s*(?:login|log-?in|sign-?in)\s+(?:is\s+)?required\b`),
		regexp.MustCompile(`(?i)\bwaiting\s+(?:for|on)\s+your\s+(?:authori[sz]ation|login|sign-?in)\b`),
	}
	// "no need to log in", "if you log in later": the words just before a match that turn it into something else
	negations = regexp.MustCompile(`(?i)(不|无需|不用|不必|没有必要|无须|如果|假如|若|如需|if |unless |no need to |don't need to |do not need to |not |when |once |after )\s*$`)
)

// authHosts are sign-in hosts, each with the path prefixes that are a sign-in there (nil: any path).
var authHosts = map[string][]string{
	"login.tailscale.com":       {"/a/", "/login"},
	"accounts.google.com":       {"/o/oauth2", "/signin/oauth", "/v3/signin", "/device", "/AccountChooser", "/ServiceLogin"},
	"login.microsoftonline.com": nil,
	"login.live.com":            nil,
	"microsoft.com":             {"/devicelogin", "/link"},
	"www.microsoft.com":         {"/devicelogin", "/link"},
	"appleid.apple.com":         {"/auth"},
	"auth.openai.com":           nil,
	"github.com":                {"/login/device", "/login/oauth"},
}

// authSegments are path parts that name a sign-in or approval page on any host.
var authSegments = []string{"oauth", "oauth2", "authorize", "activate", "device", "sso"}

// authLink says a link is a page to sign in or approve at: a sign-in page of a known host, or, on another host, a
// short path (at most two parts) with an OAuth or device-activation name in it. A longer path that merely has such a
// word somewhere (documentation, a source tree) is not one.
func authLink(raw string) bool {
	u, err := url.Parse(strings.TrimRight(raw, ".,;:!?'"))
	if err != nil || u.Host == "" {
		return false
	}
	host, path := strings.ToLower(u.Hostname()), u.Path
	if prefixes, ok := authHosts[host]; ok {
		if prefixes == nil {
			return true
		}
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(path, p) })
	}
	if strings.HasSuffix(host, ".auth0.com") || strings.HasSuffix(host, ".okta.com") || strings.HasSuffix(host, ".b2clogin.com") {
		return true
	}
	parts := strings.FieldsFunc(strings.ToLower(path), func(r rune) bool { return r == '/' })
	if len(parts) == 0 || len(parts) > 2 {
		return false
	}
	return slices.ContainsFunc(parts, func(p string) bool { return slices.Contains(authSegments, p) })
}

// ownWords is text without what is not the AI speaking to the owner: code blocks, quoted lines and quoted phrases.
func ownWords(text string) string {
	text = fencedCode.ReplaceAllString(text, "\n")
	var keep []string
	for _, l := range strings.Split(text, "\n") {
		if t := strings.TrimSpace(l); !strings.HasPrefix(t, ">") {
			keep = append(keep, l)
		}
	}
	return inlineQuote.ReplaceAllString(strings.Join(keep, "\n"), " ")
}

// addressed says a sentence puts something to the owner: it speaks to them or instructs them, and does not say the
// thing is done.
func addressed(sentence string) bool {
	if doneZh.MatchString(sentence) || doneEn.MatchString(sentence) {
		return false
	}
	return cueZh.MatchString(sentence) || cueEn.MatchString(sentence) || imperativeEn.MatchString(strings.TrimSpace(sentence))
}

// asksForAuth says the reply asks the owner to sign in or approve an authorization.
func asksForAuth(text string) bool {
	text = ownWords(text)
	n := utf8.RuneCountInString(text)
	if n == 0 || n > maxAuthRunes {
		return false
	}
	// links out of the way first, so their dots and words are not read as sentences and verbs
	var links []string
	marked := urlPattern.ReplaceAllStringFunc(text, func(l string) string {
		links = append(links, l)
		return linkMark + strconv.Itoa(len(links)-1) + "\x00"
	})
	for _, s := range sentenceEnd.Split(marked, -1) {
		refs := linkRef.FindAllStringSubmatch(s, -1)
		if len(refs) == 0 || utf8.RuneCountInString(s) > maxAuthSentence {
			continue
		}
		words := linkRef.ReplaceAllString(s, " ")
		if !addressed(words) {
			continue
		}
		// a sign-in link the owner is told to act on
		for _, r := range refs {
			if i, err := strconv.Atoi(r[1]); err == nil && i < len(links) && authLink(links[i]) && actVerb.MatchString(words) {
				return true
			}
		}
		// a device code to type at a link
		if deviceCode.MatchString(words) && enterCode.MatchString(words) {
			return true
		}
	}
	// the request in words alone: short messages only
	if n > maxAuthPlainRunes {
		return false
	}
	plain := linkRef.ReplaceAllString(marked, " ")
	for _, re := range plainAsks {
		for _, loc := range re.FindAllStringIndex(plain, -1) {
			before := plain[:loc[0]]
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
