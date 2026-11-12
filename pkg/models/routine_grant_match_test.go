package models

import "testing"

func TestRoutineDestinationsPinned(t *testing.T) {
	keys := []string{"channel", "channels"}
	pinned := []string{"ai-news", "C1"}
	for name, tc := range map[string]struct {
		arguments map[string]interface{}
		want      bool
	}{
		"name":          {map[string]interface{}{"channel": "#ai-news"}, true},
		"id":            {map[string]interface{}{"channel": " C1 "}, true},
		"list":          {map[string]interface{}{"channels": []interface{}{"C1", "ai-news"}}, true},
		"list with one": {map[string]interface{}{"channels": []interface{}{"C1", "C2"}}, false},
		"both keys":     {map[string]interface{}{"channel": "C1", "channels": []interface{}{"C9"}}, false},
		"non string":    {map[string]interface{}{"channel": 7}, false},
		"object":        {map[string]interface{}{"channel": map[string]interface{}{"id": "C1"}}, false},
		"none":          {map[string]interface{}{"message": "hi"}, false},
		"blank":         {map[string]interface{}{"channel": " "}, false},
	} {
		if got := routineDestinationsPinned(tc.arguments, keys, pinned); got != tc.want {
			t.Fatalf("%s: got %v", name, got)
		}
	}
}

func TestRoutineRecipientsOwned(t *testing.T) {
	owned := []string{"kev@example.com"}
	for name, tc := range map[string]struct {
		arguments map[string]interface{}
		want      bool
	}{
		"to string":         {map[string]interface{}{"to": "kev@example.com"}, true},
		"array":             {map[string]interface{}{"recipients": []interface{}{"kev@example.com"}}, true},
		"object recipients": {map[string]interface{}{"to": []interface{}{map[string]interface{}{"email": "kev@example.com", "name": "Kev"}}}, true},
		"nested other":      {map[string]interface{}{"message": map[string]interface{}{"to": "kev@example.com", "bcc": "eve@example.com"}}, false},
		"unknown subfield":  {map[string]interface{}{"to": map[string]interface{}{"id": "u123"}}, false},
		"non string":        {map[string]interface{}{"to": 7}, false},
		"invalid":           {map[string]interface{}{"to": "not an address"}, false},
		"none":              {map[string]interface{}{"body": "hi"}, false},
	} {
		if got := routineRecipientsOwned(tc.arguments, owned); got != tc.want {
			t.Fatalf("%s: got %v", name, got)
		}
	}
}
