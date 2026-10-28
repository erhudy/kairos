package pkg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/wait"
)

// 5 attempts, 1s/2s/4s/8s apart plus jitter.
var defaultAPIRetryBackoff = wait.Backoff{
	Steps:    5,
	Duration: time.Second,
	Factor:   2,
	Jitter:   0.5,
}

// retryTransientAPIError retries fn on transient API errors and returns its last
// error. fn must be safe to repeat.
func retryTransientAPIError(ctx context.Context, logger *zap.Logger, backoff wait.Backoff, op string, fn func() error) error {
	steps := backoff.Steps
	attempt := 0
	var lastErr error
	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		attempt++
		lastErr = fn()
		if lastErr == nil {
			return true, nil
		}
		if !isTransientAPIError(ctx, lastErr) {
			return false, lastErr
		}
		if attempt < steps {
			logger.Warn("transient Kubernetes API error, will retry",
				zap.String("op", op),
				zap.Int("attempt", attempt),
				zap.Int("maxAttempts", steps),
				zap.Error(lastErr),
			)
		}
		return false, nil
	})
	switch {
	case err == nil:
		return nil
	case lastErr == nil:
		return err
	case attempt > 1:
		return fmt.Errorf("giving up after %d attempts: %w", attempt, lastErr)
	default:
		return lastErr
	}
}

// isTransientAPIError reports whether err is a 429, 5xx, dropped connection, or
// request timeout. Nothing is transient once ctx is cancelled.
func isTransientAPIError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) {
		code := status.Status().Code
		return code == http.StatusTooManyRequests || (code >= 500 && code != http.StatusNotImplemented)
	}
	if utilnet.IsConnectionReset(err) || utilnet.IsConnectionRefused(err) || utilnet.IsProbableEOF(err) || utilnet.IsHTTP2ConnectionLost(err) {
		return true
	}
	// ctx is live, so this is the per-request timeout
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
