package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/axiom-studio/openseal/pkg/computer/bridge"
)

// Concurrency is how many calls run at once.
const Concurrency = 4

// resultTimeout bounds sending one result, retries included.
const resultTimeout = time.Minute

// maxBackoff bounds the wait between polls after a network error.
const maxBackoff = 30 * time.Second

// ServeOptions tune Serve; the zero value is the real bridge.
type ServeOptions struct {
	// Log receives one line per call and per connection problem. Commands,
	// paths and contents are never logged.
	Log io.Writer
	// Execute runs a call; the package Execute when nil.
	Execute func(context.Context, bridge.Call) bridge.Result
	// Backoff is the first wait after a failed poll; 1s when zero.
	Backoff time.Duration
	// Heartbeat is how often to tell HelloSeal this computer is still there
	// while every slot is busy; bridge.HeartbeatSeconds when zero.
	Heartbeat time.Duration
}

// Serve long-polls for calls and answers them, up to Concurrency at a time,
// until ctx ends (nil) or HelloSeal no longer knows this computer
// (ErrDisconnected).
func Serve(ctx context.Context, api *API, options ServeOptions) error {
	execute := options.Execute
	if execute == nil {
		execute = Execute
	}
	log := options.Log
	if log == nil {
		log = io.Discard
	}
	first := options.Backoff
	if first <= 0 {
		first = time.Second
	}
	heartbeat := options.Heartbeat
	if heartbeat <= 0 {
		heartbeat = bridge.HeartbeatSeconds * time.Second
	}
	// On the way out, stop running calls (cancel runs first) and wait for
	// them to send their results.
	var running sync.WaitGroup
	defer running.Wait()
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	slots := make(chan struct{}, Concurrency)
	backoff := first
	online := false
	for {
		// Take a slot before polling, so a fifth call waits on HelloSeal
		// rather than being claimed and left idle here.
		select {
		case slots <- struct{}{}:
		case <-ctx.Done():
			return serveEnd(ctx)
		default:
			if err := waitForSlot(ctx, api, slots, heartbeat, log); err != nil {
				return err
			}
		}
		call, err := api.Next(ctx, bridge.LongPollSeconds)
		if err != nil {
			<-slots
			if errors.Is(err, ErrDisconnected) {
				return ErrDisconnected
			}
			if ctx.Err() != nil {
				return serveEnd(ctx)
			}
			if online {
				fmt.Fprintf(log, "Lost the connection to HelloSeal (%v). Retrying...\n", err)
				online = false
			}
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return serveEnd(ctx)
			}
			backoff = min(backoff*2, maxBackoff)
			continue
		}
		if !online {
			fmt.Fprintln(log, "Connected. Waiting for calls.")
			online = true
		}
		backoff = first
		if call == nil {
			<-slots
			continue
		}
		// The deadline is on HelloSeal's clock; run against this one's.
		if call.DeadlineUnixMs > 0 {
			call.DeadlineUnixMs -= api.ServerOffset().Milliseconds()
		}
		running.Add(1)
		go func(call bridge.Call) {
			defer running.Done()
			defer func() { <-slots }()
			started := time.Now()
			result := execute(ctx, call)
			postCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), resultTimeout)
			defer stop()
			if err := api.PostResult(postCtx, result); err != nil {
				if errors.Is(err, ErrDisconnected) {
					cancel(ErrDisconnected)
					return
				}
				fmt.Fprintf(log, "%s: could not send the result: %v\n", call.Op, err)
				return
			}
			outcome := "done"
			if !result.OK && result.Error != nil {
				outcome = result.Error.Code
			}
			fmt.Fprintf(log, "%s: %s in %s\n", call.Op, outcome, time.Since(started).Round(time.Millisecond))
		}(*call)
	}
}

// waitForSlot waits until a call slot frees, sending a heartbeat every
// interval meanwhile: HelloSeal only hears from a computer when it polls,
// and long commands in every slot must not make it look offline. It returns
// nil with a slot taken, or Serve's result when Serve must end.
func waitForSlot(ctx context.Context, api *API, slots chan struct{}, interval time.Duration, log io.Writer) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case slots <- struct{}{}:
			return nil
		case <-ctx.Done():
			return serveEnd(ctx)
		case <-ticker.C:
			err := api.Heartbeat(ctx)
			switch {
			case errors.Is(err, ErrDisconnected):
				return ErrDisconnected
			case err != nil && ctx.Err() == nil:
				// The next heartbeat or poll tries again.
				fmt.Fprintf(log, "Could not reach HelloSeal (%v). Retrying...\n", err)
			}
		}
	}
}

func serveEnd(ctx context.Context) error {
	if errors.Is(context.Cause(ctx), ErrDisconnected) {
		return ErrDisconnected
	}
	return nil
}
