package sqlstore

import (
	"context"
	"database/sql"
)

// ExecQuerier is the part of *sql.DB, *sql.Tx and *sql.Conn the shared
// migrations use, so each store runs them on its own transaction.
type ExecQuerier interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

type RowScanner interface {
	Scan(...interface{}) error
}

type Execer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

type Querier interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

type ExecRowQuerier interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

type RowQuerier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

var _ RowQuerier = (*sql.Conn)(nil)

type MaintenanceDB interface {
	RowQuerier
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}
