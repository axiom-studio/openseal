package capability

import "testing"

func TestSkillIdentityEqualityIncludesSourceProvenance(t *testing.T) {
	selected := NewSkillIdentity(" summarize ", " 1.0.0+source.abc ", " clawhub::@alice/summarize ")
	same := NewSkillIdentity("summarize", "1.0.0+source.abc", "clawhub::@alice/summarize")
	foreign := NewSkillIdentity("summarize", "1.0.0+source.abc", "clawhub::@bob/summarize")
	if !selected.Equal(same) || selected.Key() != same.Key() {
		t.Fatalf("canonical identities differ: %#v %#v", selected, same)
	}
	if selected.Equal(foreign) || selected.Key() == foreign.Key() {
		t.Fatal("source variants must not share authority")
	}
	if NewSkillIdentity("", "1", "").Valid() || NewSkillIdentity("skill", "", "").Valid() {
		t.Fatal("incomplete identity should be invalid")
	}
}
