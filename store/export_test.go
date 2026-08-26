package store

import (
	"context"
	"database/sql"
)

// sqlConn aliases the pooled connection type for readability above.
type sqlConn = sql.Conn

// PragmaOnEveryConn reports foreign_keys and busy_timeout as observed on
// distinct pooled connections.
//
// Exported for tests only. The property under test — that every connection the
// pool opens carries the pragmas — cannot be observed through the Store
// interface, and it is the property whose absence silently disabled foreign
// keys after any interrupted query.
func (s *SQLite) PragmaOnEveryConn(ctx context.Context, n int) ([][2]int, error) {
	db, err := s.conn()
	if err != nil {
		return nil, err
	}

	// Hold each connection open while probing the next, so the pool is forced
	// to open distinct ones rather than handing back the same warm connection.
	conns := make([]*sqlConn, 0, n)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()

	out := make([][2]int, 0, n)
	for range n {
		c, err := db.Conn(ctx)
		if err != nil {
			return nil, err
		}
		conns = append(conns, c)

		var fk, bt int
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			return nil, err
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&bt); err != nil {
			return nil, err
		}
		out = append(out, [2]int{fk, bt})
	}
	return out, nil
}

// RawBlobs reads the raw trace columns for one outcome.
//
// Exported for tests only. A purge reports how many rows it touched; that
// number would be identical for a purge that cleared the wrong column or none
// at all, so the assertion that trace content is actually gone has to read the
// columns directly.
func (s *SQLite) RawBlobs(ctx context.Context, runID, caseID string) (resp, score []byte, err error) {
	db, err := s.conn()
	if err != nil {
		return nil, nil, err
	}
	err = db.QueryRowContext(ctx,
		`SELECT response_proto, score_proto FROM outcomes WHERE run_id = ? AND case_id = ?`,
		runID, caseID).Scan(&resp, &score)
	return resp, score, err
}

// ExecForTest runs one statement directly against the database.
//
// Exported for tests only, and used for exactly one thing: writing a row as an
// OLDER binary would have left it. docs/debt.md#31 is about rows this build did
// not write, so a test that produced them through this package's own writer
// would be testing the wrong binary — the writer stamps the current schema
// version on everything it inserts, which is the whole mechanism under test.
func (s *SQLite) ExecForTest(ctx context.Context, stmt string) error {
	db, err := s.conn()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, stmt)
	return err
}

// MeasurementBlobs reads the raw trace columns for one measurement.
//
// Exported for tests only, for the same reason RawBlobs is: a purge's reported
// row count is identical whether it cleared the right column, the wrong one, or
// none, so the assertion that content is gone must read the column.
func (s *SQLite) MeasurementBlobs(ctx context.Context, runID, assetID, caseID string) (resp, score []byte, err error) {
	db, err := s.conn()
	if err != nil {
		return nil, nil, err
	}
	err = db.QueryRowContext(ctx,
		`SELECT response_proto, score_proto FROM measurements
		 WHERE run_id = ? AND asset_id = ? AND case_id = ?`,
		runID, assetID, caseID).Scan(&resp, &score)
	return resp, score, err
}
