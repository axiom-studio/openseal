// Package source provides portable source-observation capabilities. Network
// access remains a host responsibility; this package owns the governed Skill
// contract and deterministic normalization shared by every host.
package source

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/capability"
	"github.com/axiom-studio/openseal/pkg/skill"
)

const (
	SkillID      = "openseal.source"
	SkillVersion = "1.0.1"
	ObserveFeed  = "observe_feed"
	MaximumItems = 100
)

type FeedObservation struct {
	StableSourceID string                 `json:"stableSourceId"`
	SourceURI      string                 `json:"sourceUri"`
	ContentDigest  string                 `json:"contentDigest"`
	Summary        string                 `json:"summary"`
	ObservedAt     time.Time              `json:"observedAt"`
	Metadata       map[string]interface{} `json:"metadata,omitempty"`
}

type FeedResult struct {
	Observations []FeedObservation `json:"sourceObservations"`
	NextCursor   string            `json:"nextCursor"`
}

func SkillDefinition() *skill.Definition {
	return &skill.Definition{
		ID: SkillID, Version: SkillVersion, Name: "Source observer",
		Description: "Observe permitted RSS or Atom sources and emit provenance-linked evidence.",
		Transport:   skill.TransportReference{Kind: "tool", Endpoint: SkillID},
		Actions: map[string]skill.Action{ObserveFeed: {
			Name: ObserveFeed, Description: "Read a permitted RSS or Atom feed and normalize its newest entries.",
			InputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false, "required": []interface{}{"url"},
				"properties": map[string]interface{}{
					"url":      map[string]interface{}{"type": "string", "format": "uri", "pattern": `^https://`},
					"maxItems": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumItems, "default": 25},
				},
			},
			OutputSchema: map[string]interface{}{
				"type": "object", "additionalProperties": false,
				"required": []interface{}{"observationRefs", "observationCount", "checkpointRevision"},
				"properties": map[string]interface{}{
					"observationRefs": map[string]interface{}{"type": "array", "minItems": 1, "maxItems": MaximumItems, "items": map[string]interface{}{
						"type": "object", "additionalProperties": false, "required": []interface{}{"id", "replayed"},
						"properties": map[string]interface{}{"id": map[string]interface{}{"type": "string", "minLength": 1}, "replayed": map[string]interface{}{"type": "boolean"}},
					}},
					"observationCount":   map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumItems},
					"checkpointRevision": map[string]interface{}{"type": "integer", "minimum": 1},
				},
			},
			SideEffect: skill.SideEffectRead, Risk: skill.RiskLevelRead,
			Permissions: []string{"network:https:read"}, Timeout: capability.Duration(30 * time.Second),
			Retry:       skill.ActionRetryPolicy{MaxAttempts: 3, InitialBackoff: capability.Duration(time.Second), MaxBackoff: capability.Duration(10 * time.Second)},
			Idempotency: skill.IdempotencySupported, EmittedEventTypes: []string{"source.observed"},
		}},
		Prompt: &capability.PromptModule{
			Instructions:  "Use observe_feed for permitted recurring research or operational source monitoring. Preserve source provenance; do not infer facts absent from observations.",
			UserInvocable: true, AllowedTools: []string{ObserveFeed},
		},
		Requirements: capability.Requirements{AlwaysAvailable: true},
	}
}

type feedDocument struct {
	Title   string      `xml:"title"`
	Channel *rssChannel `xml:"channel"`
	Entries []atomEntry `xml:"entry"`
}

type rssChannel struct {
	Title string    `xml:"title"`
	Items []rssItem `xml:"item"`
}

type rssItem struct {
	GUID        string `xml:"guid"`
	Link        string `xml:"link"`
	Title       string `xml:"title"`
	Description string `xml:"description"`
	Published   string `xml:"pubDate"`
}

type atomEntry struct {
	ID        string     `xml:"id"`
	Title     string     `xml:"title"`
	Summary   string     `xml:"summary"`
	Content   string     `xml:"content"`
	Published string     `xml:"published"`
	Updated   string     `xml:"updated"`
	Links     []atomLink `xml:"link"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
}

var tags = regexp.MustCompile(`<[^>]*>`)

// ParseFeed normalizes RSS 2.0 and Atom entries. The caller must enforce
// network policy and response-size limits before passing bytes here.
func ParseFeed(data []byte, feedURL string, maxItems int) (*FeedResult, error) {
	if maxItems < 1 || maxItems > MaximumItems {
		return nil, fmt.Errorf("maxItems must be between 1 and %d", MaximumItems)
	}
	base, err := url.Parse(feedURL)
	if err != nil || base.Scheme != "https" || base.Hostname() == "" {
		return nil, errors.New("feed URL must be an absolute HTTPS URL")
	}
	var document feedDocument
	if err := xml.Unmarshal(data, &document); err != nil {
		return nil, fmt.Errorf("parse RSS or Atom feed: %w", err)
	}
	feedTitle := cleanText(document.Title)
	result := &FeedResult{}
	if document.Channel != nil {
		feedTitle = cleanText(document.Channel.Title)
		for _, item := range document.Channel.Items {
			observation, observationErr := normalizeEntry(base, feedTitle, item.GUID, item.Link, item.Title, item.Description, item.Published)
			if observationErr == nil {
				result.Observations = append(result.Observations, observation)
			}
			if len(result.Observations) == maxItems {
				break
			}
		}
	} else {
		for _, item := range document.Entries {
			link := ""
			for _, candidate := range item.Links {
				if candidate.Rel == "" || candidate.Rel == "alternate" {
					link = candidate.Href
					break
				}
			}
			published := item.Published
			if published == "" {
				published = item.Updated
			}
			summary := item.Summary
			if summary == "" {
				summary = item.Content
			}
			observation, observationErr := normalizeEntry(base, feedTitle, item.ID, link, item.Title, summary, published)
			if observationErr == nil {
				result.Observations = append(result.Observations, observation)
			}
			if len(result.Observations) == maxItems {
				break
			}
		}
	}
	if len(result.Observations) == 0 {
		return nil, errors.New("feed contained no valid observable entries")
	}
	result.NextCursor = result.Observations[0].StableSourceID
	return result, nil
}

func normalizeEntry(base *url.URL, feedTitle, stableID, link, title, summary, published string) (FeedObservation, error) {
	resolved, err := base.Parse(strings.TrimSpace(link))
	if err != nil || resolved.Scheme != "https" || resolved.Hostname() == "" || resolved.User != nil {
		return FeedObservation{}, errors.New("entry link must resolve to an absolute HTTPS URL")
	}
	stableID = strings.TrimSpace(stableID)
	if stableID == "" {
		stableID = resolved.String()
	}
	title, summary = cleanText(title), cleanText(summary)
	if title == "" && summary == "" {
		return FeedObservation{}, errors.New("entry title or summary is required")
	}
	combined := title
	if summary != "" && summary != title {
		if combined != "" {
			combined += " — "
		}
		combined += summary
	}
	if len(combined) > 4000 {
		combined = strings.TrimSpace(combined[:4000])
	}
	observedAt := parseFeedTime(published)
	if observedAt.IsZero() {
		return FeedObservation{}, errors.New("entry timestamp is required")
	}
	digestInput := stableID + "\x00" + resolved.String() + "\x00" + combined + "\x00" + observedAt.Format(time.RFC3339Nano)
	digest := sha256.Sum256([]byte(digestInput))
	metadata := map[string]interface{}{"format": "rss-atom"}
	if feedTitle != "" {
		metadata["feedTitle"] = feedTitle
	}
	return FeedObservation{
		StableSourceID: stableID, SourceURI: resolved.String(), ContentDigest: "sha256:" + hex.EncodeToString(digest[:]),
		Summary: combined, ObservedAt: observedAt.UTC(), Metadata: metadata,
	}, nil
}

func cleanText(value string) string {
	value = html.UnescapeString(tags.ReplaceAllString(value, " "))
	return strings.Join(strings.Fields(value), " ")
}

func parseFeedTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC1123Z, time.RFC1123, time.RFC822Z, time.RFC822} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed
		}
	}
	return time.Time{}
}
