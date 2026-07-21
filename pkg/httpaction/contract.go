// Package httpaction defines the portable, credential-safe HTTP transport
// used by compiled Skills. OpenSeal owns the envelope; embedding hosts own the
// network implementation and its egress policy.
package httpaction

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	TransportName = "openseal.http.request"

	BaseURLKey             = "baseUrl"
	MethodKey              = "method"
	PathKey                = "path"
	ParametersKey          = "parameters"
	ParameterContractKey   = "parameterContract"
	CredentialNameKey      = "credentialName"
	CredentialParameterKey = "credentialParameter"
)

// Parameter is the immutable source-authored mapping used to place one
// model-visible value. Credential parameters are not represented here.
type Parameter struct {
	Name     string      `json:"name"`
	Location string      `json:"location"`
	Required bool        `json:"required,omitempty"`
	Default  interface{} `json:"default,omitempty"`
}

// Invocation is the materialized, non-secret request contract delivered to a
// governed host. The resolved credential remains out of band.
type Invocation struct {
	BaseURL             string
	Method              string
	Path                string
	Parameters          map[string]interface{}
	ParameterContract   []Parameter
	CredentialName      string
	CredentialParameter string
}

// DecodeInvocation fails closed on envelopes that could change the endpoint,
// method, parameter placement, or credential placement at runtime.
func DecodeInvocation(value map[string]interface{}) (*Invocation, error) {
	if value == nil {
		return nil, errors.New("HTTP action invocation is required")
	}
	baseURL, _ := value[BaseURLKey].(string)
	method, _ := value[MethodKey].(string)
	path, _ := value[PathKey].(string)
	credentialName, _ := value[CredentialNameKey].(string)
	credentialParameter, _ := value[CredentialParameterKey].(string)
	baseURL, method, path = strings.TrimSpace(baseURL), strings.ToUpper(strings.TrimSpace(method)), strings.TrimSpace(path)
	credentialName, credentialParameter = strings.TrimSpace(credentialName), strings.TrimSpace(credentialParameter)
	if err := ValidateEndpoint(baseURL, method, path); err != nil {
		return nil, err
	}
	if credentialName == "" || credentialParameter == "" {
		return nil, errors.New("HTTP action credential name and parameter are required")
	}
	parameters, ok := value[ParametersKey].(map[string]interface{})
	if !ok {
		encoded, err := json.Marshal(value[ParametersKey])
		if err != nil || json.Unmarshal(encoded, &parameters) != nil || parameters == nil {
			return nil, errors.New("HTTP action parameters must be an object")
		}
	}
	encoded, err := json.Marshal(value[ParameterContractKey])
	if err != nil {
		return nil, errors.New("HTTP action parameter contract is invalid")
	}
	var contract []Parameter
	if err := json.Unmarshal(encoded, &contract); err != nil || len(contract) == 0 {
		return nil, errors.New("HTTP action parameter contract is required")
	}
	known := make(map[string]Parameter, len(contract))
	for _, parameter := range contract {
		parameter.Name = strings.TrimSpace(parameter.Name)
		parameter.Location = strings.ToLower(strings.TrimSpace(parameter.Location))
		if parameter.Name == "" || parameter.Name == credentialParameter ||
			(parameter.Location != "query" && parameter.Location != "path") {
			return nil, errors.New("HTTP action parameter contract contains an invalid parameter")
		}
		if _, exists := known[parameter.Name]; exists {
			return nil, errors.New("HTTP action parameter contract contains duplicate parameters")
		}
		known[parameter.Name] = parameter
	}
	for name := range parameters {
		if _, ok := known[name]; !ok {
			return nil, fmt.Errorf("HTTP action contains undeclared parameter %s", name)
		}
	}
	for name, parameter := range known {
		if _, ok := parameters[name]; !ok && parameter.Required && parameter.Default == nil {
			return nil, fmt.Errorf("HTTP action requires parameter %s", name)
		}
	}
	return &Invocation{
		BaseURL: baseURL, Method: method, Path: path, Parameters: cloneMap(parameters),
		ParameterContract: append([]Parameter(nil), contract...), CredentialName: credentialName,
		CredentialParameter: credentialParameter,
	}, nil
}

// ValidateEndpoint restricts the portable adapter to non-mutating HTTPS
// requests. Hosts must additionally enforce tenant egress and DNS policy.
func ValidateEndpoint(baseURL, method, path string) error {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" && parsed.Path != "/" {
		return errors.New("HTTP action base URL must be an HTTPS origin")
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if method != "GET" && method != "HEAD" {
		return errors.New("HTTP action adapter currently supports only GET and HEAD operations")
	}
	requestPath, err := url.ParseRequestURI(strings.TrimSpace(path))
	if err != nil || !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") ||
		requestPath.IsAbs() || requestPath.Host != "" || requestPath.RawQuery != "" || requestPath.Fragment != "" {
		return errors.New("HTTP action path must be a fixed absolute path without query or fragment")
	}
	return nil
}

func cloneMap(value map[string]interface{}) map[string]interface{} {
	encoded, _ := json.Marshal(value)
	var result map[string]interface{}
	_ = json.Unmarshal(encoded, &result)
	return result
}
