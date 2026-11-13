package presentation

import (
	"fmt"
	"strings"
)

// Limits of the wave 1 payloads.
const (
	maxCalloutBodyRunes = 300

	minSteps           = 2
	maxSteps           = 10
	maxStepTitleRunes  = 80
	maxStepDetailRunes = 400

	maxComparisonAttributes = 10
	maxAttributeLabelRunes  = 40
	minComparisonOptions    = 2
	maxComparisonOptions    = 4
	maxOptionNameRunes      = 60
	maxOptionValueRunes     = 80
	maxProsCons             = 4
	maxProConRunes          = 80

	minTimelineEvents   = 2
	maxTimelineEvents   = 20
	maxEventWhenRunes   = 40
	maxEventTitleRunes  = 80
	maxEventDetailRunes = 300

	maxImageCaptionRunes   = 200
	minGalleryImages       = 2
	maxGalleryImages       = 12
	maxGalleryCaptionRunes = 120

	minChips          = 2
	maxChips          = 5
	maxChipLabelRunes = 40

	maxChecklistItems      = 40
	maxChecklistLabelRunes = 100
	maxChecklistGroupRunes = 40

	maxQuizQuestionRunes    = 300
	minQuizOptions          = 2
	maxQuizOptions          = 6
	maxQuizOptionLabelRunes = 120
	maxQuizExplanationRunes = 400
)

// Callout is a key takeaway, tip or warning. Its tone comes from the
// component envelope (info when unset).
type Callout struct {
	Icon string `json:"icon,omitempty"`
	Body string `json:"body"`
}

func validateCallout(v *validator, c Component, _ string) error {
	v.icon("callout.icon", c.ID, c.Callout.Icon)
	return markdown("callout.body", c.Callout.Body)
}

// Steps is a how-to in order. Checkable steps let viewers tick them off.
type Steps struct {
	Items     []Step `json:"items"`
	Checkable bool   `json:"checkable,omitempty"`
}

type Step struct {
	ID     string    `json:"id"`
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	Icon   string    `json:"icon,omitempty"`
	Image  *ImageRef `json:"image,omitempty"`
}

func validateSteps(v *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, step := range c.Steps.Items {
		at := fmt.Sprintf("steps.items[%d]", index)
		v.icon(at+".icon", c.ID, step.Icon)
		if err := firstError(itemIDs(seen, at, step.ID), markdown(at+".detail", step.Detail), v.image(at+".image", step.Image)); err != nil {
			return err
		}
	}
	return nil
}

// Comparison sets 2-4 options side by side against shared attributes.
type Comparison struct {
	Attributes  []ComparisonAttribute `json:"attributes"`
	Options     []ComparisonOption    `json:"options"`
	Recommended string                `json:"recommended,omitempty"`
	// Pickable lets the user pick one option, which answers you.
	Pickable bool `json:"pickable,omitempty"`
}

type ComparisonAttribute struct {
	Key   string `json:"key"`
	Label string `json:"label"`
}

type ComparisonOption struct {
	ID    string    `json:"id"`
	Name  string    `json:"name"`
	Image *ImageRef `json:"image,omitempty"`
	Badge string    `json:"badge,omitempty"`
	Price *Price    `json:"price,omitempty"`
	// Values maps every attribute key to this option's value.
	Values map[string]string `json:"values"`
	Pros   []string          `json:"pros,omitempty"`
	Cons   []string          `json:"cons,omitempty"`
}

// Price is an amount, optionally per month or year.
type Price struct {
	Amount
	Per string `json:"per,omitempty"`
}

// PricePeriods lists the values of Price.Per.
var PricePeriods = []string{"once", "month", "year"}

func validateComparison(v *validator, c Component, _ string) error {
	cmp := c.Comparison
	keys := map[string]bool{}
	for index, attribute := range cmp.Attributes {
		if keys[attribute.Key] {
			return fmt.Errorf("comparison.attributes[%d].key %q is used twice", index, attribute.Key)
		}
		keys[attribute.Key] = true
	}
	seen := map[string]bool{}
	for index, option := range cmp.Options {
		at := fmt.Sprintf("comparison.options[%d]", index)
		if err := firstError(itemIDs(seen, at, option.ID), v.image(at+".image", option.Image)); err != nil {
			return err
		}
		if option.Price != nil {
			if err := validateAmount(at+".price", option.Price.Amount); err != nil {
				return err
			}
		}
		for _, attribute := range cmp.Attributes {
			if _, ok := option.Values[attribute.Key]; !ok {
				return fmt.Errorf("%s.values needs a value for %q (an empty string is allowed)", at, attribute.Key)
			}
		}
		for key := range option.Values {
			if !keys[key] {
				return fmt.Errorf("%s.values.%s: no attribute has key %q", at, key, key)
			}
		}
	}
	if cmp.Recommended != "" && !seen[cmp.Recommended] {
		return fmt.Errorf("comparison.recommended %q is not the id of an option", cmp.Recommended)
	}
	return nil
}

// Timeline lists events in time order.
type Timeline struct {
	Events []TimelineEvent `json:"events"`
}

type TimelineEvent struct {
	ID string `json:"id"`
	// When is the display label, e.g. "1789", "Mon 3 Nov" or "Week 2".
	When string `json:"when"`
	// Date (YYYY-MM-DD) orders events; it is not shown.
	Date   string    `json:"date,omitempty"`
	Title  string    `json:"title"`
	Detail string    `json:"detail,omitempty"`
	Icon   string    `json:"icon,omitempty"`
	Image  *ImageRef `json:"image,omitempty"`
}

func validateTimeline(v *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, event := range c.Timeline.Events {
		at := fmt.Sprintf("timeline.events[%d]", index)
		v.icon(at+".icon", c.ID, event.Icon)
		if err := firstError(itemIDs(seen, at, event.ID), markdown(at+".detail", event.Detail), v.image(at+".image", event.Image)); err != nil {
			return err
		}
		if event.Date != "" && !validDate(event.Date) {
			return fmt.Errorf("%s.date %q is not a calendar date", at, event.Date)
		}
	}
	return nil
}

// ImageBlock shows one image from the chat with a caption.
type ImageBlock struct {
	Image   ImageRef `json:"image"`
	Caption string   `json:"caption,omitempty"`
	Aspect  string   `json:"aspect,omitempty"`
}

// ImageAspects lists the values of ImageBlock.Aspect.
var ImageAspects = []string{"auto", "square", "landscape", "portrait"}

func validateImage(v *validator, c Component, _ string) error {
	return v.image("image.image", &c.Image.Image)
}

// Gallery shows 2-12 images from the chat.
type Gallery struct {
	Images []GalleryImage `json:"images"`
	Layout string         `json:"layout,omitempty"`
}

type GalleryImage struct {
	Image   ImageRef `json:"image"`
	Caption string   `json:"caption,omitempty"`
}

// GalleryLayouts lists the values of Gallery.Layout.
var GalleryLayouts = []string{"grid", "strip"}

func validateGallery(v *validator, c Component, _ string) error {
	for index, image := range c.Gallery.Images {
		ref := image.Image
		if err := v.image(fmt.Sprintf("gallery.images[%d].image", index), &ref); err != nil {
			return err
		}
	}
	return nil
}

// Choices are quick-reply chips under the answer; a tap answers you.
type Choices struct {
	Chips []Chip `json:"chips"`
}

type Chip struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

func validateChoices(_ *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, chip := range c.Choices.Chips {
		if err := itemIDs(seen, fmt.Sprintf("choices.chips[%d]", index), chip.ID); err != nil {
			return err
		}
	}
	return nil
}

// Checklist is a shared list people tick off (packing, shopping, to-dos).
type Checklist struct {
	Items []ChecklistItem `json:"items"`
}

type ChecklistItem struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Group string `json:"group,omitempty"`
}

func validateChecklist(_ *validator, c Component, _ string) error {
	seen := map[string]bool{}
	for index, item := range c.Checklist.Items {
		if err := itemIDs(seen, fmt.Sprintf("checklist.items[%d]", index), item.ID); err != nil {
			return err
		}
	}
	return nil
}

// Quiz checks understanding with one question. AnswerKey stays on the
// server: StripAnswerKeys removes it before a visual reaches a viewer.
type Quiz struct {
	Question      string         `json:"question"`
	Options       []QuizOption   `json:"options"`
	AllowMultiple bool           `json:"allowMultiple,omitempty"`
	AnswerKey     *QuizAnswerKey `json:"answerKey,omitempty"`
}

type QuizOption struct {
	ID    string    `json:"id"`
	Label string    `json:"label"`
	Image *ImageRef `json:"image,omitempty"`
}

type QuizAnswerKey struct {
	Correct     []string `json:"correct"`
	Explanation string   `json:"explanation"`
}

func validateQuiz(v *validator, c Component, _ string) error {
	quiz := c.Quiz
	seen := map[string]bool{}
	for index, option := range quiz.Options {
		at := fmt.Sprintf("quiz.options[%d]", index)
		if err := firstError(itemIDs(seen, at, option.ID), v.image(at+".image", option.Image)); err != nil {
			return err
		}
	}
	key := quiz.AnswerKey
	if !quiz.AllowMultiple && len(key.Correct) != 1 {
		return fmt.Errorf("quiz.answerKey.correct needs exactly 1 option id unless allowMultiple is true (got %d)", len(key.Correct))
	}
	correct := map[string]bool{}
	for index, id := range key.Correct {
		if !seen[id] {
			return fmt.Errorf("quiz.answerKey.correct[%d] %q is not the id of an option", index, id)
		}
		if correct[id] {
			return fmt.Errorf("quiz.answerKey.correct[%d] %q is listed twice", index, id)
		}
		correct[id] = true
	}
	return markdown("quiz.answerKey.explanation", key.Explanation)
}

// optionLabels returns the labels of the quiz options with ids, in ids order.
func (q *Quiz) optionLabels(ids []string) []string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		for _, option := range q.Options {
			if option.ID == id {
				labels = append(labels, option.Label)
			}
		}
	}
	return labels
}

func quoteAll(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = fmt.Sprintf("%q", value)
	}
	return strings.Join(quoted, ", ")
}
