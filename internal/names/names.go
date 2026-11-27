// Package names is how a paired account is called for people: its provider's brand, then who the account is (a
// person's name or email, a bot's or agent's name), e.g. "Claude Duet · Ada". A provider's Verify reports only the
// second part (provider.RemoteAccount.Alias); the brand is added here, once, so it never shows twice. An alias that is
// an e-mail is shown by the part before @ ("Grok Bot · ada"), the whole e-mail kept in the stored name. A name the
// owner chose is shown exactly as typed and never renamed or numbered.
//
// An account row stores its display name and who chose it (store.Account.NameSource): SourceOwner, SourceAuto
// (Stored(provider, alias), the English composition), or SourceLegacy for rows from before club kept this. A legacy
// row counts as composed by club only in the exact forms earlier versions wrote (the bare brand, "<Brand> · alias",
// the raw provider id) or, for a browser service, a page title made of the site's own words; anything else is the
// owner's. Core re-verifies a legacy row once, on its next successful sync, and stores its source.
package names

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"unicode"
)

// Default is the language of the names club hands to Persona (agentsrc accounts, launchers, assistant sessions, the
// assistant's tools, a takeover's machine), which carry no locale: Chinese, the owner's, as the assistant's tool
// descriptions are. Pages use their own language.
const Default = "zh"

// Sep joins a brand and an account's alias.
const Sep = " · "

// Who chose a stored display name (the values of store.Account.NameSource).
const (
	SourceLegacy = ""
	SourceAuto   = "auto"
	SourceOwner  = "owner"
)

type brand struct {
	zh, en string
	// legacy are other ways earlier versions spelled the brand in a stored display name.
	legacy []string
	// site are the words a browser service's page titles are made of ("Claude", "Duet"); set for browser services
	// only, whose rows before 0.5 stored the tab's title.
	site []string
	// letters are the service's letter avatar (Letters), at most 2 characters and different for every service: what a
	// user guesses from the label next to it, no digits. lettersZh replaces them on Chinese pages for a label that is
	// translated. color is the avatar's tile (Tile), "#rrggbb", different for every service and dark enough for white
	// letters in light and dark themes alike.
	letters, lettersZh, color string
}

// brands are the providers' labels, the one table every page and host-facing name uses. Trademarks stay as they are
// in both languages; a label that describes rather than names (the web and self-hosted Muse, the demo) is
// translated. A label carries no brackets: "Muse（网页版） · Ada" leaves a wider gap before the dot than any other
// title, so the bracketed labels 0.6.1 and 0.6.2 showed are legacy spellings, read back as club's and shown anew.
var brands = map[string]brand{
	"grok_bot":     {zh: "Grok Bot", en: "Grok Bot", letters: "GB", color: "#52525B"},
	"muse":         {zh: "Muse 网页版", en: "Muse (web)", legacy: []string{"Muse", "Muse（网页版）"}, site: []string{"Muse", "Chat", "聊天"}, letters: "Mu", color: "#7C3AED"},
	"claude_duet":  {zh: "Claude Duet", en: "Claude Duet", site: []string{"Claude", "Duet", "Claude.ai", "Anthropic"}, letters: "Cl", color: "#C2410C"},
	"gemini_tasks": {zh: "Gemini", en: "Gemini", legacy: []string{"Gemini Tasks", "Gemini 任务"}, site: []string{"Gemini", "Google", "Google Gemini", "Tasks", "任务"}, letters: "Ge", color: "#1D4ED8"},
	"dots":         {zh: "ChatGPT Dots", en: "ChatGPT Dots", legacy: []string{"Dots"}, site: []string{"ChatGPT", "Dots", "Dot", "OpenAI"}, letters: "Ch", color: "#047857"},
	"cue":          {zh: "Cue", en: "Cue", letters: "Cu", color: "#BE123C"},
	"groq":         {zh: "Groq", en: "Groq", letters: "Gr", color: "#A21CAF"},
	"muse2api":     {zh: "Muse 自建转接", en: "Muse (own relay)", legacy: []string{"Muse (muse2api)", "Muse（自建转接）"}, letters: "Re", lettersZh: "转", color: "#0E7490"},
	"echo":         {zh: "演示账号", en: "Demo account", legacy: []string{"Echo", "Echo（演示）", "Echo (demo)"}, letters: "De", lettersZh: "演", color: "#4D7C0F"},
}

// Letters is a provider's letter avatar in lang: what an account's or a service's row draws when the app shows no
// picture (Tile), so services tell apart at a glance. Every known service has letters
// of its own; an unknown one shows the first letter of its readable name.
func Letters(lang, provider string) string {
	if b, ok := brands[provider]; ok && b.letters != "" {
		if isZh(lang) && b.lettersZh != "" {
			return b.lettersZh
		}
		return b.letters
	}
	r := []rune(humanize(lang, provider))
	return string(r[:1])
}

func isZh(lang string) bool { return lang == "" || strings.HasPrefix(lang, "zh") }

// Brand is a provider's label in lang ("zh…", "" for Chinese, anything else for English). An unknown provider's id
// is made readable ("my_site" → "My Site"), never shown raw.
func Brand(lang, provider string) string {
	if b, ok := brands[provider]; ok {
		if isZh(lang) {
			return b.zh
		}
		return b.en
	}
	return humanize(lang, provider)
}

func humanize(lang, id string) string {
	words := strings.FieldsFunc(id, func(r rune) bool { return r == '_' || r == '-' || r == '.' || unicode.IsSpace(r) })
	for i, w := range words {
		r := []rune(w)
		r[0] = unicode.ToUpper(r[0])
		words[i] = string(r)
	}
	if len(words) == 0 {
		if isZh(lang) {
			return "账号"
		}
		return "Account"
	}
	return strings.Join(words, " ")
}

// brandForms are every spelling of the provider's brand a stored name may start with, longest first.
func brandForms(provider string) []string {
	b := brands[provider]
	forms := []string{Brand("zh", provider), Brand("en", provider), provider, humanize("en", provider)}
	forms = append(forms, b.legacy...)
	slices.SortFunc(forms, func(x, y string) int { return cmp.Compare(len(y), len(x)) })
	return slices.Compact(forms)
}

func isBrand(provider, s string) bool {
	s = strings.TrimSpace(s)
	for _, f := range brandForms(provider) {
		if strings.EqualFold(s, f) {
			return true
		}
	}
	return false
}

// words splits s into words and the bracketed or punctuated bits around them.
func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune("-–—|·:：()（）[]【】,，/", r)
	})
}

// brandWords are the words the provider's brand and site are spelled with.
func brandWords(provider string) map[string]bool {
	out := map[string]bool{}
	for _, f := range append(brandForms(provider), brands[provider].site...) {
		out[strings.ToLower(f)] = true
		for _, w := range words(f) {
			out[strings.ToLower(w)] = true
		}
	}
	return out
}

// onlyBrandWords says whether every word of s is one of the brand's or site's own ("Claude", "Duet - Claude").
func onlyBrandWords(provider, s string) bool {
	ws := words(s)
	if len(ws) == 0 {
		return true
	}
	known := brandWords(provider)
	for _, w := range ws {
		if !known[strings.ToLower(w)] {
			return false
		}
	}
	return true
}

// Compose is brand + Sep + alias, or the brand alone when alias is empty or only the brand's own words ("Claude"
// for Claude Duet, "Gemini" for Gemini).
func Compose(lang, provider, alias string) string {
	alias = strings.TrimSpace(alias)
	if onlyBrandWords(provider, alias) {
		return Brand(lang, provider)
	}
	return Brand(lang, provider) + Sep + alias
}

// Alias is alias as Compose keeps it: "" when it is only the brand's own words.
func Alias(provider, alias string) string {
	alias = strings.TrimSpace(alias)
	if onlyBrandWords(provider, alias) {
		return ""
	}
	return alias
}

// Stored is the display name pairing stores (SourceAuto) for an account Verify found alias for.
func Stored(provider, alias string) string { return Compose("en", provider, alias) }

// composedAlias reads a name in the exact form club composes: a brand spelling alone (alias "") or a brand spelling,
// Sep and an alias.
func composedAlias(provider, stored string) (string, bool) {
	raw, ok := composedRaw(provider, stored)
	return Alias(provider, raw), ok
}

// composedRaw is composedAlias with the alias as written, before Alias drops the brand's own words.
func composedRaw(provider, stored string) (string, bool) {
	if isBrand(provider, stored) {
		return "", true
	}
	for _, f := range brandForms(provider) {
		if len(stored) > len(f)+len(Sep) && strings.EqualFold(stored[:len(f)], f) && stored[len(f):len(f)+len(Sep)] == Sep {
			return strings.TrimSpace(stored[len(f)+len(Sep):]), true
		}
	}
	return "", false
}

// bareBefore are the providers whose Verify, before club kept who named an account, only ever named it with the
// bare brand ("Grok Bot", "Groq", "Muse (muse2api)", "Echo"): a legacy "<Brand> · x" of theirs was typed by the owner
// (0.6.0 prefilled the rename field with the name, so editing it gives this shape).
var bareBefore = map[string]bool{"grok_bot": true, "groq": true, "muse2api": true, "echo": true}

// LegacyAlias is the x of a legacy row named "<Brand> · x" with an x that is not only the brand's own words, for a
// provider whose earlier Verify did write that shape: such a row is club's only while x is still the alias Verify
// reports; otherwise the owner wrote it. ok is false for any other row.
func LegacyAlias(provider, stored string) (x string, ok bool) {
	stored = strings.TrimSpace(stored)
	if bareBefore[provider] {
		return "", false
	}
	if raw, composed := composedRaw(provider, stored); composed && Alias(provider, raw) != "" {
		return Alias(provider, raw), true
	}
	return "", false
}

// isPageTitle says whether a legacy browser row's name is a tab title of the site's own words ("Claude Duet - Claude",
// "Gemini", "ChatGPT"), which rows before 0.5 stored.
func isPageTitle(provider, stored string) bool {
	return len(brands[provider].site) > 0 && onlyBrandWords(provider, stored)
}

// Split reads a stored display name and its source: the alias of a name club composed ("" for only the brand), or
// the whole name with owner true when it is the owner's.
func Split(provider, stored, source string) (alias string, owner bool) {
	stored = strings.TrimSpace(stored)
	if source == SourceOwner && stored != "" {
		return stored, true
	}
	if stored == "" {
		return "", false
	}
	if raw, ok := composedRaw(provider, stored); ok {
		if source == SourceLegacy && raw != "" && bareBefore[provider] {
			return stored, true // the owner's: club never wrote an alias for this provider
		}
		return Alias(provider, raw), false
	}
	if source == SourceLegacy && isPageTitle(provider, stored) {
		return "", false
	}
	// Not a shape club writes: the owner's, shown rather than guessed at.
	return stored, true
}

// Title is how an account is called in lang: the owner's own name as it is, else the brand and the alias, an e-mail
// alias shortened to the part before @ (Short).
func Title(lang, provider, stored, source string) string {
	alias, owner := Split(provider, stored, source)
	if owner {
		return alias
	}
	return Compose(lang, provider, Short(provider, alias))
}

// Short is how a composed title shows an alias: an e-mail ("ada@example.com", the alias Grok Bot, Gemini and ChatGPT
// Dots fall back to) by the part before @ ("ada"), unless that part is only the brand's own words and would vanish;
// any other alias as it is. Only titles use it: the stored name keeps the whole e-mail (Email), so a row stored
// before reads short too, and two e-mails sharing that part are told apart by Distinct's numbering.
func Short(provider, alias string) string {
	if local, ok := emailLocal(alias); ok && Alias(provider, local) != "" {
		return local
	}
	return alias
}

// emailLocal is the part before @ of s when s is one e-mail address.
func emailLocal(s string) (string, bool) {
	s = strings.TrimSpace(s)
	local, domain, ok := strings.Cut(s, "@")
	if !ok || local == "" || strings.ContainsAny(s, " \t\n") || strings.Contains(domain, "@") ||
		!strings.Contains(strings.Trim(domain, "."), ".") {
		return "", false
	}
	return local, true
}

// Email is the whole e-mail a composed title shortened (Short), for the account's page; "" for an owner's name or
// an alias that is not an e-mail.
func Email(provider, stored, source string) string {
	alias, owner := Split(provider, stored, source)
	if owner || Short(provider, alias) == alias {
		return ""
	}
	return alias
}

// Account is what Distinct needs of a paired account.
type Account struct {
	ID, Provider, Stored, Source string
	CreatedAt                    int64
}

// Distinct titles every account (Title in lang) so no two composed titles read the same: accounts whose composed
// titles collide with each other or with an owner's name (two Grok Bot accounts with no alias) are numbered in
// pairing order, "Grok Bot (1)", "Grok Bot (2)", skipping a number some other account's title already reads. An
// owner's name is never changed, even when two repeat.
func Distinct(lang string, accts []Account) map[string]string {
	out := make(map[string]string, len(accts))
	taken := map[string]bool{}
	ownerTitles := map[string]bool{}
	groups := map[string][]Account{}
	for _, a := range accts {
		t := Title(lang, a.Provider, a.Stored, a.Source)
		out[a.ID] = t
		key := strings.ToLower(t)
		taken[key] = true
		if _, owner := Split(a.Provider, a.Stored, a.Source); owner {
			ownerTitles[key] = true
			continue
		}
		groups[key] = append(groups[key], a)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, key := range keys {
		g := groups[key]
		if len(g) < 2 && !ownerTitles[key] {
			continue
		}
		slices.SortFunc(g, func(x, y Account) int {
			return cmp.Or(cmp.Compare(x.CreatedAt, y.CreatedAt), cmp.Compare(x.ID, y.ID))
		})
		n := 0
		for _, a := range g {
			base := out[a.ID]
			for {
				n++
				t := fmt.Sprintf("%s (%d)", base, n)
				if !taken[strings.ToLower(t)] {
					out[a.ID] = t
					taken[strings.ToLower(t)] = true
					break
				}
			}
		}
	}
	return out
}

// OwnerChose says whether a legacy row's (SourceLegacy) name is the owner's: anything but the exact forms club
// wrote and, for a browser service, a tab title of the site's own words.
func OwnerChose(provider, stored string) bool {
	_, owner := Split(provider, stored, SourceLegacy)
	return owner
}

// Fallback is what an account is called where only its display name is at hand (a launcher, an assistant session):
// that name, else the brand in lang, never the raw provider id.
func Fallback(lang, provider, displayName string) string {
	n := strings.TrimSpace(displayName)
	if n == "" || n == provider {
		return Brand(lang, provider)
	}
	return n
}
