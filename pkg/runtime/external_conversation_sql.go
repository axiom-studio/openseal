package runtime

import (
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/axiom-studio/openseal/pkg/models"
)

type sqliteExternalConversationScanner interface {
	Scan(...interface{}) error
}

func scanSQLiteExternalConversationEndpoint(scanner sqliteExternalConversationScanner) (*models.ExternalConversationEndpoint, error) {
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

func scanSQLiteExternalConversationInbox(scanner sqliteExternalConversationScanner) (*models.ExternalConversationInboxItem, error) {
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

func scanSQLiteExternalConversationMapping(scanner sqliteExternalConversationScanner) (*models.ExternalConversationMapping, error) {
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

func scanSQLiteExternalParticipantMapping(scanner sqliteExternalConversationScanner) (*models.ExternalParticipantMapping, error) {
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

func scanSQLiteExternalMessageMapping(scanner sqliteExternalConversationScanner) (*models.ExternalMessageMapping, error) {
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

func scanSQLiteExternalConversationDelivery(scanner sqliteExternalConversationScanner) (*models.ExternalConversationDelivery, error) {
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
