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
