package presentation

import (
	"fmt"
	"strings"
)

// Callout is a key takeaway, tip or warning. Its tone comes from the
// component envelope (info when unset).
type Callout struct {
	Icon string `json:"icon,omitempty"`
	Body string `json:"body"`
}

func validateCallout(v *validator, c Component, _ string) error {
	if c.Callout == nil {
		return payloadRequired("callout")
	}
	if err := v.icon("callout.icon", c.ID, c.Callout.Icon); err != nil {
		return err
	}
	return markdown("callout.body", c.Callout.Body, 1, 300)
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
	if c.Steps == nil {
		return payloadRequired("steps")
	}
	if err := bounded("steps.items", len(c.Steps.Items), 2, 10); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, step := range c.Steps.Items {
		at := fmt.Sprintf("steps.items[%d]", index)
		for _, check := range []error{
			itemIDs(seen, at, step.ID), required(at+".title", step.Title, 80), markdown(at+".detail", step.Detail, 0, 400),
			v.icon(at+".icon", c.ID, step.Icon), v.image(at+".image", step.Image),
		} {
			if check != nil {
				return check
			}
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
	if cmp == nil {
		return payloadRequired("comparison")
	}
	if err := bounded("comparison.attributes", len(cmp.Attributes), 1, 10); err != nil {
		return err
	}
	keys := map[string]bool{}
	for index, attribute := range cmp.Attributes {
		at := fmt.Sprintf("comparison.attributes[%d]", index)
		if !itemID.MatchString(attribute.Key) {
			return fmt.Errorf("%s.key %q must match %s", at, attribute.Key, itemIDPattern)
		}
		if keys[attribute.Key] {
			return fmt.Errorf("%s.key %q is used twice", at, attribute.Key)
		}
		keys[attribute.Key] = true
		if err := required(at+".label", attribute.Label, 40); err != nil {
			return err
		}
	}
	if err := bounded("comparison.options", len(cmp.Options), 2, 4); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, option := range cmp.Options {
		at := fmt.Sprintf("comparison.options[%d]", index)
		for _, check := range []error{
			itemIDs(seen, at, option.ID), required(at+".name", option.Name, 60), optional(at+".badge", option.Badge, 24),
			v.image(at+".image", option.Image), bounded(at+".pros", len(option.Pros), 0, 4), bounded(at+".cons", len(option.Cons), 0, 4),
		} {
			if check != nil {
				return check
			}
		}
		if option.Price != nil {
			if err := validateAmount(at+".price", option.Price.Amount); err != nil {
				return err
			}
			if err := oneOf(at+".price.per", option.Price.Per, PricePeriods); err != nil {
				return err
			}
		}
		for _, attribute := range cmp.Attributes {
			value, ok := option.Values[attribute.Key]
			if !ok {
				return fmt.Errorf("%s.values needs a value for %q (an empty string is allowed)", at, attribute.Key)
			}
			if err := optional(fmt.Sprintf("%s.values.%s", at, attribute.Key), value, 80); err != nil {
				return err
			}
		}
		for key := range option.Values {
			if !keys[key] {
				return fmt.Errorf("%s.values.%s: no attribute has key %q", at, key, key)
			}
		}
		for item, value := range option.Pros {
			if err := required(fmt.Sprintf("%s.pros[%d]", at, item), value, 80); err != nil {
				return err
			}
		}
		for item, value := range option.Cons {
			if err := required(fmt.Sprintf("%s.cons[%d]", at, item), value, 80); err != nil {
				return err
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
	if c.Timeline == nil {
		return payloadRequired("timeline")
	}
	if err := bounded("timeline.events", len(c.Timeline.Events), 2, 20); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, event := range c.Timeline.Events {
		at := fmt.Sprintf("timeline.events[%d]", index)
		for _, check := range []error{
			itemIDs(seen, at, event.ID), required(at+".when", event.When, 40), required(at+".title", event.Title, 80),
			markdown(at+".detail", event.Detail, 0, 300), v.icon(at+".icon", c.ID, event.Icon), v.image(at+".image", event.Image),
		} {
			if check != nil {
				return check
			}
		}
		if event.Date != "" && !validDate(event.Date) {
			return fmt.Errorf("%s.date %q must be a date as YYYY-MM-DD", at, event.Date)
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
	if c.Image == nil {
		return payloadRequired("image")
	}
	return firstError(v.image("image.image", &c.Image.Image), optional("image.caption", c.Image.Caption, 200), oneOf("image.aspect", c.Image.Aspect, ImageAspects))
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
	if c.Gallery == nil {
		return payloadRequired("gallery")
	}
	if err := firstError(bounded("gallery.images", len(c.Gallery.Images), 2, 12), oneOf("gallery.layout", c.Gallery.Layout, GalleryLayouts)); err != nil {
		return err
	}
	for index, image := range c.Gallery.Images {
		at := fmt.Sprintf("gallery.images[%d]", index)
		ref := image.Image
		if err := firstError(v.image(at+".image", &ref), optional(at+".caption", image.Caption, 120)); err != nil {
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
	if c.Choices == nil {
		return payloadRequired("choices")
	}
	if err := bounded("choices.chips", len(c.Choices.Chips), 2, 5); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, chip := range c.Choices.Chips {
		at := fmt.Sprintf("choices.chips[%d]", index)
		if err := firstError(itemIDs(seen, at, chip.ID), required(at+".label", chip.Label, 40)); err != nil {
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
	if c.Checklist == nil {
		return payloadRequired("checklist")
	}
	if err := bounded("checklist.items", len(c.Checklist.Items), 1, 40); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, item := range c.Checklist.Items {
		at := fmt.Sprintf("checklist.items[%d]", index)
		if err := firstError(itemIDs(seen, at, item.ID), required(at+".label", item.Label, 100), optional(at+".group", item.Group, 40)); err != nil {
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
	if quiz == nil {
		return payloadRequired("quiz")
	}
	if err := firstError(required("quiz.question", quiz.Question, 300), bounded("quiz.options", len(quiz.Options), 2, 6)); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index, option := range quiz.Options {
		at := fmt.Sprintf("quiz.options[%d]", index)
		if err := firstError(itemIDs(seen, at, option.ID), required(at+".label", option.Label, 120), v.image(at+".image", option.Image)); err != nil {
			return err
		}
	}
	key := quiz.AnswerKey
	if key == nil {
		return fmt.Errorf("quiz.answerKey is required: the correct option ids and an explanation (viewers never see it before answering)")
	}
	if quiz.AllowMultiple {
		if len(key.Correct) == 0 {
			return fmt.Errorf("quiz.answerKey.correct needs at least 1 option id")
		}
	} else if len(key.Correct) != 1 {
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
	return markdown("quiz.answerKey.explanation", key.Explanation, 0, 400)
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
