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
	ctrl "sigs.k8s.io/controller-runtime"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
)

// TestServiceReconcile_AllAnnotations exercises every supported annotation
// (except the free-certificate pair, covered separately) and asserts each maps
// to the expected field on the generated Farm / Frontend / Service status.
func TestServiceReconcile_AllAnnotations(t *testing.T) {
	ctx := context.Background()

	// Credentials Secret lives in a custom namespace to exercise
	// credentials-namespace.
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "ovh-creds", Namespace: "ovh-system"},
		Data:       map[string][]byte{"clientID": []byte("id"), "clientSecret": []byte("secret")},
	}
	// A hand-written Certificate the frontend references via tls-certificate-ref.
	cert := &iplbv1alpha1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "mycert", Namespace: "default"},
		Spec: iplbv1alpha1.CertificateSpec{
			ServiceRef:   iplbv1alpha1.ServiceRef{ServiceName: "loadbalancer-test", CredentialsSecretRef: iplbv1alpha1.SecretReference{Name: "ovh-creds", Namespace: "ovh-system"}},
			TLSSecretRef: &iplbv1alpha1.SecretReference{Name: "tls"},
		},
		Status: iplbv1alpha1.CertificateStatus{CertificateID: int64p(4242)},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Status: corev1.NodeStatus{
			Addresses: []corev1.NodeAddress{
				{Type: corev1.NodeInternalIP, Address: "10.0.0.1"},
				{Type: corev1.NodeExternalIP, Address: "203.0.113.1"},
			},
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "default",
			Annotations: map[string]string{
				// service binding
				AnnServiceName:          "loadbalancer-test",
				AnnCredentialsSecret:    "ovh-creds",
				AnnCredentialsNamespace: "ovh-system",
				AnnEndpoint:             "ovh-ca",
				// farm
				AnnProtocol:       "http",
				AnnZone:           "gra",
				AnnBalance:        "leastconn",
				AnnStickiness:     "cookie",
				AnnVrackNetworkID: "42",
				// backend servers
				AnnNodeAddressType:   "ExternalIP",
				AnnServerSSL:         "true",
				AnnServerBackup:      "true",
				AnnServerWeight:      "7",
				AnnServerHealthCheck: "true",
				// health check (farm probe)
				AnnHealthCheckProtocol: "http",
				AnnHealthCheckPort:     "8080",
				AnnHealthCheckInterval: "5",
				AnnHealthCheckURL:      "/healthz",
				AnnHealthCheckMethod:   "GET",
				// frontend TLS & ACL (allow-list comes from loadBalancerSourceRanges;
				// deniedSource is mutually exclusive with it and tested separately)
				AnnTLSCertificateRef: "mycert",
				// status
				AnnIP: "198.51.100.9",
			},
		},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeLoadBalancer,
			LoadBalancerClass:        lbClass(),
			LoadBalancerSourceRanges: []string{"10.0.0.0/8"},
			Ports: []corev1.ServicePort{
				{Name: "https", Port: 443, Protocol: corev1.ProtocolTCP, NodePort: 31443},
			},
		},
	}

	r, k8s, _ := newServiceReconciler(t, creds, cert, svc, node)
	key := types.NamespacedName{Name: "web", Namespace: "default"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// ---- Farm ----
	farm := &iplbv1alpha1.Farm{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-443", Namespace: "default"}, farm); err != nil {
		t.Fatalf("get farm: %v", err)
	}
	fs := farm.Spec
	checkStr(t, "farm.serviceName", fs.ServiceRef.ServiceName, "loadbalancer-test")
	checkStr(t, "farm.endpoint", fs.ServiceRef.Endpoint, "ovh-ca")
	checkStr(t, "farm.credSecret", fs.ServiceRef.CredentialsSecretRef.Name, "ovh-creds")
	checkStr(t, "farm.credNamespace", fs.ServiceRef.CredentialsSecretRef.Namespace, "ovh-system")
	checkStr(t, "farm.protocol", fs.Protocol, "http")
	checkStr(t, "farm.zone", fs.Zone, "gra")
	checkStr(t, "farm.balance", fs.Balance, "leastconn")
	checkStr(t, "farm.stickiness", fs.Stickiness, "cookie")
	if fs.VrackNetworkID == nil || *fs.VrackNetworkID != 42 {
		t.Errorf("farm.vrackNetworkId = %v, want 42", fs.VrackNetworkID)
	}
	// probe (health-check-*)
	if fs.Probe == nil {
		t.Fatal("farm.probe nil, want configured")
	}
	checkStr(t, "probe.type", fs.Probe.Type, "http")
	checkIntp(t, "probe.port", fs.Probe.Port, 8080)
	checkIntp(t, "probe.interval", fs.Probe.Interval, 5)
	checkStr(t, "probe.url", fs.Probe.URL, "/healthz")
	checkStr(t, "probe.method", fs.Probe.Method, "GET")
	// servers (node ExternalIP + nodePort + server-*)
	if len(fs.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(fs.Servers))
	}
	srv := fs.Servers[0]
	checkStr(t, "server.address", srv.Address, "203.0.113.1") // ExternalIP
	checkIntp(t, "server.port", srv.Port, 31443)              // nodePort
	checkBoolp(t, "server.ssl", srv.SSL, true)
	checkBoolp(t, "server.backup", srv.Backup, true)
	checkIntp(t, "server.weight", srv.Weight, 7)
	checkBoolp(t, "server.probe", srv.Probe, true)

	// ---- Frontend ----
	fe := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-443", Namespace: "default"}, fe); err != nil {
		t.Fatalf("get frontend: %v", err)
	}
	checkStr(t, "frontend.port", fe.Spec.Port, "443")
	checkStr(t, "frontend.protocol", fe.Spec.Protocol, "http")
	checkStr(t, "frontend.defaultFarmRef", fe.Spec.DefaultFarmRef, "web-443")
	checkStr(t, "frontend.tlsCertRef", fe.Spec.DefaultCertificateRef, "mycert")
	if !strSliceEqual(fe.Spec.AllowedSource, []string{"10.0.0.0/8"}) {
		t.Errorf("frontend.allowedSource = %v, want [10.0.0.0/8]", fe.Spec.AllowedSource)
	}

	// ---- Service status (ip override) ----
	got := &corev1.Service{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	ing := got.Status.LoadBalancer.Ingress
	if len(ing) != 1 || ing[0].IP != "198.51.100.9" {
		t.Errorf("service ingress = %v, want 198.51.100.9", ing)
	}
}

// TestServiceReconcile_FreeCertificateAnnotations covers the tls-free-certificate
// / tls-host pair: it must create an owned free Certificate CR and wire the
// frontend to it.
func TestServiceReconcile_FreeCertificateAnnotations(t *testing.T) {
	ctx := context.Background()
	svc := newLBService()
	svc.Annotations[AnnProtocol] = "http"
	svc.Annotations[AnnZone] = "gra"
	svc.Annotations[AnnTLSFreeCertificate] = "true"
	svc.Annotations[AnnTLSHost] = "app.example.com, www.example.com"

	r, k8s, _ := newServiceReconciler(t, credentialsSecret(), svc, newNode("n1", "10.0.0.1"))
	key := types.NamespacedName{Name: "web", Namespace: "default"}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Owned free Certificate CR created with the FQDNs.
	cert := &iplbv1alpha1.Certificate{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-cert", Namespace: "default"}, cert); err != nil {
		t.Fatalf("get free certificate: %v", err)
	}
	if cert.Spec.FreeCertificate == nil {
		t.Fatal("expected freeCertificate spec")
	}
	if !strSliceEqual(cert.Spec.FreeCertificate.FQDN, []string{"app.example.com", "www.example.com"}) {
		t.Fatalf("free cert FQDN = %v", cert.Spec.FreeCertificate.FQDN)
	}
	if len(cert.OwnerReferences) != 1 || cert.OwnerReferences[0].Kind != "Service" {
		t.Fatalf("expected Service owner reference, got %v", cert.OwnerReferences)
	}

	// Frontend wired to the free cert.
	fe := &iplbv1alpha1.Frontend{}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-80", Namespace: "default"}, fe); err != nil {
		t.Fatalf("get frontend: %v", err)
	}
	if fe.Spec.DefaultCertificateRef != "web-cert" {
		t.Fatalf("frontend.defaultCertificateRef = %q, want web-cert", fe.Spec.DefaultCertificateRef)
	}

	// Removing the free-cert annotations deletes the owned Certificate.
	cur := &corev1.Service{}
	if err := k8s.Get(ctx, key, cur); err != nil {
		t.Fatal(err)
	}
	delete(cur.Annotations, AnnTLSFreeCertificate)
	delete(cur.Annotations, AnnTLSHost)
	if err := k8s.Update(ctx, cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if err := k8s.Get(ctx, types.NamespacedName{Name: "web-cert", Namespace: "default"}, &iplbv1alpha1.Certificate{}); err == nil {
		t.Fatal("expected free certificate deleted after annotations removed")
	}
}

// ---- small assert helpers ----

func checkStr(t *testing.T, field, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %q, want %q", field, got, want)
	}
}

func checkIntp(t *testing.T, field string, got *int, want int) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %d", field, got, want)
	}
}

func checkBoolp(t *testing.T, field string, got *bool, want bool) {
	t.Helper()
	if got == nil || *got != want {
		t.Errorf("%s = %v, want %t", field, got, want)
	}
}
