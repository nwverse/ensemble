package names

import (
	"net/url"
	"regexp"
	"strings"
)

// An avatar is chosen in this order, for every picture club supplies (account rows, add rows, an account's page, the
// accounts and sessions handed to Persona):
//
//  1. Own: the account's or agent's own picture, when its service gives one (a Grok Bot's avatar from its roster).
//  2. Product: the icon of the product the account is at, by the address the app loads it from: the product's own
//     site's official icon (Icon). Only a product whose own icon could be established has one; the label's words
//     never decide it (Grok Bot is not xAI's Grok and has none here).
//  3. Tile: the service's letters on its color (TileRef, served by the pages), and the letters alone for an app that
//     draws no ref.
//
// Nothing is fetched or stored by club for 2: the app loads the icon itself, with no account of the owner's in the
// address, and draws the letters while it loads or when it fails.

// Picture kinds, in the order Avatar tries them.
const (
	PictureOwn     = "own"
	PictureProduct = "product"
	PictureTile    = "tile"
)

// icons are the products' official icons: a PNG or ICO the product's own site serves at a fixed address, the site
// the provider's code talks to (see each provider's package). A product with no entry has no icon established and
// shows its letters (the owner decides those):
//   - grok_bot: the API is api2.cursor.sh and sign-in is on cursor.com, but neither names an icon of Grok Bot's own;
//     xAI's Grok (grok.com, x.ai) is another product and never its icon.
//   - dots: chatgpt.com serves its icons only under addresses that change with every release.
//   - gemini_tasks: gemini.google.com serves no icon at a fixed address (its page points at a versioned file).
//   - muse2api: the owner's own relay, not a product with an icon; Muse's would say it is the web version.
//   - echo: the demo, no product.
var icons = map[string]string{
	"claude_duet": "https://claude.ai/apple-touch-icon.png", // claude.ai/duet, the page the provider drives
	"muse":        "https://muse.ai/favicon.ico",            // muse.ai, the site the provider drives
	"cue":         "https://manus.im/apple-icon.png",        // Manus: api.manus.ai, tasks at manus.im/app
	"groq":        "https://groq.com/apple-touch-icon.png",  // Groq: api.groq.com
}

// Icon is the official icon of provider's product, "" when none is established.
func Icon(provider string) string { return icons[provider] }

// Picture is an avatar's candidates as Avatar found them, best first: the own picture (an https URL, or a data URL a
// roster gave), the product's icon (https), the tile's ref and the letters (always set). A consumer takes the first it
// can draw (Pick).
type Picture struct {
	Own  string
	Icon string
	Ref  string
	Text string
}

// ownPictureMax bounds an own picture's data URL, as the roster's are bounded where they are read.
const ownPictureMax = 64 * 1024

var dataPicture = regexp.MustCompile(`^data:image/(png|jpeg|webp|gif|svg\+xml);base64,[A-Za-z0-9+/]+=*$`)

// HTTPS says whether v is an https picture an app may load: a host, no user or password, not overlong.
func HTTPS(v string) bool {
	if len(v) > 2048 {
		return false
	}
	u, err := url.Parse(v)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil
}

// DataType is v's image type when it is a base64 data URL of a picture ("png", "svg+xml"), "" otherwise.
func DataType(v string) string {
	if len(v) > ownPictureMax {
		return ""
	}
	if m := dataPicture.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return ""
}

// Avatar is the avatar of an account (own: its own picture, "" for none) or of a way to add one (own ""), at
// provider, in lang. An own picture that is neither https nor an image data URL is dropped.
func Avatar(lang, provider, own string) Picture {
	own = strings.TrimSpace(own)
	if !HTTPS(own) && DataType(own) == "" {
		own = ""
	}
	return Picture{Own: own, Icon: Icon(provider), Ref: TileRef(lang, provider), Text: Letters(lang, provider)}
}

// URL is the https picture an app loads for p: the own picture when it is one, else the product's icon; "" when
// neither.
func (p Picture) URL() string {
	if HTTPS(p.Own) {
		return p.Own
	}
	return p.Icon
}

// Kind is which candidate an app that loads https pictures and data URLs draws: PictureOwn, PictureProduct or
// PictureTile.
func (p Picture) Kind() string {
	switch {
	case p.Own != "":
		return PictureOwn
	case p.Icon != "":
		return PictureProduct
	}
	return PictureTile
}
