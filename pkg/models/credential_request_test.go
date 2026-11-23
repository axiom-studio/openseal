package models

import (
	"testing"
	"time"
)

func TestNormalizeCredentialRequestWebsite(t *testing.T) {
	for input, want := range map[string]string{
		"https://www.amazon.in":      "https://www.amazon.in",
		"https://WWW.Amazon.in/":     "https://www.amazon.in",
		"https://shop.example:443":   "https://shop.example",
		"http://localhost:8080":      "http://localhost:8080",
		" https://www.flipkart.com ": "https://www.flipkart.com",
	} {
		if got, err := NormalizeCredentialRequestWebsite(input); err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", input, got, err, want)
		}
	}
	for _, input := range []string{"", "amazon.in", "ftp://amazon.in", "https://amazon.in/ap/signin", "https://amazon.in?x=1", "https://user:pw@amazon.in"} {
		if _, err := NormalizeCredentialRequestWebsite(input); err == nil {
			t.Errorf("%q accepted", input)
		}
	}
}

func TestCredentialRequestValidateFieldsAndOutcome(t *testing.T) {
	now := time.Now().UTC()
	valid := func() *CredentialRequest {
		return &CredentialRequest{ID: "credential-request:call", Scope: Scope{Kind: "tenant", ID: "a"}, Owner: ObjectiveOwner{Type: OwnerTypeAgent, ID: "agent"},
			DeploymentID: "agent", AgentID: "agent", ConversationID: "chat", TriggerMessageID: "message", RunID: "run", ActionCallID: "call",
			CredentialType: "generic", Service: "Alpaca", Fields: []CredentialRequestField{{Name: "API Key ID", Secure: true}, {Name: "Secret Key", Secure: true}},
			Reason: "Add your Alpaca keys", Status: CredentialRequestStatusPending, Revision: 1, CreatedAt: now, UpdatedAt: now}
	}
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*CredentialRequest){
		"no type":            func(r *CredentialRequest) { r.CredentialType = "" },
		"untrimmed service":  func(r *CredentialRequest) { r.Service = " Alpaca" },
		"page url":           func(r *CredentialRequest) { r.Website = "https://www.amazon.in/ap/signin" },
		"repeated field":     func(r *CredentialRequest) { r.Fields = append(r.Fields, CredentialRequestField{Name: "secret key"}) },
		"empty field":        func(r *CredentialRequest) { r.Fields = []CredentialRequestField{{Name: ""}} },
		"pending credential": func(r *CredentialRequest) { r.Credential = &CredentialRequestCredential{ID: "1", Name: "Alpaca keys"} },
		"resolved without credential": func(r *CredentialRequest) {
			r.Status, r.ResolvedBy = CredentialRequestStatusResolved, "user:5"
		},
		"dismissed with credential": func(r *CredentialRequest) {
			r.Status, r.ResolvedBy, r.Credential = CredentialRequestStatusDismissed, "user:5", &CredentialRequestCredential{ID: "1", Name: "Alpaca keys"}
		},
	} {
		r := valid()
		change(r)
		if r.Validate() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}
