package store

import (
	"context"
	"fmt"
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
