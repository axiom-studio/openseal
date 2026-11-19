package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// The server re-signs every request it receives with the same key and the
// headers the client signed; the signatures must match on each request.
func TestAWSSigV4SignsEveryRequest(t *testing.T) {
	credentials := aws.Credentials{AccessKeyID: "AKIDEXAMPLE", SecretAccessKey: "secret", SessionToken: "token"}
	signed := 0
	server := mcpServer(t, func(*http.Request, string, map[string]interface{}) interface{} {
		return map[string]interface{}{"tools": []interface{}{}}
	})
	// Verify before the MCP handler consumes the body.
	inner := server.Config.Handler
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/") || !strings.Contains(auth, "/us-east-1/aws-mcp/aws4_request") {
			t.Fatalf("Authorization = %q", auth)
		}
		if r.Header.Get("X-Amz-Security-Token") != "token" {
			t.Errorf("session token not sent")
		}
		if want := resign(t, r, credentials); auth != want {
			t.Errorf("%s signature does not verify:\n got %s\nwant %s", r.Method, auth, want)
		}
		signed++
		inner.ServeHTTP(w, r)
	})
	config := pin(server)
	config.Auth = Auth{Kind: AuthSigned, Signing: &Signing{Scheme: SigningAWSSigV4, Params: map[string]string{"region": "us-east-1", "service": "aws-mcp"}, Optional: []string{SecretAWSSessionToken}}}
	secrets := Secrets{SecretAWSAccessKeyID: "AKIDEXAMPLE", SecretAWSSecretAccessKey: "secret", SecretAWSSessionToken: "token"}
	if _, err := newClient(t, config, server).Discover(t.Context(), secrets); err != nil {
		t.Fatal(err)
	}
	if signed < 3 {
		t.Fatalf("signed %d requests, want initialize, initialized and tools/list", signed)
	}
	delete(secrets, SecretAWSSessionToken)
	if _, err := newClient(t, config, server).Open(t.Context(), secrets); err == nil {
		t.Error("opened without the session token the binding requires")
	}
}

// resign signs a copy of r carrying only the headers r says it signed.
func resign(t *testing.T, r *http.Request, credentials aws.Credentials) string {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		t.Fatal(err)
	}
	auth := r.Header.Get("Authorization")
	names := strings.Split(strings.TrimSuffix(strings.SplitN(strings.SplitN(auth, "SignedHeaders=", 2)[1], ",", 2)[0], ","), ";")
	req, _ := http.NewRequest(r.Method, "https://"+r.Host+r.URL.RequestURI(), bytes.NewReader(body))
	for _, name := range names {
		if name != "host" && name != "content-length" {
			req.Header.Set(name, r.Header.Get(name))
		}
	}
	req.Header.Del("X-Amz-Date")
	req.Header.Del("X-Amz-Security-Token")
	sum := sha256.Sum256(body)
	if err := v4.NewSigner().SignHTTP(t.Context(), credentials, req, hex.EncodeToString(sum[:]), "aws-mcp", "us-east-1", at); err != nil {
		t.Fatal(err)
	}
	return req.Header.Get("Authorization")
}

func TestSigningConfigValidation(t *testing.T) {
	base := BindingConfig{Endpoint: "https://aws-mcp.us-east-1.api.aws/mcp", Transport: TransportStreamableHTTP}
	params := map[string]string{"region": "us-east-1", "service": "aws-mcp"}
	for name, tc := range map[string]struct {
		auth    Auth
		headers []SecretHeader
		ok      bool
	}{
		"valid":             {Auth{Kind: AuthSigned, Signing: &Signing{Scheme: SigningAWSSigV4, Params: params}}, nil, true},
		"no signing":        {Auth{Kind: AuthSigned}, nil, false},
		"unknown scheme":    {Auth{Kind: AuthSigned, Signing: &Signing{Scheme: "hmac", Params: params}}, nil, false},
		"missing param":     {Auth{Kind: AuthSigned, Signing: &Signing{Scheme: SigningAWSSigV4, Params: map[string]string{"region": "us-east-1"}}}, nil, false},
		"required optional": {Auth{Kind: AuthSigned, Signing: &Signing{Scheme: SigningAWSSigV4, Params: params, Optional: []string{SecretAWSAccessKeyID}}}, nil, false},
		"signing on bearer": {Auth{Kind: AuthBearer, Signing: &Signing{Scheme: SigningAWSSigV4, Params: params}}, nil, false},
		"owned header":      {Auth{Kind: AuthSigned, Signing: &Signing{Scheme: SigningAWSSigV4, Params: params}}, []SecretHeader{{Name: "X-Amz-Date", Field: "d"}}, false},
	} {
		config := base
		config.Auth, config.SecretHeaders = tc.auth, tc.headers
		if err := config.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: Validate() = %v", name, err)
		}
	}
	suggested, ok := SuggestSigning(base.Endpoint)
	if SigningHint(SigningAWSSigV4) == "" || !ok || suggested.Scheme != SigningAWSSigV4 || suggested.Params["region"] != "us-east-1" || suggested.Params["service"] != "aws-mcp" {
		t.Errorf("SuggestSigning = %+v, %v", suggested, ok)
	}
	if _, ok := SuggestSigning("https://mcp.linear.app/mcp"); ok {
		t.Error("suggested signing for a non-AWS endpoint")
	}
	config := base
	config.Auth = Auth{Kind: AuthSigned, Signing: &Signing{Scheme: SigningAWSSigV4, Params: params}}
	if got := strings.Join(config.CredentialSlots(), ","); got != "aws_access_key_id,aws_secret_access_key" {
		t.Errorf("slots = %s", got)
	}
	config.Auth.Signing.Optional = []string{SecretAWSSessionToken}
	if got := strings.Join(config.CredentialSlots(), ","); got != "aws_access_key_id,aws_secret_access_key,aws_session_token" {
		t.Errorf("slots with token = %s", got)
	}
}
