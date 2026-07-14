package workflows

import (
	"log"
	"os"
	"strconv"
	"time"
)

// Tunables for the debounce/batching behavior, overridable via env so they
// can be adjusted without a rebuild:
//
//   - ALERT_DEBOUNCE / ALERT_MAX_WINDOW control how long a group waits to
//     see if more alerts show up before it's flushed at all.
//   - MASS_ALERT_THRESHOLD controls what happens once it does flush: if
//     more than this many alerts became "unsent" together, they're
//     combined into one Slack message; otherwise each gets its own
//     message. This is what keeps a single alert as a single message
//     while still collapsing a genuine mass-failure burst.
var (
	debounceWindow = envDuration("ALERT_DEBOUNCE", 8*time.Second)
	maxGroupWindow = envDuration("ALERT_MAX_WINDOW", 45*time.Second)
	massThreshold  = envInt("MASS_ALERT_THRESHOLD", 5)
)

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %s: %v", key, v, def, err)
		return def
	}
	return d
}

func envInt(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("invalid %s=%q, using default %d: %v", key, v, def, err)
		return def
	}
	return n
}
