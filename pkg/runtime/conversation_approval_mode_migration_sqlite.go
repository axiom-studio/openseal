package runtime

import (
	"database/sql"
)

func migrateSQLiteConversationApprovalMode(db *sql.DB) error {
	_, err := db.Exec(`UPDATE conversations SET payload = json_set(payload, '$.approvalMode', ?)
		WHERE COALESCE(json_extract(payload, '$.approvalMode'), '') = ''`, string(DefaultConversationApprovalMode))
	return err
}
