package models

import (
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validElementAnchor() *ArtifactAnchor {
	return &ArtifactAnchor{
		Type: ArtifactAnchorElement, Selector: "section.tiers > div:nth-child(2)", Label: "Pro tier card", Tag: "div",
		Quote: "Most teams pick Pro", Snippet: `<div class="tier hot">…</div>`, Styles: map[string]string{"padding": "14px"},
		Rect: &AnchorRect{X: 0.1, Y: 0.3, W: 0.5, H: 0.12}, AppState: &AnchorAppState{Screen: "#/pipeline"},
	}
}

func anchoredMessage(refs ...ConversationReference) *ChannelMessage {
	return &ChannelMessage{
		ID: "m_1", Scope: Scope{Kind: "tenant", ID: "anchors"}, ConversationID: "chat", Sequence: 1,
		Sender: ConversationParticipant{Type: ConversationParticipantUser, ID: "u_1"}, Intent: MessageIntentQuestion,
		Content: "Make this card stand out", Audience: ConversationAudience{Kind: ConversationAudienceChannel}, References: refs,
		CreatedAt: time.Date(2026, 11, 17, 9, 0, 0, 0, time.UTC),
	}
}

func TestArtifactAnchorValidate(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ArtifactAnchor)
		ok     bool
	}{
		{"element", func(*ArtifactAnchor) {}, true},
		{"layout needs nothing", func(a *ArtifactAnchor) { *a = ArtifactAnchor{Type: ArtifactAnchorLayout} }, true},
		{"text", func(a *ArtifactAnchor) {
			*a = ArtifactAnchor{Type: ArtifactAnchorText, Quote: "line one\nline two", Range: "12-14"}
		}, true},
		{"cell", func(a *ArtifactAnchor) { *a = ArtifactAnchor{Type: ArtifactAnchorCell, Sheet: "Q3", Range: "B4:D9"} }, true},
		{"point", func(a *ArtifactAnchor) {
			*a = ArtifactAnchor{Type: ArtifactAnchorPoint, Point: &AnchorPoint{X: 0, Y: 1}, Page: 2}
		}, true},
		{"unknown type", func(a *ArtifactAnchor) { a.Type = "region" }, false},
		{"text without quote", func(a *ArtifactAnchor) { *a = ArtifactAnchor{Type: ArtifactAnchorText} }, false},
		{"paragraph without quote", func(a *ArtifactAnchor) { *a = ArtifactAnchor{Type: ArtifactAnchorParagraph} }, false},
		{"cell without range", func(a *ArtifactAnchor) { *a = ArtifactAnchor{Type: ArtifactAnchorCell, Sheet: "Q3"} }, false},
		{"element without label", func(a *ArtifactAnchor) { a.Label = "" }, false},
		{"element without selector", func(a *ArtifactAnchor) { a.Selector = "" }, false},
		{"point without point", func(a *ArtifactAnchor) { *a = ArtifactAnchor{Type: ArtifactAnchorPoint} }, false},
		{"quote limit", func(a *ArtifactAnchor) { a.Quote = strings.Repeat("q", 1001) }, false},
		{"snippet limit", func(a *ArtifactAnchor) { a.Snippet = strings.Repeat("s", 1501) }, false},
		{"selector limit", func(a *ArtifactAnchor) { a.Selector = strings.Repeat("s", 513) }, false},
		{"label limit", func(a *ArtifactAnchor) { a.Label = strings.Repeat("l", 121) }, false},
		{"sheet limit", func(a *ArtifactAnchor) { a.Sheet = strings.Repeat("s", 65) }, false},
		{"invalid utf-8", func(a *ArtifactAnchor) { a.Label = "bad \xff" }, false},
		{"control char", func(a *ArtifactAnchor) { a.Label = "tab\there" }, false},
		{"newline outside quote", func(a *ArtifactAnchor) { a.Selector = "div\nspan" }, false},
		{"newline in snippet", func(a *ArtifactAnchor) { a.Snippet = "<div>\n</div>" }, true},
		{"tag", func(a *ArtifactAnchor) { a.Tag = "Div" }, false},
		{"range", func(a *ArtifactAnchor) { a.Range = "b4" }, false},
		{"negative page", func(a *ArtifactAnchor) { a.Page = -1 }, false},
		{"page limit", func(a *ArtifactAnchor) { a.Page = 10001 }, false},
		{"style key", func(a *ArtifactAnchor) { a.Styles = map[string]string{"position": "absolute"} }, false},
		{"style value", func(a *ArtifactAnchor) { a.Styles = map[string]string{"color": strings.Repeat("r", 121)} }, false},
		{"too many styles", func(a *ArtifactAnchor) {
			a.Styles = map[string]string{}
			for key := range anchorStyleKeys {
				a.Styles[key] = "1"
			}
		}, false},
		{"NaN rect", func(a *ArtifactAnchor) { a.Rect.W = math.NaN() }, false},
		{"rect out of range", func(a *ArtifactAnchor) { a.Rect.X = 1.2 }, false},
		{"point out of range", func(a *ArtifactAnchor) { a.Point = &AnchorPoint{X: -0.1, Y: 0.5} }, false},
		{"app state limit", func(a *ArtifactAnchor) { a.AppState.Heading = strings.Repeat("h", 201) }, false},
		{"oversize JSON", func(a *ArtifactAnchor) {
			a.Quote, a.Snippet = strings.Repeat("q", 1000), strings.Repeat("<", 600)
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			anchor := validElementAnchor()
			tc.mutate(anchor)
			ref := ConversationReference{Kind: ConversationReferenceArtifact, ID: "file-ab12", Version: 3, Anchor: anchor}
			err := anchoredMessage(ref).Validate()
			if tc.ok && err != nil {
				t.Fatalf("rejected: %v", err)
			}
			if !tc.ok && !errors.Is(err, ErrInvalidConversation) {
				t.Fatalf("accepted or wrong error: %v", err)
			}
		})
	}

	anchored := ConversationReference{Kind: ConversationReferenceArtifact, ID: "file-ab12", Version: 3, Anchor: validElementAnchor()}
	for _, tc := range []struct {
		name    string
		message *ChannelMessage
	}{
		{"non-artifact kind", anchoredMessage(ConversationReference{Kind: ConversationReferenceRun, ID: "run-1", Anchor: validElementAnchor()})},
		{"version 0", anchoredMessage(ConversationReference{Kind: ConversationReferenceArtifact, ID: "file-ab12", Anchor: validElementAnchor()})},
		{"two anchored refs", anchoredMessage(anchored, ConversationReference{Kind: ConversationReferenceArtifact, ID: "file-cd34", Version: 1, Anchor: validElementAnchor()})},
		{"empty content", func() *ChannelMessage { m := anchoredMessage(anchored); m.Content = "  "; return m }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.message.Validate(); !errors.Is(err, ErrInvalidConversation) {
				t.Fatalf("accepted or wrong error: %v", err)
			}
		})
	}
	plain := ConversationReference{Kind: ConversationReferenceArtifact, ID: "file-cd34", Version: 1}
	if err := anchoredMessage(anchored, plain).Validate(); err != nil {
		t.Fatalf("an anchored reference beside a plain attachment was rejected: %v", err)
	}
}

func TestArtifactAnchorCloneIsDeep(t *testing.T) {
	original := validElementAnchor()
	original.Point = &AnchorPoint{X: 0.4, Y: 0.6}
	clone := original.Clone()
	if !reflect.DeepEqual(original, clone) {
		t.Fatalf("clone differs: %#v", clone)
	}
	clone.Styles["padding"], clone.Rect.X, clone.Point.Y, clone.AppState.Screen = "0", 0.9, 0.1, "#/other"
	if original.Styles["padding"] != "14px" || original.Rect.X != 0.1 || original.Point.Y != 0.6 || original.AppState.Screen != "#/pipeline" {
		t.Fatalf("clone shares memory with the original: %#v", original)
	}

	refs := []ConversationReference{{Kind: ConversationReferenceArtifact, ID: "file-ab12", Version: 3, Anchor: validElementAnchor()}}
	cloned := CloneConversationReferences(refs)
	cloned[0].Anchor.Label = "changed"
	if refs[0].Anchor.Label != "Pro tier card" {
		t.Fatal("CloneConversationReferences shares anchors")
	}
	if (*ArtifactAnchor)(nil).Clone() != nil {
		t.Fatal("nil anchor cloned to non-nil")
	}
}
