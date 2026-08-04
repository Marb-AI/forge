#!/usr/bin/env python3
"""Render the app icon into an .iconset, from the same anvil as the favicon.

The mark is one closed polygon of straight segments — that is the whole reason
this can be a script with no dependencies rather than a build tool nobody has
installed. The path below is the favicon's, unchanged, on its 32x32 grid.

macOS wants a tile, not a glyph: the icon sits in a Dock full of rounded squares,
and a bare shape floating on transparency reads as a broken download. So the anvil
goes light on a dark rounded ground, with the corner radius and the inset the
platform uses, and the same file works on both desktop backgrounds.

Everything is drawn 4x and averaged down, because the only artefact anyone would
ever notice here is a jagged corner radius.

Usage: python3 build/icon.py <out-dir>.iconset
"""

import os
import struct
import sys
import zlib

# The favicon's path, on its own 32x32 viewBox. Straight lines only.
ANVIL = [
    (1, 10), (6, 6), (30, 6), (30, 12), (20, 12), (20, 20),
    (27, 20), (27, 26), (5, 26), (5, 20), (12, 20), (12, 12), (6, 12),
]
GRID = 32.0

GROUND = (22, 24, 29)     # the favicon's own dark, which is the UI's ink
MARK = (232, 232, 232)    # and its light, so the two files stay one design

# What macOS asks for. Each is written twice, once as name@2x at double the pixels.
SIZES = [16, 32, 128, 256, 512]

SS = 4  # supersampling factor


def crossings(poly, y):
    """Where a horizontal line at y enters and leaves the polygon, in order.

    A scanline rather than a test per pixel: at 1024px supersampled the icon is
    16 million points, and asking each of them about all thirteen edges is the
    difference between a build step and a wait.
    """
    xs = []
    j = len(poly) - 1
    for i in range(len(poly)):
        xi, yi = poly[i]
        xj, yj = poly[j]
        if (yi > y) != (yj > y):
            xs.append((xj - xi) * (y - yi) / (yj - yi) + xi)
        j = i
    xs.sort()
    # Even-odd, which is the SVG's default fill rule: the spans are the pairs.
    return list(zip(xs[0::2], xs[1::2]))


def render(px):
    """One icon at px pixels square, as RGBA rows."""
    n = px * SS
    # The tile: macOS insets the artwork and rounds it. The numbers are Apple's
    # own proportions for a Big Sur icon — 824/1024 of the canvas, radius 185/824.
    inset = n * (1024 - 824) / 2 / 1024
    side = n - 2 * inset
    radius = side * 185 / 824

    # The mark, scaled to sit inside the tile with room around it.
    pad = side * 0.19
    scale = (side - 2 * pad) / GRID
    poly = [(inset + pad + x * scale, inset + pad + y * scale) for x, y in ANVIL]

    # Rows of (r, g, b, a) at supersampled resolution, then boxed down.
    acc = [[0] * (px * 4) for _ in range(px)]
    for sy in range(n):
        y = sy + 0.5
        if y < inset or y > n - inset:
            continue
        # The tile's own span on this row. Straight sides for most of the icon;
        # inside a corner's band the radius pulls the edge in by the circle.
        cy = min(max(y, inset + radius), n - inset - radius)
        dy = y - cy
        half = (radius * radius - dy * dy) ** 0.5 if dy else radius
        left, right = inset + radius - half, n - inset - radius + half

        row = acc[sy // SS]

        def paint(a, b, colour):
            r, g, bl = colour
            for sx in range(max(int(a), 0), min(int(b) + 1, n)):
                x = sx + 0.5
                if x < a or x > b:
                    continue
                i = (sx // SS) * 4
                row[i] += r
                row[i + 1] += g
                row[i + 2] += bl
                row[i + 3] += 255

        # Ground and mark tile the row between them: each pixel is painted once,
        # or the accumulator would average a colour nothing on screen ever is.
        at = left
        for a, b in crossings(poly, y):
            a, b = max(a, left), min(b, right)
            if b <= a:
                continue
            paint(at, a, GROUND)
            paint(a, b, MARK)
            at = b
        paint(at, right, GROUND)

    per = SS * SS
    return [bytes(v // per for v in row) for row in acc]


def png(path, px, rows):
    raw = b"".join(b"\x00" + row for row in rows)

    def chunk(kind, data):
        c = kind + data
        return struct.pack(">I", len(data)) + c + struct.pack(">I", zlib.crc32(c))

    with open(path, "wb") as f:
        f.write(b"\x89PNG\r\n\x1a\n")
        f.write(chunk(b"IHDR", struct.pack(">IIBBBBB", px, px, 8, 6, 0, 0, 0)))
        f.write(chunk(b"IDAT", zlib.compress(raw, 9)))
        f.write(chunk(b"IEND", b""))


def main():
    if len(sys.argv) != 2:
        sys.exit("usage: icon.py <out-dir>.iconset")
    out = sys.argv[1]
    os.makedirs(out, exist_ok=True)
    # Every size is needed at 1x and 2x, and 2x of one size is 1x of the next —
    # so each pixel count is rendered once and written under both names it has.
    want = {}
    for s in SIZES:
        want.setdefault(s, []).append(f"icon_{s}x{s}.png")
        want.setdefault(s * 2, []).append(f"icon_{s}x{s}@2x.png")
    for px in sorted(want):
        rows = render(px)
        for name in want[px]:
            png(os.path.join(out, name), px, rows)


if __name__ == "__main__":
    main()
