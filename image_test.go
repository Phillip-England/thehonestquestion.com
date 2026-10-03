package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestImageUploadPreservesEdges(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is required for image conversion")
	}
	for _, size := range []image.Point{{X: 100, Y: 200}, {X: 300, Y: 100}} {
		t.Run(size.String(), func(t *testing.T) {
			s := &server{dataDir: t.TempDir()}
			source := image.NewRGBA(image.Rect(0, 0, size.X, size.Y))
			for y := 0; y < size.Y; y++ {
				for x := 0; x < size.X; x++ {
					c := color.RGBA{G: 180, A: 255}
					if x < 10 || y < 10 || x >= size.X-10 || y >= size.Y-10 {
						c = color.RGBA{R: 240, A: 255}
					}
					source.SetRGBA(x, y, c)
				}
			}
			var input bytes.Buffer
			if err := png.Encode(&input, source); err != nil {
				t.Fatal(err)
			}
			name, err := s.saveImage(&input, "edges.png")
			if err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(filepath.Join(s.uploadsDir(), name))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			output, _, err := image.Decode(file)
			if err != nil {
				t.Fatal(err)
			}
			bounds := output.Bounds()
			if bounds.Dx() != thumbnailWidth || bounds.Dy() != thumbnailHeight {
				t.Fatalf("unexpected output dimensions: %v", bounds)
			}
			// Every edge must survive inside the fitted image, including the
			// top/bottom of portraits and the left/right of wide images.
			scale := min(float64(thumbnailWidth)/float64(size.X), float64(thumbnailHeight)/float64(size.Y))
			w, h := int(float64(size.X)*scale), int(float64(size.Y)*scale)
			left, top := (thumbnailWidth-w)/2, (thumbnailHeight-h)/2
			inset := int(3 * scale)
			for _, point := range []image.Point{
				{X: left + inset, Y: thumbnailHeight / 2},
				{X: left + w - inset, Y: thumbnailHeight / 2},
				{X: thumbnailWidth / 2, Y: top + inset},
				{X: thumbnailWidth / 2, Y: top + h - inset},
			} {
				r, g, b, _ := output.At(point.X, point.Y).RGBA()
				if r < 50000 || g > 15000 || b > 15000 {
					t.Errorf("image edge missing at %v: RGB %d, %d, %d", point, r, g, b)
				}
			}
		})
	}
}
