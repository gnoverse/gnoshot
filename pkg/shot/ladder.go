package shot

import (
	"bytes"
	"fmt"
	"image"

	"github.com/gen2brain/webp"
	xdraw "golang.org/x/image/draw"
)

// Encoded is one file of the ladder.
type Encoded struct {
	Name  string
	Bytes []byte
}

// EncodeMaster stores the master losslessly.
//
// Measured over 7 real render crops: lossless WebP is 577,732 bytes total
// against 2,244,311 for Chrome's PNG (3.9x) and 1,075,240 for WebP q80 (1.9x).
// A realm render is text on flat colour, which is the content class where
// lossless entropy coding wins and a DCT codec spends its bits on ringing
// around glyph edges.
func EncodeMaster(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := webp.Encode(&buf, img, webp.Options{Lossless: true, Method: 4}); err != nil {
		return nil, fmt.Errorf("encode master: %w", err)
	}
	return buf.Bytes(), nil
}

// Ladder derives every rung from one master.
//
// The rule inverts below native resolution: lossless wins on the master and is
// the worst option on every derivative, because downscaling turns crisp glyph
// edges into anti-aliased gradients. On the og rung lossless measured 48,150
// bytes against 16,366 for q80, i.e. 2.9x worse.
func Ladder(img image.Image) ([]Encoded, error) {
	out := make([]Encoded, 0, len(Rungs))
	for _, r := range Rungs {
		dst := CoverTop(img, r.W, r.H)
		var buf bytes.Buffer
		if err := webp.Encode(&buf, dst, webp.Options{Quality: r.Quality, Method: 4}); err != nil {
			return nil, fmt.Errorf("encode %s: %w", r.Name, err)
		}
		out = append(out, Encoded{Name: r.Name + ".webp", Bytes: buf.Bytes()})
	}
	return out, nil
}

// CoverTop crops the source to the target aspect ratio anchored at the top
// left, then scales it to exactly w by h.
//
// Anchored at the top rather than centred because a realm render's first
// screenful is the part that identifies it: the heading and the opening lines.
// Centring a 3,000-pixel render lands the thumbnail somewhere in the middle of
// a table.
func CoverTop(src image.Image, w, h int) image.Image {
	sb := src.Bounds()
	sw, sh := sb.Dx(), sb.Dy()
	if sw <= 0 || sh <= 0 || w <= 0 || h <= 0 {
		return image.NewRGBA(image.Rect(0, 0, maxInt(w, 1), maxInt(h, 1)))
	}
	// Prefer keeping the full width: a render is a column of text and cropping
	// its sides loses more than cropping its foot.
	cw, ch := sw, sw*h/w
	if ch > sh {
		ch = sh
		cw = sh * w / h
		if cw > sw {
			cw = sw
		}
	}
	crop := image.Rect(sb.Min.X, sb.Min.Y, sb.Min.X+cw, sb.Min.Y+ch)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	// CatmullRom rather than ApproxBiLinear: the content is text, and the
	// cheaper kernel turns 16 px body type into mush at the thumb rungs.
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), src, crop, xdraw.Over, nil)
	return dst
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
