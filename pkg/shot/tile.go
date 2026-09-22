package shot

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"sync"

	"github.com/gen2brain/webp"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/gomono"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// TileKind is the state a generated tile stands in for.
//
// Six of them, and the consumer picks from JSON it already holds rather than
// from a failed image load: a listing row knows a package is inert before it
// requests a picture of it, so the error handler is the last-resort net and not
// the mechanism.
type TileKind string

const (
	TileParked    TileKind = "parked"
	TilePure      TileKind = "pure"
	TileNoRender  TileKind = "no-render"
	TileDirectory TileKind = "directory"
	TileNone      TileKind = "none"
	TileError     TileKind = "error"
)

// tileCopy is the wording each tile carries. Two strings, not one: a short
// label for somebody scanning a dense table, and the plain line, which is the
// point. The plain line is always in the alt text, at every size, because a
// screen reader has no size.
var tileCopy = map[TileKind][2]string{
	TileParked:    {"not enabled yet", "Published, but still waiting for approval before anyone can use it."},
	TilePure:      {"code library", "Building blocks other programs use. It has no page of its own."},
	TileNoRender:  {"no page to show", "This program works. It just does not display anything."},
	TileDirectory: {"folder", "A folder, not a program. The things inside it have their own pages."},
	TileNone:      {"no preview", "No picture available for this one."},
	TileError:     {"unavailable", "Could not load a picture of this one just now."},
}

// TileCopy returns the short label and the plain line for a tile kind.
func TileCopy(k TileKind) (short, plain string) {
	c, ok := tileCopy[k]
	if !ok {
		c = tileCopy[TileNone]
	}
	return c[0], c[1]
}

// TileKindFor maps a manifest entry to the tile that should stand in for it.
func TileKindFor(matched Matched, state Status) TileKind {
	switch {
	case state == StatusInert:
		return TileParked
	case state == StatusNotFound:
		return TileNone
	case state == StatusError:
		return TileError
	case matched == MatchedDirectory:
		return TileDirectory
	case matched == MatchedStatus:
		return TileNone
	}
	return TileNone
}

var (
	fontOnce   sync.Once
	faceCache  sync.Map // cacheKey -> font.Face
	boldFont   *opentype.Font
	monoFont   *opentype.Font
	fontLoaded bool
)

func loadFonts() {
	fontOnce.Do(func() {
		b, err1 := opentype.Parse(gobold.TTF)
		m, err2 := opentype.Parse(gomono.TTF)
		if err1 == nil && err2 == nil {
			boldFont, monoFont, fontLoaded = b, m, true
		}
	})
}

func face(f *opentype.Font, size float64) font.Face {
	key := fmt.Sprintf("%p/%.1f", f, size)
	if v, ok := faceCache.Load(key); ok {
		return v.(font.Face)
	}
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	faceCache.Store(key, fc)
	return fc
}

// Tile draws the placeholder for one path and one state at one rung.
//
// Never a broken image and never a blank one. The reader this exists for does
// not know what a realm is, and a blank box reads as "the site is broken"
// rather than "this one has no picture yet".
//
// Deterministic: the colour is seeded from sha256 of the path, so the same
// package gets the same tile on every surface and across restarts, and the
// whole thing is cacheable forever.
func Tile(path string, kind TileKind, w, h int) image.Image {
	loadFonts()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	base, accent := tileColors(path)
	gradient(img, base, accent)

	if !fontLoaded || w < 96 {
		return img
	}
	short, plain := TileCopy(kind)
	leaf, prefix := splitPath(path)

	// Scale the type to the box so one function serves a 160x90 row and a
	// 1200x630 social card. Capped, because type that scales linearly with a
	// 7x taller box stops being a label and becomes a banner.
	unit := math.Min(float64(h)/90.0, 3.0)
	pad := int(8 * unit)
	ink := color.RGBA{0x11, 0x14, 0x18, 0xff}
	dim := color.RGBA{0x11, 0x14, 0x18, 0xaa}

	y := pad + int(13*unit)
	if prefix != "" && h >= 180 {
		drawText(img, face(monoFont, 8*unit), dim, pad, y, prefix, w-2*pad)
		y += int(12 * unit)
	}
	drawText(img, face(boldFont, 15*unit), ink, pad, y, leaf, w-2*pad)
	y += int(15 * unit)
	drawText(img, face(monoFont, 8.5*unit), dim, pad, y, strings.ToUpper(short), w-2*pad)
	if h >= 300 {
		y += int(14 * unit)
		f := face(monoFont, 9*unit)
		for _, line := range wrapToWidth(f, plain, w-2*pad) {
			drawText(img, f, dim, pad, y, line, w-2*pad)
			y += int(13 * unit)
		}
	}
	return img
}

// EncodeTile renders a tile straight to WebP at a rung.
func EncodeTile(path string, kind TileKind, r Rung) ([]byte, error) {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, Tile(path, kind, r.W, r.H), webp.Options{Quality: r.Quality, Method: 4}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// tileColors derives two related pastel tones from the path. Pastel because the
// tile carries dark text and has to stay readable, and because a listing of
// fifty of them should read as a set rather than a colour test card.
func tileColors(path string) (color.RGBA, color.RGBA) {
	sum := sha256.Sum256([]byte(path))
	hue := float64(uint16(sum[0])<<8|uint16(sum[1])) / 65535.0 * 360.0
	base := hsl(hue, 0.42, 0.90)
	accent := hsl(math.Mod(hue+28, 360), 0.46, 0.82)
	return base, accent
}

func gradient(img *image.RGBA, a, b color.RGBA) {
	bnd := img.Bounds()
	w, h := bnd.Dx(), bnd.Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			t := (float64(x)/float64(w) + float64(y)/float64(h)) / 2
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(float64(a.R)*(1-t) + float64(b.R)*t),
				G: uint8(float64(a.G)*(1-t) + float64(b.G)*t),
				B: uint8(float64(a.B)*(1-t) + float64(b.B)*t),
				A: 0xff,
			})
		}
	}
}

func hsl(h, s, l float64) color.RGBA {
	c := (1 - math.Abs(2*l-1)) * s
	x := c * (1 - math.Abs(math.Mod(h/60, 2)-1))
	m := l - c/2
	var r, g, b float64
	switch {
	case h < 60:
		r, g, b = c, x, 0
	case h < 120:
		r, g, b = x, c, 0
	case h < 180:
		r, g, b = 0, c, x
	case h < 240:
		r, g, b = 0, x, c
	case h < 300:
		r, g, b = x, 0, c
	default:
		r, g, b = c, 0, x
	}
	return color.RGBA{uint8((r + m) * 255), uint8((g + m) * 255), uint8((b + m) * 255), 0xff}
}

func drawText(dst draw.Image, f font.Face, col color.Color, x, y int, s string, maxW int) {
	if f == nil || s == "" {
		return
	}
	d := &font.Drawer{Dst: dst, Src: image.NewUniform(col), Face: f}
	for font.MeasureString(f, s).Ceil() > maxW && len(s) > 1 {
		s = s[:len(s)-1]
	}
	d.Dot = fixed.P(x, y)
	d.DrawString(s)
}

// splitPath separates the leaf a reader identifies the package by from the
// prefix that is the same on every row.
func splitPath(p string) (leaf, prefix string) {
	p = strings.TrimPrefix(p, "gno.land/")
	parts := strings.Split(p, "/")
	// A trailing version segment is not the name of anything.
	if n := len(parts); n > 1 && len(parts[n-1]) > 1 && parts[n-1][0] == 'v' && isDigits(parts[n-1][1:]) {
		parts = parts[:n-1]
	}
	if len(parts) == 0 {
		return p, ""
	}
	leaf = parts[len(parts)-1]
	prefix = strings.Join(parts[:len(parts)-1], "/")
	if prefix != "" {
		prefix += "/"
	}
	return leaf, prefix
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// wrapToWidth breaks a line by measuring it in the face it will be drawn in,
// rather than by counting characters: the tile uses a proportional face for the
// name and a monospaced one for the prose, and a character count is wrong for
// one of them whichever it is tuned for.
func wrapToWidth(f font.Face, s string, maxW int) []string {
	if f == nil || maxW <= 0 {
		return nil
	}
	var out []string
	line := ""
	for _, w := range strings.Fields(s) {
		try := w
		if line != "" {
			try = line + " " + w
		}
		if font.MeasureString(f, try).Ceil() <= maxW {
			line = try
			continue
		}
		if line != "" {
			out = append(out, line)
		}
		line = w
	}
	if line != "" {
		out = append(out, line)
	}
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
