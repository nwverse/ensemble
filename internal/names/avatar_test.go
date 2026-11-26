package names

import (
	"bytes"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// productHosts are the sites each provider's code talks to (its API, sign-in or the page it drives), which is where
// its product's icon may come from; a provider missing here may have no icon.
var productHosts = map[string][]string{
	"claude_duet":  {"claude.ai"},                            // browser.SiteClaudeDuet: https://claude.ai/duet
	"muse":         {"muse.ai"},                              // browser.SiteMuse: https://muse.ai/
	"muse2api":     {"muse.ai"},                              // muse2api drives muse.ai's web chat
	"cue":          {"manus.im"},                             // cue.Origin https://api.manus.ai, task_url https://manus.im/app/…
	"groq":         {"groq.com"},                             // groq.Origin https://api.groq.com
	"dots":         {"chatgpt.com"},                          // browser.SiteDots
	"gemini_tasks": {"gemini.google.com", "www.gstatic.com"}, // browser.SiteGeminiTasks; Google's product branding
	"grok_bot":     {"is1-ssl.mzstatic.com"},                 // the App Store artwork of its own app, co.anysphere.sand
}

// Every service's avatar is, in order: its own picture when it has one (https, or an image data URL), else its
// product's official icon (https, on the product's own site), else its tile and letters. The letters are always
// there for while a picture loads and when it fails.
func TestAvatarOrderPerService(t *testing.T) {
	own := map[string]bool{"https://images.example.com/bot.png": true, "data:image/png;base64,iVBORw0KGgo=": true,
		"data:image/svg+xml;base64,PHN2Zy8+": true}
	refused := []string{"http://images.example.com/bot.png", "https://user:pw@images.example.com/bot.png",
		"javascript:alert(1)", "data:text/html;base64,PGI+", "https://images.example.com/" + strings.Repeat("a", 2100)}
	for p := range brands {
		for _, lang := range []string{"zh-Hans", "en"} {
			base := Avatar(lang, p, "")
			if base.Own != "" || base.Text != Letters(lang, p) || base.Text == "" || base.Ref != TileRef(lang, p) || base.Ref == "" {
				t.Errorf("%s %s: %+v", p, lang, base)
			}
			if base.Icon != Icon(p) {
				t.Errorf("%s: icon %q, table %q", p, base.Icon, Icon(p))
			}
			for o := range own {
				if got := Avatar(lang, p, o); got.Own != o || got.Icon != base.Icon || got.Ref != base.Ref || got.Text != base.Text {
					t.Errorf("%s: own %q gave %+v", p, o, got)
				}
			}
			for _, o := range refused {
				if got := Avatar(lang, p, o); got != base {
					t.Errorf("%s: refused own %q gave %+v", p, o, got)
				}
			}
		}
		icon := Icon(p)
		if icon == "" {
			continue
		}
		u, err := url.Parse(icon)
		if err != nil || !HTTPS(icon) || u.RawQuery != "" || u.Fragment != "" {
			t.Errorf("%s: icon %q is not a plain https address", p, icon)
			continue
		}
		ok := false
		for _, h := range productHosts[p] {
			ok = ok || u.Hostname() == h || strings.HasSuffix(u.Hostname(), "."+h)
		}
		if !ok {
			t.Errorf("%s: icon %q is not on the product's own site %v", p, icon, productHosts[p])
		}
		if ext := strings.ToLower(filepath.Ext(u.Path)); ext != ".png" && ext != ".ico" {
			t.Errorf("%s: icon %q is not a PNG or ICO", p, icon)
		}
	}
}

// Grok Bot is its own product (its API is api2.cursor.sh, its sign-in cursor.com), not xAI's Grok: its icon is the
// App Store artwork of its own app (co.anysphere.sand), and no icon ever resolves to grok.com or x.ai, nor to any
// address with "grok" in it. With no own picture it shows that artwork, with its letters for when it fails.
func TestGrokBotIsNotGrok(t *testing.T) {
	for _, lang := range []string{"zh-Hans", "en"} {
		got := Avatar(lang, "grok_bot", "")
		u, err := url.Parse(got.Icon)
		if err != nil || u.Scheme != "https" || u.Hostname() != "is1-ssl.mzstatic.com" ||
			got.Ref != TileRef(lang, "grok_bot") || got.Text != "GB" {
			t.Errorf("Grok Bot without a picture: %+v", got)
		}
	}
	for p, icon := range icons {
		h := strings.ToLower(icon)
		if strings.Contains(h, "x.ai") || strings.Contains(h, "grok") || strings.Contains(h, "twitter") {
			t.Errorf("%s: icon %s is xAI's", p, icon)
		}
	}
}

// No file in the repository is a picture except the generated letter tiles tiles.json lists: no brand artwork is
// committed, as a file, an image by its bytes, or an image data URL in the source.
func TestNoPictureButTheTiles(t *testing.T) {
	out, err := exec.Command("git", "-C", "../..", "ls-files", "-z").Output()
	if err != nil {
		t.Skipf("no git checkout: %v", err)
	}
	tiles := map[string]bool{}
	for _, s := range TileSpecs() {
		tiles["internal/names/tiles/"+s.File+".png"] = true
	}
	magic := [][]byte{{0x89, 'P', 'N', 'G'}, {0xFF, 0xD8, 0xFF}, []byte("GIF8"), {0, 0, 1, 0}, []byte("RIFF")}
	for _, f := range strings.Split(strings.TrimRight(string(out), "\x00"), "\x00") {
		data, err := os.ReadFile(filepath.Join("../..", f))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if tiles[f] {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f))
		switch ext {
		case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".ico", ".svg", ".icns", ".bmp":
			t.Errorf("%s is a picture", f)
			continue
		}
		for _, m := range magic {
			if bytes.HasPrefix(data, m) && ext != ".go" {
				t.Errorf("%s holds a picture", f)
			}
		}
		if strings.HasSuffix(f, "_test.go") {
			continue // tests carry tiny made-up data URLs
		}
		if i := bytes.Index(data, []byte("data:image/")); i >= 0 && bytes.Contains(data[i:min(len(data), i+40)], []byte(";base64,")) &&
			bytes.Contains(data[i:min(len(data), i+400)], []byte("iVBOR")) {
			t.Errorf("%s embeds a picture", f)
		}
	}
}
