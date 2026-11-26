#!/usr/bin/env python3
"""Draw the services' avatar tiles: internal/names/tiles/<file>.png from internal/names/tiles/tiles.json.

Each tile is a plain square of the service's color with its one or two letters in white, centred: no brand
artwork. The table in internal/names/names.go is the source; refresh tiles.json from it first, then draw:

  UPDATE_TILES=1 go test ./internal/names -run TestTilesFollowTheTable
  python3 -I scripts/avatar-tiles.py [font]

font is a bold font with Latin and Chinese letters (default: Noto Sans CJK SC Bold, index 2 of NotoSansCJK-Bold.ttc).
Needs Pillow. TestTilesFollowTheTable checks every tile is there, the right size and the right color.
"""
import json
import os
import sys

from PIL import Image, ImageDraw, ImageFont

SIZE = 128  # pixels a side: a 40 px header avatar at 3x
SCALE = 4  # drawn this much larger, then reduced, for smooth edges
FONT = "/usr/share/fonts/opentype/noto/NotoSansCJK-Bold.ttc"
FONT_INDEX = 2  # Simplified Chinese in Noto's collection


def color(hexstr):
    return tuple(int(hexstr[i:i + 2], 16) for i in (1, 3, 5))


def tile(spec, font_path, index):
    big = SIZE * SCALE
    img = Image.new("RGB", (big, big), color(spec["color"]))
    letters = spec["letters"]
    wide = any(ord(ch) > 0x2E80 for ch in letters)
    # a Chinese character fills more of the square than two Latin letters
    size = int(big * (0.50 if wide else 0.44))
    font = ImageFont.truetype(font_path, size, index=index)
    draw = ImageDraw.Draw(img)
    left, top, right, bottom = draw.textbbox((0, 0), letters, font=font)
    x = (big - (right - left)) / 2 - left
    y = (big - (bottom - top)) / 2 - top
    draw.text((x, y), letters, font=font, fill=(255, 255, 255))
    return img.resize((SIZE, SIZE), Image.LANCZOS)


def main():
    root = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "internal", "names", "tiles")
    font_path = sys.argv[1] if len(sys.argv) > 1 else FONT
    index = FONT_INDEX if font_path.endswith(".ttc") else 0
    with open(os.path.join(root, "tiles.json"), encoding="utf-8") as f:
        specs = json.load(f)
    for spec in specs:
        out = os.path.join(root, spec["file"] + ".png")
        tile(spec, font_path, index).save(out, optimize=True)
        print(out)


if __name__ == "__main__":
    main()
