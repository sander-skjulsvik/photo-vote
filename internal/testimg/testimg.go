// Package testimg generates images for tests and the dev server, so the repo
// never needs real photos.
package testimg

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"
)

// Red, Green and Blue are the corner colours Corners uses.
var (
	Red   = color.RGBA{220, 20, 20, 255}
	Green = color.RGBA{20, 200, 20, 255}
	Blue  = color.RGBA{20, 20, 220, 255}
)

// Gradient returns a colourful picture based on seed.
func Gradient(w, h, seed int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	r0, g0, b0 := (seed*67)%200, (seed*131)%200, (seed*197)%200
	for y := range h {
		for x := range w {
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(r0 + 55*x/w),
				G: uint8(g0 + 55*y/h),
				B: uint8(b0 + 55*(x+y)/(w+h)),
				A: 255,
			})
		}
	}
	// A circle, so each picture has a subject.
	cx, cy, rad := w/2, h/2, min(w, h)/4
	for y := range h {
		for x := range w {
			if (x-cx)*(x-cx)+(y-cy)*(y-cy) < rad*rad {
				img.SetRGBA(x, y, color.RGBA{255 - uint8(r0), 255 - uint8(g0), 255 - uint8(b0), 255})
			}
		}
	}
	return img
}

// Corners returns a white image with a red block at the top left, green at the
// top right and blue at the bottom left, which makes rotation and mirroring
// easy to check.
func Corners(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(img, img.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	block := min(w, h) / 3
	fill := func(x, y int, c color.RGBA) {
		draw.Draw(img, image.Rect(x, y, x+block, y+block), image.NewUniform(c), image.Point{}, draw.Src)
	}
	fill(0, 0, Red)
	fill(w-block, 0, Green)
	fill(0, h-block, Blue)
	return img
}

// JPEG encodes an image.
func JPEG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// PNG encodes an image.
func PNG(img image.Image) []byte {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

// WithEXIF inserts an EXIF block into a JPEG, with the given orientation tag
// and a fake private marker in it, the way a camera would.
func WithEXIF(jpegData []byte, orientation int, marker string, order binary.ByteOrder) []byte {
	var tiff bytes.Buffer
	if order == binary.LittleEndian {
		tiff.WriteString("II")
	} else {
		tiff.WriteString("MM")
	}
	binary.Write(&tiff, order, uint16(42))
	binary.Write(&tiff, order, uint32(8)) // first directory right after this header
	binary.Write(&tiff, order, uint16(2)) // two entries

	// A tag before orientation, as cameras write several.
	binary.Write(&tiff, order, uint16(0x010F))
	binary.Write(&tiff, order, uint16(2))
	binary.Write(&tiff, order, uint32(1))
	binary.Write(&tiff, order, uint32(0))

	binary.Write(&tiff, order, uint16(0x0112))
	binary.Write(&tiff, order, uint16(3)) // SHORT
	binary.Write(&tiff, order, uint32(1))
	binary.Write(&tiff, order, uint16(orientation))
	binary.Write(&tiff, order, uint16(0))

	binary.Write(&tiff, order, uint32(0)) // no further directories
	tiff.WriteString(marker)

	segment := append([]byte("Exif\x00\x00"), tiff.Bytes()...)
	var out bytes.Buffer
	out.Write(jpegData[:2]) // start of image
	out.Write([]byte{0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(segment)+2))
	out.Write(segment)
	out.Write(jpegData[2:])
	return out.Bytes()
}
