package runtime

import "testing"

func TestNamedAtStartAddressesAMemberByName(t *testing.T) {
	roster := conversationTeamRoster{members: map[string]conversationTeamMember{
		"cove": {name: "Cove"}, "miso": {name: "Miso"}, "coven": {name: "Coven"},
	}}
	cases := map[string]string{
		"Cove can you use miso's help after you find out the details?": "cove",
		"hey Miso, draft the blog":                                     "miso",
		"@cove look at this":                                           "cove",
		"Coven, you there?":                                            "coven",
		"Covering the launch, who's on it?":                            "",
		"Can Miso and Cove both look?":                                 "",
	}
	for content, want := range cases {
		got, ok := namedAtStart(content, roster)
		if got != want || ok != (want != "") {
			t.Errorf("namedAtStart(%q) = %q, %v; want %q", content, got, ok, want)
		}
	}
}
