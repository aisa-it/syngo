package store

import (
	"context"
	"fmt"
	"time"
)

// DestinationState — состояние ретраев до чужого сервера.
type DestinationState struct {
	Destination   string
	FailureTS     *time.Time
	RetryLastTS   *time.Time
	RetryInterval time.Duration
}

// Destination возвращает состояние destination (нулевое, если записи нет).
func (db *DB) Destination(ctx context.Context, name string) (DestinationState, error) {
	const query = `
		SELECT destination, failure_ts, retry_last_ts, retry_interval
		FROM destinations WHERE destination = $1`

	var state DestinationState

	var intervalMS int64

	err := db.pool.QueryRow(ctx, query, name).Scan(&state.Destination, &state.FailureTS, &state.RetryLastTS, &intervalMS)
	if err != nil {
		if isNoRows(err) {
			return DestinationState{Destination: name}, nil
		}

		return DestinationState{}, fmt.Errorf("store: destination %s: %w", name, err)
	}

	state.RetryInterval = time.Duration(intervalMS) * time.Millisecond

	return state, nil
}

// SaveDestinationFailure фиксирует неудачу и следующий интервал ретрая.
func (db *DB) SaveDestinationFailure(ctx context.Context, name string, at time.Time, retryInterval time.Duration) error {
	const query = `
		INSERT INTO destinations (destination, failure_ts, retry_last_ts, retry_interval)
		VALUES ($1, $2, $2, $3)
		ON CONFLICT (destination) DO UPDATE SET
			failure_ts = EXCLUDED.failure_ts,
			retry_last_ts = EXCLUDED.retry_last_ts,
			retry_interval = EXCLUDED.retry_interval`

	if _, err := db.pool.Exec(ctx, query, name, at, retryInterval.Milliseconds()); err != nil {
		return fmt.Errorf("store: save destination failure %s: %w", name, err)
	}

	return nil
}

// SaveDestinationSuccess сбрасывает состояние ретраев.
func (db *DB) SaveDestinationSuccess(ctx context.Context, name string) error {
	const query = `
		INSERT INTO destinations (destination, failure_ts, retry_last_ts, retry_interval)
		VALUES ($1, NULL, NULL, 0)
		ON CONFLICT (destination) DO UPDATE SET
			failure_ts = NULL, retry_last_ts = NULL, retry_interval = 0`

	if _, err := db.pool.Exec(ctx, query, name); err != nil {
		return fmt.Errorf("store: save destination success %s: %w", name, err)
	}

	return nil
}
