package artifactoutput

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

func TestConsumeValidatesAndRemovesTransientBytes(t *testing.T) {
	content := []byte("png")
	output := map[string]interface{}{
		EncodedBytesField: base64.StdEncoding.EncodeToString(content), "filename": "evidence.png",
		"requirementName": "visual-proof", "artifactType": "browser-screenshot", "mediaType": "image/png",
		"digest": fmt.Sprintf("sha256:%x", sha256.Sum256(content)), "sizeBytes": float64(len(content)),
	}
	envelope, public, err := Consume(output, []string{"browser-screenshot"})
	if err != nil || string(envelope.Bytes) != "png" || envelope.Filename != "evidence.png" || len(public) != 0 {
		t.Fatalf("consume = %#v, %#v, %v", envelope, public, err)
	}
	if _, present := output[EncodedBytesField]; !present {
		t.Fatal("Consume mutated caller output")
	}
}

func TestConsumeFailsClosed(t *testing.T) {
	content := []byte("png")
	valid := map[string]interface{}{
		EncodedBytesField: base64.StdEncoding.EncodeToString(content), "filename": "evidence.png",
		"requirementName": "proof", "artifactType": "browser-screenshot", "mediaType": "image/png",
		"digest": fmt.Sprintf("sha256:%x", sha256.Sum256(content)), "sizeBytes": len(content),
	}
	for name, mutate := range map[string]func(map[string]interface{}){
		"undeclared": func(value map[string]interface{}) {},
		"path":       func(value map[string]interface{}) { value["filename"] = "../evidence.png" },
		"type":       func(value map[string]interface{}) { value["artifactType"] = "report" },
		"digest":     func(value map[string]interface{}) { value["digest"] = "sha256:" + strings.Repeat("0", 64) },
		"size":       func(value map[string]interface{}) { value["sizeBytes"] = 99 },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := clone(valid)
			mutate(candidate)
			allowed := []string{"browser-screenshot"}
			if name == "undeclared" {
				allowed = nil
			}
			if _, _, err := Consume(candidate, allowed); err == nil {
				t.Fatal("unsafe artifact envelope was accepted")
			}
		})
	}
}
