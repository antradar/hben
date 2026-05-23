package main

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// probe sends sequential requests to measure baseline response time.
// Returns the p50 duration used to pace the ramp phase.
func probe(ctx context.Context, client *http.Client, url string, customUA string, rotateUA bool, count int, interval time.Duration, s *stats) time.Duration {
	fmt.Printf("\n[PROBE] Sending %d sequential requests (interval %s)...\n", count, interval)

	var durations []time.Duration
	for i := 0; i < count; i++ {
		select {
		case <-ctx.Done():
			return p50(durations)
		default:
		}
		ua := pickUA(customUA, rotateUA)
		start := time.Now()
		res := doRequest(ctx, client, url, ua, time.Time{})
		dur := time.Since(start)
		s.record(dur, res.status, "probe", false)
		durations = append(durations, dur)

		mark := "ok"
		if res.status >= 500 || res.status == 0 {
			mark = fmt.Sprintf("FAIL(%d)", res.status)
		}
		fmt.Printf("  [%d/%d] %6.3fs  status=%3d  ua=%s  %s\n",
			i+1, count, dur.Seconds(), res.status, shortUA(ua), mark)

		if i < count-1 {
			select {
			case <-time.After(interval):
			case <-ctx.Done():
				return p50(durations)
			}
		}
	}

	baseline := p50(durations)
	fmt.Printf("[PROBE] Baseline p50: %.3fs\n", baseline.Seconds())
	return baseline
}

func p50(durations []time.Duration) time.Duration {
	if len(durations) == 0 {
		return time.Second
	}
	sorted := make([]time.Duration, len(durations))
	copy(sorted, durations)
	sortDurations(sorted)
	return sorted[len(sorted)*50/100]
}

func sortDurations(d []time.Duration) {
	for i := 1; i < len(d); i++ {
		for j := i; j > 0 && d[j] < d[j-1]; j-- {
			d[j], d[j-1] = d[j-1], d[j]
		}
	}
}