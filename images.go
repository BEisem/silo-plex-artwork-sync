package main

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // decode PNG posters for re-encoding
	"net/http"
)

// Posters are uploaded to Silo as bytes (images/upload for movies and series,
// the collection poster endpoint for collections). Silo stores at most 10 MiB
// per image and only JPEG, PNG or WebP, so anything larger is re-encoded as
// JPEG first. Plex posters are almost always far smaller.
const (
	maxUploadBytes = 10<<20 - 64<<10 // a little headroom under Silo's 10 MiB limit
	jpegReencodeQ  = 92
)

// fitForUpload returns data unchanged when Silo will accept it, and otherwise
// a JPEG re-encoding small enough to upload.
func fitForUpload(data []byte) ([]byte, error) {
	if len(data) <= maxUploadBytes {
		return data, nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("image is %d bytes and not JPEG/PNG, cannot shrink it below Silo's 10 MiB limit", len(data))
	}
	for q := jpegReencodeQ; q >= 50; q -= 10 {
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
			return nil, err
		}
		if buf.Len() <= maxUploadBytes {
			return buf.Bytes(), nil
		}
	}
	return nil, fmt.Errorf("image still over %d bytes after re-encoding", maxUploadBytes)
}

func sniffImageType(data []byte) string {
	switch ct := http.DetectContentType(data); ct {
	case "image/jpeg", "image/png", "image/webp":
		return ct
	default:
		return "image/jpeg"
	}
}
