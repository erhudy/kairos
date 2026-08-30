package pkg

import "time"

const (
	CRON_LAST_RESTARTED_AT_KEY    = "kairos.erhudy.com/cron-last-restarted-at"
	CRON_PATTERN_KEY              = "kairos.erhudy.com/cron-pattern"
	LAST_RESTARTED_AT_TIME_FORMAT = time.RFC3339
	// API_CALL_TIMEOUT bounds each apiserver call made by a restart so a hung
	// request cannot park a firing goroutine indefinitely.
	API_CALL_TIMEOUT = 10 * time.Second
)
