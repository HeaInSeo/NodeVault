package podbridge5

import (
	"fmt"
	"time"
)

const (
	DefaultAddAndCopyDryRun       = false
	DefaultHealthcheckInterval    = 30 * time.Second
	DefaultHealthcheckRetries     = 3
	DefaultHealthcheckTimeout     = 5 * time.Second
	DefaultHealthcheckStartPeriod = 0 * time.Second
	MinHealthcheckTimeout         = 1 * time.Second
)

func DefaultHealthcheckPolicy() (interval string, retries uint, timeout, startPeriod string) {
	return DefaultHealthcheckInterval.String(), DefaultHealthcheckRetries, DefaultHealthcheckTimeout.String(), DefaultHealthcheckStartPeriod.String()
}

func parsePolicyDuration(fieldName, value string, minDur time.Duration, allowZero bool) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", fieldName, err)
	}
	if allowZero && d == 0 {
		return 0, nil
	}
	if d < minDur {
		return 0, fmt.Errorf("%s must be at least %s", fieldName, minDur)
	}
	return d, nil
}

func parseHealthcheckInterval(value string) (time.Duration, error) {
	if value == "disable" {
		value = "0"
	}
	return parsePolicyDuration("healthcheck-interval", value, 0, true)
}

func parseHealthcheckTimeout(value string) (time.Duration, error) {
	return parsePolicyDuration("healthcheck-timeout", value, MinHealthcheckTimeout, false)
}

func parseHealthcheckStartPeriod(value string) (time.Duration, error) {
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid healthcheck-start-period: %w", err)
	}
	if d < 0 {
		return 0, fmt.Errorf("healthcheck-start-period must be %s or greater", DefaultHealthcheckStartPeriod)
	}
	return d, nil
}
