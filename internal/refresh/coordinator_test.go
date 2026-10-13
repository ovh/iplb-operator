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

package refresh

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ovh/go-ovh/ovh"

	"github.com/ovhcloud/iplb-operator/internal/fakeovh"
	"github.com/ovhcloud/iplb-operator/internal/iplb"
)

func TestIsClientError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain", fmt.Errorf("boom"), false},
		{"no-frontend 400", &ovh.APIError{Code: 400, Message: "Can not refresh: this Load Balancer has no Frontend"}, true},
		{"404", &ovh.APIError{Code: 404}, true},
		{"wrapped 400", fmt.Errorf("refresh: %w", &ovh.APIError{Code: 400}), true},
		{"500 transient", &ovh.APIError{Code: 500}, false},
		{"429 transient", &ovh.APIError{Code: 429}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClientError(tc.err); got != tc.want {
				t.Fatalf("isClientError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestCoalescesBurstIntoSingleRefresh(t *testing.T) {
	fake := fakeovh.New()
	fake.PendingCount = 3 // simulate staged changes
	svc := iplb.New(fake, "svc")

	c := New(40 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	// A burst of requests within the debounce window must collapse to one apply.
	for i := 0; i < 10; i++ {
		c.RequestRefresh("svc", svc)
		time.Sleep(2 * time.Millisecond)
	}

	waitFor(t, time.Second, func() bool { return fake.RefreshCount >= 1 })
	time.Sleep(80 * time.Millisecond) // allow any stray timers to fire

	if fake.RefreshCount != 1 {
		t.Fatalf("expected exactly 1 refresh, got %d", fake.RefreshCount)
	}
	if fake.PendingCount != 0 {
		t.Fatalf("expected pending changes cleared, got %d", fake.PendingCount)
	}
}

func TestSkipsRefreshWhenNoPendingChanges(t *testing.T) {
	fake := fakeovh.New() // PendingCount == 0
	svc := iplb.New(fake, "svc")

	c := New(20 * time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = c.Start(ctx) }()

	c.RequestRefresh("svc", svc)
	time.Sleep(120 * time.Millisecond)

	if fake.RefreshCount != 0 {
		t.Fatalf("expected no refresh when nothing is pending, got %d", fake.RefreshCount)
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before timeout")
}
