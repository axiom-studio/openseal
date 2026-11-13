package modelstep

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"image/png"

	"github.com/axiom-studio/openseal/pkg/models"
)

// MaxModelImageEdge is the longest side an image is sent to a model at.
// Anthropic models scale anything larger to about this size themselves and
// bill by pixels, and OpenAI's detail modes scale further down, so a larger
// image only costs bytes, latency and (for per-pixel pricing) tokens.
const MaxModelImageEdge = 1568

// maxModelImagePixels refuses to decode pictures larger than this (16 MP),
// the same bound chat attachments use, so a decode never balloons memory.
const maxModelImagePixels = 16_000_000

// modelImageJPEGQualities are tried in order until the copy fits the hosted
// media size rule.
var modelImageJPEGQualities = []int{85, 75, 60, 45}

// fitModelImage returns the image as it is sent to a model: unchanged when it
// is already within MaxModelImageEdge and the hosted size rule, otherwise a
// copy scaled to MaxModelImageEdge on its long side (PNG when it has
// transparency and fits, else JPEG). ok is false when it cannot be sent.
// What is not decoded here (WebP, or bytes that do not decode) is sent only as
// stored, within the hosted size rule.
func fitModelImage(data []byte, mediaType string) ([]byte, string, bool) {
	if len(data) == 0 {
		return nil, "", false
	}
	if mediaType != "image/png" && mediaType != "image/jpeg" {
		return data, mediaType, renderImageBytesValid(data)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > maxModelImagePixels {
		return data, mediaType, renderImageBytesValid(data)
	}
	if max(config.Width, config.Height) <= MaxModelImageEdge && renderImageBytesValid(data) {
		return data, mediaType, true
	}
	source, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return data, mediaType, renderImageBytesValid(data)
	}
	scaled := scaleImageToEdge(source, MaxModelImageEdge)
	if !imageOpaque(scaled) {
		var out bytes.Buffer
		if (&png.Encoder{CompressionLevel: png.BestCompression}).Encode(&out, scaled) == nil && renderImageBytesValid(out.Bytes()) {
			return out.Bytes(), "image/png", true
		}
	}
	flat := image.NewRGBA(scaled.Bounds())
	draw.Draw(flat, flat.Bounds(), image.NewUniform(color.White), image.Point{}, draw.Src)
	draw.Draw(flat, flat.Bounds(), scaled, scaled.Bounds().Min, draw.Over)
	for _, quality := range modelImageJPEGQualities {
		var out bytes.Buffer
		if jpeg.Encode(&out, flat, &jpeg.Options{Quality: quality}) == nil && renderImageBytesValid(out.Bytes()) {
			return out.Bytes(), "image/jpeg", true
		}
	}
	return nil, "", false
}

// scaleImageToEdge shrinks src so its long side is at most edge, averaging
// the source pixels that fall in each destination pixel (a box filter, which
// is what downscaling a photo needs). Smaller images are copied as they are.
func scaleImageToEdge(src image.Image, edge int) *image.RGBA {
	bounds := src.Bounds()
	rgba := image.NewRGBA(image.Rect(0, 0, bounds.Dx(), bounds.Dy()))
	draw.Draw(rgba, rgba.Bounds(), src, bounds.Min, draw.Src)
	width, height := rgba.Bounds().Dx(), rgba.Bounds().Dy()
	if max(width, height) <= edge {
		return rgba
	}
	dstW, dstH := width*edge/max(width, height), height*edge/max(width, height)
	dstW, dstH = max(dstW, 1), max(dstH, 1)
	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	for y := 0; y < dstH; y++ {
		y0, y1 := y*height/dstH, max((y+1)*height/dstH, y*height/dstH+1)
		for x := 0; x < dstW; x++ {
			x0, x1 := x*width/dstW, max((x+1)*width/dstW, x*width/dstW+1)
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				row := rgba.Pix[sy*rgba.Stride:]
				for sx := x0; sx < x1; sx++ {
					p := row[sx*4 : sx*4+4]
					r, g, b, a, n = r+uint64(p[0]), g+uint64(p[1]), b+uint64(p[2]), a+uint64(p[3]), n+1
				}
			}
			o := dst.Pix[y*dst.Stride+x*4:]
			o[0], o[1], o[2], o[3] = uint8(r/n), uint8(g/n), uint8(b/n), uint8(a/n)
		}
	}
	return dst
}

func imageOpaque(img *image.RGBA) bool {
	for i := 3; i < len(img.Pix); i += 4 {
		if img.Pix[i] != 0xff {
			return false
		}
	}
	return true
}

// modelImageSourceValid is the size rule for an image read before it is
// fitted: up to models.MaxModelImageSourceBytes.
func modelImageSourceValid(data []byte) bool {
	return len(data) > 0 && len(data) <= models.MaxModelImageSourceBytes
}
