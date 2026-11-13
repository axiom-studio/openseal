package presentation

import (
	"context"
	"errors"
	"fmt"
	"regexp"

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
	if ref == nil || v.opts.SkipHostChecks {
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

// Limits shared by several payloads.
const (
	maxUnitRunes  = 24 // the unit of an amount, stat, chart, breakdown or progress item
	maxBadgeRunes = 24
	maxRating     = 5
)

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
	return validateFormat(at, amount.Format, amount.Currency)
}

// validateFormat checks a number format and its currency.
func validateFormat(at string, format StatFormat, code string) error {
	if format == StatCurrency {
		return validateCurrency(at, code)
	}
	if code != "" {
		return fmt.Errorf("%s.currency is only used with format currency", at)
	}
	return nil
}

const currencyPattern = `^[A-Z]{3}$`

func validateCurrency(at, code string) error {
	if code == "" {
		return fmt.Errorf("%s.currency is required with format currency, e.g. GBP, USD or EUR", at)
	}
	if parsed, err := currency.ParseISO(code); err != nil || parsed.String() != code {
		return fmt.Errorf("%s.currency %q is not an ISO 4217 code such as GBP, USD or EUR", at, code)
	}
	return nil
}

// itemIDPattern is the id of an item inside a component (steps, options,
// chips).
const itemIDPattern = `^[a-z0-9][a-z0-9_-]{0,39}$`

// itemIDs checks one item id and records it in seen.
func itemIDs(seen map[string]bool, at, id string) error {
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
func markdown(field, value string) error {
	if markdownHTML.MatchString(value) {
		return fmt.Errorf("%s must not contain HTML tags; use markdown", field)
	}
	if markdownImage.MatchString(value) {
		return fmt.Errorf("%s must not contain markdown images; use an image field", field)
	}
	return nil
}

var errPayloadMissing = errors.New("payload is required")
