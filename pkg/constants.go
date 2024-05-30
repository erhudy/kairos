package pkg

import "time"

const (
	CRON_LAST_RESTARTED_AT_KEY    = "kairos.erhudy.com/cron-last-restarted-at"
	CRON_PATTERN_KEY              = "kairos.erhudy.com/cron-pattern"
	LAST_RESTARTED_AT_TIME_FORMAT = time.RFC3339
)
