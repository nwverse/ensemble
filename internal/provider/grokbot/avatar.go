package grokbot

import (
	"encoding/base64"
	"net/url"
	"regexp"
	"unicode/utf16"

	"github.com/nwverse/ensemble/internal/provider/web"
)

// The avatar algorithm is a port of grok-bot-sdk's avatar.mjs (MIT, RongleCat, algorithm grok-bot-0.70.2).

var defaultShapes = []string{"blob", "pebble", "squircle", "tablet", "wedge", "hex", "cloud", "teardrop"}

var validShapes = map[string]bool{"blob": true, "pebble": true, "bean": true, "egg": true, "squircle": true,
	"tablet": true, "capsule": true, "cylinder": true, "hex": true, "gem": true, "crystal": true, "wedge": true,
	"shield": true, "dome": true, "arch": true, "cloud": true, "teardrop": true, "leaf": true, "square": true,
	"sparkle": true, "clover": true, "flower": true, "heart": true, "house": true, "star": true, "pumpkin": true}

var defaultColors = []string{"brown", "red", "orange", "yellow", "green", "cyan", "blue", "violet", "magenta", "gray"}

var palette = map[string]string{"black": "#000000", "brown": "#936439", "red": "#FF263C", "orange": "#FF6700",
	"yellow": "#FF9800", "green": "#00C972", "cyan": "#00BCA6", "blue": "#1084FE", "violet": "#9159FE",
	"magenta": "#FF309B", "gray": "#777777"}

var shapePaths = map[string]string{
	"blob":     "M50 8a42 42 0 1 1 0 84a42 42 0 0 1 0-84z",
	"squircle": "M22 14h56a22 22 0 0 1 22 22v28a22 22 0 0 1-22 22H22A22 22 0 0 1 0 64V36A22 22 0 0 1 22 14z",
	"cloud":    "M24 64c-10 0-14-10-8-16c-6-10 2-22 14-18c4-12 22-16 32-6c12-8 28 2 24 16c12 2 14 18 4 24c-2 12-16 16-28 10c-8 10-28 8-38-10z",
	"teardrop": "M50 6C72 28 94 46 86 68C78 92 60 98 50 98C40 98 22 92 14 68C6 46 28 28 50 6z",
	"hex":      "M50 6L88 28V72L50 94L12 72V28z",
	"pebble":   "M50 28C76 22 98 40 94 58C100 80 74 96 50 92C24 98 4 78 10 56C2 36 26 24 50 28z",
	"wedge":    "M50 10L88 78Q92 94 74 94H26Q8 94 12 78Z",
	"capsule":  "M36 6H64A18 18 0 0 1 64 94H36A18 18 0 0 1 36 6Z",
}

const (
	pictureURLMax  = 2048
	pictureDataMax = 64 * 1024
)

var pictureDataURL = regexp.MustCompile(`^data:image/(?:png|jpeg|webp|gif);base64,[A-Za-z0-9+/]+=*$`)

func hashKey(key string) uint32 {
	h := uint32(2166136261)
	for _, c := range utf16.Encode([]rune(key)) {
		h = (h ^ uint32(c)) * 16777619
	}
	return h
}

// Character is the shape and color the official client derives from an agent id alone.
func Character(key string) (shape, color string) {
	h := hashKey(key)
	s := h
	s = (s ^ (s >> 16)) * 73244475
	s = (s ^ (s >> 13)) * 3266489909
	s = s ^ (s >> 16)
	seed := h + 1831565813
	sample := (seed ^ (seed >> 15)) * (1 | seed)
	sample = (sample + (sample^(sample>>7))*(61|sample)) ^ sample
	random := float64(sample^(sample>>14)) / 4294967296
	return defaultShapes[s%uint32(len(defaultShapes))], defaultColors[int(random*float64(len(defaultColors)))]
}

func pictureURL(v string) string {
	if v == "" {
		return ""
	}
	if len(v) >= 5 && v[:5] == "data:" {
		if pictureDataURL.MatchString(v) && len(v) <= pictureDataMax {
			return v
		}
		return ""
	}
	if len(v) > pictureURLMax {
		return ""
	}
	if u, err := url.Parse(v); err == nil && u.Scheme == "https" && u.Host != "" {
		return v
	}
	return ""
}

// AvatarURL is the avatar a roster record shows: its own picture when it has an allowed one (https, or a small
// image data URL), else a generated shape in the palette color as an SVG data URL. "" without an agent id.
func AvatarURL(agent map[string]any) string {
	id := web.Text(agent["agentId"])
	if id == "" {
		return ""
	}
	picture := web.Text(agent["avatarUrl"])
	if picture == "" {
		picture = web.Text(agent["avatarDataUrl"])
	}
	if p := pictureURL(picture); p != "" {
		return p
	}
	shape, color := Character(id)
	if s := web.Text(agent["avatarShape"]); validShapes[s] {
		shape = s
	}
	if c := web.Text(agent["avatarColor"]); palette[c] != "" {
		color = c
	}
	path, ok := shapePaths[shape]
	if !ok {
		path = shapePaths["blob"]
	}
	svg := `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 100 100"><path d="` + path + `" fill="` + palette[color] + `"/></svg>`
	return "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte(svg))
}
