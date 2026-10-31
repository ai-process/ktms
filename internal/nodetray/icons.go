package nodetray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Icons are drawn here rather than shipped: one shape per state, black on
// transparent for the macOS menu bar (a template image follows the theme)
// and colored for Linux panels.
const iconSize = 44 // shown at 22 pt on Retina

var iconColors = map[IconKind]color.NRGBA{
	IconFull:    {0x2e, 0xa0, 0x43, 0xff},
	IconPartial: {0x2e, 0xa0, 0x43, 0xff},
	IconPaused:  {0x8a, 0x8f, 0x98, 0xff},
	IconWaiting: {0xd9, 0x8e, 0x04, 0xff},
	IconWarn:    {0xd9, 0x8e, 0x04, 0xff},
}

// Icon returns the PNG for a state; template is the monochrome macOS variant.
func Icon(k IconKind, template bool) []byte {
	c := iconColors[k]
	if template {
		c = color.NRGBA{0, 0, 0, 0xff}
	}
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	ctr, r := float64(iconSize)/2, float64(iconSize)/2-5
	for y := range iconSize {
		for x := range iconSize {
			if a := coverage(k, float64(x)+0.5-ctr, float64(y)+0.5-ctr, r); a > 0 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(float64(c.A) * a)})
			}
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img) // an in-memory encode of a valid image does not fail
	return b.Bytes()
}

// coverage is how much of the pixel at (dx, dy) from the center a shape fills.
func coverage(k IconKind, dx, dy, r float64) float64 {
	d := math.Hypot(dx, dy)
	edge := func(dist float64) float64 { return math.Max(0, math.Min(1, 0.5-dist)) }
	ring := edge(math.Abs(d-r) - 2)
	switch k {
	case IconFull:
		return edge(d - r)
	case IconPartial: // a ring with its left half filled
		if dx < 0 {
			return edge(d - r)
		}
		return ring
	case IconPaused: // two bars
		bar := math.Max(edge(math.Abs(dx+6)-3), edge(math.Abs(dx-6)-3))
		return math.Min(bar, edge(math.Abs(dy)-r+2))
	case IconWaiting: // a ring with a dot
		return math.Max(ring, edge(d-5))
	default: // a triangle with a notch
		inside := dy < r*0.8 && math.Abs(dx) < (dy+r)*0.6
		notch := math.Abs(dx) < 2 && dy > -r*0.3 && dy < r*0.35
		if inside && !notch {
			return 1
		}
		return 0
	}
}
