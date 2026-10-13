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

package iplb

import (
	"encoding/json"
	"testing"
)

// rawClient returns a canned JSON body for any GET, mimicking the real
// OVHcloud API response shape.
type rawClient struct{ getBody string }

func (c rawClient) Get(_ string, res any) error   { return json.Unmarshal([]byte(c.getBody), res) }
func (c rawClient) Post(_ string, _, _ any) error { return nil }
func (c rawClient) Put(_ string, _, _ any) error  { return nil }
func (c rawClient) Delete(_ string, _ any) error  { return nil }

// TestHasPendingChanges_RealFieldName guards against the pendingChanges field
// being misnamed: the OVHcloud API returns "number", not "numberOfRemaining".
func TestHasPendingChanges_RealFieldName(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"staged changes present", `[{"zone":"gra","number":5}]`, true},
		{"all applied", `[{"zone":"gra","number":0}]`, false},
		{"empty list", `[]`, false},
		{"multiple zones, one pending", `[{"zone":"bhs","number":0},{"zone":"gra","number":2}]`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(rawClient{getBody: tc.body}, "loadbalancer-test")
			got, err := s.HasPendingChanges()
			if err != nil {
				t.Fatalf("HasPendingChanges: %v", err)
			}
			if got != tc.want {
				t.Fatalf("HasPendingChanges() = %v, want %v (body %s)", got, tc.want, tc.body)
			}
		})
	}
}
