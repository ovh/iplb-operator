/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package refresh coordinates the OVHcloud IP Load Balancer "apply" step.
//
// The IPLB API stages every write; nothing takes effect until POST
// /ipLoadbalancing/{serviceName}/refresh is called. Refresh is a service-wide,
// relatively expensive operation, and many resources (farms, frontends,
// certificates) may be changing at once. The Coordinator debounces refresh
// requests per service so a burst of changes collapses into a single apply,
// and it only calls refresh when pendingChanges reports outstanding work.
package refresh

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/ovh/go-ovh/ovh"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/ovhcloud/iplb-operator/internal/iplb"
)

// DefaultDebounce is how long the coordinator waits for additional change
// requests before applying a service's staged configuration.
const DefaultDebounce = 5 * time.Second

// Coordinator debounces and serializes refresh ("apply") operations per IPLB
// service. It implements manager.Runnable.
type Coordinator struct {
	debounce time.Duration
	log      logr.Logger

	mu      sync.Mutex
	pending map[string]*request
	queue   chan string
}

type request struct {
	svc   *iplb.Service
	timer *time.Timer
}

// New returns a Coordinator with the given debounce window (0 uses
// DefaultDebounce).
func New(debounce time.Duration) *Coordinator {
	if debounce <= 0 {
		debounce = DefaultDebounce
	}
	return &Coordinator{
		debounce: debounce,
		log:      ctrl.Log.WithName("refresh-coordinator"),
		pending:  map[string]*request{},
		queue:    make(chan string, 256),
	}
}

// RequestRefresh schedules an apply for the given service. Repeated calls
// within the debounce window are coalesced into a single refresh. svc carries
// the authenticated client used to perform the apply.
func (c *Coordinator) RequestRefresh(serviceName string, svc *iplb.Service) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if req, ok := c.pending[serviceName]; ok {
		req.svc = svc
		req.timer.Reset(c.debounce)
		return
	}

	name := serviceName
	timer := time.AfterFunc(c.debounce, func() {
		// Hand the key to the worker; keep the entry so further requests
		// during the apply schedule another pass.
		select {
		case c.queue <- name:
		default:
			c.log.Info("refresh queue full, dropping coalesced request", "service", name)
		}
	})
	c.pending[serviceName] = &request{svc: svc, timer: timer}
}

// Start runs the worker loop until the context is cancelled. It satisfies
// manager.Runnable so it can be registered with mgr.Add.
func (c *Coordinator) Start(ctx context.Context) error {
	c.log.Info("starting refresh coordinator", "debounce", c.debounce)
	for {
		select {
		case <-ctx.Done():
			return nil
		case name := <-c.queue:
			c.process(ctx, name)
		}
	}
}

func (c *Coordinator) process(ctx context.Context, serviceName string) {
	c.mu.Lock()
	req := c.pending[serviceName]
	delete(c.pending, serviceName)
	c.mu.Unlock()
	if req == nil || req.svc == nil {
		return
	}

	log := c.log.WithValues("service", serviceName)

	pending, err := req.svc.HasPendingChanges()
	if err != nil {
		log.Error(err, "failed to check pending changes; will retry on next request")
		return
	}
	if !pending {
		log.V(1).Info("no pending changes, skipping refresh")
		return
	}

	if err := req.svc.Refresh(); err != nil {
		// A 4xx means the staged configuration cannot be applied as-is (e.g.
		// "Can not refresh: this Load Balancer has no Frontend"). Re-arming would
		// just hot-loop; the change that makes it applyable (a reconcile staging a
		// frontend, etc.) re-requests a refresh on its own. Log quietly and stop.
		if isClientError(err) {
			log.V(1).Info("refresh not applicable yet; waiting for configuration to change",
				"reason", err.Error())
			return
		}
		log.Error(err, "refresh failed; will retry on next request")
		// Transient (5xx/network): re-arm so a subsequent attempt retries.
		c.RequestRefresh(serviceName, req.svc)
		return
	}
	log.Info("applied staged configuration")
}

// isClientError reports whether err is an OVHcloud API 4xx that needs the staged
// configuration to change before a refresh can succeed. 429 (rate limited) is
// excluded: it is transient and should be retried.
func isClientError(err error) bool {
	var apiErr *ovh.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Code >= 400 && apiErr.Code < 500 && apiErr.Code != 429
}

// NeedLeaderElection ensures only the leader applies configuration, avoiding
// concurrent refreshes from multiple replicas.
func (c *Coordinator) NeedLeaderElection() bool { return true }
