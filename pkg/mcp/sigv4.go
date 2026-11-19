package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// SigningAWSSigV4 is AWS Signature Version 4 from an IAM access key, which
// AWS endpoints such as the AWS MCP Server require. Params are region and
// service.
const SigningAWSSigV4 = "aws-sigv4"

// Secret fields of aws-sigv4.
const (
	SecretAWSAccessKeyID     = "aws_access_key_id"
	SecretAWSSecretAccessKey = "aws_secret_access_key"
	SecretAWSSessionToken    = "aws_session_token"
)

var awsScope = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

type awsSigV4 struct{}

func (awsSigV4) fields() []CredentialField {
	return []CredentialField{
		{Name: SecretAWSAccessKeyID, Label: "Access key ID"},
		{Name: SecretAWSSecretAccessKey, Label: "Secret access key", Secret: true},
		{Name: SecretAWSSessionToken, Label: "Session token", Secret: true, Optional: true},
	}
}

func (awsSigV4) hint() string {
	return "Create an IAM access key in your AWS account: IAM → Users → a user for your agents → Security credentials → Create access key. " +
		"Give that user only the permissions agents need, such as ReadOnlyAccess. Leave the session token empty unless the key is temporary."
}

func (awsSigV4) checkParams(params map[string]string) error {
	if len(params) != 2 || !awsScope.MatchString(params["region"]) || !awsScope.MatchString(params["service"]) {
		return fmt.Errorf("MCP %s signing needs exactly a region and a service", SigningAWSSigV4)
	}
	return nil
}

func (awsSigV4) ownsHeader(name string) bool { return strings.HasPrefix(name, "x-amz-") }

// suggest reads service and region from <service>.<region>.api.aws, as AWS's
// own MCP proxy does.
func (awsSigV4) suggest(endpoint *url.URL) (map[string]string, bool) {
	labels := strings.Split(strings.ToLower(endpoint.Hostname()), ".")
	if len(labels) != 4 || labels[2] != "api" || labels[3] != "aws" || !awsScope.MatchString(labels[0]) || !awsScope.MatchString(labels[1]) {
		return nil, false
	}
	return map[string]string{"service": labels[0], "region": labels[1]}, true
}

// sign covers each request's method, URL, headers, body and time, so it
// cannot be a fixed session header.
func (awsSigV4) sign(params map[string]string, secrets Secrets) func(*http.Request, []byte) error {
	credentials := aws.Credentials{
		AccessKeyID:     secrets[SecretAWSAccessKeyID],
		SecretAccessKey: secrets[SecretAWSSecretAccessKey],
		SessionToken:    secrets[SecretAWSSessionToken],
	}
	signer := v4.NewSigner()
	region, service := params["region"], params["service"]
	return func(req *http.Request, body []byte) error {
		sum := sha256.Sum256(body)
		if err := signer.SignHTTP(req.Context(), credentials, req, hex.EncodeToString(sum[:]), service, region, time.Now()); err != nil {
			return errors.New("MCP request could not be signed with the AWS key")
		}
		return nil
	}
}
