package hostadapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/axiom-studio/openseal/pkg/delivery"
)

func TestEmailDeliveryExecutorBuildsVerifiedMIMEAndReturnsCredentialFreeReceipt(t *testing.T) {
	pdf := []byte("%PDF-1.4\nverified")
	digest := sha256.Sum256(pdf)
	var captured emailDeliveryEnvelope
	executor := &EmailDeliveryExecutor{
		now: func() time.Time { return time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC) },
		send: func(_ context.Context, envelope emailDeliveryEnvelope) error {
			captured = envelope
			return nil
		},
	}
	result, err := executor.Execute(context.Background(), &StepDefinition{Config: map[string]interface{}{
		delivery.EmailCredentialName:      `{"provider":"smtp","host":"mailpit.axiomcd.svc","port":1025,"from":"agents@axiom.local","tls":"none"}`,
		delivery.ActionCallIDTransportKey: "action-1",
		"to":                              []interface{}{"research@example.com"},
		"subject":                         "Market research report",
		"body":                            "The reviewed cited report is attached.",
		delivery.AttachmentsTransportKey: []interface{}{map[string]interface{}{
			"id": "pdf-report", "version": 1, "filename": "research.pdf", "mediaType": "application/pdf",
			"digest": "sha256:" + hex.EncodeToString(digest[:]), "sizeBytes": len(pdf), "dataBase64": base64.StdEncoding.EncodeToString(pdf),
		}},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if captured.ActionCallID != "action-1" || captured.Credential.Password != "" || len(captured.Recipients) != 1 {
		t.Fatalf("captured envelope=%#v", captured)
	}
	message := string(captured.Message)
	for _, expected := range []string{"Message-ID: <openseal-", "Market research report", "research.pdf", base64.StdEncoding.EncodeToString(pdf)} {
		if !strings.Contains(message, expected) {
			t.Fatalf("MIME message missing %q:\n%s", expected, message)
		}
	}
	if strings.Contains(message, delivery.EmailCredentialName) {
		t.Fatal("MIME message exposed credential metadata")
	}
	if result.Output["receiptId"] != "smtp:action-1" || result.Output["status"] != "accepted" || result.Output["recipientCount"] != 1 || result.Output["deliveredAt"] != "2026-07-13T01:02:03Z" {
		t.Fatalf("receipt=%#v", result.Output)
	}
	refs := result.Output["artifactRefs"].([]interface{})
	if len(refs) != 1 || refs[0].(map[string]interface{})["id"] != "pdf-report" {
		t.Fatalf("artifactRefs=%#v", refs)
	}
}

func TestEmailDeliveryExecutorFailsClosedOnCredentialAndAttachmentDrift(t *testing.T) {
	executor := &EmailDeliveryExecutor{now: time.Now, send: func(context.Context, emailDeliveryEnvelope) error { return nil }}
	base := map[string]interface{}{
		delivery.EmailCredentialName:      `{"provider":"smtp","host":"smtp.example.com","port":25,"from":"agents@example.com","tls":"none"}`,
		delivery.ActionCallIDTransportKey: "action-1", "to": []interface{}{"research@example.com"}, "subject": "Research", "body": "Report",
		delivery.AttachmentsTransportKey: []interface{}{},
	}
	if _, err := executor.Execute(context.Background(), &StepDefinition{Config: base}, nil); err == nil || !strings.Contains(err.Error(), "unencrypted SMTP") {
		t.Fatalf("public plaintext SMTP error=%v", err)
	}
	base[delivery.EmailCredentialName] = `{"provider":"smtp","host":"mailpit.axiomcd.svc","port":1025,"from":"agents@axiom.local","tls":"none"}`
	base[delivery.AttachmentsTransportKey] = []interface{}{map[string]interface{}{"id": "pdf", "version": 1, "filename": "report.pdf", "mediaType": "application/pdf", "digest": "sha256:bad", "sizeBytes": 3, "dataBase64": "AAAA"}}
	if _, err := executor.Execute(context.Background(), &StepDefinition{Config: base}, nil); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("attachment drift error=%v", err)
	}
}
