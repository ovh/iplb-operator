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

package controller

import (
	"context"
	"errors"
	"time"

	"github.com/ovh/go-ovh/ovh"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/iplb"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
)

// requeuePending is how long to wait before re-checking whether a service's
// staged changes have been applied. It is a little longer than the refresh
// coordinator's debounce window so a single apply usually settles things.
const requeuePending = 10 * time.Second

// IPLB protocols. UDP is restricted: its farms/frontends/servers support far
// fewer fields than http/tcp (no balance, stickiness, probe, ssl, routes, …).
const (
	protocolHTTP = "http"
	protocolTCP  = "tcp"
	protocolUDP  = "udp"
)

// requeueFreeCert is how long to wait between polls while a managed Let's
// Encrypt certificate is being issued (ACME validation takes a few minutes).
const requeueFreeCert = 30 * time.Second

// requeueTerminal is how long to wait before retrying after a terminal
// configuration error (a 4xx from the OVHcloud API). It is deliberately long:
// such errors need a spec/permission fix, so retrying fast only spams logs.
const requeueTerminal = 2 * time.Minute

// isNotFound reports whether err is an OVHcloud API 404. Used to make deletes
// idempotent (an already-gone object is success).
func isNotFound(err error) bool {
	var apiErr *ovh.APIError
	return errors.As(err, &apiErr) && apiErr.Code == 404
}

// isTerminalAPIError reports whether err is an OVHcloud API client error (4xx)
// that will keep failing until the configuration or permissions are fixed.
// Such errors should surface on the resource status rather than being retried
// in a tight loop. Server-side (5xx) and rate-limit (429) errors are transient
// and excluded so they keep their normal exponential backoff.
func isTerminalAPIError(err error) bool {
	var apiErr *ovh.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.Code {
	case 400, 401, 403, 404, 409, 422:
		return true
	default:
		return false
	}
}

// serviceFor builds an IPLB Service (authenticated client scoped to one
// serviceName) from a ServiceRef.
func serviceFor(ctx context.Context, f ovhclient.Factory, ref iplbv1alpha1.ServiceRef, namespace string) (*iplb.Service, error) {
	c, err := f.ClientFor(ctx, ref, namespace)
	if err != nil {
		return nil, err
	}
	return iplb.New(c, ref.ServiceName), nil
}

// setReady updates the Ready condition on a resource's condition slice.
func setReady(conds *[]metav1.Condition, generation int64, status metav1.ConditionStatus, reason, msg string) {
	meta.SetStatusCondition(conds, metav1.Condition{
		Type:               iplbv1alpha1.ConditionReady,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: generation,
	})
}

// ---- small pointer / comparison helpers ----

func intp(v int) *int       { return &v }
func boolp(v bool) *bool    { return &v }
func int64p(v int64) *int64 { return &v }

func intVal(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}

func intpEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func int64pEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func boolpEqual(a, b *bool) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func strSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func defaultStr(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
