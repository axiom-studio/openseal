package skillgrpc

import (
	"context"
	"errors"
	"testing"

	"github.com/axiom-studio/skills.sdk/executor"
	skillpb "github.com/axiom-studio/skills.sdk/grpc/skillpb"
	"google.golang.org/grpc"
)

type actionFailureSkillClient struct {
	captureSkillClient
	failure *skillpb.Error
}

func (c *actionFailureSkillClient) Execute(context.Context, *skillpb.ExecuteRequest, ...grpc.CallOption) (*skillpb.ExecuteResponse, error) {
	return &skillpb.ExecuteResponse{Error: c.failure}, nil
}

func TestClientPreservesTypedSourceFailureAndLegacyUnknownError(t *testing.T) {
	for _, known := range []bool{true, false} {
		code := "source_rate_limited"
		if !known {
			code = "legacy_error"
		}
		client := &Client{client: &actionFailureSkillClient{failure: &skillpb.Error{Type: code, Message: "legacy message", Details: map[string]string{"httpStatus": "429", "retryAfterSeconds": "12", "retryable": "true", "url": "SECRET"}}}}
		result, err := client.ExecuteWithContext(t.Context(), &executor.StepDefinition{Id: "node", Type: "browser.read", Config: map[string]interface{}{}}, &contextResolver{}, ExecutionContext{})
		var typed *ActionError
		if result != nil || err == nil || errors.As(err, &typed) != known {
			t.Fatalf("typed boundary mismatch: %#v %v", result, err)
		}
		if known {
			if typed.Code() != code || typed.Details()["retryAfterSeconds"] != "12" || typed.Details()["retryable"] != "false" || typed.Details()["url"] != "" || err.Error() != "This source returned HTTP 429 (rate limited). Try again later." {
				t.Fatalf("typed failure changed: %#v", typed)
			}
		} else if err.Error() != "legacy_error: legacy message" {
			t.Fatalf("non-browser handling changed: %v", err)
		}
	}
}
