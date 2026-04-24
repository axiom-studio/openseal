package executor

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

const NodeTypeEmail = "email"

// EmailExecutor sends emails via SMTP or email service APIs
type EmailExecutor struct {
	httpClient *http.Client
}

func NewEmailExecutor() *EmailExecutor {
	return &EmailExecutor{
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *EmailExecutor) Type() string {
	return NodeTypeEmail
}

func (e *EmailExecutor) Execute(ctx context.Context, step *StepDefinition, resolver TemplateResolver) (*StepResult, error) {
	config := step.Config
	if config == nil {
		return nil, fmt.Errorf("email step requires config")
	}

	provider, _ := config["provider"].(string)
	if provider == "" {
		provider = "smtp"
	}

	to, _ := config["to"].(string)
	if to == "" {
		return nil, fmt.Errorf("email step requires 'to'")
	}
	to = resolver.ResolveString(to)

	subject, _ := config["subject"].(string)
	subject = resolver.ResolveString(subject)

	body, _ := config["body"].(string)
	body = resolver.ResolveString(body)

	switch provider {
	case "smtp":
		return e.sendSMTP(config, resolver, to, subject, body)
	case "sendgrid":
		return e.sendSendGrid(ctx, config, resolver, to, subject, body)
	case "ses":
		return nil, fmt.Errorf("SES provider not yet implemented")
	default:
		return nil, fmt.Errorf("unknown email provider: %s", provider)
	}
}

func (e *EmailExecutor) sendSMTP(config map[string]interface{}, resolver TemplateResolver, to, subject, body string) (*StepResult, error) {
	host, _ := config["host"].(string)
	if host == "" {
		host = "localhost"
	}
	host = resolver.ResolveString(host)

	port, _ := config["port"].(float64)
	if port == 0 {
		port = 587
	}

	from, _ := config["from"].(string)
	if from == "" {
		return nil, fmt.Errorf("SMTP requires 'from' address")
	}
	from = resolver.ResolveString(from)

	username, _ := config["username"].(string)
	username = resolver.ResolveString(username)

	password, _ := config["password"].(string)
	password = resolver.ResolveString(password)

	isHTML := false
	if html, ok := config["html"].(bool); ok {
		isHTML = html
	}

	contentType := "text/plain"
	if isHTML {
		contentType = "text/html"
	}

	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\nContent-Type: %s; charset=UTF-8\r\n\r\n%s",
		from, to, subject, contentType, body)

	addr := fmt.Sprintf("%s:%d", host, int(port))

	var auth smtp.Auth
	if username != "" && password != "" {
		auth = smtp.PlainAuth("", username, password, host)
	}

	recipients := strings.Split(to, ",")
	for i := range recipients {
		recipients[i] = strings.TrimSpace(recipients[i])
	}

	// Try TLS first, then fall back to plain
	var err error
	tlsConfig := &tls.Config{ServerName: host}

	conn, err := tls.Dial("tcp", addr, tlsConfig)
	if err == nil {
		client, clientErr := smtp.NewClient(conn, host)
		if clientErr == nil {
			defer client.Close()
			if auth != nil {
				if authErr := client.Auth(auth); authErr != nil {
					return nil, fmt.Errorf("SMTP auth failed: %w", authErr)
				}
			}
			if fromErr := client.Mail(from); fromErr != nil {
				return nil, fmt.Errorf("SMTP mail from failed: %w", fromErr)
			}
			for _, rcpt := range recipients {
				if rcptErr := client.Rcpt(rcpt); rcptErr != nil {
					return nil, fmt.Errorf("SMTP rcpt failed: %w", rcptErr)
				}
			}
			w, dataErr := client.Data()
			if dataErr != nil {
				return nil, fmt.Errorf("SMTP data failed: %w", dataErr)
			}
			_, _ = w.Write([]byte(msg))
			_ = w.Close()
			_ = client.Quit()

			return &StepResult{
				Output: map[string]interface{}{
					"success": true,
					"to":      to,
					"subject": subject,
				},
			}, nil
		}
	}

	// Fall back to smtp.SendMail
	err = smtp.SendMail(addr, auth, from, recipients, []byte(msg))
	if err != nil {
		return nil, fmt.Errorf("SMTP send failed: %w", err)
	}

	return &StepResult{
		Output: map[string]interface{}{
			"success": true,
			"to":      to,
			"subject": subject,
		},
	}, nil
}

func (e *EmailExecutor) sendSendGrid(ctx context.Context, config map[string]interface{}, resolver TemplateResolver, to, subject, body string) (*StepResult, error) {
	apiKey, _ := config["apiKey"].(string)
	if apiKey == "" {
		return nil, fmt.Errorf("SendGrid requires 'apiKey'")
	}
	apiKey = resolver.ResolveString(apiKey)

	from, _ := config["from"].(string)
	if from == "" {
		return nil, fmt.Errorf("SendGrid requires 'from' address")
	}
	from = resolver.ResolveString(from)

	fromName, _ := config["fromName"].(string)
	fromName = resolver.ResolveString(fromName)

	isHTML := false
	if html, ok := config["html"].(bool); ok {
		isHTML = html
	}

	contentType := "text/plain"
	if isHTML {
		contentType = "text/html"
	}

	payload := map[string]interface{}{
		"personalizations": []map[string]interface{}{
			{
				"to": []map[string]string{
					{"email": to},
				},
			},
		},
		"from": map[string]string{
			"email": from,
			"name":  fromName,
		},
		"subject": subject,
		"content": []map[string]string{
			{
				"type":  contentType,
				"value": body,
			},
		},
	}

	jsonBody, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal SendGrid payload: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", "https://api.sendgrid.com/v3/mail/send", bytes.NewReader(jsonBody))
	if err != nil {
		return nil, fmt.Errorf("failed to create SendGrid request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SendGrid request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	success := resp.StatusCode >= 200 && resp.StatusCode < 300

	return &StepResult{
		Output: map[string]interface{}{
			"success":    success,
			"statusCode": resp.StatusCode,
			"response":   string(respBody),
			"to":         to,
			"subject":    subject,
		},
	}, nil
}
