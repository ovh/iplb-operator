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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/fakeovh"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
	"github.com/ovhcloud/iplb-operator/internal/refresh"
)

func TestCertificateReconcile_FreeCertificate(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)
	cert := &iplbv1alpha1.Certificate{
		ObjectMeta: metav1.ObjectMeta{Name: "mycert", Namespace: "default"},
		Spec: iplbv1alpha1.CertificateSpec{
			ServiceRef:      iplbv1alpha1.ServiceRef{ServiceName: "loadbalancer-test", CredentialsSecretRef: iplbv1alpha1.SecretReference{Name: "ovh-creds"}},
			FreeCertificate: &iplbv1alpha1.FreeCertificateSpec{FQDN: []string{"app.example.com"}},
		},
	}
	k8s := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(credentialsSecret(), cert).
		WithStatusSubresource(&iplbv1alpha1.Certificate{}).
		Build()
	fakeAPI := fakeovh.New()
	factory := ovhclient.NewFactoryWithBuilder(k8s, func(_, _, _ string) (ovhclient.Client, error) {
		return fakeAPI, nil
	})
	r := &CertificateReconciler{Client: k8s, Scheme: s, Factory: factory, Refresh: refresh.New(time.Hour), Recorder: events.NewFakeRecorder(16)}
	key := types.NamespacedName{Name: "mycert", Namespace: "default"}

	// First passes: add finalizer, then request issuance and wait.
	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	got := &iplbv1alpha1.Certificate{}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if !got.Status.FreeCertificateRequested {
		t.Fatal("expected free certificate to have been requested")
	}
	if got.Status.CertificateID != nil {
		t.Fatalf("did not expect a certificate id before issuance, got %v", got.Status.CertificateID)
	}
	if fakeAPI.Count("/freeCertificate") != 1 {
		t.Fatalf("expected exactly one freeCertificate request, got %d", fakeAPI.Count("/freeCertificate"))
	}

	// Reconcile again before issuance: must not request a second time.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatal(err)
	}
	if n := fakeAPI.Count("/freeCertificate"); n != 1 {
		t.Fatalf("expected no duplicate request, got %d", n)
	}

	// Simulate OVHcloud issuing the certificate: it appears in the ssl list.
	issued := fakeAPI.SeedSSL("loadbalancer-test", map[string]any{
		"displayName": "mycert", "subject": "app.example.com", "san": []any{"app.example.com"}, "type": "built",
	})
	fakeAPI.PendingCount = 0

	for i := 0; i < 4; i++ {
		if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	}
	if err := k8s.Get(ctx, key, got); err != nil {
		t.Fatal(err)
	}
	if got.Status.CertificateID == nil || *got.Status.CertificateID != issued {
		t.Fatalf("expected adopted issued certificate id %d, got %v", issued, got.Status.CertificateID)
	}
	if !isReady(got.Status.Conditions) {
		t.Fatal("expected Ready=True once the certificate is issued and applied")
	}
}
