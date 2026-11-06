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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/fakeovh"
	"github.com/ovhcloud/iplb-operator/internal/iplb"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
	"github.com/ovhcloud/iplb-operator/internal/refresh"
)

func reconcileFrontendUntilStable(t *testing.T, r *FrontendReconciler, key types.NamespacedName) {
	t.Helper()
	for i := 0; i < 8; i++ {
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if res.RequeueAfter == 0 {
			return
		}
	}
}

func newFrontendReconciler(t *testing.T, objs ...client.Object) (*FrontendReconciler, client.Client, *fakeovh.Fake) {
	t.Helper()
	s := testScheme(t)
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&iplbv1alpha1.Frontend{}, &iplbv1alpha1.Farm{}, &iplbv1alpha1.Certificate{}).
		Build()
	fakeAPI := fakeovh.New()
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &FrontendReconciler{Client: k8s, Scheme: s, Factory: factory, Refresh: refresh.New(time.Hour)}
	return r, k8s, fakeAPI
}

func baseFrontend(proto string) *iplbv1alpha1.Frontend {
	return &iplbv1alpha1.Frontend{
		ObjectMeta: metav1.ObjectMeta{Name: "fe", Namespace: "default"},
		Spec: iplbv1alpha1.FrontendSpec{
			ServiceRef: iplbv1alpha1.ServiceRef{
				ServiceName:          "loadbalancer-test",
				CredentialsSecretRef: iplbv1alpha1.SecretReference{Name: "ovh-creds"},
			},
			Protocol: proto,
			Port:     "443",
		},
	}
}

func TestFrontendReconcile_TLSFromCertificateRef(t *testing.T) {
	ctx := context.Background()
	cert := &iplbv1alpha1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "mycert", Namespace: "default"},
		Spec: iplbv1alpha1.CertificateSpec{
			ServiceRef:   iplbv1alpha1.ServiceRef{ServiceName: "loadbalancer-test", CredentialsSecretRef: iplbv1alpha1.SecretReference{Name: "ovh-creds"}},
			TLSSecretRef: &iplbv1alpha1.SecretReference{Name: "tls"},
		},
		Status: iplbv1alpha1.CertificateStatus{CertificateID: int64p(7788)},
	}
	fe := baseFrontend("http")
	fe.Spec.DefaultCertificateRef = "mycert"

	r, k8s, fakeAPI := newFrontendReconciler(t, credentialsSecret(), cert, fe)
	key := types.NamespacedName{Name: "fe", Namespace: "default"}
	reconcileFrontendUntilStable(t, r, key)

	got := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.FrontendID == nil {
		t.Fatal("expected frontendId set")
	}
	if got.Status.ResolvedSslID == nil || *got.Status.ResolvedSslID != 7788 {
		t.Fatalf("expected resolved ssl id 7788, got %v", got.Status.ResolvedSslID)
	}
	api := iplb.New(fakeAPI, "loadbalancer-test")
	cur, err := api.GetFrontend("http", *got.Status.FrontendID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.DefaultSslID == nil || *cur.DefaultSslID != 7788 {
		t.Fatalf("expected frontend defaultSslId 7788, got %v", cur.DefaultSslID)
	}
	if cur.SSL == nil || !*cur.SSL {
		t.Fatalf("expected ssl implied true, got %v", cur.SSL)
	}
}

func TestFrontendReconcile_UDPOmitsTLS(t *testing.T) {
	ctx := context.Background()
	fe := baseFrontend("udp")
	fe.Spec.SSL = boolp(true)
	fe.Spec.AllowedSource = []string{"10.0.0.0/8"}
	fe.Spec.DeniedSource = []string{"192.0.2.0/24"}

	r, k8s, fakeAPI := newFrontendReconciler(t, credentialsSecret(), fe)
	key := types.NamespacedName{Name: "fe", Namespace: "default"}
	reconcileFrontendUntilStable(t, r, key)

	got := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	api := iplb.New(fakeAPI, "loadbalancer-test")
	cur, err := api.GetFrontend("udp", *got.Status.FrontendID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.SSL != nil {
		t.Fatalf("expected no ssl on udp frontend, got %v", cur.SSL)
	}
	if len(cur.AllowedSource) != 0 || len(cur.DeniedSource) != 0 {
		t.Fatalf("expected no allowed/denied source on udp frontend, got allow=%v deny=%v", cur.AllowedSource, cur.DeniedSource)
	}
}

func TestFrontendReconcile_DeniedSource(t *testing.T) {
	ctx := context.Background()
	fe := baseFrontend("http")
	fe.Spec.Port = "80"
	fe.Spec.DeniedSource = []string{"10.1.2.0/24", "203.0.113.7/32"}

	r, k8s, fakeAPI := newFrontendReconciler(t, credentialsSecret(), fe)
	key := types.NamespacedName{Name: "fe", Namespace: "default"}
	reconcileFrontendUntilStable(t, r, key)

	got := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	api := iplb.New(fakeAPI, "loadbalancer-test")
	cur, err := api.GetFrontend("http", *got.Status.FrontendID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cur.DeniedSource) != 2 || cur.DeniedSource[0] != "10.1.2.0/24" || cur.DeniedSource[1] != "203.0.113.7/32" {
		t.Fatalf("expected deniedSource preserved, got %v", cur.DeniedSource)
	}
	if len(cur.AllowedSource) != 0 {
		t.Fatalf("expected no allowedSource, got %v", cur.AllowedSource)
	}
}

func TestFrontendReconcile_RejectsAllowAndDeny(t *testing.T) {
	ctx := context.Background()
	fe := baseFrontend("http")
	fe.Spec.Port = "80"
	fe.Spec.AllowedSource = []string{"10.0.0.0/8"}
	fe.Spec.DeniedSource = []string{"10.1.2.0/24"}

	r, k8s, fakeAPI := newFrontendReconciler(t, credentialsSecret(), fe)
	r.Recorder = events.NewFakeRecorder(8)
	key := types.NamespacedName{Name: "fe", Namespace: "default"}

	// Drives through finalizer-add then the validation rejection (no hot-loop).
	reconcileFrontendUntilStable(t, r, key)

	// No frontend created on the API.
	api := iplb.New(fakeAPI, "loadbalancer-test")
	if ids, _ := api.ListFrontends("http"); len(ids) != 0 {
		t.Fatalf("expected no frontend created for invalid spec, got %d", len(ids))
	}
	// Ready=False with a descriptive message.
	got := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if isReady(got.Status.Conditions) {
		t.Fatal("expected Ready=False for mutually-exclusive sources")
	}
}

func TestFrontendReconcile_HTTPRoutes(t *testing.T) {
	ctx := context.Background()
	fe := baseFrontend("http")
	fe.Spec.Port = "80"
	fe.Spec.Routes = []iplbv1alpha1.Route{
		{
			Name:   "to-https",
			Action: iplbv1alpha1.RouteAction{Type: "redirect", Target: "https://${host}${path}${arguments}", Status: intp(302)},
			Rules: []iplbv1alpha1.RouteRule{
				{Field: "path", Match: "startswith", Pattern: "/app"},
			},
		},
	}

	r, k8s, fakeAPI := newFrontendReconciler(t, credentialsSecret(), fe)
	key := types.NamespacedName{Name: "fe", Namespace: "default"}
	reconcileFrontendUntilStable(t, r, key)

	got := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	id, ok := got.Status.RouteIDs["to-https"]
	if !ok {
		t.Fatalf("expected route id tracked in status, got %v", got.Status.RouteIDs)
	}
	api := iplb.New(fakeAPI, "loadbalancer-test")
	route, err := api.GetRoute(id)
	if err != nil {
		t.Fatal(err)
	}
	if route.Action.Type != "redirect" || route.Action.Status == nil || *route.Action.Status != 302 {
		t.Fatalf("unexpected route action: %+v", route.Action)
	}
	ruleIDs, err := api.ListRouteRules(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ruleIDs) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(ruleIDs))
	}

	// Drop the route from the spec: it must be deleted and untracked.
	got.Spec.Routes = nil
	if err := k8s.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	reconcileFrontendUntilStable(t, r, key)
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Status.RouteIDs["to-https"]; ok {
		t.Fatal("expected route untracked after removal")
	}
	if ids, _ := api.ListRoutes(); len(ids) != 0 {
		t.Fatalf("expected route deleted, got %d routes", len(ids))
	}
}
