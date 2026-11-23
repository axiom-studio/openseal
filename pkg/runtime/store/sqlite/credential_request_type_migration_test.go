package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/axiom-studio/openseal/pkg/models"
)

func TestSQLiteCredentialRequestKindBecomesCredentialType(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requests.db")
	store, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	db := openTestSQLiteDB(t, path)
	for id, kind := range map[string]string{"login": "website_login", "card": "payment_card"} {
		payload := `{"id":"` + id + `","scope":{"kind":"tenant","id":"a"},"owner":{"type":"agent","id":"agent"},"kind":"` + kind + `","reason":"Add it","status":"pending","revision":1}`
		if _, err := db.Exec(`INSERT INTO credential_requests(scope_kind,scope_id,id,deployment_id,conversation_id,status,revision,created_at,payload) VALUES('tenant','a',?,'agent','chat','pending',1,CURRENT_TIMESTAMP,?)`, id, payload); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if store, err = New(path); err != nil {
			t.Fatal(err)
		}
		for id, want := range map[string]string{"login": "http_basic_auth", "card": "payment_card"} {
			r, err := store.GetCredentialRequest(context.Background(), models.Scope{Kind: "tenant", ID: "a"}, id)
			if err != nil || r == nil || r.CredentialType != want {
				t.Fatalf("%s: %#v %v", id, r, err)
			}
			var kinds int
			if err := db.QueryRow(`SELECT COUNT(*) FROM credential_requests WHERE json_type(payload, '$.kind') IS NOT NULL`).Scan(&kinds); err != nil || kinds != 0 {
				t.Fatalf("kind left on %d rows: %v", kinds, err)
			}
		}
	}
	_ = store.Close()
}
