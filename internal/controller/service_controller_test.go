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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/fakeovh"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
)

func lbClass() *string { c := LoadBalancerClass; return &c }

func newLBService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
			Annotations: map[string]string{
				AnnServiceName:       "loadbalancer-test",
				AnnCredentialsSecret: "ovh-creds",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:              corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass: lbClass(),
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 80, Protocol: corev1.ProtocolTCP, NodePort: 30080},
			},
		},
	}
}

func newNode(name, ip string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.NodeStatus{
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: ip}},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

func newServiceReconciler(t *testing.T, objs ...client.Object) (*ServiceReconciler, client.Client, *fakeovh.Fake) {
	t.Helper()
	s := testScheme(t)
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&corev1.Service{}).
		Build()

	fakeAPI := fakeovh.New()
	fakeAPI.ServiceIP = "203.0.113.10"
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &ServiceReconciler{Client: k8s, Scheme: s, Factory: factory, Recorder: events.NewFakeRecorder(32)}
	return r, k8s, fakeAPI
}

func TestServiceReconcile_CreatesFarmFrontendAndStatus(t *testing.T) {
	ctx := context.Background()
	r, k8s, _ := newServiceReconciler(t, credentialsSecret(), newLBService(),
		newNode("n1", "10.0.0.1"), newNode("n2", "10.0.0.2"))
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Farm created with node IPs on the nodePort, owned by the Service.
	farmKey := types.NamespacedName{Name: "web-80", Namespace: "default"}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, farmKey, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.Protocol != "tcp" {
		t.Fatalf("expected protocol tcp, got %q", farm.Spec.Protocol)
	}
	if len(farm.Spec.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(farm.Spec.Servers))
	}
	for _, srv := range farm.Spec.Servers {
		if srv.Port == nil || *srv.Port != 30080 {
			t.Fatalf("expected server port 30080, got %v", srv.Port)
		}
	}
	if len(farm.OwnerReferences) != 1 || farm.OwnerReferences[0].Kind != "Service" {
		t.Fatalf("expected Service owner reference, got %v", farm.OwnerReferences)
	}

	// Frontend created listening on the Service port, targeting the Farm.
	fe := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, farmKey, fe); err != nil {
		t.Fatalf("get frontend: %v", err)
	}
	if fe.Spec.Port != "80" {
		t.Fatalf("expected frontend port 80, got %q", fe.Spec.Port)
	}
	if fe.Spec.DefaultFarmRef != "web-80" {
		t.Fatalf("expected defaultFarmRef web-80, got %q", fe.Spec.DefaultFarmRef)
	}

	// The IPLB IP is written back to the Service status.
	svc := &corev1.Service{}
	if err := k8s.Get(ctx, key, svc); err != nil {
		t.Fatal(err)
	}
	if len(svc.Status.LoadBalancer.Ingress) != 1 || svc.Status.LoadBalancer.Ingress[0].IP != "203.0.113.10" {
		t.Fatalf("expected ingress IP 203.0.113.10, got %v", svc.Status.LoadBalancer.Ingress)
	}
}

func TestServiceReconcile_PrunesRemovedPort(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Spec.Ports = append(svcObj.Spec.Ports,
		corev1.ServicePort{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP, NodePort: 30443})
	r, k8s, _ := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	for _, n := range []string{"web-80", "web-443"} {
		if err := k8s.Get(ctx, types.NamespacedName{Name: n, Namespace: "default"}, &iplbv1alpha1.Farm{}); err != nil {
			t.Fatalf("expected farm %s: %v", n, err)
		}
	}

	// Drop the 443 port and reconcile: its Farm/Frontend must be pruned.
	cur := &corev1.Service{}
	if err := k8s.Get(ctx, key, cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.Ports = cur.Spec.Ports[:1]
	if err := k8s.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-443", Namespace: "default"}, &iplbv1alpha1.Farm{}); err == nil {
		t.Fatal("expected farm web-443 to be pruned")
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, &iplbv1alpha1.Farm{}); err != nil {
		t.Fatalf("expected farm web-80 to remain: %v", err)
	}
}

func TestServiceReconcile_AutoDefaultsSingleZone(t *testing.T) {
	ctx := context.Background()
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), newLBService(), newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.Zone != "gra" {
		t.Fatalf("expected zone gra auto-defaulted, got %q", farm.Spec.Zone)
	}
}

func TestServiceReconcile_MultipleZonesRequiresAnnotation(t *testing.T) {
	ctx := context.Background()
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), newLBService(), newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra", "bhs"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected a requeue while waiting for the zone annotation")
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, &iplbv1alpha1.Farm{}); err == nil {
		t.Fatal("expected no farm created until a zone is chosen")
	}

	// Pick a zone explicitly: the farm is now created with it.
	cur := &corev1.Service{}
	if err := k8s.Get(ctx, key, cur); err != nil {
		t.Fatal(err)
	}
	cur.Annotations[AnnZone] = "bhs"
	if err := k8s.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.Zone != "bhs" {
		t.Fatalf("expected zone bhs, got %q", farm.Spec.Zone)
	}
}

func TestServiceReconcile_VrackNetworkFromAnnotation(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Annotations[AnnVrackNetworkID] = "42"
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.VrackNetworkID == nil || *farm.Spec.VrackNetworkID != 42 {
		t.Fatalf("expected vrackNetworkId 42, got %v", farm.Spec.VrackNetworkID)
	}
}

func TestServiceReconcile_AutoDiscoversSingleVrackNetwork(t *testing.T) {
	ctx := context.Background()
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), newLBService(), newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	fakeAPI.VrackNetworkIDs = []int64{99}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.VrackNetworkID == nil || *farm.Spec.VrackNetworkID != 99 {
		t.Fatalf("expected auto-discovered vrackNetworkId 99, got %v", farm.Spec.VrackNetworkID)
	}
}

func TestServiceReconcile_MultipleVrackNetworksRequiresAnnotation(t *testing.T) {
	ctx := context.Background()
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), newLBService(), newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	fakeAPI.VrackNetworkIDs = []int64{99, 100}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter == 0 {
		t.Fatal("expected a requeue while waiting for the vrack-network-id annotation")
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, &iplbv1alpha1.Farm{}); err == nil {
		t.Fatal("expected no farm created until a vRack network is chosen")
	}
}

func TestServiceReconcile_StickinessFromSessionAffinity(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Spec.SessionAffinity = corev1.ServiceAffinityClientIP
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.Stickiness != "sourceIp" {
		t.Fatalf("expected stickiness sourceIp from ClientIP affinity, got %q", farm.Spec.Stickiness)
	}
	if farm.Spec.Probe != nil {
		t.Fatalf("expected no probe when no health-check annotations, got %+v", farm.Spec.Probe)
	}
}

func TestServiceReconcile_HealthCheckFromAnnotations(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Annotations[AnnStickiness] = "cookie"
	svcObj.Annotations[AnnHealthCheckProtocol] = "http"
	svcObj.Annotations[AnnHealthCheckURL] = "/healthz"
	svcObj.Annotations[AnnHealthCheckInterval] = "10"
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.Stickiness != "cookie" {
		t.Fatalf("expected stickiness cookie (annotation override), got %q", farm.Spec.Stickiness)
	}
	p := farm.Spec.Probe
	if p == nil {
		t.Fatal("expected a probe to be configured")
	}
	if p.Type != "http" || p.URL != "/healthz" || p.Interval == nil || *p.Interval != 10 {
		t.Fatalf("unexpected probe: %+v", p)
	}
	if p.Port != nil {
		t.Fatalf("expected probe port unset (defaults to backend port), got %v", p.Port)
	}
}

func TestServiceReconcile_InvalidHealthCheckPort(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Annotations[AnnHealthCheckPort] = "notanint"
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile should not hard-error on bad annotation: %v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, &iplbv1alpha1.Farm{}); err == nil {
		t.Fatal("expected no farm created with an invalid health-check-port annotation")
	}
}

func TestServiceReconcile_ServerSettings(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Annotations[AnnServerSSL] = "true"
	svcObj.Annotations[AnnServerBackup] = "false"
	svcObj.Annotations[AnnServerWeight] = "5"
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"), newNode("n2", "10.0.0.2"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if len(farm.Spec.Servers) != 2 {
		t.Fatalf("expected 2 servers, got %d", len(farm.Spec.Servers))
	}
	for _, s := range farm.Spec.Servers {
		if s.SSL == nil || !*s.SSL {
			t.Fatalf("expected SSL=true on server %s, got %v", s.Address, s.SSL)
		}
		if s.Backup == nil || *s.Backup {
			t.Fatalf("expected Backup=false on server %s, got %v", s.Address, s.Backup)
		}
		if s.Weight == nil || *s.Weight != 5 {
			t.Fatalf("expected Weight=5 on server %s, got %v", s.Address, s.Weight)
		}
		// The probe is always explicit; with no health-check it is disabled.
		if s.Probe == nil || *s.Probe {
			t.Fatalf("expected Probe=false (no health-check) on server %s, got %v", s.Address, s.Probe)
		}
	}
}

func TestServiceReconcile_HealthCheckCanBeDisabled(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Annotations[AnnHealthCheckProtocol] = "tcp"
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.Servers[0].Probe == nil || !*farm.Spec.Servers[0].Probe {
		t.Fatalf("expected probe enabled while health-check set, got %v", farm.Spec.Servers[0].Probe)
	}

	// Remove the health-check: the server probe must flip to an explicit false so
	// the change is reconciled to the API, not left at its previous true value.
	cur := &corev1.Service{}
	if err := k8s.Get(ctx, key, cur); err != nil {
		t.Fatal(err)
	}
	delete(cur.Annotations, AnnHealthCheckProtocol)
	if err := k8s.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	for _, s := range farm.Spec.Servers {
		if s.Probe == nil || *s.Probe {
			t.Fatalf("expected probe explicitly disabled after removing health-check, got %v", s.Probe)
		}
	}
}

func TestServiceReconcile_ServerProbeDefaultsWhenHealthCheckSet(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Annotations[AnnHealthCheckProtocol] = "tcp"
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	for _, s := range farm.Spec.Servers {
		if s.Probe == nil || !*s.Probe {
			t.Fatalf("expected server Probe defaulted to true when health-check is set, got %v", s.Probe)
		}
	}
}

func TestServiceReconcile_UDPSetsFarmPort(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Spec.Ports = []corev1.ServicePort{
		{Name: "dns", Port: 5005, Protocol: corev1.ProtocolUDP, NodePort: 30549},
	}
	r, k8s, fakeAPI := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	fakeAPI.ServiceZones = []string{"gra"}
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-5005", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	if farm.Spec.Protocol != "udp" {
		t.Fatalf("expected udp protocol, got %q", farm.Spec.Protocol)
	}
	// UDP farms require an explicit port: it must be the backend nodePort.
	if farm.Spec.Port == nil || *farm.Spec.Port != 30549 {
		t.Fatalf("expected udp farm port 30549, got %v", farm.Spec.Port)
	}
}

func TestServiceReconcile_RejectsAllowAndDeny(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Annotations[AnnZone] = "gra"
	svcObj.Annotations[AnnDeniedSource] = "10.1.0.0/16"
	svcObj.Spec.LoadBalancerSourceRanges = []string{"10.0.0.0/8"}
	r, k8s, _ := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile should not hard-error: %v", err)
	}
	// No farm/frontend created when allow-list and deny-list are both set.
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, &iplbv1alpha1.Farm{}); err == nil {
		t.Fatal("expected no farm created when both allow-list and deny-list are set")
	}
}

func TestServiceReconcile_IgnoresUnmanagedService(t *testing.T) {
	ctx := context.Background()
	svcObj := newLBService()
	svcObj.Spec.LoadBalancerClass = nil // not opted in
	r, k8s, _ := newServiceReconciler(t, credentialsSecret(), svcObj, newNode("n1", "10.0.0.1"))
	key := types.NamespacedName{Name: "web", Namespace: "default"}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, &iplbv1alpha1.Farm{}); err == nil {
		t.Fatal("expected no farm for an unmanaged Service")
	}
}
