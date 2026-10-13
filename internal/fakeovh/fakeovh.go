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

// Package fakeovh provides an in-memory implementation of ovhclient.Client
// modelling the subset of the OVHcloud IP Load Balancing API used by the
// operator. It is intended for tests.
package fakeovh

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/ovh/go-ovh/ovh"
)

// notFound mirrors the real go-ovh 404 error type so callers' isNotFound checks
// behave the same against the fake as against the live API.
func notFound(path string) error {
	return &ovh.APIError{Code: 404, Message: "not found: " + path}
}

// Fake is an in-memory OVHcloud IPLB API. It implements ovhclient.Client.
type Fake struct {
	mu sync.Mutex

	objects map[string]map[int64]map[string]any // collectionKey -> id -> object
	nextID  int64

	// PendingCount is the number of staged-but-unapplied changes.
	PendingCount int
	// RefreshCount counts how many times Refresh was applied.
	RefreshCount int
	// ServiceIP is returned as the ipLoadbalancing field by a GET on the
	// service root (/ipLoadbalancing/{serviceName}).
	ServiceIP string
	// ServiceZones is returned as the zone field by a GET on the service root.
	ServiceZones []string
	// VrackNetworkIDs is returned by a GET on /vrack/network.
	VrackNetworkIDs []int64
}

// New returns an empty Fake.
func New() *Fake {
	return &Fake{objects: map[string]map[int64]map[string]any{}, nextID: 0}
}

func idField(collection string) string {
	switch {
	case strings.HasSuffix(collection, "/server"):
		return "serverId"
	case strings.HasSuffix(collection, "/farm"):
		return "farmId"
	case strings.HasSuffix(collection, "/frontend"):
		return "frontendId"
	case strings.HasSuffix(collection, "/ssl"):
		return "id"
	case strings.HasSuffix(collection, "/route"):
		return "routeId"
	case strings.HasSuffix(collection, "/rule"):
		return "ruleId"
	}
	return "id"
}

// parse splits a path into its collection key and an optional trailing id.
// e.g. "/ipLoadbalancing/svc/http/farm/12" -> ("/ipLoadbalancing/svc/http/farm", 12, true)
func parse(path string) (collection string, id int64, hasID bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	last := parts[len(parts)-1]
	if n, err := strconv.ParseInt(last, 10, 64); err == nil {
		return "/" + strings.Join(parts[:len(parts)-1], "/"), n, true
	}
	return "/" + strings.Join(parts, "/"), 0, false
}

func (f *Fake) coll(key string) map[int64]map[string]any {
	if f.objects[key] == nil {
		f.objects[key] = map[int64]map[string]any{}
	}
	return f.objects[key]
}

func decode(src any, dst any) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}

func (f *Fake) Get(path string, res any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.HasSuffix(path, "/pendingChanges") {
		zones := []map[string]any{}
		if f.PendingCount > 0 {
			zones = append(zones, map[string]any{"zone": "all", "number": f.PendingCount})
		}
		return decode(zones, res)
	}

	if strings.HasSuffix(path, "/vrack/network") {
		ids := f.VrackNetworkIDs
		if ids == nil {
			ids = []int64{}
		}
		return decode(ids, res)
	}

	// Service root, e.g. "/ipLoadbalancing/loadbalancer-test".
	if strings.HasPrefix(path, "/ipLoadbalancing/") && strings.Count(path, "/") == 2 {
		return decode(map[string]any{
			"ipLoadbalancing": f.ServiceIP,
			"ipv4":            f.ServiceIP,
			"zone":            f.ServiceZones,
		}, res)
	}

	collection, id, hasID := parse(path)
	if !hasID {
		// List: return the ids in the collection.
		ids := []int64{}
		for k := range f.coll(collection) {
			ids = append(ids, k)
		}
		return decode(ids, res)
	}
	obj, ok := f.coll(collection)[id]
	if !ok {
		return notFound(path)
	}
	return decode(obj, res)
}

func (f *Fake) Post(path string, body, res any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if strings.HasSuffix(path, "/refresh") {
		f.PendingCount = 0
		f.RefreshCount++
		return nil
	}

	collection, _, _ := parse(path)
	obj := map[string]any{}
	if body != nil {
		if err := decode(body, &obj); err != nil {
			return err
		}
	}
	f.nextID++
	id := f.nextID
	obj[idField(collection)] = id
	f.coll(collection)[id] = obj
	f.PendingCount++
	if res != nil {
		return decode(obj, res)
	}
	return nil
}

func (f *Fake) Put(path string, body, res any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	collection, id, hasID := parse(path)
	if !hasID {
		return fmt.Errorf("PUT requires an id: %s", path)
	}
	if _, ok := f.coll(collection)[id]; !ok {
		return notFound(path)
	}
	obj := map[string]any{}
	if err := decode(body, &obj); err != nil {
		return err
	}
	obj[idField(collection)] = id
	f.coll(collection)[id] = obj
	f.PendingCount++
	if res != nil {
		return decode(obj, res)
	}
	return nil
}

func (f *Fake) Delete(path string, res any) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	collection, id, hasID := parse(path)
	if !hasID {
		return fmt.Errorf("DELETE requires an id: %s", path)
	}
	if _, ok := f.coll(collection)[id]; !ok {
		return notFound(path)
	}
	delete(f.coll(collection), id)
	f.PendingCount++
	return nil
}

// SeedSSL inserts a pre-existing SSL certificate (as if already issued by the
// freeCertificate flow) into the given service and returns its id. Used by tests.
func (f *Fake) SeedSSL(serviceName string, obj map[string]any) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := "/ipLoadbalancing/" + serviceName + "/ssl"
	f.nextID++
	obj["id"] = f.nextID
	f.coll(key)[f.nextID] = obj
	return f.nextID
}

// Count returns the number of objects stored in the given collection suffix
// (e.g. "/http/farm" or "/ssl"); used by tests.
func (f *Fake) Count(collectionSuffix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for key, objs := range f.objects {
		if strings.HasSuffix(key, collectionSuffix) {
			return len(objs)
		}
	}
	return 0
}
