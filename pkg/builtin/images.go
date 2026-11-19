package builtin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/axiom-studio/openseal/pkg/skill"
)

// find_image limits.
const (
	MaximumImageQueryRunes   = 200
	MaximumImagePageURLRunes = 2048
	MaximumImageResults      = 5
	DefaultImageResults      = 3
)

// FindImageAction finds real photos for visuals in one call: licensed
// photos on Wikimedia Commons first, then the main photos of web pages the
// host finds for the same query. The host fetches each one and saves it in
// the chat's files, so a visual refers to it by artifact id and version like
// any other image in the chat.
func FindImageAction() skill.Action {
	text := map[string]interface{}{"type": "string"}
	size := map[string]interface{}{"type": "integer", "minimum": 1}
	image := object([]interface{}{"image", "title", "credit", "license", "sourceUrl"}, map[string]interface{}{
		"image": object([]interface{}{"artifactId", "version"}, map[string]interface{}{
			"artifactId": map[string]interface{}{"type": "string", "minLength": 1}, "version": map[string]interface{}{"type": "integer", "minimum": 1},
		}),
		"title": text, "credit": text,
		"license":   map[string]interface{}{"type": "string", "description": "Empty for a photo from a web page."},
		"sourceUrl": map[string]interface{}{"type": "string", "minLength": 1},
		"width":     size, "height": size,
	})
	return skill.Action{
		Name:        FindImage,
		Description: "Find real photos to show in a visual and save them in this chat. Searches Wikimedia Commons for licensed photos, then the web. Use each result's image in a visual and show its credit.",
		InputSchema: object([]interface{}{"query"}, map[string]interface{}{
			"query": map[string]interface{}{"type": "string", "minLength": 1, "maxLength": MaximumImageQueryRunes,
				"description": "What the photo shows, such as Tokyo skyline at night or Eiffel Tower."},
			"limit": map[string]interface{}{"type": "integer", "minimum": 1, "maximum": MaximumImageResults, "description": "At most this many photos; 3 when omitted."},
		}),
		OutputSchema: object([]interface{}{"images"}, map[string]interface{}{
			"images": map[string]interface{}{"type": "array", "maxItems": MaximumImageResults, "items": image},
		}),
		SideEffect: skill.SideEffectWrite, Risk: skill.RiskLevelWrite, Idempotency: skill.IdempotencyRequired,
		Retry: skill.ActionRetryPolicy{MaxAttempts: 1},
	}
}

// FindImageInput is a checked find_image call.
type FindImageInput struct {
	Query string
	Limit int
}

// ParseFindImageInput checks find_image's arguments for every host. Error
// text reaches the model.
func ParseFindImageInput(arguments map[string]interface{}) (FindImageInput, error) {
	var raw struct {
		Query *string `json:"query"`
		Limit *int    `json:"limit"`
	}
	encoded, err := json.Marshal(arguments)
	if err != nil {
		return FindImageInput{}, errors.New("invalid image search")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&raw) != nil || raw.Query == nil {
		return FindImageInput{}, errors.New("give a query, and optionally limit")
	}
	input := FindImageInput{Query: strings.Join(strings.Fields(*raw.Query), " "), Limit: DefaultImageResults}
	if input.Query == "" || utf8.RuneCountInString(input.Query) > MaximumImageQueryRunes {
		return FindImageInput{}, fmt.Errorf("the query is 1 to %d characters", MaximumImageQueryRunes)
	}
	if raw.Limit != nil {
		if *raw.Limit < 1 || *raw.Limit > MaximumImageResults {
			return FindImageInput{}, fmt.Errorf("limit is 1 to %d", MaximumImageResults)
		}
		input.Limit = *raw.Limit
	}
	return input, nil
}
