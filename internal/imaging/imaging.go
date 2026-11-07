// Package imaging turns an uploaded file into the only version the app keeps: a
// freshly encoded JPEG, resized and turned the right way up. Nothing from the
// original file survives except its pixels, which removes metadata such as GPS
// location, camera details and embedded thumbnails.
package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	stddraw "image/draw"
	"image/jpeg"
	_ "image/png" // registers PNG with image.Decode

	"golang.org/x/image/draw"
)

const (
	// MaxLongEdge is the longest side of a stored image, in pixels.
	MaxLongEdge = 1600
	// MaxPixels stops very large images from being decoded into memory. It's
	// checked from the header, before decoding, so a small file claiming huge
	// dimensions costs nothing. It allows full-size images from high-resolution
	// cameras.
	MaxPixels = 64_000_000

	jpegQuality = 85
)

var (
	// ErrUnsupported means the file isn't a JPEG or PNG.
	ErrUnsupported = errors.New("only JPEG and PNG files are supported")
	// ErrTooLarge means the image has too many pixels.
	ErrTooLarge = errors.New("the image has too many pixels")
)

// Result is a processed image.
type Result struct {
	JPEG   []byte
	Width  int
	Height int
}

// Process validates, decodes, resizes, orients and re-encodes an image.
func Process(data []byte) (Result, error) {
	cfg, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Result{}, ErrUnsupported
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return Result{}, ErrUnsupported
	}
	if int64(cfg.Width)*int64(cfg.Height) > MaxPixels {
		return Result{}, ErrTooLarge
	}

	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Result{}, fmt.Errorf("decode image: %w", err)
	}

	orientation := 1
	if format == "jpeg" {
		orientation = jpegOrientation(data)
	}
	out := orient(resize(src), orientation)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return Result{}, fmt.Errorf("encode image: %w", err)
	}
	b := out.Bounds()
	return Result{JPEG: buf.Bytes(), Width: b.Dx(), Height: b.Dy()}, nil
}

// resize copies src to a new image no longer than MaxLongEdge on its long side.
// Transparency is flattened onto white, since JPEG has no alpha.
func resize(src image.Image) *image.RGBA {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if long := max(w, h); long > MaxLongEdge {
		w = max(1, w*MaxLongEdge/long)
		h = max(1, h*MaxLongEdge/long)
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	stddraw.Draw(dst, dst.Bounds(), image.NewUniform(color.White), image.Point{}, stddraw.Src)
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// orient applies an EXIF orientation (1 to 8). The tag has to be applied here
// because re-encoding removes it, which would otherwise leave photos sideways.
func orient(src *image.RGBA, orientation int) *image.RGBA {
	if orientation < 2 || orientation > 8 {
		return src
	}
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := w, h
	if orientation >= 5 {
		dw, dh = h, w
	}
	dst := image.NewRGBA(image.Rect(0, 0, dw, dh))
	for y := range h {
		for x := range w {
			var dx, dy int
			switch orientation {
			case 2: // mirrored
				dx, dy = w-1-x, y
			case 3: // upside down
				dx, dy = w-1-x, h-1-y
			case 4: // upside down and mirrored
				dx, dy = x, h-1-y
			case 5: // mirrored and turned
				dx, dy = y, x
			case 6: // needs turning 90° clockwise
				dx, dy = h-1-y, x
			case 7: // mirrored and turned the other way
				dx, dy = h-1-y, w-1-x
			case 8: // needs turning 90° anticlockwise
				dx, dy = y, w-1-x
			}
			dst.SetRGBA(dx, dy, src.RGBAAt(x, y))
		}
	}
	return dst
}

// jpegOrientation reads the EXIF orientation tag from a JPEG. It returns 1, the
// normal orientation, if there is no tag or the data is malformed.
func jpegOrientation(data []byte) int {
	if len(data) < 2 || data[0] != 0xFF || data[1] != 0xD8 {
		return 1
	}
	for pos := 2; pos+4 <= len(data); {
		if data[pos] != 0xFF {
			return 1
		}
		marker := data[pos+1]
		if marker == 0xFF { // padding before a marker
			pos++
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // image data or end of image
			return 1
		}
		size := int(binary.BigEndian.Uint16(data[pos+2:]))
		if size < 2 || pos+2+size > len(data) {
			return 1
		}
		segment := data[pos+4 : pos+2+size]
		if marker == 0xE1 && bytes.HasPrefix(segment, []byte("Exif\x00\x00")) {
			return exifOrientation(segment[6:])
		}
		pos += 2 + size
	}
	return 1
}

// exifOrientation finds tag 0x0112 in the first image directory of a TIFF block.
func exifOrientation(tiff []byte) int {
	if len(tiff) < 8 {
		return 1
	}
	var order binary.ByteOrder
	switch string(tiff[:2]) {
	case "II":
		order = binary.LittleEndian
	case "MM":
		order = binary.BigEndian
	default:
		return 1
	}
	if order.Uint16(tiff[2:]) != 42 {
		return 1
	}
	offset := uint64(order.Uint32(tiff[4:]))
	if offset+2 > uint64(len(tiff)) {
		return 1
	}
	count := uint64(order.Uint16(tiff[offset:]))
	entries := offset + 2
	for i := range count {
		entry := entries + i*12
		if entry+12 > uint64(len(tiff)) {
			return 1
		}
		if order.Uint16(tiff[entry:]) != 0x0112 {
			continue
		}
		const typeShort = 3
		if order.Uint16(tiff[entry+2:]) != typeShort {
			return 1
		}
		if v := int(order.Uint16(tiff[entry+8:])); v >= 1 && v <= 8 {
			return v
		}
		return 1
	}
	return 1
}
