package pkg

import "time"

const (
	CRON_LAST_RESTARTED_AT_KEY    = "kairos.erhudy.com/cron-last-restarted-at"
	CRON_PATTERN_KEY              = "kairos.erhudy.com/cron-pattern"
	TIME_ZONE_KEY                 = "kairos.erhudy.com/cron-timezone"
	LAST_RESTARTED_AT_TIME_FORMAT = time.RFC3339
)
