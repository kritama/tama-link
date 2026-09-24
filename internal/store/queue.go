package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/kritama/tama-link/internal/submission"
)

// ListRunnable returns pending submission IDs for one execution strategy in
// deterministic creation order. A worker still acquires the per-submission
// lease and reloads state before execution; this list is only recovery input.
func (s *Store) ListRunnable(ctx context.Context, strategy string) ([]string, error) {
	if strategy == "" {
		return nil, fmt.Errorf("execution strategy is required")
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT submission_id
		FROM submissions
		WHERE strategy = ? AND status IN ('accepted', 'queued', 'running')
		ORDER BY created_at, submission_id`, strategy)
	if err != nil {
		return nil, fmt.Errorf("list runnable submissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list runnable submissions: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runnable submissions: %w", err)
	}
	return ids, nil
}

// ListByStatus returns submission IDs for one strategy whose status is in
// the supplied set, oldest first. Statuses must be known normalized states.
// Callers still acquire the per-submission lease before acting.
func (s *Store) ListByStatus(ctx context.Context, strategy string, statuses []submission.State) ([]string, error) {
	if strategy == "" || len(statuses) == 0 {
		return nil, fmt.Errorf("execution strategy and statuses are required")
	}
	placeholders := make([]string, len(statuses))
	args := make([]any, 0, len(statuses)+1)
	args = append(args, strategy)
	for i, status := range statuses {
		if !submission.Valid(status) {
			return nil, fmt.Errorf("unknown status %q", status)
		}
		placeholders[i] = "?"
		args = append(args, string(status))
	}
	query := `
		SELECT submission_id
		FROM submissions
		WHERE strategy = ? AND status IN (` + strings.Join(placeholders, ",") + `)
		ORDER BY created_at, submission_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list submissions: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list submissions: %w", err)
	}
	return ids, nil
}
