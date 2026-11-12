package runtime

import (
	"database/sql"

	"github.com/axiom-studio/openseal/pkg/models"
)

func migrateSQLiteConversationApprovalMode(db *sql.DB) error {
	_, err := db.Exec(`UPDATE conversations SET payload = json_set(payload, '$.approvalMode', ?)
		WHERE COALESCE(json_extract(payload, '$.approvalMode'), '') = ''`, string(models.DefaultConversationApprovalMode))
	return err
}
