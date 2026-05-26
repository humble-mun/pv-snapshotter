//go:build linux

package config

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
)

const (
	waitPollInterval   = 2 * time.Second
	waitHTTPTimeout    = waitPollInterval
	waitRetryBaseLevel = 10 // verbosity level used on the first retry
)

// WaitUntilReady blocks until the pv-snapshotter daemon's /readyz endpoint
// returns HTTP 200, or until ctx is cancelled.
//
// The daemon exposes both gRPC and HTTP (h2c) on the same Unix socket.
// We probe over HTTP because /readyz reflects application-level readiness
// (all registered readiness checks pass), not just socket-open readiness.
//
// Retry log verbosity decreases with each attempt: the first retries are
// logged at V(waitRetryBaseLevel) and become progressively more visible,
// reaching V(1) after waitRetryBaseLevel-1 retries and staying there.
//
// Returns ctx.Err() if the context is cancelled before the daemon becomes
// ready, nil once /readyz returns 200.
func WaitUntilReady(ctx context.Context, logger logr.Logger, socketPath string) error {
	logger.Info("waiting for pv-snapshotter daemon to become ready",
		"socket", socketPath, "probe", "/readyz")

	client := unixHTTPClient(socketPath)

	for times := 0; ; times++ {
		if err := probeReadyz(ctx, client); err == nil {
			logger.Info("pv-snapshotter daemon is ready", "socket", socketPath)
			return nil
		}

		level := waitRetryBaseLevel - times
		if level < 1 {
			level = 1
		}
		logger.V(level).Info("daemon not yet ready, retrying",
			"socket", socketPath, "interval", waitPollInterval, "attempt", times+1)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitPollInterval):
		}
	}
}

// unixHTTPClient returns an *http.Client that dials socketPath as a Unix
// domain socket.  The Host header value is irrelevant for Unix sockets but
// must be a valid non-empty string; we use "localhost".
func unixHTTPClient(socketPath string) *http.Client {
	return &http.Client{
		Timeout: waitHTTPTimeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
}

// probeReadyz issues a single HTTP GET http://localhost/readyz through the
// Unix socket client and returns nil iff the response status is 200.
func probeReadyz(ctx context.Context, client *http.Client) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost/readyz", nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("readyz returned %d", resp.StatusCode)
	}
	return nil
}
