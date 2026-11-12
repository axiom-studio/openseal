package runtime

// teamCoordinationPaths are the JSON locations of the retired coordination
// policy (speaker caps and participation flags) in every durable artifact that
// can carry a Team definition. Team channels now route each message to the
// members it addresses, or to one member a router picks.
var teamCoordinationPaths = []struct{ table, postgres, sqlite string }{
	{"team_definitions", "{coordination}", "$.coordination"},
	{"team_definition_amendments", "{candidate,coordination}", "$.candidate.coordination"},
}
