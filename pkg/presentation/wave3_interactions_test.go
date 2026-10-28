package presentation

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestInteractionWave3(t *testing.T) {
	poll := pollOf(false, "thai", "pizza", "pub")
	multiple := pollOf(true, "thai", "pizza", "pub")
	explorer := interest()
	for _, tc := range []struct {
		name      string
		component Component
		actionID  string
		rule      InteractionRule
		err       string
	}{
		{"vote", poll, PollVoteAction, InteractionRule{Kind: InteractionVote, Shared: true, WakeWhenEveryoneVoted: true}, ""},
		{"vote without action", poll, "", InteractionRule{}, `poll interactions take the action id "vote"`},
		{"share", explorer, ExplorerShareAction, InteractionRule{Kind: InteractionShare, NotifyAgent: true}, ""},
		{"share wrong action", explorer, "vote", InteractionRule{}, `explorer interactions take the action id "share"`},
		{"routine", routineOf(&Binding{Kind: BindingRunbook, ID: "a"}), "", InteractionRule{}, "does not accept interactions"},
		{"agenda action", agendaOf(), "vote", InteractionRule{}, "agenda interactions do not take an action id"},
	} {
		rule, err := Interaction(tc.component, tc.actionID)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("%s: error = %v, want %q", tc.name, err, tc.err)
			}
			continue
		}
		if err != nil || rule != tc.rule {
			t.Fatalf("%s: rule %+v, err %v", tc.name, rule, err)
		}
	}
	if err := ValidateInteractionItem(poll, PollVoteAction, "thai"); err == nil {
		t.Fatal("vote took an item key")
	}
	for _, tc := range []struct {
		component Component
		values    map[string]interface{}
		err       string
	}{
		{poll, map[string]interface{}{"optionIds": []interface{}{"thai"}}, ""},
		{poll, map[string]interface{}{"optionIds": []interface{}{"thai", "pizza"}}, "optionIds needs exactly one option id (got 2)"},
		{poll, map[string]interface{}{"optionIds": []interface{}{}}, "at least one option id"},
		{poll, map[string]interface{}{"optionIds": []interface{}{"sushi"}}, `poll has no option "sushi"`},
		{poll, map[string]interface{}{"optionIds": "thai"}, "optionIds must be a list of option ids"},
		{poll, map[string]interface{}{"selected": []interface{}{"thai"}}, `unexpected value "selected"`},
		{multiple, map[string]interface{}{"optionIds": []interface{}{"thai", "pub"}}, ""},
		{multiple, map[string]interface{}{"optionIds": []interface{}{"thai", "thai"}}, `option "thai" is chosen twice`},
		{explorer, map[string]interface{}{"inputs": map[string]interface{}{"deposit": 1000.0, "rate": 5.0, "years": 10.0}}, ""},
		{explorer, map[string]interface{}{"inputs": map[string]interface{}{"deposit": 1000.0, "rate": 5.0}}, "inputs.years is required"},
		{explorer, map[string]interface{}{"inputs": map[string]interface{}{"deposit": 1000.0, "rate": 5.0, "years": 10.0, "fee": 1.0}}, `explorer has no input "fee"`},
		{explorer, map[string]interface{}{"inputs": map[string]interface{}{"deposit": 50.0, "rate": 5.0, "years": 10.0}}, "inputs.deposit 50 is outside 100-20000"},
		{explorer, map[string]interface{}{"inputs": map[string]interface{}{"deposit": 1000.0, "rate": 5.2, "years": 10.0}}, "inputs.rate 5.2 is not on a step of 0.5"},
		{explorer, map[string]interface{}{"inputs": map[string]interface{}{"deposit": "1000", "rate": 5.0, "years": 10.0}}, "inputs.deposit must be a number"},
		{explorer, map[string]interface{}{"inputs": []interface{}{1.0}}, "inputs must map each input id to a number"},
		{explorer, map[string]interface{}{"outputs": map[string]interface{}{"balance": 1.0}}, `unexpected value "outputs"`},
	} {
		action := PollVoteAction
		if tc.component.Type == "explorer" {
			action = ExplorerShareAction
		}
		err := ValidateInteractionValues(tc.component, action, tc.values)
		if tc.err == "" && err != nil || tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)) {
			t.Fatalf("%s %v: error = %v, want %q", tc.component.Type, tc.values, err, tc.err)
		}
	}
}

func TestPollOpen(t *testing.T) {
	poll := pollOf(false, "a", "b")
	if err := PollOpen(poll, time.Now()); err != nil {
		t.Fatalf("open poll: %v", err)
	}
	poll.Poll.ClosesAt = "2026-11-01T18:00:00+01:00"
	closes := time.Date(2026, 11, 1, 17, 0, 0, 0, time.UTC)
	if err := PollOpen(poll, closes.Add(-time.Second)); err != nil {
		t.Fatalf("before closesAt: %v", err)
	}
	for _, at := range []time.Time{closes, closes.Add(time.Hour)} {
		if err := PollOpen(poll, at); !errors.Is(err, ErrPollClosed) || err.Error() != "This poll has closed" {
			t.Fatalf("at %v: %v", at, err)
		}
	}
	if err := PollOpen(agendaOf(), time.Now()); err == nil {
		t.Fatal("agenda is not a poll")
	}
}

func TestPollOutcome(t *testing.T) {
	poll := pollOf(true, "thai", "pizza", "pub")
	votes := map[string][]string{
		"user:1": {"thai"}, "user:2": {"thai", "pizza"}, "user:3": {"thai"}, "user:4": {"sushi"}, "user:5": {"pizza", "pizza"},
	}
	tally, err := PollOutcome(poll, votes, nil)
	if err != nil || tally.Counts["thai"] != 3 || tally.Counts["pizza"] != 2 || tally.Counts["pub"] != 0 || tally.Voters != 4 || tally.Participants != 0 || tally.EveryoneVoted {
		t.Fatalf("tally = %+v, %v", tally, err)
	}
	people := []string{"user:1", "user:2", "user:3", "user:4", "user:5"}
	if tally, _ := PollOutcome(poll, votes, people); tally.Participants != 5 || tally.EveryoneVoted {
		t.Fatalf("user:4 voted for no option: %+v", tally)
	}
	if EveryoneVoted(poll, votes, people) {
		t.Fatal("everyone voted with an invalid vote")
	}
	votes["user:4"] = []string{"pub"}
	if !EveryoneVoted(poll, votes, people) || EveryoneVoted(poll, votes, append(people, "user:6")) {
		t.Fatal("everyone voted is wrong")
	}
	if !EveryoneVoted(poll, map[string][]string{"user:1": {"pub"}}, []string{"user:1"}) {
		t.Fatal("in a one-to-one chat the first vote completes the poll")
	}
	if EveryoneVoted(poll, votes, nil) {
		t.Fatal("no participants means nobody to wait for, not everyone")
	}
	if _, err := PollOutcome(agendaOf(), votes, nil); err == nil {
		t.Fatal("agenda tallied")
	}
	if ids, ok := PollVote(json.RawMessage(`{"optionIds":["thai","pub"]}`)); !ok || strings.Join(ids, ",") != "thai,pub" {
		t.Fatalf("PollVote = %v %v", ids, ok)
	}
	if _, ok := PollVote(json.RawMessage(`{"optionIds":[]}`)); ok {
		t.Fatal("empty vote read")
	}
}

func TestPollTexts(t *testing.T) {
	poll := pollOf(false, "thai", "pizza", "pub")
	tally, _ := PollOutcome(poll, map[string][]string{"a": {"pizza"}, "b": {"thai"}, "c": {"thai"}, "d": {"thai"}}, []string{"a", "b", "c", "d", "e"})
	if line, ok := PollStateLine(poll, tally); !ok || line != `Poll "Dinner": Thai 3, Pizza 1, Pub 0 (4 of 5 voted)` {
		t.Fatalf("state line = %q", line)
	}
	tally.Participants = 0
	if line, _ := PollStateLine(poll, tally); line != `Poll "Dinner": Thai 3, Pizza 1, Pub 0 (4 voted)` {
		t.Fatalf("state line without participants = %q", line)
	}
	values := json.RawMessage(`{"optionIds":["thai"]}`)
	if text := CardResponseText(poll, PollVoteAction, "", values, nil); text != `Card response (poll "Dinner"): voted "Thai"` {
		t.Fatalf("vote text = %q", text)
	}
	done, _ := PollOutcome(poll, map[string][]string{"a": {"pizza"}, "b": {"thai"}, "c": {"thai"}, "d": {"thai"}}, []string{"a", "b", "c", "d"})
	outcome, _ := json.Marshal(done)
	if text := CardResponseText(poll, PollVoteAction, "", values, outcome); text != `Card response (poll "Dinner"): everyone voted — Thai 3, Pizza 1, Pub 0` {
		t.Fatalf("wake text = %q", text)
	}
	if _, ok := PollStateLine(agendaOf(), tally); ok {
		t.Fatal("agenda has a poll line")
	}
}

func TestExplorerShare(t *testing.T) {
	explorer := interest()
	explorer.Explorer.Outputs = append(explorer.Explorer.Outputs, ExplorerOutput{ID: "gain", Label: "Interest", Expr: "deposit * (1 + rate / 100) ^ years - deposit", Format: StatCurrency, Currency: "GBP", Digits: minutes(0)})
	outcome, err := ExplorerShare(explorer, map[string]float64{"deposit": 10000, "rate": 5, "years": 10})
	if err != nil {
		t.Fatal(err)
	}
	want := 10000 * math.Pow(1.05, 10)
	if math.Abs(outcome.Outputs["balance"]-want) > 1e-6 || math.Abs(outcome.Outputs["gain"]-(want-10000)) > 1e-6 || outcome.Inputs["years"] != 10 {
		t.Fatalf("outcome = %+v", outcome)
	}
	// The server recomputes; numbers sent by a client never reach the outcome.
	encoded, _ := json.Marshal(outcome)
	text := CardResponseText(explorer, ExplorerShareAction, "", json.RawMessage(`{"inputs":{"deposit":10000,"rate":5,"years":10}}`), encoded)
	if text != `Card response (explorer "Savings"): deposit 10,000, rate 5, years 10 → Final balance 16,288.95, Interest GBP 6,289` {
		t.Fatalf("share text = %q", text)
	}
	inputs, err := ExplorerShareInputs(json.RawMessage(`{"inputs":{"deposit":500,"rate":0.5,"years":1}}`))
	if err != nil || inputs["deposit"] != 500 {
		t.Fatalf("inputs = %v, %v", inputs, err)
	}
	if _, err := ExplorerShareInputs(json.RawMessage(`{"inputs":{"deposit":500},"outputs":{}}`)); err == nil {
		t.Fatal("outputs accepted from the client")
	}
	if _, err := ExplorerShare(explorer, map[string]float64{"deposit": 10000, "rate": 5, "years": math.Inf(1)}); err == nil {
		t.Fatal("infinite input accepted")
	}
	// An output that fails at the shared inputs is an error, not a number.
	divide := explorerOf([]ExplorerInput{slider("x", 0, 5, 1, 1)}, output("a", "1 / x"))
	if _, err := ExplorerShare(divide, map[string]float64{"x": 0}); err == nil || !strings.Contains(err.Error(), `output "a" cannot be worked out`) {
		t.Fatalf("division by zero: %v", err)
	}
	toggle := explorerOf([]ExplorerInput{slider("price", 1, 10, 1, 5), {ID: "member", Label: "Member", Kind: "toggle", Default: num(0)}}, output("total", "if(member, price * 0.5, price)"))
	shared, err := ExplorerShare(toggle, map[string]float64{"price": 8, "member": 1})
	if err != nil || shared.Outputs["total"] != 4 {
		t.Fatalf("toggle share = %+v, %v", shared, err)
	}
	if _, err := ExplorerShare(toggle, map[string]float64{"price": 8, "member": 0.5}); err == nil || !strings.Contains(err.Error(), "inputs.member must be 0 or 1") {
		t.Fatalf("half a toggle: %v", err)
	}
	encoded, _ = json.Marshal(shared)
	if text := CardResponseText(toggle, ExplorerShareAction, "", nil, encoded); text != `Card response (explorer "Savings"): price 8, member on → Total 4.00` {
		t.Fatalf("toggle text = %q", text)
	}
	if _, err := ExplorerShare(pollOf(false, "a", "b"), nil); err == nil {
		t.Fatal("poll shared")
	}
}
