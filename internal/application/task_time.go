package application

import (
	"math"
	"time"
)

// maxTimerMs is the largest millisecond count that fits in time.Duration.
// Larger Tasks integers are persisted exactly and capped only for timers.
const maxTimerMs = int64(math.MaxInt64 / int64(time.Millisecond))

// timerWait converts a validated poll interval into a timer duration. Zero
// or negative values use one second. Values above the duration range are
// capped rather than wrapped.
func timerWait(ms int64) time.Duration {
	if ms <= 0 {
		return time.Second
	}
	if ms > maxTimerMs {
		ms = maxTimerMs
	}
	wait := time.Duration(ms) * time.Millisecond
	if wait < taskPollFloor {
		return taskPollFloor
	}
	return wait
}
