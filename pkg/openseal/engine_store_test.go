package openseal

import (
	"testing"
)

func TestDefaultStoreServesRunAuditsAndSkillSources(t *testing.T) {
	engine, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { engine.Close() })
	if engine.runbookAudits == nil {
		t.Fatal("default engine has no Run execution audit")
	}
	if _, err := engine.SkillSourceContentProvider(); err != nil {
		t.Fatalf("default engine has no skill source store: %v", err)
	}
}
