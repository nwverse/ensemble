package names

import (
	"bytes"
	"encoding/json"
	"image"
	"image/png"
	"io/fs"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
)

// The tiles in tiles/ are the brands table's: tiles.json is TileSpecs (UPDATE_TILES=1 writes it), every spec has its
// PNG and no PNG is left over, each a 128 px square of its color whose middle has white letters. The colors differ
// from each other and carry white letters in both themes.
func TestTilesFollowTheTable(t *testing.T) {
	specs := TileSpecs()
	want, _ := json.MarshalIndent(specs, "", "  ")
	want = append(want, '\n')
	if os.Getenv("UPDATE_TILES") != "" {
		if err := os.WriteFile("tiles/tiles.json", want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile("tiles/tiles.json")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("tiles/tiles.json is not the brands table's (UPDATE_TILES=1 go test ./internal/names, then python3 -I scripts/avatar-tiles.py): %v\n%s", err, want)
	}
	files := map[string]bool{}
	for _, s := range specs {
		files[s.File+".png"] = true
		data, ok := Tile(tileRefPrefix + s.File)
		if !ok {
			t.Errorf("%s: no tile", s.File)
			continue
		}
		img, err := png.Decode(bytes.NewReader(data))
		if err != nil {
			t.Errorf("%s: %v", s.File, err)
			continue
		}
		if b := img.Bounds(); b.Dx() != 128 || b.Dy() != 128 || len(data) > 8<<10 {
			t.Errorf("%s: %dx%d, %d bytes", s.File, b.Dx(), b.Dy(), len(data))
		}
		if c := rgb(img, 2, 2); c != hexColor(s.Color) {
			t.Errorf("%s: corner %v, the table's %s", s.File, c, s.Color)
		}
		if !hasWhite(img) {
			t.Errorf("%s: no white letters", s.File)
		}
	}
	entries, _ := fs.ReadDir(tileFiles, "tiles")
	for _, e := range entries {
		if !files[e.Name()] {
			t.Errorf("tiles/%s is in no spec", e.Name())
		}
	}

	seen := map[string]string{}
	for p, b := range brands {
		if b.color == "" {
			t.Errorf("%s has no color", p)
			continue
		}
		if other, dup := seen[b.color]; dup {
			t.Errorf("%s and %s share %s", p, other, b.color)
		}
		seen[b.color] = p
		c := hexColor(b.color)
		if r := (1.05) / (luminance(c) + 0.05); r < 4.5 {
			t.Errorf("%s: white on %s is %.1f:1, under 4.5:1", p, b.color, r)
		}
		for q, o := range brands {
			if q > p && o.color != "" {
				if d := deltaE(c, hexColor(o.color)); d < 25 {
					t.Errorf("%s %s and %s %s look alike (ΔE %.0f)", p, b.color, q, o.color, d)
				}
			}
		}
	}
}

// A tile's ref names the tile with the letters the same language shows; anything else names none, and nothing is
// fetched: the tiles are in the program.
func TestTileRefs(t *testing.T) {
	for _, c := range []struct{ lang, provider, ref string }{
		{"zh-Hans", "dots", "tile/dots"}, {"en", "dots", "tile/dots"},
		{"zh-Hans", "muse2api", "tile/muse2api.zh"}, {"en", "muse2api", "tile/muse2api"},
		{"", "echo", "tile/echo.zh"}, {"en", "echo", "tile/echo"},
		{"zh-Hans", "my_site", ""},
	} {
		if got := TileRef(c.lang, c.provider); got != c.ref {
			t.Errorf("TileRef(%q, %q) = %q, want %q", c.lang, c.provider, got, c.ref)
		}
	}
	for _, s := range TileSpecs() {
		if !strings.HasSuffix(s.File, ".zh") && Letters("en", s.File) != s.Letters {
			t.Errorf("%s: tile %q, letters %q", s.File, s.Letters, Letters("en", s.File))
		}
		if base, ok := strings.CutSuffix(s.File, ".zh"); ok && Letters("zh", base) != s.Letters {
			t.Errorf("%s: tile %q, letters %q", s.File, s.Letters, Letters("zh", base))
		}
	}
	for _, ref := range []string{"", "tile/", "tile/nope", "dots", "tile/../names.go", "tile/tiles/dots", "https://example.com/a.png"} {
		if _, ok := Tile(ref); ok {
			t.Errorf("Tile(%q) answered", ref)
		}
	}
}

func rgb(img image.Image, x, y int) [3]uint8 {
	r, g, b, _ := img.At(x, y).RGBA()
	return [3]uint8{uint8(r >> 8), uint8(g >> 8), uint8(b >> 8)}
}

func hasWhite(img image.Image) bool {
	b := img.Bounds()
	for y := b.Min.Y + b.Dy()/4; y < b.Max.Y-b.Dy()/4; y++ {
		for x := b.Min.X + b.Dx()/4; x < b.Max.X-b.Dx()/4; x++ {
			if c := rgb(img, x, y); c[0] > 245 && c[1] > 245 && c[2] > 245 {
				return true
			}
		}
	}
	return false
}

func hexColor(s string) [3]uint8 {
	v, _ := strconv.ParseUint(strings.TrimPrefix(s, "#"), 16, 32)
	return [3]uint8{uint8(v >> 16), uint8(v >> 8), uint8(v)}
}

func linear(c uint8) float64 {
	v := float64(c) / 255
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

func luminance(c [3]uint8) float64 {
	return 0.2126*linear(c[0]) + 0.7152*linear(c[1]) + 0.0722*linear(c[2])
}

// deltaE is the CIE76 distance of two sRGB colors.
func deltaE(a, b [3]uint8) float64 {
	lab := func(c [3]uint8) [3]float64 {
		r, g, bl := linear(c[0]), linear(c[1]), linear(c[2])
		x := (0.4124*r + 0.3576*g + 0.1805*bl) / 0.95047
		y := 0.2126*r + 0.7152*g + 0.0722*bl
		z := (0.0193*r + 0.1192*g + 0.9505*bl) / 1.08883
		f := func(t float64) float64 {
			if t > 0.008856 {
				return math.Cbrt(t)
			}
			return 7.787*t + 16.0/116
		}
		return [3]float64{116*f(y) - 16, 500 * (f(x) - f(y)), 200 * (f(y) - f(z))}
	}
	p, q := lab(a), lab(b)
	return math.Sqrt((p[0]-q[0])*(p[0]-q[0]) + (p[1]-q[1])*(p[1]-q[1]) + (p[2]-q[2])*(p[2]-q[2]))
}
