package main

import (
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type result struct {
	ts       time.Time
	duration time.Duration
	status   int  // 0 = timeout/error, otherwise HTTP status
	phase    string
	isTarpit bool
}

type stats struct {
	mu          sync.Mutex
	results     []result
	totalReqs   atomic.Int64
	successReqs atomic.Int64
	failReqs    atomic.Int64
	backoffEvts atomic.Int64
	tarpitReqs  atomic.Int64
	curConc     atomic.Int32
	peakConc    atomic.Int32
}

func (s *stats) incConc() {
	v := s.curConc.Add(1)
	// Track peak
	for {
		p := s.peakConc.Load()
		if v <= p || s.peakConc.CompareAndSwap(p, v) {
			break
		}
	}
}

func (s *stats) decConc() {
	s.curConc.Add(-1)
}

func (s *stats) record(d time.Duration, status int, phase string, isTarpit bool) {
	r := result{ts: time.Now(), duration: d, status: status, phase: phase, isTarpit: isTarpit}
	s.mu.Lock()
	s.results = append(s.results, r)
	s.mu.Unlock()
	s.totalReqs.Add(1)
	if isTarpit {
		s.tarpitReqs.Add(1)
	} else if status >= 200 && status < 400 {
		s.successReqs.Add(1)
	} else {
		s.failReqs.Add(1)
	}
}

func (s *stats) is5xxOrTimeout(status int) bool {
	return status >= 500 || status == 0
}

func (s *stats) durations() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := make([]time.Duration, len(s.results))
	for i, r := range s.results {
		d[i] = r.duration
	}
	return d
}

func percentiles(durations []time.Duration) (p50, p95, p99, max time.Duration) {
	n := len(durations)
	if n == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(durations, func(i, j int) bool {
		return durations[i] < durations[j]
	})
	p50 = durations[n*50/100]
	p95 = durations[n*95/100]
	if n > 1 {
		p99 = durations[n*99/100]
	} else {
		p99 = durations[0]
	}
	max = durations[n-1]
	return
}

func (s *stats) report(targetURL string, baseline time.Duration, slowMultiplier float64, slowMin time.Duration, lanehogCount int) {
	s.mu.Lock()
	res := make([]result, len(s.results))
	copy(res, s.results)
	s.mu.Unlock()

	total := s.totalReqs.Load()
	fail := s.failReqs.Load()
	backoff := s.backoffEvts.Load()
	tarpit := s.tarpitReqs.Load()
	peakConc := s.peakConc.Load()

	// Count acceptable vs unacceptable in sustain phase only
	// Threshold is the larger of (baseline * multiplier) or slowMin
	slowThreshold := time.Duration(float64(baseline) * slowMultiplier)
	if slowThreshold < slowMin {
		slowThreshold = slowMin
	}
	var acceptable, unacceptable, sustainTotal int
	for _, r := range res {
		if r.phase != "sustain" {
			continue
		}
		if r.isTarpit {
			continue
		}
		sustainTotal++
		if r.status >= 200 && r.status < 400 && r.duration <= slowThreshold {
			acceptable++
		} else {
			unacceptable++
		}
	}
	acceptablePct := 0.0
	if sustainTotal > 0 {
		acceptablePct = float64(acceptable) / float64(sustainTotal) * 100
	}

	fmt.Println()
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("  HBEN REPORT")
	fmt.Printf("  Target:           %s\n", targetURL)
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  Total requests:   %d\n", total)
	fmt.Printf("  Acceptable:       %d/%d  (%.1f%%)  [threshold: %.1fx baseline = %.3fs]\n", acceptable, sustainTotal, acceptablePct, slowMultiplier, slowThreshold.Seconds())
	fmt.Printf("  Unacceptable:     %d/%d  (%.1f%%)\n", unacceptable, sustainTotal, 100-acceptablePct)
	fmt.Printf("  HTTP failures:    %d\n", fail)
	fmt.Printf("  Backoff events:   %d\n", backoff)
	if tarpit > 0 {
		fmt.Printf("  Tarpit responses: %d  (excluded from stats, CDN throttling detected)\n", tarpit)
	}
	fmt.Printf("  Peak concurrency: %d\n", peakConc)
	if lanehogCount > 0 {
		fmt.Printf("  Lanehog workers:  %d  (not counted in stats)\n", lanehogCount)
	}

	// Per-phase breakdown
	phases := []string{"probe", "ramp", "sustain"}
	for _, phase := range phases {
		var durations []time.Duration
		var phaseOk, phaseFail, phaseAccept, phaseUnaccept int
		statusDist := map[int]int{}
		for _, r := range res {
			if r.phase != phase {
				continue
			}
			durations = append(durations, r.duration)
			if r.isTarpit {
				continue
			}
			if r.status >= 200 && r.status < 400 {
				phaseOk++
			} else {
				phaseFail++
				statusDist[r.status]++
			}
			if r.status >= 200 && r.status < 400 && r.duration <= slowThreshold {
				phaseAccept++
			} else {
				phaseUnaccept++
			}
		}
		if len(durations) == 0 {
			continue
		}
		p50, p95, p99, max := percentiles(durations)
		phaseAcceptPct := 0.0
		if len(durations) > 0 {
			phaseAcceptPct = float64(phaseAccept) / float64(len(durations)) * 100
		}
		fmt.Println()
		fmt.Printf("  ── Phase: %-10s  (%d requests) ──\n", phase, len(durations))
		fmt.Printf("     p50: %7.3fs   p95: %7.3fs   p99: %7.3fs   max: %7.3fs\n",
			p50.Seconds(), p95.Seconds(), p99.Seconds(), max.Seconds())
		fmt.Printf("     acceptable: %d/%d (%.1f%%)\n", phaseAccept, len(durations), phaseAcceptPct)
		fmt.Printf("     ok: %d  fail: %d\n", phaseOk, phaseFail)
		if len(statusDist) > 0 {
			fmt.Print("     fail status codes: ")
			codes := make([]int, 0, len(statusDist))
			for c := range statusDist {
				codes = append(codes, c)
			}
			sort.Ints(codes)
			for _, c := range codes {
				fmt.Printf("%d=%d  ", c, statusDist[c])
			}
			fmt.Println()
		}
	}
	fmt.Println("═══════════════════════════════════════════════════════")
}