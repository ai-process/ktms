package adapters

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/jpeg" // registers JPEG for image.DecodeConfig
	_ "image/png"  // registers PNG for image.DecodeConfig

	"github.com/ai-process/ktms/internal/llm"
)

// SystemOneImageLimits are what a vendor's System One takes as an image. Vendors
// reject rather than resize, and one bad image fails the whole request, so the
// proxy says why before the call instead of relaying a vendor 400.
type SystemOneImageLimits struct {
	Name                 string // the vendor as people know it, for messages
	MinSide, MaxSide     int
	MinPixels, MaxPixels int
	MaxBytes             int
	MaxImages            int // per call, 0 for no limit of its own
	MaxImagesWithBox     int // per call with a box question, 0 for MaxImages
}

// typeSafeImageLimits are TypeSafe's (docs.typesafe.ai/images).
var typeSafeImageLimits = SystemOneImageLimits{
	Name:    "TypeSafe",
	MinSide: 280, MaxSide: 1568,
	MinPixels: 280 * 280, MaxPixels: 1568 * 1568,
	MaxBytes: 16 << 20,
}

// format checks one image from its header only, without decoding pixels, and
// returns System One's format name. PNG and JPEG are the protocol's formats.
func (l *SystemOneImageLimits) format(i int, data []byte) (string, error) {
	bad := func(format string, args ...any) error {
		return &llm.InvalidMediaError{Part: i, Msg: fmt.Sprintf(format, args...)}
	}
	if len(data) > l.MaxBytes {
		return "", bad("image is %d bytes, max %d for %s", len(data), l.MaxBytes, l.Name)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", bad("not a PNG or JPEG image %s can read: %v", l.Name, err)
	}
	if format != "png" && format != "jpeg" {
		return "", bad("image is %s; %s takes PNG or JPEG", format, l.Name)
	}
	w, h := cfg.Width, cfg.Height
	if w < l.MinSide || h < l.MinSide || (l.MaxSide > 0 && (w > l.MaxSide || h > l.MaxSide)) {
		return "", bad("image is %dx%d px; %s needs each side %s px (scale it, keeping the aspect ratio)", w, h, l.Name, bounds(l.MinSide, l.MaxSide))
	}
	if px := w * h; px < l.MinPixels || (l.MaxPixels > 0 && px > l.MaxPixels) {
		return "", bad("image has %d pixels; %s needs %s", px, l.Name, bounds(l.MinPixels, l.MaxPixels))
	}
	if format == "png" {
		if pngAnimated(data) {
			return "", bad("animated PNG; %s takes still images only", l.Name)
		}
		return "png", nil
	}
	if o := jpegOrientation(data); o > 1 {
		return "", bad("JPEG has EXIF orientation %d, which %s ignores: rotate the pixels upright and drop the tag", o, l.Name)
	}
	return "jpg", nil
}

// bounds says a limit as "within 280..1568", or "at least 1" when there is no top.
func bounds(lo, hi int) string {
	if hi <= 0 {
		return fmt.Sprintf("at least %d", lo)
	}
	return fmt.Sprintf("within %d..%d", lo, hi)
}

// pngAnimated reports an APNG: an acTL chunk before the first IDAT.
func pngAnimated(data []byte) bool {
	const sigLen = 8
	for p := sigLen; p+8 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[p:]))
		kind := string(data[p+4 : p+8])
		switch kind {
		case "acTL":
			return true
		case "IDAT":
			return false
		}
		next := p + 12 + n
		if n < 0 || next <= p {
			return false
		}
		p = next
	}
	return false
}

// jpegOrientation reads the EXIF orientation tag of a JPEG, 0 when absent.
func jpegOrientation(data []byte) int {
	for p := 2; p+4 <= len(data) && data[p] == 0xFF; {
		marker := data[p+1]
		if marker == 0xDA || marker == 0xD9 { // start of scan, end of image
			return 0
		}
		n := int(binary.BigEndian.Uint16(data[p+2:]))
		end := p + 2 + n
		if n < 2 || end > len(data) {
			return 0
		}
		if marker == 0xE1 && n >= 8 && string(data[p+4:p+10]) == "Exif\x00\x00" {
			return exifOrientation(data[p+10 : end])
		}
		p = end
	}
	return 0
}

// exifOrientation finds tag 0x0112 in IFD0 of a TIFF block.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 0
	}
	var bo binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		bo = binary.LittleEndian
	case "MM":
		bo = binary.BigEndian
	default:
		return 0
	}
	ifd := int(bo.Uint32(tiff[4:]))
	if ifd < 8 || ifd+2 > len(tiff) {
		return 0
	}
	count := int(bo.Uint16(tiff[ifd:]))
	for k := 0; k < count; k++ {
		e := ifd + 2 + 12*k
		if e+12 > len(tiff) {
			return 0
		}
		if bo.Uint16(tiff[e:]) == 0x0112 {
			return int(bo.Uint16(tiff[e+8:]))
		}
	}
	return 0
}
