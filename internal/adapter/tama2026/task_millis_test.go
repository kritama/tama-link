package tama2026

import (
	"testing"
	"time"

	"github.com/kritama/tama-link/internal/upstream"
)

func TestNormalizeTaskStateKeepsWideMillisecondIntegers(t *testing.T) {
	const maxSafe = int64(9_007_199_254_740_991)
	boundary := int64(time.Duration(1<<63-1) / time.Millisecond)

	state := &upstream.TaskState{
		TaskID:         "task-1",
		Status:         upstream.TaskWorking,
		CreatedAt:      "2026-09-11T10:00:00Z",
		LastUpdatedAt:  "2026-09-11T10:00:01Z",
		TTLMs:          maxSafe,
		PollIntervalMs: boundary + 1,
	}
	snap, err := normalizeTaskState(state, []byte(`{"client":{}}`))
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if snap.TTLMs != maxSafe || snap.PollIntervalMs != boundary+1 {
		t.Fatalf("ttl %d poll %d", snap.TTLMs, snap.PollIntervalMs)
	}
}
