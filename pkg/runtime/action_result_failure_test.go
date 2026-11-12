package runtime

import "testing"

func TestExplicitActionFailureEnvelopePreservesResourceData(t *testing.T) {
	for _, output := range []map[string]interface{}{
		{"success": false, "error": "the connection failed"}, {"isError": true}, {"error": "The operation failed."}, {"success": false},
	} {
		if explicitActionResultFailure(output) == nil {
			t.Fatalf("explicit failure not recognized: %#v", output)
		}
	}
	for _, output := range []map[string]interface{}{
		{"status": "failed"}, {"ok": false}, {"success": true, "error": "historic diagnostic"}, {"result": map[string]interface{}{"success": false, "error": "document content"}},
	} {
		if err := explicitActionResultFailure(output); err != nil {
			t.Fatalf("fetched data was mistaken for operation failure: %#v %v", output, err)
		}
	}
}
