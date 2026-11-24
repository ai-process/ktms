package nodetray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"time"
)

// Icons are drawn here rather than shipped: one shape per state, black on
// transparent for the macOS menu bar (a template image follows the theme)
// and colored for Linux panels. Running requests are squares beside it.
const iconSize = 44 // the menu bar shows it at 16 pt, 32 px on Retina

// Layout in pixels. With requests the shape moves left and shrinks; two rows
// of squares fill column by column, and a third column shows only a sliver.
const (
	shapeR        = 17.0 // the shape's own radius, alone in the icon
	dockX, dockR  = 11.0, 10.5
	sqSize, sqGap = 7.0, 2.0
	sqLeft, sqTop = 24.0, 14.0
	maxSquares    = 6 // the rest are past the edge
	AnimSteps     = 6
	AnimEvery     = 35 * time.Millisecond
	squarePitch   = sqSize + sqGap
	iconCenter    = iconSize / 2.0
)

var iconColors = map[IconKind]color.NRGBA{
	IconLevel:   {0x2e, 0xa0, 0x43, 0xff},
	IconPaused:  {0x8a, 0x8f, 0x98, 0xff},
	IconWaiting: {0xd9, 0x8e, 0x04, 0xff},
	IconWarn:    {0xd9, 0x8e, 0x04, 0xff},
}

// Frame is one picture of the icon: a state, the level its circle fills to
// and the squares, caught T of the way through a move from From to To.
type Frame struct {
	Kind     IconKind
	Level    uint32 // 0-100; the circle of IconLevel fills this much
	From, To int
	T        float64 // 0 at the start of a move, 1 at rest
}

// Still is a frame at rest.
func Still(k IconKind, level uint32, requests int) Frame {
	return Frame{Kind: k, Level: level, From: requests, To: requests, T: 1}
}

// Icon returns the PNG of a frame; template is the monochrome macOS variant.
func Icon(f Frame, template bool) []byte {
	c := iconColors[f.Kind]
	if template {
		c = color.NRGBA{0, 0, 0, 0xff}
	}
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	t := math.Max(0, math.Min(1, f.T))
	ease := 1 - (1-t)*(1-t)*(1-t)
	dock := lerp(docked(f.From), docked(f.To), ease)
	cx, r := lerp(iconCenter, dockX, dock), lerp(shapeR, dockR, dock)
	scale := shapeR / r // shape units per pixel
	for y := range iconSize {
		for x := range iconSize {
			px, py := float64(x)+0.5, float64(y)+0.5
			a := shapeCoverage(f.Kind, f.Level, (px-cx)*scale, (py-iconCenter)*scale, scale)
			a = math.Max(a, squaresCoverage(f.From, f.To, ease, px, py))
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(float64(c.A) * a)})
			}
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img) // an in-memory encode of a valid image does not fail
	return b.Bytes()
}

func docked(n int) float64 {
	if n > 0 {
		return 1
	}
	return 0
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

// edge turns a signed distance in pixels from a shape's border into coverage.
func edge(dist float64) float64 { return math.Max(0, math.Min(1, 0.5-dist)) }

// squaresCoverage draws the squares that stay and those flying in from or
// out to the right edge; ease is how far the flight has gone.
func squaresCoverage(from, to int, ease, px, py float64) float64 {
	lo, hi := min(from, to), min(max(from, to), maxSquares)
	out := 0.0
	for i := range hi {
		x := sqLeft + float64(i/2)*squarePitch
		y := sqTop + float64(i%2)*squarePitch
		if i >= lo {
			away := float64(iconSize) - x // to just past the edge
			if to > from {
				x += away * (1 - ease)
			} else {
				x += away * ease
			}
		}
		half := sqSize / 2
		a := math.Min(edge(math.Abs(px-x-half)-half), edge(math.Abs(py-y-half)-half))
		out = math.Max(out, a)
	}
	return out
}

// shapeCoverage is how much of the pixel at (dx, dy) from the center, in
// shape units, a shape fills; scale is shape units per pixel.
func shapeCoverage(k IconKind, level uint32, dx, dy, scale float64) float64 {
	r := shapeR
	d := math.Hypot(dx, dy)
	px := func(dist float64) float64 { return edge(dist / scale) }
	ring := px(math.Abs(d-r) - 2)
	switch k {
	case IconLevel: // a ring with a sector filled clockwise from 12 o'clock
		return math.Max(ring, px(d-r)*sector(dx, dy, scale, float64(min(level, 100))/100))
	case IconPaused: // two bars
		bar := math.Max(px(math.Abs(dx+6)-3), px(math.Abs(dx-6)-3))
		return math.Min(bar, px(math.Abs(dy)-r+2))
	case IconWaiting: // a ring with a dot
		return math.Max(ring, px(d-5))
	default: // a triangle with a notch
		inside := dy < r*0.8 && math.Abs(dx) < (dy+r)*0.6
		notch := math.Abs(dx) < 2 && dy > -r*0.3 && dy < r*0.35
		if inside && !notch {
			return 1
		}
		return 0
	}
}

// sector is the share of a pixel inside the first frac of a turn, sampled 4x4.
func sector(dx, dy, scale, frac float64) float64 {
	switch {
	case frac <= 0:
		return 0
	case frac >= 1:
		return 1
	}
	in := 0
	for i := range 4 {
		for j := range 4 {
			sx, sy := dx+(float64(i)-1.5)/4*scale, dy+(float64(j)-1.5)/4*scale
			a := math.Atan2(sx, -sy) // 0 at 12 o'clock, growing clockwise
			if a < 0 {
				a += 2 * math.Pi
			}
			if a < frac*2*math.Pi {
				in++
			}
		}
	}
	return float64(in) / 16
}
