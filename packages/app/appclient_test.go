package appclient

import "testing"

func TestEntriesResolveToEmbeddedSources(t *testing.T) {
	for module, file := range Entries {
		if source, ok := Source(file); !ok || source == "" {
			t.Fatalf("%s: %s is not embedded", module, file)
		}
	}
	if _, ok := Source("src/client.test.ts"); ok {
		t.Fatal("tests must not be served as app code")
	}
	if _, ok := Source("../../go.mod"); ok {
		t.Fatal("only package sources may be read")
	}
}
