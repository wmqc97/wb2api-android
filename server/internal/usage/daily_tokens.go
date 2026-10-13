package usage

import (
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
)

// DailyAccountTokens reads today's hourly buckets using their recorded last
// event timestamp, which also works when a usage file came from a UTC container.
func (r *Recorder) DailyAccountTokens(now time.Time) map[string]map[string]pool.DailyModelTokens {
	out := map[string]map[string]pool.DailyModelTokens{}
	if r == nil {
		return out
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	day := pool.DailyTokenDay(now)
	for _, b := range r.buckets {
		t, ok := bucketStart(b.Scope)
		if b.Last > 0 {
			t = time.Unix(b.Last, 0)
			ok = true
		}
		if !ok || pool.DailyTokenDay(t) != day {
			continue
		}
		if out[b.UID] == nil {
			out[b.UID] = map[string]pool.DailyModelTokens{}
		}
		m := pool.CanonicalTokenModel(b.Model)
		u := out[b.UID][m]
		u.Day = day
		u.Input += b.PT
		u.Output += b.CT
		u.Total += b.TT
		out[b.UID][m] = u
	}
	return out
}
