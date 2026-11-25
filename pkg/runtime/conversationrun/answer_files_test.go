package conversationrun

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
	"github.com/axiom-studio/openseal/pkg/presentation"
	"github.com/axiom-studio/openseal/pkg/runtime/agentrun"
)

// fakeChatFiles is the host's chat file store: files by path, each
// snapshot a stored version.
type fakeChatFiles struct {
	mu    sync.Mutex
	files map[string]LinkedFile
	asked []string
}

func (c *fakeChatFiles) SnapshotChatFile(_ context.Context, run *models.AgentRun, target string) (LinkedFile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asked = append(c.asked, target)
	file, ok := c.files[target]
	if !ok || run == nil {
		return LinkedFile{}, fmt.Errorf("%s: %w", target, ErrLinkedFileNotFound)
	}
	return file, nil
}

func newFakeChatFiles() *fakeChatFiles {
	return &fakeChatFiles{files: map[string]LinkedFile{
		"images/cat.png":     {ID: "file-cat", Version: 2, MediaType: "image/png"},
		"docs/q3 report.pdf": {ID: "file-report", Version: 1, MediaType: "application/pdf"},
	}}
}

// The answer's links to chat files are stored, attached in answer order and
// rewritten to the stored file; files the Run saved but the answer does not
// link stay out, and web links are left alone.
func TestAnswerAttachesExactlyTheLinkedFiles(t *testing.T) {
	chatFiles := newFakeChatFiles()
	forFinalDeliveryStores(t, finalDeliveryOptions{chatFiles: chatFiles}, func(t *testing.T, f *finalDeliveryFixture) {
		f.governed("draw", models.ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{
			map[string]interface{}{"id": "gen-1", "version": 1, "path": "images/hero.png"},
			map[string]interface{}{"id": "gen-draft", "version": 1, "path": "images/draft.png"},
		}})
		answer, _ := f.finalEntry("Here you go.\n\n![The hero](./images/hero.png)\n\n![A cat](images/cat.png) and the [report](docs/q3%20report.pdf), "+
			"again [report](</docs/q3 report.pdf>), see [the site](https://example.com/a.png).", nil)
		result := f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 {
			t.Fatalf("answers = %#v", answers)
		}
		wantContent := "Here you go.\n\n![The hero](" + presentation.ArtifactContentURL("gen-1", 1) + ")\n\n![A cat](" + presentation.ArtifactContentURL("file-cat", 2) +
			") and the [report](" + presentation.ArtifactContentURL("file-report", 1) + "), again [report](" + presentation.ArtifactContentURL("file-report", 1) +
			"), see [the site](https://example.com/a.png)."
		if answers[0].Content != wantContent {
			t.Fatalf("content = %q\nwant      %q", answers[0].Content, wantContent)
		}
		want := []models.ConversationReference{
			{Kind: models.ConversationReferenceRun, ID: f.run.ID},
			{Kind: models.ConversationReferenceArtifact, ID: "gen-1", Version: 1},
			{Kind: models.ConversationReferenceArtifact, ID: "file-cat", Version: 2},
			{Kind: models.ConversationReferenceArtifact, ID: "file-report", Version: 1},
		}
		if fmt.Sprint(answers[0].References) != fmt.Sprint(want) {
			t.Fatalf("references = %#v\nwant %#v", answers[0].References, want)
		}
		if summary := result.Output[FinalOutputSummaryKey]; summary != "Here you go.\n\n and the report, again report, see [the site](https://example.com/a.png)." {
			t.Fatalf("summary = %q", summary)
		}
		// A file the Run saved is found without the host.
		if strings.Contains(strings.Join(chatFiles.asked, ","), "hero") {
			t.Fatalf("the host was asked for a saved file: %v", chatFiles.asked)
		}
	})
}

// An image link to a file that is not an image becomes a file link.
func TestImageLinkToAFileIsAFileLink(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{chatFiles: newFakeChatFiles()}, func(t *testing.T, f *finalDeliveryFixture) {
		answer, _ := f.finalEntry("![The report](<docs/q3 report.pdf>)", nil)
		f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		if answers := f.answers(); len(answers) != 1 || answers[0].Content != "[The report]("+presentation.ArtifactContentURL("file-report", 1)+")" {
			t.Fatalf("answers = %#v", answers)
		}
	})
}

// A link to a file that is not in the chat is asked about once; the next
// answer is posted with the link's text in its place, and the Run records it.
func TestUnknownFileLinkIsAskedOnceThenLeftAsText(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{chatFiles: newFakeChatFiles()}, func(t *testing.T, f *finalDeliveryFixture) {
		f.governed("draw", models.ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{
			map[string]interface{}{"id": "gen-1", "version": 1, "path": "images/hero.png"},
		}})
		first, step := f.finalEntry("![The plan](images/plan.png) and ![A cat](images/cat.png)", nil)
		result := f.mustDeliver(first, agentrun.FinalDeliveryRetryFinal)
		if !isFileLinkRepairRequest(result.RejectionText) || !strings.Contains(result.RejectionText, "images/plan.png") ||
			strings.Contains(result.RejectionText, "images/cat.png") || !strings.Contains(result.RejectionText, "images/hero.png") {
			t.Fatalf("rejection = %q", result.RejectionText)
		}
		if len(f.answers()) != 0 {
			t.Fatal("an answer with an unknown link was posted before asking")
		}
		f.reject(step, result.RejectionText)
		second, _ := f.finalEntry("![The plan](images/plan.png) and ![A cat](images/cat.png)", nil)
		f.mustDeliver(second, agentrun.FinalDeliveryComplete)
		answers := f.answers()
		if len(answers) != 1 || answers[0].Content != "The plan and ![A cat]("+presentation.ArtifactContentURL("file-cat", 2)+")" || len(answers[0].References) != 2 {
			t.Fatalf("answers = %#v", answers)
		}
		events, err := f.store.ListActivity(f.ctx, models.ActivityFilter{Scope: f.run.Scope, RunID: f.run.ID, EventTypes: []string{answerLinkRejectedEvent}, Limit: 10})
		if err != nil || len(events) != 1 || events[0].Payload["target"] != "images/plan.png" {
			t.Fatalf("link rejected events = %#v, %v", events, err)
		}
	})
}

// Without a host chat file store only the files the Run's tools saved
// resolve.
func TestFileLinksWithoutHostResolveSavedFilesOnly(t *testing.T) {
	forFinalDeliveryStores(t, finalDeliveryOptions{}, func(t *testing.T, f *finalDeliveryFixture) {
		f.governed("draw", models.ActionCallStatusSucceeded, map[string]interface{}{"artifactRefs": []interface{}{
			map[string]interface{}{"id": "gen-1", "version": 1, "path": "images/hero.png"},
			map[string]interface{}{"id": "gen-1", "version": 2, "path": "images/hero.png"},
		}})
		answer, _ := f.finalEntry("![Hero](gen-1)", nil)
		f.mustDeliver(answer, agentrun.FinalDeliveryComplete)
		if answers := f.answers(); len(answers) != 1 || answers[0].Content != "![Hero]("+presentation.ArtifactContentURL("gen-1", 2)+")" {
			t.Fatalf("answers = %#v", answers)
		}
	})
}
