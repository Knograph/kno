package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestIsBusyRecognizesLockContention.
//
// The classifier decides whether an open waits or fails, so a miss here turns a
// recoverable race into a user-facing SQL error, and a false positive turns a
// real failure into six retries and a longer wait for the same error.
func TestIsBusyRecognizesLockContention(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"the observed message", errors.New("database is locked (5) (SQLITE_BUSY)"), true},
		{"wrapped", errors.New("applying schema: database is locked (5)"), true},
		{"table-level lock", errors.New("database table is locked"), true},
		{"a unique violation is not contention", errors.New("UNIQUE constraint failed: runs.id"), false},
		{"a missing column is not contention", errors.New("no such column: score_value"), false},
		{"a read-only file is not contention", errors.New("attempt to write a readonly database"), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isBusy(tc.err); got != tc.want {
				t.Errorf("isBusy(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestRetryOnBusyGivesUpAndReturnsTheLastError: a genuinely locked database
// must fail while the user is still watching, not spin forever.
func TestRetryOnBusyGivesUpAndReturnsTheLastError(t *testing.T) {
	t.Parallel()

	locked := errors.New("database is locked (5)")
	attempts := 0
	err := retryOnBusy(context.Background(), func() error {
		attempts++
		return locked
	})

	if !errors.Is(err, locked) {
		t.Errorf("err = %v, want the underlying lock error", err)
	}
	if attempts != openBusyRetries {
		t.Errorf("attempted %d times, want %d", attempts, openBusyRetries)
	}
}

// TestRetryOnBusySucceedsOnceContentionClears is the case the retry exists for.
func TestRetryOnBusySucceedsOnceContentionClears(t *testing.T) {
	t.Parallel()

	attempts := 0
	err := retryOnBusy(context.Background(), func() error {
		attempts++
		if attempts < 3 {
			return errors.New("database is locked (5)")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryOnBusy: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempted %d times, want 3", attempts)
	}
}

// TestRetryOnBusyDoesNotRetryOtherErrors: retrying a missing column six times
// just makes the same failure slower.
func TestRetryOnBusyDoesNotRetryOtherErrors(t *testing.T) {
	t.Parallel()

	fatal := errors.New("no such column: score_value")
	attempts := 0
	err := retryOnBusy(context.Background(), func() error {
		attempts++
		return fatal
	})

	if !errors.Is(err, fatal) {
		t.Errorf("err = %v, want the original error unchanged", err)
	}
	if attempts != 1 {
		t.Errorf("attempted %d times, want 1", attempts)
	}
}

// TestRetryOnBusyHonorsCancellation: a Ctrl-C during startup contention must
// end the wait rather than serve out the backoff.
func TestRetryOnBusyHonorsCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0

	start := time.Now()
	err := retryOnBusy(ctx, func() error {
		attempts++
		cancel() // cancelled while the first backoff is pending
		return errors.New("database is locked (5)")
	})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if attempts != 1 {
		t.Errorf("attempted %d times after cancellation, want 1", attempts)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %v after cancellation; the backoff should be abandoned", elapsed)
	}
}

// TestApplySchemaAndMigrationFailClosedOnABrokenHandle.
//
// Both take a write lock before doing anything, and both must surface a failure
// there rather than proceeding to run DDL against a database they do not hold.
// A partial upgrade is worse than a refused one.
func TestApplySchemaAndMigrationFailClosedOnABrokenHandle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, err := NewSQLite(ctx, t.TempDir()+"/kno.db")
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	db := s.db
	if err := s.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	if err := applySchema(ctx, db); err == nil {
		t.Error("applySchema succeeded against a closed database")
	}
	if err := applyMigration(ctx, db, migrations[0]); err == nil {
		t.Error("applyMigration succeeded against a closed database")
	}
	if _, err := userVersion(ctx, db); err == nil {
		t.Error("userVersion succeeded against a closed database")
	}
}

// TestMigrateRefusesWhenTheVersionCannotBeRead: a database whose version is
// unreadable must not be migrated on the assumption that it is version 0.
func TestMigrateRefusesWhenTheVersionCannotBeRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	s, err := NewSQLite(ctx, t.TempDir()+"/kno.db")
	if err != nil {
		t.Fatalf("opening: %v", err)
	}
	db := s.db
	if err := s.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}

	if err := migrate(ctx, db); err == nil {
		t.Error("migrate proceeded without being able to read user_version; " +
			"assuming version 0 would re-apply every step against real data")
	}
}
