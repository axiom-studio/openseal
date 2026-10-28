package presentation

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ErrPollClosed rejects a vote after the poll's closesAt. Hosts answer it
// with 422 "This poll has closed".
var ErrPollClosed = errors.New("This poll has closed")

// PollOpen returns ErrPollClosed when component is a poll whose closesAt is
// at or before now. Hosts call it before storing a vote.
func PollOpen(component Component, now time.Time) error {
	if component.Type != "poll" || component.Poll == nil {
		return fmt.Errorf("component %q is not a poll", component.ID)
	}
	if component.Poll.ClosesAt == "" {
		return nil
	}
	closes, err := time.Parse(time.RFC3339, component.Poll.ClosesAt)
	if err != nil {
		return fmt.Errorf("poll %q has an invalid closesAt", component.ID)
	}
	if !now.Before(closes) {
		return ErrPollClosed
	}
	return nil
}

func stringList(raw interface{}, key string) ([]string, error) {
	var list []string
	switch values := raw.(type) {
	case []string:
		list = append(list, values...)
	case []interface{}:
		for _, value := range values {
			id, ok := value.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of option ids", key)
			}
			list = append(list, id)
		}
	default:
		return nil, fmt.Errorf("%s must be a list of option ids", key)
	}
	return list, nil
}

func validatePollVote(poll *Poll, ids []string) error {
	if poll == nil {
		return errors.New("poll payload is required")
	}
	if len(ids) == 0 {
		return errors.New("optionIds needs at least one option id")
	}
	if !poll.Multiple && len(ids) != 1 {
		return fmt.Errorf("optionIds needs exactly one option id (got %d)", len(ids))
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return fmt.Errorf("option %q is chosen twice", id)
		}
		seen[id] = true
		if _, ok := poll.optionLabel(id); !ok {
			return fmt.Errorf("poll has no option %q", id)
		}
	}
	return nil
}

func (p *Poll) optionLabel(id string) (string, bool) {
	for _, option := range p.Options {
		if option.ID == id {
			return option.Label, true
		}
	}
	return "", false
}

// PollVote reads the option ids of stored vote values ({"optionIds": [...]}).
func PollVote(values json.RawMessage) ([]string, bool) {
	var decoded struct {
		OptionIDs []string `json:"optionIds"`
	}
	if json.Unmarshal(values, &decoded) != nil || len(decoded.OptionIDs) == 0 {
		return nil, false
	}
	return decoded.OptionIDs, true
}

// PollTally counts the latest vote of each person on a poll.
type PollTally struct {
	// Counts maps every option id to its number of votes.
	Counts map[string]int `json:"counts"`
	// Voters is how many people have a vote.
	Voters int `json:"voters"`
	// Participants is how many people can vote, when the host knows them.
	Participants int `json:"participants,omitempty"`
	// EveryoneVoted is set when every participant has a vote.
	EveryoneVoted bool `json:"everyoneVoted,omitempty"`
}

// PollOutcome tallies a poll. votes maps a host-chosen key per person (e.g.
// "user:23") to that person's latest vote (read with PollVote from
// ListLatestSurfaceInteractions without AcrossActors, filtered to the
// component and action id "vote"). Option ids that are not options of the
// poll are ignored, and a person left with none is not counted. participants
// are the keys of the conversation's human participants; when given, the
// tally records how many there are and whether every one has voted. Hosts
// store the tally as the outcome of the vote that completed it.
func PollOutcome(component Component, votes map[string][]string, participants []string) (PollTally, error) {
	if component.Type != "poll" || component.Poll == nil {
		return PollTally{}, fmt.Errorf("component %q is not a poll", component.ID)
	}
	tally := PollTally{Counts: make(map[string]int, len(component.Poll.Options))}
	for _, option := range component.Poll.Options {
		tally.Counts[option.ID] = 0
	}
	voted := map[string]bool{}
	for voter, ids := range votes {
		seen := map[string]bool{}
		for _, id := range ids {
			if _, ok := tally.Counts[id]; ok && !seen[id] {
				seen[id] = true
				tally.Counts[id]++
				voted[voter] = true
			}
		}
	}
	tally.Voters = len(voted)
	if len(participants) > 0 {
		tally.Participants = len(participants)
		tally.EveryoneVoted = true
		for _, participant := range participants {
			tally.EveryoneVoted = tally.EveryoneVoted && voted[participant]
		}
	}
	return tally, nil
}

// EveryoneVoted reports whether every one of participants (the keys of the
// conversation's human participants, as in votes) has a vote on the poll.
// The host wakes the agent the first time it turns true for a visual
// version, with CardResponseText over the completing vote and its tally.
// In a one-to-one chat the single user's first vote completes it.
func EveryoneVoted(component Component, votes map[string][]string, participants []string) bool {
	tally, err := PollOutcome(component, votes, participants)
	return err == nil && tally.EveryoneVoted
}

// tallyText lists options by votes, most first, e.g. "Thai 3, Pizza 1".
func (p *Poll) tallyText(tally PollTally) string {
	options := append([]PollOption(nil), p.Options...)
	sort.SliceStable(options, func(i, j int) bool { return tally.Counts[options[i].ID] > tally.Counts[options[j].ID] })
	parts := make([]string, len(options))
	for index, option := range options {
		parts[index] = fmt.Sprintf("%s %d", option.Label, tally.Counts[option.ID])
	}
	return strings.Join(parts, ", ")
}

// PollStateLine summarises a poll for the agent's card state, e.g.
// `Poll "Dinner": Thai 3, Pizza 1 (4 of 5 voted)`, or "(4 voted)" when the
// tally does not know the participants.
func PollStateLine(component Component, tally PollTally) (string, bool) {
	if component.Type != "poll" || component.Poll == nil {
		return "", false
	}
	voted := fmt.Sprintf("%d voted", tally.Voters)
	if tally.Participants > 0 {
		voted = fmt.Sprintf("%d of %d voted", tally.Voters, tally.Participants)
	}
	return fmt.Sprintf("Poll %q: %s (%s)", componentName(component), component.Poll.tallyText(tally), voted), true
}

func componentName(component Component) string {
	if name := strings.TrimSpace(component.Title); name != "" {
		return name
	}
	return component.ID
}

// pollResponseText is "everyone voted — Thai 3, Pizza 1" for the vote that
// completed the poll (its outcome is a tally with everyoneVoted), else
// `voted "Thai"`.
func pollResponseText(component Component, values, outcome json.RawMessage) (string, bool) {
	if component.Poll == nil {
		return "", false
	}
	var tally PollTally
	if len(outcome) > 0 && json.Unmarshal(outcome, &tally) == nil && tally.EveryoneVoted {
		return "everyone voted — " + component.Poll.tallyText(tally), true
	}
	ids, ok := PollVote(values)
	if !ok {
		return "", false
	}
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		if label, found := component.Poll.optionLabel(id); found {
			labels = append(labels, label)
		}
	}
	if len(labels) == 0 {
		return "", false
	}
	return "voted " + quoteAll(labels), true
}

// ExplorerShareOutcome is what a host stores for an explorer share: the
// inputs as sent and the outputs the server worked out from them.
type ExplorerShareOutcome struct {
	Inputs  map[string]float64 `json:"inputs"`
	Outputs map[string]float64 `json:"outputs"`
}

// ExplorerShare checks shared inputs against an explorer component (every
// input given once, within its range and on a step, toggles 0 or 1) and
// recomputes every output with the server's evaluator. Hosts store the
// result as the share's outcome and never trust numbers from the client.
func ExplorerShare(component Component, inputs map[string]float64) (ExplorerShareOutcome, error) {
	if component.Type != "explorer" || component.Explorer == nil {
		return ExplorerShareOutcome{}, fmt.Errorf("component %q is not an explorer", component.ID)
	}
	explorer := component.Explorer
	declared := map[string]ExplorerInput{}
	for _, input := range explorer.Inputs {
		declared[input.ID] = input
	}
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, ok := declared[name]; !ok {
			return ExplorerShareOutcome{}, fmt.Errorf("explorer has no input %q", name)
		}
	}
	env := make(map[string]float64, len(explorer.Inputs))
	for _, input := range explorer.Inputs {
		value, ok := inputs[input.ID]
		if !ok {
			return ExplorerShareOutcome{}, fmt.Errorf("inputs.%s is required", input.ID)
		}
		if err := explorerValue(input, value); err != nil {
			return ExplorerShareOutcome{}, fmt.Errorf("inputs.%s %v", input.ID, err)
		}
		env[input.ID] = value
	}
	types := explorerTypes(explorer.Inputs)
	outcome := ExplorerShareOutcome{Inputs: env, Outputs: make(map[string]float64, len(explorer.Outputs))}
	for _, output := range explorer.Outputs {
		program, err := compileExplorerExpr("outputs."+output.ID, output.Expr, types)
		if err != nil {
			return ExplorerShareOutcome{}, err
		}
		value, err := program.Eval(env)
		if err != nil {
			return ExplorerShareOutcome{}, fmt.Errorf("output %q cannot be worked out for these inputs: %v", output.ID, err)
		}
		outcome.Outputs[output.ID] = value
	}
	return outcome, nil
}

// ExplorerShareInputs reads the inputs of stored share values
// ({"inputs": {...}}) for ExplorerShare.
func ExplorerShareInputs(values json.RawMessage) (map[string]float64, error) {
	var decoded map[string]interface{}
	if err := json.Unmarshal(values, &decoded); err != nil {
		return nil, errors.New("values must be an object")
	}
	if err := onlyKeys(decoded, "inputs"); err != nil {
		return nil, err
	}
	return explorerInputValues(decoded["inputs"])
}

func explorerInputValues(raw interface{}) (map[string]float64, error) {
	switch inputs := raw.(type) {
	case map[string]float64:
		return inputs, nil
	case map[string]interface{}:
		values := make(map[string]float64, len(inputs))
		for name, value := range inputs {
			number, ok := value.(float64)
			if !ok {
				return nil, fmt.Errorf("inputs.%s must be a number", name)
			}
			values[name] = number
		}
		return values, nil
	}
	return nil, errors.New("inputs must map each input id to a number")
}

// explorerResponseText is "years 10, rate 5 → Final balance 16,470.09".
func explorerResponseText(component Component, outcome json.RawMessage) (string, bool) {
	var shared ExplorerShareOutcome
	if component.Explorer == nil || len(outcome) == 0 || json.Unmarshal(outcome, &shared) != nil || len(shared.Outputs) == 0 {
		return "", false
	}
	var inputs, outputs []string
	for _, input := range component.Explorer.Inputs {
		value, ok := shared.Inputs[input.ID]
		if !ok {
			continue
		}
		text := formatAmount(value, input.Format, input.Currency, "", -1)
		if input.Kind == "toggle" {
			text = "off"
			if value != 0 {
				text = "on"
			}
		}
		inputs = append(inputs, input.ID+" "+text)
	}
	for _, output := range component.Explorer.Outputs {
		value, ok := shared.Outputs[output.ID]
		if !ok {
			continue
		}
		digits := 2
		if output.Digits != nil {
			digits = *output.Digits
		}
		outputs = append(outputs, output.Label+" "+formatAmount(value, output.Format, output.Currency, output.Unit, digits))
	}
	return strings.Join(inputs, ", ") + " → " + strings.Join(outputs, ", "), true
}

// formatAmount writes a number for the agent: grouped thousands, digits
// decimals (-1 for as many as needed), a currency code before it, % or the
// unit after it.
func formatAmount(value float64, format StatFormat, currency, unit string, digits int) string {
	text := groupedNumber(value, digits)
	switch format {
	case StatCurrency:
		text = currency + " " + text
	case StatPercent:
		text += "%"
	}
	if unit != "" {
		text += " " + unit
	}
	return text
}

func groupedNumber(value float64, digits int) string {
	text := strconv.FormatFloat(value, 'f', digits, 64)
	if digits < 0 {
		text = formatNumber(value)
	}
	if strings.ContainsAny(text, "eE") {
		return text
	}
	sign := ""
	if strings.HasPrefix(text, "-") {
		sign, text = "-", text[1:]
	}
	whole, fraction := text, ""
	if dot := strings.IndexByte(text, '.'); dot >= 0 {
		whole, fraction = text[:dot], text[dot:]
	}
	var grouped strings.Builder
	for index, digit := range whole {
		if index > 0 && (len(whole)-index)%3 == 0 {
			grouped.WriteByte(',')
		}
		grouped.WriteRune(digit)
	}
	return sign + grouped.String() + fraction
}
