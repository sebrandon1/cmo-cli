package cert

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseWindow reads an expiry window: a Go duration (72h, 90m) or a number of
// days (30d), which time.ParseDuration doesn't accept.
func ParseWindow(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("window %q: want a number of days, such as 30d", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("window %q: want a duration such as 30d or 72h", s)
	}
	return d, nil
}

// Expiring are the certificates that expire before now+within, or already
// have. A certificate not yet issued has no expiry and is left out.
func Expiring(list []Summary, within time.Duration, now time.Time) []Summary {
	limit := now.Add(within)
	out := []Summary{} // [] rather than null in -o json
	for _, s := range list {
		if !s.NotAfter.IsZero() && s.NotAfter.Before(limit) {
			out = append(out, s)
		}
	}
	return out
}
