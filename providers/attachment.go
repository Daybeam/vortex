package providers

import (
	"bytes"
	"image"
	"image/jpeg"
	"image/png"
	"os"
)

const (
	maxImageDimension = 2048
	resizeThreshold   = 1 << 20 // 1MB — images larger than this are resized
)

// readAttachmentData reads an attachment file and, if it's a large image
// (png/jpeg > 1MB), resizes it to fit within 2048×2048 before returning.
// This caps the token cost per image at ~5500 tokens regardless of original
// resolution. Non-image files and small images are returned as-is.
//
// If decoding or resizing fails, the original data is returned unchanged
// (graceful degradation — never blocks the provider call).
//
// See docs/plan/VLM_IMAGE_CONTEXT_OPTIMIZATION_DESIGN.md §Phase 2.
func readAttachmentData(path, mimeType string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) <= resizeThreshold {
		return data, nil // small enough — no resize needed
	}
	if mimeType != "image/png" && mimeType != "image/jpeg" && mimeType != "image/jpg" {
		return data, nil // not a resizable image format
	}
	return resizeImage(data, mimeType)
}

// resizeImage decodes, resizes to fit maxImageDimension×maxImageDimension
// preserving aspect ratio, and re-encodes. Returns original data on any
// decode/encode failure (graceful degradation).
func resizeImage(data []byte, mimeType string) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, nil // decode failed — return original
	}

	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= maxImageDimension && h <= maxImageDimension {
		return data, nil // already within limits
	}

	// Scale preserving aspect ratio.
	if w > h {
		h = h * maxImageDimension / w
		w = maxImageDimension
	} else {
		w = w * maxImageDimension / h
		h = maxImageDimension
	}
	if w < 1 {
		w = 1
	}
	if h < 1 {
		h = 1
	}

	// Nearest-neighbor resize (stdlib-only, no x/image dependency).
	// Quality is lower than bilinear but sufficient for token-cost capping.
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	srcW, srcH := bounds.Dx(), bounds.Dy()
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			sx := bounds.Min.X + x*srcW/w
			sy := bounds.Min.Y + y*srcH/h
			dst.Set(x, y, img.At(sx, sy))
		}
	}

	var buf bytes.Buffer
	if mimeType == "image/png" {
		if err := png.Encode(&buf, dst); err != nil {
			return data, nil
		}
	} else {
		if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: 85}); err != nil {
			return data, nil
		}
	}
	return buf.Bytes(), nil
}
