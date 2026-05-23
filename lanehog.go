package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

const (
	defaultLanehogBodySize   = 2048
	defaultLanehogChunkSize  = 32
	defaultLanehogChunkDelay = 500 * time.Millisecond
)

type lanehogMode int

const (
	lanehogGET  lanehogMode = iota // persistent GET loop
	lanehogPOST                    // slow POST body
)

// slowReader drips bytes from a buffer at a controlled rate.
// Respects context cancellation between chunks.
type slowReader struct {
	data       []byte
	pos        int
	chunkSize  int
	chunkDelay time.Duration
	ctx        context.Context
}

func (r *slowReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	// Sleep between chunks, but abort early if context is cancelled
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-time.After(r.chunkDelay):
	}
	n = r.chunkSize
	if n > len(p) {
		n = len(p)
	}
	if r.pos+n > len(r.data) {
		n = len(r.data) - r.pos
	}
	copy(p, r.data[r.pos:r.pos+n])
	r.pos += n
	return n, nil
}

// lanehogProbe validates the lanehog mode before committing workers.
// For POST mode: sends a test POST to check if the endpoint accepts it.
// Returns the resolved mode (may fall back to GET) and an error if unrecoverable.
func lanehogProbe(ctx context.Context, client *http.Client, url string, customUA string, rotateUA bool, mode lanehogMode, bodySize int) (lanehogMode, error) {
	if mode != lanehogPOST {
		return mode, nil
	}

	fmt.Println("[LANEHOG] Probing POST acceptance...")

	// Send a complete POST (no slow delays — just verify the endpoint accepts it)
	body := make([]byte, bodySize)
	reader := &slowReader{data: body, chunkSize: bodySize, chunkDelay: 0, ctx: context.Background()}
	req, err := http.NewRequestWithContext(ctx, "POST", url, io.NopCloser(reader))
	if err != nil {
		return lanehogGET, fmt.Errorf("POST probe: %w (falling back to GET)", err)
	}
	req.Header.Set("User-Agent", pickUA(customUA, rotateUA))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(bodySize)

	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[LANEHOG] POST probe failed: %v — falling back to GET\n", err)
		return lanehogGET, nil
	}
	resp.Body.Close()

	switch resp.StatusCode {
	case 200, 201, 202, 204:
		fmt.Printf("[LANEHOG] POST accepted (status %d)\n", resp.StatusCode)
		return lanehogPOST, nil
	case 405, 403:
		fmt.Printf("[LANEHOG] POST rejected (status %d) — falling back to GET\n", resp.StatusCode)
		return lanehogGET, nil
	case 413:
		return lanehogGET, fmt.Errorf("POST body too large (413) — reduce -lanehog-body-size or use GET mode")
	default:
		fmt.Printf("[LANEHOG] POST returned unexpected status %d — falling back to GET\n", resp.StatusCode)
		return lanehogGET, nil
	}
}

// lanehogWorker sends requests slowly in a loop to tie up upstream workers.
// It does NOT record to stats — its impact is implicit.
// Uses ctx for immediate cancellation when the main test ends.
func lanehogWorker(ctx context.Context, client *http.Client, url string, customUA string, rotateUA bool,
	mode lanehogMode, bodySize int, chunkSize int, chunkDelay time.Duration, workerID int, verbose bool) {

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ua := pickUA(customUA, rotateUA)

		if mode == lanehogPOST {
			lanehogPOSTRequest(ctx, client, url, ua, bodySize, chunkSize, chunkDelay, workerID, verbose)
		} else {
			lanehogGETRequest(ctx, client, url, ua, workerID, verbose)
		}
	}
}

func lanehogPOSTRequest(ctx context.Context, client *http.Client, url string, ua string,
	bodySize int, chunkSize int, chunkDelay time.Duration, workerID int, verbose bool) {

	body := make([]byte, bodySize)
	reader := &slowReader{data: body, chunkSize: chunkSize, chunkDelay: chunkDelay, ctx: ctx}

	req, err := http.NewRequestWithContext(ctx, "POST", url, io.NopCloser(reader))
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(bodySize)

	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if verbose {
			fmt.Printf("  [LANEHOG] h%02d  POST error after %6.1fs: %v\n", workerID, dur.Seconds(), err)
		}
		return
	}
	resp.Body.Close()
	if verbose {
		fmt.Printf("  [LANEHOG] h%02d  POST done in %6.1fs  status=%d\n", workerID, dur.Seconds(), resp.StatusCode)
	}
}

func lanehogGETRequest(ctx context.Context, client *http.Client, url string, ua string, workerID int, verbose bool) {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return
	}
	req.Header.Set("User-Agent", ua)

	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		if verbose {
			fmt.Printf("  [LANEHOG] h%02d  GET error after %6.1fs\n", workerID, dur.Seconds())
		}
		return
	}
	resp.Body.Close()
	if verbose {
		fmt.Printf("  [LANEHOG] h%02d  GET done in %6.1fs  status=%d\n", workerID, dur.Seconds(), resp.StatusCode)
	}
}