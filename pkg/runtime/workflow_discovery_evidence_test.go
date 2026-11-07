package runtime

import (
	"reflect"
	"strings"
	"testing"
)

func TestWorkflowDiscoverySkipsUnrelatedActionPayloadsAcrossStores(t *testing.T) {
	for _, kind := range []string{"memory", "sqlite"} {
		for _, declaresProof := range []bool{false, true} {
			name := kind + "/legacy-source"
			if declaresProof {
				name = kind + "/evidence-source"
			}
			t.Run(name, func(t *testing.T) {
				store := workflowEvidenceStore(t, kind)
				run, catalog, _, _ := workflowActionFixture(t, store, declaresProof)
				if declaresProof {
					run = persistWorkflowEvidenceCall(t, store, workflowEvidenceCall(run, "explicit-proof", "D-known"))
				}
				run.Checkpoint = map[string]interface{}{"previous": map[string]interface{}{"actionCallId": "large-browser-call", "bindingId": "browser-account", "action": "screenshot", "result": strings.Repeat("x", 1<<20)}}
				points := pointWorkflowEvidenceStore(store)
				sources, err := workflowSources(t.Context(), points, catalog, run)
				if err != nil || len(sources) != 1 || len(points.lookups) != 0 || points.scans != 0 || len(sources[0].KnownSubjects) != 0 {
					t.Fatalf("unrelated latest action fetched: sources=%#v reads=%v error=%v", sources, points.lookups, err)
				}
				// An explicit candidate may still load a full durable record; the
				// API bounds lookup count rather than underlying payload bytes.
				sources, err = workflowSources(t.Context(), points, catalog, run, "explicit-proof")
				if err != nil {
					t.Fatal(err)
				}
				if declaresProof {
					if !reflect.DeepEqual(points.lookups, []string{"explicit-proof"}) || len(sources[0].KnownSubjects) != 1 {
						t.Fatalf("explicit durable proof reads=%v subjects=%#v", points.lookups, sources[0].KnownSubjects)
					}
				} else if len(points.lookups) != 0 {
					t.Fatalf("source without proof declarations fetched receipt: %v", points.lookups)
				}
			})
		}
	}
}
