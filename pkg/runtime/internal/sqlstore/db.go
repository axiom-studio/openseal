package sqlstore

import (
	"context"
	"database/sql"
)

// These interfaces name the parts of *sql.DB, *sql.Tx and *sql.Conn a
// function uses, so each store runs it on its pool, its transaction or a
// pinned connection.

// RowScanner is a *sql.Row or *sql.Rows positioned on a row.
type RowScanner interface {
	Scan(...interface{}) error
}

type Querier interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

type RowQuerier interface {
	QueryRowContext(context.Context, string, ...interface{}) *sql.Row
}

type Execer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

// Reader reads one row or a set of rows.
type Reader interface {
	RowQuerier
	Querier
}

// ExecQuerier is what the shared migrations use, so each store runs them on
// its own transaction.
type ExecQuerier interface {
	Execer
	Querier
}

type ExecRowQuerier interface {
	Execer
	RowQuerier
}

// ReadWriter reads and writes rows.
type ReadWriter interface {
	Reader
	Execer
}

var _ RowQuerier = (*sql.Conn)(nil)
