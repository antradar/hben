package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

func main() {
	targetURL := flag.String("url", "", "Target URL (required)")
	maxConcurrency := flag.Int("max-concurrency", 5, "Peak concurrent goroutines")
	probeCount := flag.Int("probe-count", 5, "Sequential requests in probe phase")
	probeInterval := flag.Duration("probe-interval", 2*time.Second, "Delay between probe requests")
	rampSteps := flag.Int("ramp-steps", 5, "Number of concurrency increments during ramp")
	rampDuration := flag.Duration("ramp-duration", 10*time.Second, "Total ramp phase duration")
	sustainDuration := flag.Duration("sustain-duration", 30*time.Second, "How long to sustain peak load after ramp")
	timeout := flag.Duration("timeout", 30*time.Second, "Per-request HTTP timeout")
	backoffThreshold := flag.Int("backoff-threshold", 3, "Consecutive 5xx/timeout before backing off")
	backoffCooldown := flag.Duration("backoff-cooldown", 2*time.Second, "Pause duration when backoff triggered")
	noBanner := flag.Bool("no-banner", false, "Remove hben branding from User-Agent string")
	rotateUA := flag.Bool("rotate-ua", false, "Rotate UA per request from built-in pool (mimics attacker, implies -no-banner)")
	customUA := flag.String("user-agent", "", "Override User-Agent string (disables branding)")
	slowThreshold := flag.Float64("slow-threshold", 3.0, "Response times exceeding this multiple of baseline are unacceptable")
	slowMin := flag.Duration("slow-min", 50*time.Millisecond, "Minimum absolute threshold for unacceptable responses")
	tarpitThreshold := flag.Float64("tarpit-threshold", 0, "Enable tarpit detection: response times exceeding this multiple of baseline indicate CDN throttling (0=disabled)")
	lanehogCount := flag.Int("lanehog-count", -1, "Number of lanehog workers (-1=auto 25%, 0=disabled)")
	lanehogModeFlag := flag.String("lanehog-mode", "get", "Lanehog mode: get (persistent GET loop) or post (slow POST body)")
	lanehogBodySize := flag.Int("lanehog-body-size", defaultLanehogBodySize, "POST body size in bytes (post mode only)")
	lanehogChunkSize := flag.Int("lanehog-chunk-size", defaultLanehogChunkSize, "Bytes per chunk sent (post mode only)")
	lanehogChunkDelay := flag.Duration("lanehog-chunk-delay", defaultLanehogChunkDelay, "Delay between chunks (post mode only)")
	lanehogVerbose := flag.Bool("lanehog-verbose", false, "Print per-request lanehog output")
	flag.Parse()

	if *targetURL == "" {
		fmt.Fprintln(os.Stderr, "Error: -url is required")
		flag.Usage()
		os.Exit(1)
	}

	// Resolve User-Agent: explicit override > rotate > no-banner > branded default
	resolvedUA := defaultBrandedUA
	if *customUA != "" {
		resolvedUA = *customUA
	} else if *rotateUA {
		resolvedUA = "" // pickUA will rotate from pool
	} else if *noBanner {
		resolvedUA = defaultPlainUA
	}
	useRotate := *rotateUA

	// Resolve lanehog count: -1 = auto (25% of max-concurrency), 0 = disabled
	lhCount := *lanehogCount
	if lhCount < 0 {
		lhCount = *maxConcurrency / 4
		if lhCount < 1 {
			lhCount = 1
		}
	}

	// Resolve lanehog mode
	var lhMode lanehogMode
	switch *lanehogModeFlag {
	case "post":
		lhMode = lanehogPOST
	case "get", "":
		lhMode = lanehogGET
	default:
		fmt.Fprintf(os.Stderr, "Error: unknown lanehog-mode %q (use 'get' or 'post')\n", *lanehogModeFlag)
		os.Exit(1)
	}

	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Println("  Antradar HTTP Benchmark (HBEN)")
	fmt.Println("═══════════════════════════════════════════════════════")
	fmt.Printf("  Target:           %s\n", *targetURL)
	fmt.Printf("  Max concurrency:  %d\n", *maxConcurrency)
	fmt.Printf("  Probe:            %d reqs @ %s interval\n", *probeCount, *probeInterval)
	fmt.Printf("  Ramp:             %d steps over %s\n", *rampSteps, *rampDuration)
	fmt.Printf("  Sustain:          %s\n", *sustainDuration)
	fmt.Printf("  Timeout:          %s\n", *timeout)
	fmt.Printf("  Backoff:          %d consecutive → %s cooldown\n", *backoffThreshold, *backoffCooldown)
	fmt.Printf("  User-Agent:       %s\n", uaLabel(resolvedUA, useRotate))
	fmt.Printf("  Slow threshold:  %.1fx baseline (min %s)\n", *slowThreshold, *slowMin)
	if *tarpitThreshold > 0 {
		fmt.Printf("  Tarpit detect:  %.1fx baseline\n", *tarpitThreshold)
	}
	if lhCount > 0 {
		modeStr := "GET loop"
		if lhMode == lanehogPOST {
			modeStr = fmt.Sprintf("slow POST (body=%dB, chunk=%dB, delay=%s)", *lanehogBodySize, *lanehogChunkSize, *lanehogChunkDelay)
		}
		fmt.Printf("  Lanehog:          %d workers, %s\n", lhCount, modeStr)
	} else {
		fmt.Printf("  Lanehog:          off\n")
	}
	fmt.Println()

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			DisableKeepAlives:   true,
			MaxIdleConnsPerHost: -1,
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Lanehog POST mode needs a client with no timeout (slow body send takes minutes)
	var lanehogClient *http.Client
	if lhMode == lanehogPOST {
		lanehogClient = &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				DisableKeepAlives:   true,
				MaxIdleConnsPerHost: -1,
				TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
			},
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	s := &stats{}

	// Graceful shutdown on SIGINT
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Println("\n[!] Interrupt received, shutting down...")
		cancel()
	}()

	// Phase 1: Probe
	baseline := probe(ctx, client, *targetURL, resolvedUA, useRotate, *probeCount, *probeInterval, s)
	if ctx.Err() != nil {
		s.report(baseline, *slowThreshold, *slowMin, lhCount)
		return
	}

	// Lanehog probe (validate POST acceptance before committing workers)
	if lhCount > 0 && lhMode == lanehogPOST {
		resolvedMode, err := lanehogProbe(ctx, client, *targetURL, resolvedUA, useRotate, lhMode, *lanehogBodySize)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		if resolvedMode != lhMode {
			fmt.Printf("[LANEHOG] Switched from POST to GET mode\n")
			lhMode = resolvedMode
			lanehogClient = nil
		}
	}

	// Spawn lanehog workers before ramp
	var lhWg sync.WaitGroup
	if lhCount > 0 {
		modeStr := "GET"
		if lhMode == lanehogPOST {
			modeStr = "slow-POST"
		}
		fmt.Printf("\n[LANEHOG] Spawning %d workers (%s mode)\n", lhCount, modeStr)
		for i := 0; i < lhCount; i++ {
			lhWg.Add(1)
			go func(workerID int) {
				defer lhWg.Done()
				c := client
				if lhMode == lanehogPOST && lanehogClient != nil {
					c = lanehogClient
				}
				lanehogWorker(ctx, c, *targetURL, resolvedUA, useRotate,
					lhMode, *lanehogBodySize, *lanehogChunkSize, *lanehogChunkDelay, workerID, *lanehogVerbose)
			}(i)
		}
	}

	// Phase 2+3: Ramp then sustain with same workers
	rampEnd := time.Now().Add(*rampDuration)
	totalEnd := time.Now().Add(*rampDuration + *sustainDuration)

	stepDur := *rampDuration / time.Duration(*rampSteps)
	concPerStep := *maxConcurrency / *rampSteps
	if concPerStep < 1 {
		concPerStep = 1
	}

	fmt.Printf("\n[RAMP] %d → %d concurrency over %s (%d steps, baseline=%.3fs)\n",
		1, *maxConcurrency, *rampDuration, *rampSteps, baseline.Seconds())

	var wg sync.WaitGroup

	for step := 0; step < *rampSteps; step++ {
		newWorkers := concPerStep
		if step == *rampSteps-1 {
			currentTotal := step*concPerStep + newWorkers
			if currentTotal < *maxConcurrency {
				newWorkers += *maxConcurrency - currentTotal
			}
		}

		currentConc := step*concPerStep + newWorkers
		fmt.Printf("[RAMP] Step %d: +%d workers → %d concurrent\n", step+1, newWorkers, currentConc)

		for i := 0; i < newWorkers; i++ {
			wg.Add(1)
			s.incConc()
			go func(workerID int) {
				defer wg.Done()
				defer s.decConc()
				worker(ctx, client, *targetURL, resolvedUA, useRotate, rampEnd, totalEnd,
					*backoffThreshold, *backoffCooldown, *tarpitThreshold, s, baseline, workerID)
			}(step*concPerStep + i)

			if baseline > 0 {
				stagger := baseline / time.Duration(currentConc)
				if stagger < 10*time.Millisecond {
					stagger = 10 * time.Millisecond
				}
				if stagger > 2*time.Second {
					stagger = 2 * time.Second
				}
				time.Sleep(stagger)
			}
		}

		if step < *rampSteps-1 {
			select {
			case <-ctx.Done():
				wg.Wait()
				cancel() // kill lanehog workers too
				lhWg.Wait()
				s.report(baseline, *slowThreshold, *slowMin, lhCount)
				return
			case <-time.After(stepDur):
			}
		}
	}

	fmt.Printf("\n[SUSTAIN] All %d workers running for %s (backoff after %d consecutive failures)\n",
		*maxConcurrency, *sustainDuration, *backoffThreshold)

	// Wait for main workers to finish
	wg.Wait()
	// Cancel context to kill lanehog workers (even mid-request)
	cancel()
	lhWg.Wait()
	s.report(baseline, *slowThreshold, *slowMin, lhCount)
}

func worker(ctx context.Context, client *http.Client, url string, customUA string, rotateUA bool,
	rampEnd, totalEnd time.Time, backoffThreshold int, backoffCooldown time.Duration,
	tarpitMultiplier float64, s *stats, baseline time.Duration, workerID int) {

	consecutiveFails := 0
	lastPhase := "ramp"

	for time.Now().Before(totalEnd) {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ua := pickUA(customUA, rotateUA)

		// Compute tarpit deadline: if response exceeds this, cancel early
		var tarpitDeadline time.Time
		if tarpitMultiplier > 0 && baseline > 0 {
			tarpitDeadline = time.Now().Add(time.Duration(float64(baseline) * tarpitMultiplier))
		}

		start := time.Now()
		res := doRequest(ctx, client, url, ua, tarpitDeadline)
		dur := time.Since(start)

		isTarpit := res.isTarpit

		// Also detect tarpit on slow successful responses
		if !isTarpit && tarpitMultiplier > 0 && baseline > 0 && res.status >= 200 && res.status < 400 {
			tarpitThreshold := time.Duration(float64(baseline) * tarpitMultiplier)
			if dur > tarpitThreshold {
				isTarpit = true
			}
		}

		minDelay := 50 * time.Millisecond
		if baseline > 0 {
			candidate := baseline / 4
			if candidate > minDelay {
				minDelay = candidate
			}
		}
		if dur < minDelay {
			contextSleep(ctx, minDelay-dur)
		}

		phase := "ramp"
		if time.Now().After(rampEnd) {
			phase = "sustain"
		}
		if phase != lastPhase {
			consecutiveFails = 0
			lastPhase = phase
		}
		s.record(dur, res.status, phase, isTarpit)

		if isTarpit {
			fmt.Printf("  [%s] w%02d  TARPIT  %6.3fs  (>%0.0fx baseline, backing off 30s)\n",
				phase, workerID, dur.Seconds(), tarpitMultiplier)
			consecutiveFails = 0
			contextSleep(ctx, 30*time.Second)
			jitter := time.Duration(workerID%5) * 200 * time.Millisecond
			contextSleep(ctx, jitter)
			continue
		}

		if s.is5xxOrTimeout(res.status) {
			consecutiveFails++

			if phase == "sustain" && consecutiveFails >= backoffThreshold {
				s.backoffEvts.Add(1)
				fmt.Printf("  [SUSTAIN] w%02d  BACKOFF (consecutive=%d, cooling %s)\n",
					workerID, consecutiveFails, backoffCooldown)
				contextSleep(ctx, backoffCooldown)
				jitter := time.Duration(workerID%5) * 100 * time.Millisecond
				contextSleep(ctx, jitter)
				consecutiveFails = 0
				continue
			}

			if phase == "ramp" {
				fmt.Printf("  [RAMP] w%02d  %6.3fs  status=%3d  (failure)\n",
					workerID, dur.Seconds(), res.status)
			} else {
				fmt.Printf("  [SUSTAIN] w%02d  %6.3fs  status=%3d  consecutive_fails=%d\n",
					workerID, dur.Seconds(), res.status, consecutiveFails)
			}
		} else {
			if consecutiveFails > 0 {
				fmt.Printf("  [%s] w%02d  recovered (was %d consecutive fails)\n",
					phase, workerID, consecutiveFails)
			}
			consecutiveFails = 0
		}
	}
}

type requestResult struct {
	status   int
	isTarpit bool
}

func doRequest(ctx context.Context, client *http.Client, url string, ua string, tarpitDeadline time.Time) requestResult {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return requestResult{status: 0}
	}
	req.Header.Set("User-Agent", ua)

	// If we exceed the tarpit deadline, cancel the request early
	if !tarpitDeadline.IsZero() && time.Now().After(tarpitDeadline) {
		return requestResult{status: 0, isTarpit: true}
	}

	resp, err := client.Do(req)
	if err != nil {
		// Check if this was a tarpit cancellation (deadline exceeded during request)
		if !tarpitDeadline.IsZero() && time.Now().After(tarpitDeadline) {
			return requestResult{status: 0, isTarpit: true}
		}
		return requestResult{status: 0}
	}
	resp.Body.Close()
	return requestResult{status: resp.StatusCode}
}

// contextSleep sleeps for the given duration but returns early if ctx is cancelled.
func contextSleep(ctx context.Context, d time.Duration) {
	select {
	case <-time.After(d):
	case <-ctx.Done():
	}
}

func pickUA(resolvedUA string, rotate bool) string {
	if rotate {
		return uaPool[rand.Intn(len(uaPool))]
	}
	return resolvedUA
}

func shortUA(ua string) string {
	if len(ua) > 50 {
		return ua[:50] + "..."
	}
	return ua
}

func uaLabel(resolvedUA string, rotate bool) string {
	if rotate {
		return "rotate from built-in pool (18 UAs, no branding)"
	}
	// Show the full UA — it's important to see the hben branding
	return resolvedUA
}