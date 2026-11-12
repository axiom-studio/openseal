package sqlstore

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
)

func ScanExternalConversationEndpoint(scanner RowScanner) (*models.ExternalConversationEndpoint, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value models.ExternalConversationEndpoint
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func ScanExternalConversationInbox(scanner RowScanner) (*models.ExternalConversationInboxItem, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value models.ExternalConversationInboxItem
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func ScanExternalConversationMapping(scanner RowScanner) (*models.ExternalConversationMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value models.ExternalConversationMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func ScanExternalParticipantMapping(scanner RowScanner) (*models.ExternalParticipantMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value models.ExternalParticipantMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func ScanExternalMessageMapping(scanner RowScanner) (*models.ExternalMessageMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value models.ExternalMessageMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func ScanExternalConversationDelivery(scanner RowScanner) (*models.ExternalConversationDelivery, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value models.ExternalConversationDelivery
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}
