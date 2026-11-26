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

// System One's image limits (docs.typesafe.ai/images). It rejects rather than
// resizes, and one bad image fails the whole request, so the proxy says why
// before the call instead of relaying a vendor 400.
const (
	typeSafeMinSide      = 280
	typeSafeMaxSide      = 1568
	typeSafeMinPixels    = 280 * 280
	typeSafeMaxPixels    = 1568 * 1568
	typeSafeMaxImageSize = 16 << 20
)

// typeSafeImageFormat checks one image against those limits from its header
// only, without decoding pixels, and returns System One's format name.
func typeSafeImageFormat(i int, data []byte) (string, error) {
	bad := func(format string, args ...any) error {
		return &llm.InvalidMediaError{Part: i, Msg: fmt.Sprintf(format, args...)}
	}
	if len(data) > typeSafeMaxImageSize {
		return "", bad("image is %d bytes, max %d for TypeSafe", len(data), typeSafeMaxImageSize)
	}
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return "", bad("not a PNG or JPEG image TypeSafe can read: %v", err)
	}
	if format != "png" && format != "jpeg" {
		return "", bad("image is %s; TypeSafe takes PNG or JPEG", format)
	}
	w, h := cfg.Width, cfg.Height
	if w < typeSafeMinSide || h < typeSafeMinSide || w > typeSafeMaxSide || h > typeSafeMaxSide {
		return "", bad("image is %dx%d px; TypeSafe needs each side within %d..%d px (scale it, keeping the aspect ratio)", w, h, typeSafeMinSide, typeSafeMaxSide)
	}
	if px := w * h; px < typeSafeMinPixels || px > typeSafeMaxPixels {
		return "", bad("image has %d pixels; TypeSafe needs %d..%d", px, typeSafeMinPixels, typeSafeMaxPixels)
	}
	if format == "png" {
		if pngAnimated(data) {
			return "", bad("animated PNG; TypeSafe takes still images only")
		}
		return "png", nil
	}
	if o := jpegOrientation(data); o > 1 {
		return "", bad("JPEG has EXIF orientation %d, which TypeSafe ignores: rotate the pixels upright and drop the tag", o)
	}
	return "jpg", nil
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
