package presentation

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"

	"golang.org/x/text/currency"
)

// ImageRef points at an image already in the conversation (a generated
// image, an upload, an app screenshot). Visuals never carry image URLs.
type ImageRef struct {
	ArtifactID string `json:"artifactId"`
	Version    int64  `json:"version"`
}

// ImageResolver is implemented by the host. ResolveImage returns nil when the
// artifact exists, is visible from the publishing conversation and is an
// image; otherwise an error the agent can act on.
type ImageResolver interface {
	ResolveImage(ctx context.Context, ref ImageRef) error
}

// ImageResolverFunc adapts a function to ImageResolver.
type ImageResolverFunc func(ctx context.Context, ref ImageRef) error

func (f ImageResolverFunc) ResolveImage(ctx context.Context, ref ImageRef) error {
	return f(ctx, ref)
}

func (v *validator) image(path string, ref *ImageRef) error {
	if ref == nil {
		return nil
	}
	if !opaqueID.MatchString(ref.ArtifactID) {
		return fmt.Errorf("%s.artifactId %q must be the id of an image in this chat", path, ref.ArtifactID)
	}
	if ref.Version < 1 {
		return fmt.Errorf("%s.version must be 1 or more", path)
	}
	if v.opts.SkipHostChecks {
		return nil
	}
	if v.opts.Images == nil {
		return fmt.Errorf("%s: this host cannot show images yet; leave it out", path)
	}
	if err := v.opts.Images.ResolveImage(v.ctx, *ref); err != nil {
		return fmt.Errorf("%s (image %s version %d): %w", path, ref.ArtifactID, ref.Version, err)
	}
	return nil
}

// Amount is a number the host formats in the viewer's locale, with the same
// conventions as a stat: percent 12 means 12%, duration is in seconds, and
// currency (ISO 4217) is required exactly when format is currency.
type Amount struct {
	Value    float64    `json:"value"`
	Format   StatFormat `json:"format,omitempty"`
	Currency string     `json:"currency,omitempty"`
	Unit     string     `json:"unit,omitempty"`
}

func validateAmount(at string, amount Amount) error {
	if math.IsNaN(amount.Value) || math.IsInf(amount.Value, 0) {
		return fmt.Errorf("%s.value must be a finite number", at)
	}
	if err := optional(at+".unit", amount.Unit, 24); err != nil {
		return err
	}
	return validateFormat(at, amount.Format, amount.Currency)
}

// validateFormat checks a number format and its currency.
func validateFormat(at string, format StatFormat, code string) error {
	switch format {
	case "", StatNumber, StatPercent, StatDuration:
		if code != "" {
			return fmt.Errorf("%s.currency is only used with format currency", at)
		}
	case StatCurrency:
		return validateCurrency(at, code)
	default:
		return fmt.Errorf("%s.format %q is not one of number, currency, percent, duration", at, format)
	}
	return nil
}

func validateCurrency(at, code string) error {
	if code == "" {
		return fmt.Errorf("%s.currency is required with format currency, e.g. GBP, USD or EUR", at)
	}
	if parsed, err := currency.ParseISO(code); err != nil || parsed.String() != code {
		return fmt.Errorf("%s.currency %q is not an ISO 4217 code such as GBP, USD or EUR", at, code)
	}
	return nil
}

// Item ids name the items of a component (steps, options, chips) and are
// unique within it.
var itemID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,39}$`)

const itemIDPattern = `^[a-z0-9][a-z0-9_-]{0,39}$`

// itemIDs checks one item id and records it in seen.
func itemIDs(seen map[string]bool, at, id string) error {
	if !itemID.MatchString(id) {
		return fmt.Errorf("%s.id %q must match %s (lowercase letters, digits, - and _)", at, id, itemIDPattern)
	}
	if seen[id] {
		return fmt.Errorf("%s.id %q is used twice", at, id)
	}
	seen[id] = true
	return nil
}

var (
	markdownHTML  = regexp.MustCompile(`</?[A-Za-z][A-Za-z0-9-]*(\s[^<>]*)?/?>`)
	markdownImage = regexp.MustCompile(`!\[[^\]]*\]\(`)
)

// markdown checks a markdown field: GFM without HTML or images. $…$ math is
// allowed.
func markdown(field, value string, minimum, maximum int) error {
	if err := boundedText(value, minimum, maximum); err != nil {
		return fmt.Errorf("%s %w", field, err)
	}
	if markdownHTML.MatchString(value) {
		return fmt.Errorf("%s must not contain HTML tags; use markdown", field)
	}
	if markdownImage.MatchString(value) {
		return fmt.Errorf("%s must not contain markdown images; use an image field", field)
	}
	return nil
}

// bounded checks a list length.
func bounded(field string, length, minimum, maximum int) error {
	if length < minimum || length > maximum {
		if minimum == 0 {
			return fmt.Errorf("%s allows at most %d (got %d)", field, maximum, length)
		}
		return fmt.Errorf("%s needs %d-%d (got %d)", field, minimum, maximum, length)
	}
	return nil
}

func oneOf(field, value string, allowed []string) error {
	if value != "" && !contains(allowed, value) {
		return fmt.Errorf("%s %q is not one of %s", field, value, strings.Join(allowed, ", "))
	}
	return nil
}

var errPayloadMissing = errors.New("payload is required")

func payloadRequired(name string) error {
	return fmt.Errorf("%s %w", name, errPayloadMissing)
}
