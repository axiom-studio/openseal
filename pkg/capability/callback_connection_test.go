package capability

import (
	"strings"
	"testing"
)

func TestNormalizeCallbackAdapterManagedConnectionOnly(t *testing.T) {
	for _, kind := range []string{"polling", "websocket"} {
		t.Run(kind, func(t *testing.T) {
			adapter := managedCallbackAdapter(kind)
			adapter.Name = " Telegram updates "
			adapter.Transport.Connection.Kind = " " + kind + " "
			adapter.Transport.Connection.Endpoint = " telegram.callback.polling "
			adapter.Transport.Connection.Credentials = []string{" bot_token "}
			normalized, err := NormalizeCallbackAdapter(adapter)
			if err != nil {
				t.Fatal(err)
			}
			if normalized.Name != "Telegram updates" || len(normalized.EventTypes) != 0 ||
				normalized.Transport.Connection.Kind != kind || normalized.Transport.Connection.Endpoint != "telegram.callback.polling" ||
				strings.Join(normalized.Transport.Connection.Credentials, ",") != "bot_token" {
				t.Fatalf("normalized connection-only adapter = %#v", normalized)
			}
		})
	}
}

func TestNormalizeCallbackAdapterRejectsInvalidConnectionOnly(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CallbackAdapter)
	}{
		{"no managed connection", func(a *CallbackAdapter) { a.Transport.Connection = nil }},
		{"unsupported connection", func(a *CallbackAdapter) { a.Transport.Connection.Kind = "stdin" }},
		{"no endpoint", func(a *CallbackAdapter) { a.Transport.Connection.Endpoint = "" }},
		{"no projected credentials", func(a *CallbackAdapter) { a.Transport.Connection.Credentials = nil }},
		{"undeclared credential", func(a *CallbackAdapter) { a.Transport.Connection.Credentials = []string{"other"} }},
		{"unprojected shared credential", func(a *CallbackAdapter) { a.Transport.Connection.SharedByCredential = "other" }},
		{"unused credential", func(a *CallbackAdapter) {
			a.Credentials = append(a.Credentials, CredentialRequirement{Name: "extra", Kind: "telegram_bot_token"})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter := managedCallbackAdapter("polling")
			test.mutate(&adapter)
			if _, err := NormalizeCallbackAdapter(adapter); err == nil {
				t.Fatal("invalid connection-only adapter was accepted")
			}
		})
	}
}

func managedCallbackAdapter(kind string) CallbackAdapter {
	return CallbackAdapter{
		ProtocolVersion: CallbackAdapterProtocolV1, Name: "Telegram updates",
		Description: "Receive Telegram updates through a signed conversation gateway.", Provider: "telegram",
		EventTypes: []string{}, Credentials: []CredentialRequirement{{Name: "bot_token", Kind: "telegram_bot_token"}},
		Transport: CallbackAdapterTransport{
			Kind: "http", IngressEndpoint: "telegram.callback.ingress",
			Connection: &CallbackAdapterConnectionTransport{
				Kind: kind, Endpoint: "telegram.callback.polling", Credentials: []string{"bot_token"}, SharedByCredential: "bot_token",
			},
		},
	}
}
