package runtime

import (
	"database/sql"
)

// Match strings.TrimSpace at the SQL eligibility boundary. SQL TRIM's default
// only handles an ordinary space, which could enqueue an empty destination or
// omit a valid reporting root containing another Unicode whitespace character.
const runTerminalReportingWhitespace = " \t\n\v\f\r\u0085\u00a0\u1680\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000"

func runTerminalReportLeaseResult(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return ErrLeaseLost
	}
	return nil
}
