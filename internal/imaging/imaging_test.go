package imaging

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"testing"

	"github.com/sander-skjulsvik/photo-vote/internal/testimg"
)

func decode(t *testing.T, data []byte) image.Image {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("output isn't a valid JPEG: %v", err)
	}
	return img
}

func near(a color.Color, b color.RGBA) bool {
	r, g, bl, _ := a.RGBA()
	diff := func(x uint32, y uint8) int {
		d := int(x>>8) - int(y)
		if d < 0 {
			d = -d
		}
		return d
	}
	return diff(r, b.R) < 40 && diff(g, b.G) < 40 && diff(bl, b.B) < 40
}

func TestOrientationEndsUpRightWayUp(t *testing.T) {
	// The source has red top left, green top right and blue bottom left. Each
	// case gives where those corners must be once the tag has been applied.
	type corners struct{ topLeft, topRight, bottomLeft, bottomRight color.RGBA }
	white := color.RGBA{255, 255, 255, 255}
	red, green, blue := testimg.Red, testimg.Green, testimg.Blue
	tests := []struct {
		orientation int
		want        corners
	}{
		{1, corners{red, green, blue, white}},
		{2, corners{green, red, white, blue}},
		{3, corners{white, blue, green, red}},
		{4, corners{blue, white, red, green}},
		{5, corners{red, blue, green, white}},
		{6, corners{blue, red, white, green}},
		{7, corners{white, green, blue, red}},
		{8, corners{green, white, red, blue}},
	}
	const w, h = 300, 150
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, tt := range tests {
			src := testimg.WithEXIF(testimg.JPEG(testimg.Corners(w, h)), tt.orientation, "", order)
			res, err := Process(src)
			if err != nil {
				t.Fatalf("orientation %d: %v", tt.orientation, err)
			}
			wantW, wantH := w, h
			if tt.orientation >= 5 {
				wantW, wantH = h, w
			}
			if res.Width != wantW || res.Height != wantH {
				t.Errorf("orientation %d: size %dx%d, want %dx%d", tt.orientation, res.Width, res.Height, wantW, wantH)
			}
			img := decode(t, res.JPEG)
			const in = 10 // sample inside each corner block, away from the edge
			got := []struct {
				name string
				c    color.Color
				want color.RGBA
			}{
				{"top left", img.At(in, in), tt.want.topLeft},
				{"top right", img.At(wantW-in, in), tt.want.topRight},
				{"bottom left", img.At(in, wantH-in), tt.want.bottomLeft},
				{"bottom right", img.At(wantW-in, wantH-in), tt.want.bottomRight},
			}
			for _, g := range got {
				if !near(g.c, g.want) {
					t.Errorf("orientation %d (%v): %s is %v, want %v", tt.orientation, order, g.name, g.c, g.want)
				}
			}
		}
	}
}

func TestMetadataIsRemoved(t *testing.T) {
	const marker = "GPS-00.0000N-00.0000E-PRIVATE"
	src := testimg.WithEXIF(testimg.JPEG(testimg.Gradient(200, 100, 1)), 1, marker, binary.LittleEndian)
	if !bytes.Contains(src, []byte(marker)) {
		t.Fatal("test is broken: the marker isn't in the input")
	}
	res, err := Process(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, leftover := range []string{marker, "Exif"} {
		if bytes.Contains(res.JPEG, []byte(leftover)) {
			t.Errorf("%q survived processing", leftover)
		}
	}
}

func TestResizesToLongEdge(t *testing.T) {
	tests := []struct{ w, h, wantW, wantH int }{
		{4000, 2000, 1600, 800},
		{2000, 4000, 800, 1600},
		{800, 600, 800, 600}, // never enlarged
	}
	for _, tt := range tests {
		res, err := Process(testimg.JPEG(testimg.Gradient(tt.w, tt.h, 1)))
		if err != nil {
			t.Fatal(err)
		}
		if res.Width != tt.wantW || res.Height != tt.wantH {
			t.Errorf("%dx%d became %dx%d, want %dx%d", tt.w, tt.h, res.Width, res.Height, tt.wantW, tt.wantH)
		}
	}
}

func TestPNGBecomesJPEGOnWhite(t *testing.T) {
	src := image.NewRGBA(image.Rect(0, 0, 50, 50)) // fully transparent
	res, err := Process(testimg.PNG(src))
	if err != nil {
		t.Fatal(err)
	}
	if !near(decode(t, res.JPEG).At(25, 25), color.RGBA{255, 255, 255, 255}) {
		t.Error("transparent PNG wasn't flattened onto white")
	}
}

func TestRejectsOtherFiles(t *testing.T) {
	tests := map[string][]byte{
		"empty":     {},
		"text":      []byte("hello"),
		"svg":       []byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`),
		"html":      []byte("<!doctype html><script>alert(1)</script>"),
		"gif":       []byte("GIF89a\x01\x00\x01\x00\x00\x00\x00;"),
		"truncated": testimg.JPEG(testimg.Gradient(100, 100, 1))[:40],
	}
	for name, data := range tests {
		if _, err := Process(data); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

func TestRejectsTooManyPixelsWithoutDecoding(t *testing.T) {
	// A valid PNG header claiming 100,000 x 100,000 pixels, with no image data.
	// This has to be refused from the header alone.
	header := testimg.PNG(image.NewRGBA(image.Rect(0, 0, 1, 1)))[:33]
	binary.BigEndian.PutUint32(header[16:], 100_000)
	binary.BigEndian.PutUint32(header[20:], 100_000)
	binary.BigEndian.PutUint32(header[29:], crc32.ChecksumIEEE(header[12:29]))
	if _, err := Process(header); !errors.Is(err, ErrTooLarge) {
		t.Errorf("err = %v, want ErrTooLarge", err)
	}
}

func TestMalformedEXIFIsIgnored(t *testing.T) {
	good := testimg.WithEXIF(testimg.JPEG(testimg.Gradient(60, 60, 1)), 6, "", binary.LittleEndian)
	// Corrupt every byte of the EXIF block in turn. Any result is fine as long as
	// it doesn't panic and the orientation stays valid.
	for i := 4; i < 60 && i < len(good); i++ {
		bad := bytes.Clone(good)
		bad[i] ^= 0xFF
		if o := jpegOrientation(bad); o < 1 || o > 8 {
			t.Errorf("byte %d: orientation %d", i, o)
		}
	}
}

func FuzzJPEGOrientation(f *testing.F) {
	f.Add(testimg.WithEXIF(testimg.JPEG(testimg.Gradient(8, 8, 1)), 6, "", binary.BigEndian))
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xE1, 0x00, 0x02})
	f.Fuzz(func(t *testing.T, data []byte) {
		if o := jpegOrientation(data); o < 1 || o > 8 {
			t.Errorf("orientation %d", o)
		}
	})
}
