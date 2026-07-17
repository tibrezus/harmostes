package worker

import (
	"fmt"
	"time"
)

// RetryConfig controls exponential backoff retry behavior.
// Exported for use by external integrators.
type RetryConfig struct {
	MaxAttempts int
	BaseDelay   time.Duration
}

// DefaultRetryConfig is the default retry configuration.
var DefaultRetryConfig = RetryConfig{
	MaxAttempts: 3,
	BaseDelay:   100 * time.Millisecond,
}

// WithRetry calls fn up to MaxAttempts times with exponential backoff.
// Returns the last error if all attempts fail.
func WithRetry(cfg RetryConfig, fn func() error) error {
	var lastErr error
	for i := 0; i < cfg.MaxAttempts; i++ {
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		time.Sleep(cfg.BaseDelay * time.Duration(1<<i))
	}
	return fmt.Errorf("failed after %d attempts: %w", cfg.MaxAttempts, lastErr)
}
