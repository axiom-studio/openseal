package runtime

import (
	"database/sql"
	"encoding/json"
	"errors"
)

type sqliteExternalConversationScanner interface {
	Scan(...interface{}) error
}

func scanSQLiteExternalConversationEndpoint(scanner sqliteExternalConversationScanner) (*ExternalConversationEndpoint, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationEndpoint
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalConversationInbox(scanner sqliteExternalConversationScanner) (*ExternalConversationInboxItem, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationInboxItem
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalConversationMapping(scanner sqliteExternalConversationScanner) (*ExternalConversationMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalParticipantMapping(scanner sqliteExternalConversationScanner) (*ExternalParticipantMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalParticipantMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalMessageMapping(scanner sqliteExternalConversationScanner) (*ExternalMessageMapping, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalMessageMapping
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}

func scanSQLiteExternalConversationDelivery(scanner sqliteExternalConversationScanner) (*ExternalConversationDelivery, error) {
	var payload string
	if err := scanner.Scan(&payload); errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var value ExternalConversationDelivery
	if err := json.Unmarshal([]byte(payload), &value); err != nil {
		return nil, err
	}
	return &value, value.Validate()
}
