package modelstep

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand/v2"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func encodedImage(t *testing.T, width, height int, alpha bool, format string) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	random := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = uint8(random.IntN(256)), uint8(random.IntN(256)), uint8(random.IntN(256)), 0xff
		if alpha && i%16 == 0 {
			img.Pix[i+3] = 0x40
		}
	}
	var out bytes.Buffer
	var err error
	if format == "png" {
		err = png.Encode(&out, img)
	} else {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 95})
	}
	if err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func TestFitModelImageKeepsSmallImagesAsStored(t *testing.T) {
	data := encodedImage(t, 800, 600, false, "jpeg")
	sent, mediaType, ok := fitModelImage(data, "image/jpeg")
	if !ok || mediaType != "image/jpeg" || !bytes.Equal(sent, data) {
		t.Fatalf("small image changed: ok=%v type=%s same=%v", ok, mediaType, bytes.Equal(sent, data))
	}
}

// A large phone photo (well over 1 MiB) is scaled to MaxModelImageEdge and
// fits the hosted media rule instead of being skipped.
func TestFitModelImageScalesLargePhotosToTheEdgeAndSizeLimit(t *testing.T) {
	data := encodedImage(t, 4000, 3000, false, "jpeg")
	if len(data) <= models.MaxModelStepMediaBytes {
		t.Fatalf("fixture is only %d bytes; want a photo over 1 MiB", len(data))
	}
	sent, mediaType, ok := fitModelImage(data, "image/jpeg")
	if !ok || mediaType != "image/jpeg" || len(sent) > models.MaxModelStepMediaBytes {
		t.Fatalf("large photo: ok=%v type=%s bytes=%d", ok, mediaType, len(sent))
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(sent))
	if err != nil || config.Width != MaxModelImageEdge || config.Height != 1176 {
		t.Fatalf("scaled to %dx%d (%v); want %dx1176", config.Width, config.Height, err, MaxModelImageEdge)
	}
}

func TestFitModelImageKeepsTransparencyAsPNGWhenItFits(t *testing.T) {
	data := encodedImage(t, 2000, 400, true, "png")
	sent, mediaType, ok := fitModelImage(data, "image/png")
	if !ok || len(sent) > models.MaxModelStepMediaBytes {
		t.Fatalf("transparent PNG: ok=%v bytes=%d", ok, len(sent))
	}
	decoded, _, err := image.Decode(bytes.NewReader(sent))
	if err != nil || decoded.Bounds().Dx() != MaxModelImageEdge {
		t.Fatalf("decoded %v, %v", decoded.Bounds(), err)
	}
	if mediaType == "image/png" {
		if _, _, _, a := decoded.At(0, 0).RGBA(); a == 0xffff && color.NRGBAModel.Convert(decoded.At(0, 0)).(color.NRGBA).A == 0xff {
			t.Fatal("transparent PNG lost its transparency")
		}
	}
}

// What cannot be decoded is sent as stored within the hosted size rule, as
// before scaling existed, and refused above it.
func TestFitModelImageSendsUndecodableBytesOnlyWithinTheSizeRule(t *testing.T) {
	if sent, _, ok := fitModelImage([]byte("not an image"), "image/png"); !ok || string(sent) != "not an image" {
		t.Fatal("small undecodable bytes were not sent as stored")
	}
	big := bytes.Repeat([]byte{1}, models.MaxModelStepMediaBytes+1)
	if _, _, ok := fitModelImage(big, "image/webp"); ok {
		t.Fatal("WebP over the size rule was accepted though it cannot be scaled")
	}
	if _, _, ok := fitModelImage(big, "image/png"); ok {
		t.Fatal("undecodable bytes over the size rule were accepted")
	}
}
