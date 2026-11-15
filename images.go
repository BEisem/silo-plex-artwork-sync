package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
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

// fitArtwork checks an image of type t against Silo's limits and returns the
// bytes to upload. The real format is read from the bytes: anything that is
// not JPEG, PNG or WebP (an HTML error page, a corrupt download) is refused.
//
// Logos are kept transparent: a logo is never turned into a JPEG. One over
// the limit is re-encoded as PNG, then scaled down by halves (keeping its
// alpha channel) until it fits; a WebP logo over the limit is refused, as
// the standard library cannot decode WebP. Other types reuse fitForUpload.
func fitArtwork(t ArtworkType, data []byte) ([]byte, error) {
	switch ct := http.DetectContentType(data); ct {
	case "image/jpeg", "image/png", "image/webp":
		if len(data) <= maxUploadBytes {
			return data, nil
		}
		if t != ArtLogo {
			return fitForUpload(data)
		}
		if ct == "image/webp" {
			return nil, fmt.Errorf("WebP logo is %d bytes, over Silo's 10 MiB limit, and cannot be shrunk without losing transparency; skipped", len(data))
		}
		return shrinkLogo(data)
	default:
		return nil, fmt.Errorf("not a JPEG, PNG or WebP image (detected %s); skipped", ct)
	}
}

// shrinkLogo re-encodes a logo as PNG, halving its size until it fits.
func shrinkLogo(data []byte) ([]byte, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("logo is %d bytes and could not be decoded to shrink it: %v", len(data), err)
	}
	enc := png.Encoder{CompressionLevel: png.BestCompression}
	for range 4 {
		var buf bytes.Buffer
		if err := enc.Encode(&buf, img); err != nil {
			return nil, err
		}
		if buf.Len() <= maxUploadBytes {
			return buf.Bytes(), nil
		}
		b := img.Bounds()
		if b.Dx() < 512 {
			break
		}
		img = halve(img)
	}
	return nil, fmt.Errorf("logo still over %d bytes after transparent PNG re-encoding and downscaling; skipped", maxUploadBytes)
}

// halve scales an image to half its width and height by averaging 2x2
// blocks in premultiplied alpha, so transparent edges keep their colour.
func halve(src image.Image) *image.NRGBA {
	b := src.Bounds()
	w, h := b.Dx()/2, b.Dy()/2
	dst := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			var r, g, bl, a uint32
			for dy := range 2 {
				for dx := range 2 {
					pr, pg, pb, pa := src.At(b.Min.X+2*x+dx, b.Min.Y+2*y+dy).RGBA()
					r, g, bl, a = r+pr, g+pg, bl+pb, a+pa
				}
			}
			var c color.NRGBA
			if a > 0 {
				c = color.NRGBA{R: uint8(r * 0xff / a), G: uint8(g * 0xff / a), B: uint8(bl * 0xff / a), A: uint8(a / 4 >> 8)}
			}
			dst.SetNRGBA(x, y, c)
		}
	}
	return dst
}
