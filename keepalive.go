package main

import (
	"context"
	"sync"
	"time"

	sdkruntime "github.com/Silo-Server/silo-plugin-sdk/pkg/pluginsdk/runtime"
)

// Silo stops a plugin process that has gone 30 minutes without use. Only
// calls between Silo and the plugin count as use: Silo calling the plugin (a
// scheduled task, an HTTP route such as the webhook) or the plugin calling
// back through the runtime host. The REST calls the plugin makes with its API
// key don't count.
//
// A sync can't run inside the scheduled-task call, because Silo gives that
// call only 10 seconds, and a webhook sync runs on the plugin's own timer
// anyway. So while a run is in progress or scheduled, the plugin calls the
// runtime host every few minutes, which keeps Silo from stopping it mid-run or
// before a delayed webhook sync starts.
var heartbeatEvery = 5 * time.Minute

var heartbeatOnce sync.Once

// startHeartbeat starts the keep-alive loop once per process.
func (p *plugin) startHeartbeat() {
	heartbeatOnce.Do(func() {
		go func() {
			t := time.NewTicker(heartbeatEvery)
			defer t.Stop()
			for range t.C {
				p.heartbeat()
			}
		}()
	})
}

// busy reports whether a run is in progress or a webhook sync is waiting to
// start.
func (p *plugin) busy() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running || p.timer != nil
}

// heartbeat makes one cheap runtime-host call when the plugin has work in
// progress or pending. An idle plugin sends nothing, so Silo can still stop
// it when there's nothing to do.
func (p *plugin) heartbeat() {
	if !p.busy() {
		return
	}
	host := sdkruntime.Host()
	if host == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := host.GetHostInfo(ctx); err != nil {
		p.log.Warn("keep-alive call to Silo failed", "error", err)
	}
}
