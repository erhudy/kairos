package pkg

import "time"

const (
	CRON_LAST_RESTARTED_AT_KEY    = "kairos.erhudy.com/cron-last-restarted-at"
	CRON_PATTERN_KEY              = "kairos.erhudy.com/cron-pattern"
	RESTART_AFTER_KEY             = "kairos.erhudy.com/restart-after"
	RESTART_AFTER_MODE_KEY        = "kairos.erhudy.com/restart-after-mode"
	RESTART_AFTER_WAIT_KEY        = "kairos.erhudy.com/restart-after-wait"
	LAST_RESTARTED_AT_TIME_FORMAT = time.RFC3339
	// API_CALL_TIMEOUT bounds each apiserver call made by a restart so a hung
	// request cannot park a firing goroutine indefinitely.
	API_CALL_TIMEOUT = 10 * time.Second
	// CHAIN_POLL_INTERVAL is how often a chain step re-checks its predecessor's
	// rollout status while waiting for it to become healthy again.
	CHAIN_POLL_INTERVAL = 5 * time.Second

	CHAIN_MODE_HEALTH            = "health"
	CHAIN_MODE_HEALTH_PLUS_WAIT  = "health-plus-wait"
	CHAIN_MODE_DISPLAY_HEALTH    = "health"
	CHAIN_MODE_DISPLAY_PLUS_WAIT = "health+wait"

	CHAIN_OUTCOME_COMPLETED = "completed"
	CHAIN_OUTCOME_TIMEOUT   = "timeout"
	CHAIN_OUTCOME_ABORTED   = "aborted"
)
