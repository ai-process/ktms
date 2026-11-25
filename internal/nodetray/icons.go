package nodetray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"slices"
	"time"
)

// Icons are drawn here rather than shipped: one shape per state, black on
// transparent for the macOS menu bar (a template image follows the theme)
// and colored for Linux panels. Running requests are squares beside it.
const iconSize = 32 // the menu bar shows it at 16 pt: one pixel per Retina pixel keeps edges sharp

// Layout in pixels, squares on whole ones. With requests the shape moves left
// and shrinks; two rows of squares fill column by column, and a third column
// shows only a sliver.
const (
	shapeR        = 17.0 // the radius shapes are drawn at, in their own units
	aloneR        = 12.5 // the shape's radius alone in the icon
	dockX, dockR  = 8.0, 7.5
	sqSize, sqGap = 5.0, 2.0
	sqLeft, sqTop = 17.0, 10.0
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

// Off is a square's place outside the icon: arriving from the right edge,
// or leaving over the top.
const Off = -1

// Square is one request's square, moving between two slots.
type Square struct{ From, To int }

// Frame is one picture of the icon: a state, the level its circle fills to
// and the squares, caught T of the way through their moves.
type Frame struct {
	Kind    IconKind
	Level   uint32 // 0-100; the circle of IconLevel fills this much
	Squares []Square
	Aside   [2]bool // the shape sits small at the left: before and after the move
	T       float64 // 0 at the start of a move, 1 at rest
}

// Still is a frame at rest with n squares.
func Still(k IconKind, level uint32, n int) Frame {
	f := Frame{Kind: k, Level: level, Aside: [2]bool{n > 0, n > 0}, T: 1}
	for i := range n {
		f.Squares = append(f.Squares, Square{i, i})
	}
	return f
}

// Moving is a frame t of the way from one list of running requests to the next.
func Moving(k IconKind, level uint32, prev, next []uint64, t float64) Frame {
	return Frame{Kind: k, Level: level, Squares: Move(prev, next), Aside: [2]bool{len(prev) > 0, len(next) > 0}, T: t}
}

// Move plans the squares from one list of running requests to the next: a
// finished one leaves from its own slot, the rest close up, new ones arrive.
func Move(prev, next []uint64) []Square {
	at := make(map[uint64]int, len(next))
	for j, id := range next {
		at[id] = j
	}
	var out []Square
	for i, id := range prev {
		j, ok := at[id]
		if !ok {
			j = Off
		}
		out = append(out, Square{i, j})
		delete(at, id)
	}
	for j, id := range next {
		if _, ok := at[id]; ok {
			out = append(out, Square{Off, j})
		}
	}
	return out
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
	dock := lerp(b2f(f.Aside[0]), b2f(f.Aside[1]), ease)
	cx, r := lerp(iconCenter, dockX, dock), lerp(aloneR, dockR, dock)
	scale := shapeR / r // shape units per pixel
	boxes := squareBoxes(f.Squares, ease)
	for y := range iconSize {
		for x := range iconSize {
			px, py := float64(x)+0.5, float64(y)+0.5
			a := shapeCoverage(f.Kind, f.Level, (px-cx)*scale, (py-iconCenter)*scale, scale)
			for _, b := range boxes {
				half := sqSize / 2
				a = math.Max(a, math.Min(edge(math.Abs(px-b[0]-half)-half), edge(math.Abs(py-b[1]-half)-half)))
			}
			if a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(float64(c.A) * a)})
			}
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img) // an in-memory encode of a valid image does not fail
	return b.Bytes()
}

func docked(slot int) float64 {
	if slot >= 0 && slot < maxSquares {
		return 1
	}
	return 0
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

func lerp(a, b, t float64) float64 { return a + (b-a)*t }

// edge turns a signed distance in pixels from a shape's border into coverage.
func edge(dist float64) float64 { return math.Max(0, math.Min(1, 0.5-dist)) }

func slotXY(i int) (x, y float64) {
	return sqLeft + float64(i/2)*squarePitch, sqTop + float64(i%2)*squarePitch
}

// squareBoxes places each square ease of the way through its move: arrivals
// come in from the right edge, departures leave over the top.
func squareBoxes(sq []Square, ease float64) [][2]float64 {
	var out [][2]float64
	for _, q := range sq {
		if docked(q.From) == 0 && docked(q.To) == 0 {
			continue // never inside the icon
		}
		var x0, y0, x1, y1 float64
		switch {
		case q.From == Off:
			x1, y1 = slotXY(q.To)
			x0, y0 = iconSize, y1
		case q.To == Off:
			x0, y0 = slotXY(q.From)
			x1, y1 = x0, -sqSize
		default:
			x0, y0 = slotXY(q.From)
			x1, y1 = slotXY(q.To)
		}
		out = append(out, [2]float64{lerp(x0, x1, ease), lerp(y0, y1, ease)})
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

// HoldAside is how long the shape stays small after the last request ends,
// so a gap of a moment between requests does not make it jump.
const HoldAside = 3 * time.Second

// IconState is what the icon shows at rest.
type IconState struct {
	Kind  IconKind
	Fill  uint32
	IDs   []uint64 // the requests drawn as squares, oldest first
	Aside bool     // the shape sits small at the left
}

func (s IconState) Same(o IconState) bool {
	return s.Kind == o.Kind && s.Fill == o.Fill && s.Aside == o.Aside && slices.Equal(s.IDs, o.IDs)
}

// Still is the frame of the state at rest.
func (s IconState) Still() Frame {
	f := Still(s.Kind, s.Fill, len(s.IDs))
	f.Aside = [2]bool{s.Aside, s.Aside}
	return f
}

// MoveTo is the frame t of the way from s to next.
func (s IconState) MoveTo(next IconState, t float64) Frame {
	return Frame{Kind: next.Kind, Level: next.Fill, Squares: Move(s.IDs, next.IDs), Aside: [2]bool{s.Aside, next.Aside}, T: t}
}

// NextIcon is the icon for v after prev; lastBusy is when requests last ran.
// A positive recheck means the shape is held aside: look again after it.
func NextIcon(v View, prev IconState, lastBusy, now time.Time) (s IconState, recheck time.Duration) {
	s = IconState{Kind: v.Icon, Fill: v.Fill, IDs: v.Squares, Aside: len(v.Squares) > 0}
	if !s.Aside && prev.Aside {
		if left := HoldAside - now.Sub(lastBusy); left > 0 {
			s.Aside, recheck = true, left
		}
	}
	return s, recheck
}
