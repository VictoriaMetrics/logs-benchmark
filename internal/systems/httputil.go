package systems

// Shared HTTP plumbing: one tuned client and the request helpers every system
// uses - doReq (ingest, retrying), doQuery (query, one-shot), and waitHTTP
// (readiness poll). None of it knows about the benchmark workload.

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// httpClient raises the idle connection limits so workers reuse TCP connections.
var httpClient = &http.Client{
	Timeout: 5 * time.Minute,
	Transport: &http.Transport{
		MaxIdleConns:        128,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     30 * time.Second,
	},
}

// maxRetryDuration bounds the total time doReq spends on one request, including the
// backoff and the requests themselves.
const maxRetryDuration = 30 * time.Second

// doReq sends a request and retries on 429/502/503/504 and transport errors with
// exponential backoff, giving up after maxRetryDuration or when ctx is cancelled.
// Returns the final HTTP status alongside the body. status is 0 when the request
// never got a response.
func doReq(ctx context.Context, method, reqURL, contentType string, body []byte) (respBody []byte, status int, retries int, err error) {
	ctx, cancel := context.WithTimeout(ctx, maxRetryDuration)
	defer cancel()
	backoff := 100 * time.Millisecond
	for {
		req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(body))
		if err != nil {
			return nil, 0, retries, err
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		resp, err := httpClient.Do(req)
		switch {
		case err == nil:
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			status = resp.StatusCode
			if status >= 200 && status < 300 {
				return rb, status, retries, nil
			}
			if status != 429 && status != 502 && status != 503 && status != 504 {
				return nil, status, retries, fmt.Errorf("%s %s: status %d: %s", method, reqURL, status, truncate(string(rb), 300))
			}
			// retryable status: back off below
		case ctx.Err() != nil:
			// the retry budget ran out during the request
			return nil, status, retries, fmt.Errorf("%s %s: giving up after %s: %w", method, reqURL, maxRetryDuration, err)
		}
		// transport error (budget still live) or retryable status: back off,
		// unless the budget runs out while we wait
		retries++
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, status, retries, fmt.Errorf("%s %s: giving up after %s (last status %d)", method, reqURL, maxRetryDuration, status)
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}

// doQuery sends one request and returns the fully-read body (no retry).
func doQuery(ctx context.Context, method, reqURL, contentType string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	rb, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: status %d: %s", method, reqURL, resp.StatusCode, truncate(string(rb), 300))
	}
	return rb, nil
}

var (
	waitHTTPDeadline       = 3 * time.Minute
	waitHTTPAttemptTimeout = 10 * time.Second
)

// waitHTTP polls reqURL until ok accepts the response or waitHTTPDeadline elapses.
// ok decides what "ready" means for the caller (a 2xx, a parsed health status, ...).
func waitHTTP(reqURL string, ok func(*http.Response, []byte) error) error {
	deadline := time.Now().Add(waitHTTPDeadline)
	var lastErr error
	for time.Now().Before(deadline) {
		remaining := time.Until(deadline)
		timeout := min(waitHTTPAttemptTimeout, remaining)
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		req, err := http.NewRequestWithContext(ctx, "GET", reqURL, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := httpClient.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			resp.Body.Close()
			if readErr != nil {
				lastErr = readErr
			} else if err := ok(resp, body); err != nil {
				lastErr = err
			} else {
				cancel()
				return nil
			}
		} else {
			lastErr = err
		}
		cancel()
		if sleep := min(2*time.Second, time.Until(deadline)); sleep > 0 {
			time.Sleep(sleep)
		}
	}
	if lastErr != nil {
		return fmt.Errorf("timeout waiting for %s: last error: %w", reqURL, lastErr)
	}
	return fmt.Errorf("timeout waiting for %s", reqURL)
}

func status2xx(r *http.Response, _ []byte) error {
	if r.StatusCode >= 200 && r.StatusCode < 300 {
		return nil
	}
	return fmt.Errorf("status %d", r.StatusCode)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
