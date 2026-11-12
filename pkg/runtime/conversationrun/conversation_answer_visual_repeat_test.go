package conversationrun

import (
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
)

// A visual that only shows images the answer already carries is dropped, so
// a generated image is shown once.
func TestVisualRepeatsAttachedImages(t *testing.T) {
	refs := []models.ConversationReference{{Kind: models.ConversationReferenceRun, ID: "run"}, {Kind: models.ConversationReferenceArtifact, ID: "file-cat", Version: 1}}
	image := func(id string) presentation.Component {
		return presentation.Component{Image: &presentation.ImageBlock{Image: presentation.ImageRef{ArtifactID: id, Version: 1}}}
	}
	if !visualRepeatsAttachedImages(presentation.Visual{Components: []presentation.Component{image("file-cat")}}, refs) {
		t.Fatal("a visual of the attached image was kept")
	}
	if visualRepeatsAttachedImages(presentation.Visual{Components: []presentation.Component{image("file-dog")}}, refs) {
		t.Fatal("a visual of another image was dropped")
	}
	text := presentation.Component{Type: "text"}
	if visualRepeatsAttachedImages(presentation.Visual{Components: []presentation.Component{image("file-cat"), text}}, refs) {
		t.Fatal("a visual with more than the attached image was dropped")
	}
}
