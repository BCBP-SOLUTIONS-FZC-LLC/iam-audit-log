package main

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

type errorLogger interface {
	Error(msg string, fields map[string]any)
}

// supervise runs a background loop (catalog poller, export worker, ops
// monitor) until ctx ends. A panic in it is recovered and logged with its
// stack, and the loop restarts after backoff. Unrecovered, the panic would
// crash every in-flight request and consumer in the pod; a silently dead
// loop would leave the plan map, exports or alert gauges frozen.
func supervise(ctx context.Context, log errorLogger, name string, backoff time.Duration, run func(context.Context)) {
	for {
		panicked := func() (p bool) {
			defer func() {
				if r := recover(); r != nil {
					p = true
					log.Error("background loop panicked — restarting", map[string]any{
						"loop": name, "panic": fmt.Sprint(r), "stack": string(debug.Stack()),
					})
				}
			}()
			run(ctx)
			return false
		}()
		if !panicked || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}
