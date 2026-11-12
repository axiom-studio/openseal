package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	MaximumAskUserQuestionBytes = 4096
)

type AskUserArgs struct {
	Question      string          `json:"question"`
	Options       []askUserOption `json:"options,omitempty"`
	AllowMultiple bool            `json:"allowMultiple,omitempty"`
	// AllowOther lets the person answer in their own words alongside the options.
	AllowOther bool `json:"allowOther,omitempty"`
}

type askUserOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// askUserOptionID is the kernel-assigned id of the index-th option ("o1".."o6").
func askUserOptionID(index int) string { return fmt.Sprintf("o%d", index+1) }

// ChannelQuestion is the posted shape of the options (nil without options).
func (a *AskUserArgs) ChannelQuestion() *ChannelQuestion {
	if a == nil || len(a.Options) == 0 {
		return nil
	}
	question := &ChannelQuestion{AllowMultiple: a.AllowMultiple, AllowOther: a.AllowOther}
	for index, option := range a.Options {
		question.Options = append(question.Options, ChannelQuestionOption{ID: askUserOptionID(index), Label: option.Label, Description: option.Description})
	}
	return question
}

// SelectedLabels maps chosen option ids to their labels, in option order.
func (a *AskUserArgs) SelectedLabels(ids []string) []string {
	labels := []string{}
	for index, option := range a.Options {
		if slices.Contains(ids, askUserOptionID(index)) {
			labels = append(labels, option.Label)
		}
	}
	return labels
}

// ParseAskUserArgs strictly decodes and checks the arguments of an ask_user
// call: a trimmed question and, when given, distinct trimmed options.
func ParseAskUserArgs(raw json.RawMessage) (*AskUserArgs, error) {
	args := &AskUserArgs{}
	if err := DecodeControlArguments(raw, args); err != nil {
		return nil, err
	}
	question := strings.TrimSpace(args.Question)
	if question == "" {
		return nil, errors.New("question must not be empty")
	}
	if len(question) > MaximumAskUserQuestionBytes || !utf8.ValidString(question) {
		return nil, fmt.Errorf("question must be valid text of at most %d bytes", MaximumAskUserQuestionBytes)
	}
	args.Question = question
	if len(args.Options) == 0 {
		args.Options = nil
		if args.AllowMultiple || args.AllowOther {
			return nil, errors.New("allowMultiple and allowOther need options")
		}
		return args, nil
	}
	if len(args.Options) < MinimumAskUserOptions || len(args.Options) > MaximumAskUserOptions {
		return nil, fmt.Errorf("options must list %d to %d choices", MinimumAskUserOptions, MaximumAskUserOptions)
	}
	seen := make(map[string]struct{}, len(args.Options))
	for index := range args.Options {
		option := &args.Options[index]
		option.Label = strings.TrimSpace(option.Label)
		option.Description = strings.TrimSpace(option.Description)
		if err := ValidateChannelQuestionOptionText(option.Label, option.Description); err != nil {
			return nil, err
		}
		if _, exists := seen[option.Label]; exists {
			return nil, fmt.Errorf("option label %q is listed twice", option.Label)
		}
		seen[option.Label] = struct{}{}
	}
	return args, nil
}

// AskUserEntryArgs decodes an ask_user entry's arguments (nil when the entry
// is not a valid ask_user call).
func AskUserEntryArgs(entry *SessionEntry) *AskUserArgs {
	if entry == nil || entry.Type != SessionEntryTool || entry.Tool == nil || entry.Tool.Name != string(ControlToolAskUser) {
		return nil
	}
	args, err := ParseAskUserArgs(entry.Tool.Arguments)
	if err != nil {
		return nil
	}
	return args
}

// AskUserEntryQuestion is the posted text of an ask_user entry: its trimmed
// question argument ("" when the entry is not an ask_user call).
func AskUserEntryQuestion(entry *SessionEntry) string {
	if args := AskUserEntryArgs(entry); args != nil {
		return args.Question
	}
	return ""
}
