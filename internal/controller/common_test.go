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
	"fmt"
	"testing"

	"github.com/ovh/go-ovh/ovh"
)

func TestIsTerminalAPIError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"plain error", fmt.Errorf("boom"), false},
		{"400 wrapped", fmt.Errorf("creating farm: %w", &ovh.APIError{Code: 400, Message: "Missing zone parameter"}), true},
		{"404", &ovh.APIError{Code: 404, Message: "This service does not exist"}, true},
		{"403", &ovh.APIError{Code: 403}, true},
		{"409", &ovh.APIError{Code: 409}, true},
		{"429 rate limit is transient", &ovh.APIError{Code: 429}, false},
		{"500 is transient", &ovh.APIError{Code: 500}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTerminalAPIError(tc.err); got != tc.want {
				t.Fatalf("isTerminalAPIError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
