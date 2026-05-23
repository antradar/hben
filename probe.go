package main

import (
	"fmt"
	"net/http"
	"time"
)

// probe sends sequential requests to measure baseline response time.
// Returns the p50 duration used to pace the ramp phase.
func probe(client *http.Client, url string, customUA string, rotateUA bool, count int, interval time.Duration, s *stats) time.Duration {
	fmt.Printf("\n[PROBE] Sending %d sequential requests (interval %s)...\n", count, interval)

	var durations []time.Duration
	for i := 0; i < count; i++ {
		ua := pickUA(customUA, rotateUA)
		start := time.Now()
		status := doRequest(client, url, ua)
		dur := time.Since(start)
		s.record(dur, status, "probe")
		durations = append(durations, dur)

		mark := "ok"
		if status >= 500 || status == 0 {
			mark = fmt.Sprintf("FAIL(%d)", status)
		}
		fmt.Printf("  [%d/%d] %6.3fs  status=%3d  ua=%s  %s\n",
			i+1, count, dur.Seconds(), status, shortUA(ua), mark)

		if i < count-1 {
			time.Sleep(interval)
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