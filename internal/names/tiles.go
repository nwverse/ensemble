package names

import (
	"embed"
	"slices"
	"strings"
)

// A service's avatar tile: its letters (Letters) in white on its own color (the brands table), a small square PNG the
// pages serve themselves by ref (ui.image, UI version 6), so every service's avatar has a color of its own where an app
// draws every letter avatar alike. Plain color and letters only, no brand artwork. The tiles are drawn from TileSpecs
// by scripts/avatar-tiles.py into tiles/ (tiles.json is TileSpecs as the script reads it); TestTilesFollowTheTable
// holds them to the table.

//go:embed tiles/*.png
var tileFiles embed.FS

// tileRefPrefix starts a tile's ref, the extension's own name for the picture.
const tileRefPrefix = "tile/"

// TileSpec is one tile: its file in tiles/ (without .png), its letters and its color.
type TileSpec struct {
	File    string `json:"file"`
	Letters string `json:"letters"`
	Color   string `json:"color"`
}

// TileSpecs are the tiles the brands table asks for: one per service with a color, and one more for the Chinese
// letters of a service that has them, by file.
func TileSpecs() []TileSpec {
	var specs []TileSpec
	for p, b := range brands {
		if b.color == "" {
			continue
		}
		specs = append(specs, TileSpec{File: p, Letters: b.letters, Color: b.color})
		if b.lettersZh != "" {
			specs = append(specs, TileSpec{File: p + ".zh", Letters: b.lettersZh, Color: b.color})
		}
	}
	slices.SortFunc(specs, func(x, y TileSpec) int { return strings.Compare(x.File, y.File) })
	return specs
}

// TileRef is the ref of provider's tile in lang (the one with Letters(lang, provider) on it), "" for a provider
// without one.
func TileRef(lang, provider string) string {
	b, ok := brands[provider]
	if !ok || b.color == "" {
		return ""
	}
	if isZh(lang) && b.lettersZh != "" {
		return tileRefPrefix + provider + ".zh"
	}
	return tileRefPrefix + provider
}

// Tile is the PNG a TileRef names; false for any other ref.
func Tile(ref string) ([]byte, bool) {
	file, ok := strings.CutPrefix(ref, tileRefPrefix)
	if !ok || file == "" || strings.ContainsAny(file, "/\\") {
		return nil, false
	}
	data, err := tileFiles.ReadFile("tiles/" + file + ".png")
	if err != nil {
		return nil, false
	}
	return data, true
}
