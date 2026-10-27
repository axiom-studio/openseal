package presentation

import (
	"fmt"
	"regexp"
)

// Icons is the curated Lucide subset an icon field may name. The frontend
// mirrors this list; keep it sorted. An unknown but well-formed name is a
// Validate warning and renders as a neutral dot.
var Icons = []string{
	"activity", "alarm-clock", "apple", "archive", "arrow-down", "arrow-left", "arrow-right", "arrow-up",
	"atom", "award", "baby", "ban", "banknote", "bed", "bell", "bike", "book", "book-open", "bookmark", "brain",
	"briefcase", "building", "bus", "calculator", "calendar", "calendar-check", "calendar-days", "camera",
	"car", "carrot", "cat", "chart-bar", "chart-column", "chart-line", "chart-pie", "check", "chef-hat",
	"circle-alert", "circle-check", "circle-help", "circle-x", "clipboard", "clipboard-list", "clock", "cloud",
	"coffee", "coins", "compass", "credit-card", "dna", "dog", "dollar-sign", "droplet", "dumbbell",
	"external-link", "eye", "file", "file-text", "flag", "flame", "flask-conical", "flower", "folder", "gauge",
	"gift", "globe", "graduation-cap", "hand", "heart", "heart-pulse", "history", "house", "image", "info",
	"key", "languages", "layers", "layout-grid", "leaf", "lightbulb", "link", "list", "list-checks", "lock",
	"lock-open", "mail", "map", "map-pin", "medal", "message-circle", "mic", "minus", "moon", "mountain",
	"music", "package", "paw-print", "pencil", "percent", "phone", "piggy-bank", "pill", "pizza", "plane",
	"plus", "pound-sterling", "power", "receipt", "refresh-cw", "repeat", "rocket", "scale", "school", "search",
	"send", "settings", "share-2", "shield", "shield-check", "shopping-bag", "shopping-cart", "sigma",
	"sliders-horizontal", "smartphone", "smile", "sparkles", "sprout", "star", "stethoscope", "store", "sun",
	"table", "tag", "target", "thermometer", "thumbs-down", "thumbs-up", "timer", "train-front", "trash-2",
	"tree-pine", "trending-down", "trending-up", "trophy", "truck", "user", "users", "utensils", "wallet",
	"weight", "x", "zap",
}

var (
	iconSet     = map[string]bool{}
	iconPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

func init() {
	for _, name := range Icons {
		iconSet[name] = true
	}
}

// KnownIcon reports whether name is in Icons.
func KnownIcon(name string) bool {
	return iconSet[name]
}

func (v *validator) icon(path, componentID, name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 40 || !iconPattern.MatchString(name) {
		return fmt.Errorf("%s %q must be a lowercase Lucide icon name such as \"circle-check\"", path, name)
	}
	if !KnownIcon(name) {
		v.warn("unknown_icon", path, componentID, "icon %q is not in the curated set and renders as a dot", name)
	}
	return nil
}
