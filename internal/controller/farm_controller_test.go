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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/fakeovh"
	"github.com/ovhcloud/iplb-operator/internal/iplb"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
	"github.com/ovhcloud/iplb-operator/internal/refresh"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := iplbv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func credentialsSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ovh-creds", Namespace: "default"},
		Data: map[string][]byte{
			ovhclient.KeyClientID:     []byte("id"),
			ovhclient.KeyClientSecret: []byte("secret"),
		},
	}
}

func newFarm() *iplbv1alpha1.Farm {
	return &iplbv1alpha1.Farm{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"},
		Spec: iplbv1alpha1.FarmSpec{
			ServiceRef: iplbv1alpha1.ServiceRef{
				ServiceName:          "loadbalancer-test",
				CredentialsSecretRef: iplbv1alpha1.SecretReference{Name: "ovh-creds"},
			},
			Protocol: "http",
			Balance:  "roundrobin",
			Servers: []iplbv1alpha1.FarmServer{
				{Address: "10.0.0.1", Port: intp(8080)},
				{Address: "10.0.0.2", Port: intp(8080)},
			},
		},
	}
}

// reconcileUntilStable drives Reconcile until it returns no requeue, capping at
// a few iterations (finalizer add and create each cause a requeue).
func reconcileUntilStable(t *testing.T, r *FarmReconciler, key types.NamespacedName) {
	t.Helper()
	for i := 0; i < 6; i++ {
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func TestFarmReconcile_CreateUpdateDelete(t *testing.T) {
	s := testScheme(t)
	farm := newFarm()
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(credentialsSecret(), farm).
		WithStatusSubresource(&iplbv1alpha1.Farm{}).
		Build()

	fakeAPI := fakeovh.New()
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &FarmReconciler{Client: k8s, Scheme: s, Factory: factory, Refresh: refresh.New(time.Hour)}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	// --- create ---
	reconcileUntilStable(t, r, key)

	got := &iplbv1alpha1.Farm{}
	if err := k8s.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.FarmID == nil {
		t.Fatal("expected status.farmId to be set")
	}
	if n := fakeAPI.Count("/http/farm"); n != 1 {
		t.Fatalf("expected 1 farm in API, got %d", n)
	}
	if n := fakeAPI.Count("/server"); n != 2 {
		t.Fatalf("expected 2 servers in API, got %d", n)
	}

	// --- update: drop one server, add another ---
	got.Spec.Servers = []iplbv1alpha1.FarmServer{
		{Address: "10.0.0.1", Port: intp(8080)},
		{Address: "10.0.0.3", Port: intp(9090)},
	}
	if err := k8s.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileUntilStable(t, r, key)
	if n := fakeAPI.Count("/server"); n != 2 {
		t.Fatalf("expected 2 servers after update, got %d", n)
	}

	// --- becomes Ready once the service has no pending changes ---
	fakeAPI.PendingCount = 0 // simulate the coordinator having applied
	reconcileUntilStable(t, r, key)
	if err := k8s.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	if !isReady(got.Status.Conditions) {
		t.Fatal("expected Ready=True after refresh applied")
	}

	// --- delete: finalizer triggers API deletion ---
	if err := k8s.Delete(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileUntilStable(t, r, key)
	if n := fakeAPI.Count("/http/farm"); n != 0 {
		t.Fatalf("expected farm deleted from API, got %d", n)
	}
	if err := k8s.Get(context.Background(), key, got); !apiNotFound(err) {
		t.Fatalf("expected Farm object gone, got err=%v", err)
	}
}

func TestFarmReconcile_ClearsProbeOnRemoval(t *testing.T) {
	s := testScheme(t)
	farm := newFarm()
	farm.Spec.Probe = &iplbv1alpha1.Probe{Type: "tcp", Interval: intp(30)}
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(credentialsSecret(), farm).
		WithStatusSubresource(&iplbv1alpha1.Farm{}).
		Build()

	fakeAPI := fakeovh.New()
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &FarmReconciler{Client: k8s, Scheme: s, Factory: factory, Refresh: refresh.New(time.Hour)}
	key := types.NamespacedName{Name: "web", Namespace: "default"}
	svc := iplb.New(fakeAPI, "loadbalancer-test")

	// --- create: the farm gets a probe ---
	reconcileUntilStable(t, r, key)
	got := &iplbv1alpha1.Farm{}
	if err := k8s.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	cur, err := svc.GetFarm("http", *got.Status.FarmID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Probe == nil || cur.Probe.Type != "tcp" {
		t.Fatalf("expected tcp probe after create, got %+v", cur.Probe)
	}

	// --- remove the probe from the spec: it must be cleared on the API ---
	got.Spec.Probe = nil
	if err := k8s.Update(context.Background(), got); err != nil {
		t.Fatal(err)
	}
	reconcileUntilStable(t, r, key)
	cur, err = svc.GetFarm("http", *got.Status.FarmID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Probe != nil && cur.Probe.Type != "" {
		t.Fatalf("expected probe cleared after removal, got %+v", cur.Probe)
	}
}

func TestFarmReconcile_UDPStripsUnsupportedFields(t *testing.T) {
	s := testScheme(t)
	farm := newFarm()
	farm.Spec.Protocol = "udp"
	farm.Spec.Balance = "roundrobin"
	farm.Spec.Stickiness = "sourceIp"
	farm.Spec.Probe = &iplbv1alpha1.Probe{Type: "tcp"}
	farm.Spec.Servers = []iplbv1alpha1.FarmServer{
		{Address: "10.0.0.1", Port: intp(53), Weight: intp(10), SSL: boolp(true), Probe: boolp(true)},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(credentialsSecret(), farm).
		WithStatusSubresource(&iplbv1alpha1.Farm{}).
		Build()
	fakeAPI := fakeovh.New()
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &FarmReconciler{Client: k8s, Scheme: s, Factory: factory, Refresh: refresh.New(time.Hour)}
	key := types.NamespacedName{Name: "web", Namespace: "default"}
	reconcileUntilStable(t, r, key)

	got := &iplbv1alpha1.Farm{}
	if err := k8s.Get(context.Background(), key, got); err != nil {
		t.Fatal(err)
	}
	api := iplb.New(fakeAPI, "loadbalancer-test")
	cur, err := api.GetFarm("udp", *got.Status.FarmID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Balance != "" || cur.Stickiness != "" || cur.Probe != nil {
		t.Fatalf("expected udp farm to omit balance/stickiness/probe, got %+v", cur)
	}
	sid := got.Status.ServerIDs["10.0.0.1"]
	srv, err := api.GetServer("udp", *got.Status.FarmID, sid)
	if err != nil {
		t.Fatal(err)
	}
	if srv.Weight != nil || srv.SSL != nil || srv.Probe != nil {
		t.Fatalf("expected udp server to omit weight/ssl/probe, got %+v", srv)
	}
}

func TestFarmReconcile_RecreatesVanishedFarm(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)
	farm := newFarm()
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(credentialsSecret(), farm).
		WithStatusSubresource(&iplbv1alpha1.Farm{}).
		Build()
	fakeAPI := fakeovh.New()
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &FarmReconciler{Client: k8s, Scheme: s, Factory: factory, Refresh: refresh.New(time.Hour)}
	key := types.NamespacedName{Name: "web", Namespace: "default"}
	api := iplb.New(fakeAPI, "loadbalancer-test")

	reconcileUntilStable(t, r, key)
	got := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	oldID := *got.Status.FarmID

	// Simulate the staged farm being discarded on the API side.
	if err := api.DeleteFarm("http", oldID); err != nil {
		t.Fatal(err)
	}

	// The reconciler must notice the 404, reset status, and recreate the farm.
	reconcileUntilStable(t, r, key)
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.FarmID == nil {
		t.Fatal("expected farm recreated with a new id")
	}
	if _, err := api.GetFarm("http", *got.Status.FarmID); err != nil {
		t.Fatalf("expected recreated farm to exist: %v", err)
	}
}

func TestFarmReconcile_DeleteIsIdempotentWhenAlreadyGone(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)
	farm := newFarm()
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(credentialsSecret(), farm).
		WithStatusSubresource(&iplbv1alpha1.Farm{}).
		Build()
	fakeAPI := fakeovh.New()
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &FarmReconciler{Client: k8s, Scheme: s, Factory: factory, Refresh: refresh.New(time.Hour)}
	key := types.NamespacedName{Name: "web", Namespace: "default"}
	api := iplb.New(fakeAPI, "loadbalancer-test")

	reconcileUntilStable(t, r, key)
	got := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	// Farm already gone on the API, then delete the CR: the finalizer must still
	// complete (404 treated as success) and the object disappear.
	if err := api.DeleteFarm("http", *got.Status.FarmID); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Delete(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileUntilStable(t, r, key)
	if err := k8s.Get(ctx, key, got); !apiNotFound(err) {
		t.Fatalf("expected Farm object gone after delete, err=%v", err)
	}
}

func isReady(conds []metav1.Condition) bool {
	for _, c := range conds {
		if c.Type == iplbv1alpha1.ConditionReady {
			return c.Status == metav1.ConditionTrue
		}
	}
	return false
}

func apiNotFound(err error) bool {
	return err != nil && client.IgnoreNotFound(err) == nil
}
