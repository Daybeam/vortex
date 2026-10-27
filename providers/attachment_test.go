package providers

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestReadAttachmentData_NoResizeForSmallFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.png")

	// Create a tiny 10×10 PNG (< 1MB).
	small := image.NewRGBA(image.Rect(0, 0, 10, 10))
	for y := 0; y < 10; y++ {
		for x := 0; x < 10; x++ {
			small.Set(x, y, color.RGBA{R: 255, G: 0, B: 0, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, small); err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	data, err := readAttachmentData(path, "image/png")
	if err != nil {
		t.Fatalf("readAttachmentData failed: %v", err)
	}
	if len(data) != len(buf.Bytes()) {
		t.Errorf("small file should not be resized: got %d bytes, original %d", len(data), len(buf.Bytes()))
	}
}

func TestReadAttachmentData_NonImageNotResized(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.txt")

	// Create a file > 1MB but not an image.
	largeData := bytes.Repeat([]byte("x"), resizeThreshold+100)
	if err := os.WriteFile(path, largeData, 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	data, err := readAttachmentData(path, "text/plain")
	if err != nil {
		t.Fatalf("readAttachmentData failed: %v", err)
	}
	if len(data) != len(largeData) {
		t.Errorf("non-image file should not be resized: got %d bytes, original %d", len(data), len(largeData))
	}
}

func TestReadAttachmentData_ResizesLargePNG(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.png")

	// Create a 3000×3000 PNG with noise-like data (won't compress well → > 1MB).
	large := image.NewRGBA(image.Rect(0, 0, 3000, 3000))
	seed := uint32(12345)
	for y := 0; y < 3000; y++ {
		for x := 0; x < 3000; x++ {
			seed = seed*1103515245 + 12345 // LCG pseudo-random
			large.Set(x, y, color.RGBA{
				R: uint8(seed >> 16),
				G: uint8(seed >> 8),
				B: uint8(seed),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, large); err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write failed: %v", err)
	}
	if len(buf.Bytes()) <= resizeThreshold {
		t.Skipf("test image too small (%d bytes), skipping resize test", len(buf.Bytes()))
	}

	data, err := readAttachmentData(path, "image/png")
	if err != nil {
		t.Fatalf("readAttachmentData failed: %v", err)
	}

	// Verify the resized image decodes and fits within 2048×2048.
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode resized image failed: %v", err)
	}
	bounds := img.Bounds()
	if bounds.Dx() > maxImageDimension || bounds.Dy() > maxImageDimension {
		t.Errorf("resized image should fit within %d×%d, got %d×%d", maxImageDimension, maxImageDimension, bounds.Dx(), bounds.Dy())
	}
	if len(data) >= len(buf.Bytes()) {
		t.Errorf("resized image should be smaller: got %d bytes, original %d", len(data), len(buf.Bytes()))
	}
}

func TestReadAttachmentData_MissingFile(t *testing.T) {
	_, err := readAttachmentData("/nonexistent/file.png", "image/png")
	if err == nil {
		t.Error("expected error for missing file")
	}
}
