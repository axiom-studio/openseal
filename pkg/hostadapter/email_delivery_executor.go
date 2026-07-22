package hostadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/axiom-studio/openseal/pkg/delivery"
)

const maximumDeliveryAttachmentBytes = 20 << 20

type emailDeliveryCredential struct {
	Provider string `json:"provider"`
	Host     string `json:"host"`
	Port     int    `json:"port"`
	From     string `json:"from"`
	TLS      string `json:"tls"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

type emailDeliveryAttachment struct {
	ID        string
	Version   int64
	Filename  string
	MediaType string
	Digest    string
	SizeBytes int64
	Data      []byte
}

type emailDeliveryEnvelope struct {
	Credential   emailDeliveryCredential
	Recipients   []string
	Message      []byte
	ActionCallID string
}

type emailDeliverySendFunc func(context.Context, emailDeliveryEnvelope) error

// EmailDeliveryExecutor is the Atlas adapter for OpenSeal's portable delivery
// contract. It accepts only trusted attachment transport and an opaque
// credential resolved by Sentinel; neither is returned to the model.
type EmailDeliveryExecutor struct {
	send emailDeliverySendFunc
	now  func() time.Time
}

func NewEmailDeliveryExecutor() *EmailDeliveryExecutor {
	return &EmailDeliveryExecutor{send: sendSMTPEmail, now: time.Now}
}

func (e *EmailDeliveryExecutor) Type() string { return delivery.SkillID }

func (e *EmailDeliveryExecutor) Execute(ctx context.Context, step *StepDefinition, _ TemplateResolver) (*StepResult, error) {
	if e == nil || e.send == nil || e.now == nil || step == nil || step.Config == nil {
		return nil, errors.New("email delivery executor is not configured")
	}
	credential, err := parseEmailDeliveryCredential(step.Config[delivery.EmailCredentialName])
	if err != nil {
		return nil, err
	}
	recipients, err := deliveryRecipients(step.Config["to"])
	if err != nil {
		return nil, err
	}
	subject, _ := step.Config["subject"].(string)
	body, _ := step.Config["body"].(string)
	actionCallID, _ := step.Config[delivery.ActionCallIDTransportKey].(string)
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(body) == "" || strings.TrimSpace(actionCallID) == "" || strings.ContainsAny(subject, "\r\n") {
		return nil, errors.New("email delivery subject, body, and action identity are required")
	}
	attachments, err := decodeEmailDeliveryAttachments(step.Config[delivery.AttachmentsTransportKey])
	if err != nil {
		return nil, err
	}
	deliveredAt := e.now().UTC()
	message, err := buildEmailDeliveryMessage(credential.From, recipients, subject, body, actionCallID, deliveredAt, attachments)
	if err != nil {
		return nil, err
	}
	if err = e.send(ctx, emailDeliveryEnvelope{Credential: credential, Recipients: recipients, Message: message, ActionCallID: actionCallID}); err != nil {
		return nil, fmt.Errorf("email provider did not accept delivery: %w", err)
	}
	artifactRefs := make([]interface{}, 0, len(attachments))
	for _, attachment := range attachments {
		artifactRefs = append(artifactRefs, map[string]interface{}{"id": attachment.ID, "version": attachment.Version})
	}
	return &StepResult{Output: map[string]interface{}{
		"receiptId": "smtp:" + actionCallID, "status": "accepted", "recipientCount": len(recipients),
		"deliveredAt": deliveredAt.Format(time.RFC3339Nano), "artifactRefs": artifactRefs,
	}}, nil
}

func parseEmailDeliveryCredential(value interface{}) (emailDeliveryCredential, error) {
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return emailDeliveryCredential{}, errors.New("email delivery credential is required")
	}
	var credential emailDeliveryCredential
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&credential); err != nil {
		return emailDeliveryCredential{}, errors.New("email delivery credential is invalid")
	}
	if credential.Provider != "smtp" || strings.TrimSpace(credential.Host) == "" || credential.Port < 1 || credential.Port > 65535 {
		return emailDeliveryCredential{}, errors.New("email delivery SMTP provider, host, and port are required")
	}
	credential.Host = strings.TrimSpace(credential.Host)
	address, err := mail.ParseAddress(strings.TrimSpace(credential.From))
	if err != nil || address.Address != strings.TrimSpace(credential.From) {
		return emailDeliveryCredential{}, errors.New("email delivery sender is invalid")
	}
	credential.From = address.Address
	switch credential.TLS {
	case "implicit", "starttls":
	case "none":
		if !localSMTPHost(credential.Host) {
			return emailDeliveryCredential{}, errors.New("unencrypted SMTP is restricted to local or cluster service hosts")
		}
	default:
		return emailDeliveryCredential{}, errors.New("email delivery TLS mode must be implicit, starttls, or none")
	}
	if (credential.Username == "") != (credential.Password == "") {
		return emailDeliveryCredential{}, errors.New("email delivery SMTP username and password must be configured together")
	}
	return credential, nil
}

func localSMTPHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" || strings.HasSuffix(host, ".svc") || strings.HasSuffix(host, ".svc.cluster.local") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func deliveryRecipients(value interface{}) ([]string, error) {
	values, ok := value.([]interface{})
	if !ok || len(values) < 1 || len(values) > delivery.MaximumRecipients {
		return nil, errors.New("email delivery recipients are invalid")
	}
	result, seen := make([]string, 0, len(values)), make(map[string]bool, len(values))
	for _, candidate := range values {
		text, ok := candidate.(string)
		address, err := mail.ParseAddress(strings.TrimSpace(text))
		if !ok || err != nil || address.Address != strings.TrimSpace(text) || seen[strings.ToLower(address.Address)] {
			return nil, errors.New("email delivery recipient is invalid or duplicated")
		}
		seen[strings.ToLower(address.Address)] = true
		result = append(result, address.Address)
	}
	return result, nil
}

func decodeEmailDeliveryAttachments(value interface{}) ([]emailDeliveryAttachment, error) {
	values, ok := value.([]interface{})
	if !ok {
		return nil, errors.New("trusted email attachment transport is required")
	}
	result, total := make([]emailDeliveryAttachment, 0, len(values)), int64(0)
	for _, candidate := range values {
		raw, ok := candidate.(map[string]interface{})
		if !ok {
			return nil, errors.New("trusted email attachment is invalid")
		}
		attachment := emailDeliveryAttachment{
			ID: stringConfig(raw["id"]), Version: int64Config(raw["version"]), Filename: stringConfig(raw["filename"]),
			MediaType: stringConfig(raw["mediaType"]), Digest: stringConfig(raw["digest"]), SizeBytes: int64Config(raw["sizeBytes"]),
		}
		if attachment.ID == "" || attachment.Version < 1 || attachment.Filename == "" || strings.ContainsAny(attachment.Filename, "\r\n/\\") ||
			attachment.MediaType == "" || attachment.SizeBytes < 0 || attachment.SizeBytes > maximumDeliveryAttachmentBytes || total+attachment.SizeBytes > maximumDeliveryAttachmentBytes {
			return nil, errors.New("trusted email attachment metadata is invalid")
		}
		encoded := stringConfig(raw["dataBase64"])
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || int64(len(data)) != attachment.SizeBytes {
			return nil, errors.New("trusted email attachment bytes are invalid")
		}
		hash := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(hash[:]) != attachment.Digest {
			return nil, errors.New("trusted email attachment digest is invalid")
		}
		attachment.Data = data
		result, total = append(result, attachment), total+attachment.SizeBytes
	}
	return result, nil
}

func buildEmailDeliveryMessage(from string, recipients []string, subject, body, actionCallID string, deliveredAt time.Time, attachments []emailDeliveryAttachment) ([]byte, error) {
	var output bytes.Buffer
	boundaryHash := sha256.Sum256([]byte(actionCallID))
	boundary := "openseal-" + hex.EncodeToString(boundaryHash[:12])
	messageID := "<openseal-" + hex.EncodeToString(boundaryHash[:]) + "@delivery.local>"
	fmt.Fprintf(&output, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\nMessage-ID: %s\r\nMIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=%q\r\n\r\n", from, strings.Join(recipients, ", "), mime.QEncoding.Encode("utf-8", subject), deliveredAt.Format(time.RFC1123Z), messageID, boundary)
	writer := multipart.NewWriter(&output)
	if err := writer.SetBoundary(boundary); err != nil {
		return nil, err
	}
	bodyHeader := textproto.MIMEHeader{}
	bodyHeader.Set("Content-Type", "text/plain; charset=utf-8")
	bodyHeader.Set("Content-Transfer-Encoding", "8bit")
	bodyPart, err := writer.CreatePart(bodyHeader)
	if err != nil {
		return nil, err
	}
	if _, err = io.WriteString(bodyPart, strings.ReplaceAll(strings.ReplaceAll(body, "\r\n", "\n"), "\n", "\r\n")); err != nil {
		return nil, err
	}
	for _, attachment := range attachments {
		header := textproto.MIMEHeader{}
		header.Set("Content-Type", attachment.MediaType)
		header.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", mime.QEncoding.Encode("utf-8", attachment.Filename)))
		header.Set("Content-Transfer-Encoding", "base64")
		part, partErr := writer.CreatePart(header)
		if partErr != nil {
			return nil, partErr
		}
		if _, partErr = io.WriteString(part, wrapBase64(base64.StdEncoding.EncodeToString(attachment.Data))); partErr != nil {
			return nil, partErr
		}
	}
	if err = writer.Close(); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func wrapBase64(value string) string {
	var output strings.Builder
	for len(value) > 76 {
		output.WriteString(value[:76] + "\r\n")
		value = value[76:]
	}
	output.WriteString(value + "\r\n")
	return output.String()
}

func sendSMTPEmail(ctx context.Context, envelope emailDeliveryEnvelope) error {
	address := net.JoinHostPort(envelope.Credential.Host, strconv.Itoa(envelope.Credential.Port))
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	var connection net.Conn
	var err error
	tlsConfig := &tls.Config{ServerName: envelope.Credential.Host, MinVersion: tls.VersionTLS12}
	if envelope.Credential.TLS == "implicit" {
		connection, err = (&tls.Dialer{NetDialer: dialer, Config: tlsConfig}).DialContext(ctx, "tcp", address)
	} else {
		connection, err = dialer.DialContext(ctx, "tcp", address)
	}
	if err != nil {
		return err
	}
	defer connection.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	}
	client, err := smtp.NewClient(connection, envelope.Credential.Host)
	if err != nil {
		return err
	}
	defer client.Close()
	if envelope.Credential.TLS == "starttls" {
		if err = client.StartTLS(tlsConfig); err != nil {
			return err
		}
	}
	if envelope.Credential.Username != "" {
		if err = client.Auth(smtp.PlainAuth("", envelope.Credential.Username, envelope.Credential.Password, envelope.Credential.Host)); err != nil {
			return err
		}
	}
	if err = client.Mail(envelope.Credential.From); err != nil {
		return err
	}
	for _, recipient := range envelope.Recipients {
		if err = client.Rcpt(recipient); err != nil {
			return err
		}
	}
	data, err := client.Data()
	if err != nil {
		return err
	}
	if _, err = data.Write(envelope.Message); err != nil {
		_ = data.Close()
		return err
	}
	if err = data.Close(); err != nil {
		return err
	}
	return client.Quit()
}

func stringConfig(value interface{}) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func int64Config(value interface{}) int64 {
	switch typed := value.(type) {
	case int:
		return int64(typed)
	case int64:
		return typed
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed)
		}
	}
	return 0
}
