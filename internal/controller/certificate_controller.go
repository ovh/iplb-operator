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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/iplb"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
	"github.com/ovhcloud/iplb-operator/internal/refresh"
)

// CertificateReconciler reconciles a Certificate object
type CertificateReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Factory  ovhclient.Factory
	Refresh  *refresh.Coordinator
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=certificates/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=certificates/finalizers,verbs=update

func (r *CertificateReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cert iplbv1alpha1.Certificate
	if err := r.Get(ctx, req.NamespacedName, &cert); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	svc, err := serviceFor(ctx, r.Factory, cert.Spec.ServiceRef, cert.Namespace)
	if err != nil {
		setReady(&cert.Status.Conditions, cert.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonError, err.Error())
		_ = r.Status().Update(ctx, &cert)
		return ctrl.Result{}, err
	}

	if !cert.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &cert, svc)
	}

	if controllerutil.AddFinalizer(&cert, iplbv1alpha1.Finalizer) {
		if err := r.Update(ctx, &cert); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// Free (Let's Encrypt) certificate mode is handled separately: there is no
	// material to upload, the issuance is asynchronous, and the issued cert is
	// adopted from the ssl list once OVHcloud has validated the FQDN(s).
	if cert.Spec.FreeCertificate != nil {
		return r.reconcileFreeCertificate(ctx, &cert, svc)
	}
	if cert.Spec.TLSSecretRef == nil {
		return r.fail(ctx, &cert, fmt.Errorf("one of spec.tlsSecretRef or spec.freeCertificate is required"))
	}

	crt, key, err := r.readTLSSecret(ctx, &cert)
	if err != nil {
		return r.fail(ctx, &cert, err)
	}
	fingerprint := fingerprintOf(crt, key)
	displayName := defaultStr(cert.Spec.DisplayName, cert.Name)

	changed := false
	switch {
	case cert.Status.CertificateID == nil:
		// Create (or adopt by displayName).
		id, err := r.ensureCertificate(ctx, svc, displayName, crt, key)
		if err != nil {
			return r.fail(ctx, &cert, err)
		}
		cert.Status.CertificateID = &id
		cert.Status.Fingerprint = fingerprint
		changed = true
	case cert.Status.Fingerprint != fingerprint:
		// Rotation: the SSL API has no update of certificate material, so
		// replace the certificate (delete then create).
		if err := svc.DeleteCertificate(*cert.Status.CertificateID); err != nil {
			return r.fail(ctx, &cert, fmt.Errorf("deleting rotated certificate %d: %w", *cert.Status.CertificateID, err))
		}
		created, err := svc.CreateCertificate(&iplb.Certificate{DisplayName: displayName, Certificate: crt, Key: key})
		if err != nil {
			return r.fail(ctx, &cert, fmt.Errorf("recreating rotated certificate: %w", err))
		}
		log.Info("rotated certificate", "certificateId", created.ID)
		cert.Status.CertificateID = &created.ID
		cert.Status.Fingerprint = fingerprint
		changed = true
	}

	cert.Status.ObservedGeneration = cert.Generation
	return r.finish(ctx, &cert, svc, changed)
}

func (r *CertificateReconciler) readTLSSecret(ctx context.Context, cert *iplbv1alpha1.Certificate) (crt, key string, err error) {
	ns := cert.Spec.TLSSecretRef.Namespace
	if ns == "" {
		ns = cert.Namespace
	}
	var secret corev1.Secret
	skey := types.NamespacedName{Namespace: ns, Name: cert.Spec.TLSSecretRef.Name}
	if err := r.Get(ctx, skey, &secret); err != nil {
		return "", "", fmt.Errorf("reading TLS secret %s: %w", skey, err)
	}
	crt = string(secret.Data[corev1.TLSCertKey])
	key = string(secret.Data[corev1.TLSPrivateKeyKey])
	if crt == "" || key == "" {
		return "", "", fmt.Errorf("secret %s missing %q and/or %q", skey, corev1.TLSCertKey, corev1.TLSPrivateKeyKey)
	}
	return crt, key, nil
}

func (r *CertificateReconciler) ensureCertificate(ctx context.Context, svc *iplb.Service, displayName, crt, key string) (int64, error) {
	log := logf.FromContext(ctx)
	ids, err := svc.ListCertificates()
	if err != nil {
		return 0, fmt.Errorf("listing certificates: %w", err)
	}
	for _, id := range ids {
		existing, err := svc.GetCertificate(id)
		if err != nil {
			return 0, fmt.Errorf("getting certificate %d: %w", id, err)
		}
		if existing.DisplayName == displayName {
			log.Info("adopted existing certificate", "certificateId", id, "displayName", displayName)
			return id, nil
		}
	}
	created, err := svc.CreateCertificate(&iplb.Certificate{DisplayName: displayName, Certificate: crt, Key: key})
	if err != nil {
		return 0, fmt.Errorf("creating certificate: %w", err)
	}
	log.Info("created certificate", "certificateId", created.ID, "displayName", displayName)
	return created.ID, nil
}

// reconcileFreeCertificate requests and tracks a managed Let's Encrypt
// certificate. It adopts an already-issued certificate matching the FQDN(s);
// otherwise it requests issuance once and waits (issuance is asynchronous).
func (r *CertificateReconciler) reconcileFreeCertificate(ctx context.Context, cert *iplbv1alpha1.Certificate, svc *iplb.Service) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	fqdns := cert.Spec.FreeCertificate.FQDN

	// Already resolved: settle status (and re-apply if there are pending changes).
	if cert.Status.CertificateID != nil {
		cert.Status.ObservedGeneration = cert.Generation
		return r.finish(ctx, cert, svc, false)
	}

	// Adopt an already-issued certificate covering the requested FQDN(s).
	id, found, err := r.findCertificateByFQDN(svc, fqdns)
	if err != nil {
		return r.fail(ctx, cert, err)
	}
	if found {
		log.Info("adopted issued free certificate", "certificateId", id, "fqdn", fqdns)
		cert.Status.CertificateID = &id
		cert.Status.ObservedGeneration = cert.Generation
		return r.finish(ctx, cert, svc, false)
	}

	// Request issuance exactly once, then poll until the certificate appears.
	if !cert.Status.FreeCertificateRequested {
		if err := svc.RequestFreeCertificate(fqdns); err != nil {
			return r.fail(ctx, cert, fmt.Errorf("requesting free certificate: %w", err))
		}
		cert.Status.FreeCertificateRequested = true
		if r.Recorder != nil {
			r.Recorder.Eventf(cert, corev1.EventTypeNormal, "FreeCertificateRequested",
				"Requested Let's Encrypt certificate for %v", fqdns)
		}
		log.Info("requested free certificate", "fqdn", fqdns)
	}

	setReady(&cert.Status.Conditions, cert.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonPending,
		"waiting for Let's Encrypt certificate issuance")
	if err := r.Status().Update(ctx, cert); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueFreeCert}, nil
}

// findCertificateByFQDN returns the id of an ssl certificate covering all of the
// requested FQDNs, if one already exists on the service.
func (r *CertificateReconciler) findCertificateByFQDN(svc *iplb.Service, fqdns []string) (int64, bool, error) {
	ids, err := svc.ListCertificates()
	if err != nil {
		return 0, false, fmt.Errorf("listing certificates: %w", err)
	}
	for _, id := range ids {
		c, err := svc.GetCertificate(id)
		if err != nil {
			return 0, false, fmt.Errorf("getting certificate %d: %w", id, err)
		}
		if certificateCoversFQDNs(c, fqdns) {
			return id, true, nil
		}
	}
	return 0, false, nil
}

// certificateCoversFQDNs reports whether the certificate covers every requested
// FQDN, matching against its SANs and subject.
func certificateCoversFQDNs(c *iplb.Certificate, fqdns []string) bool {
	if len(fqdns) == 0 {
		return false
	}
	covered := func(name string) bool {
		for _, s := range c.San {
			if s == name {
				return true
			}
		}
		return c.Subject == name || strings.Contains(c.Subject, name)
	}
	for _, f := range fqdns {
		if !covered(f) {
			return false
		}
	}
	return true
}

func (r *CertificateReconciler) reconcileDelete(ctx context.Context, cert *iplbv1alpha1.Certificate, svc *iplb.Service) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(cert, iplbv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	if cert.Status.CertificateID != nil {
		if err := svc.DeleteCertificate(*cert.Status.CertificateID); err != nil && !isNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting certificate %d: %w", *cert.Status.CertificateID, err)
		}
		log.Info("deleted certificate", "certificateId", *cert.Status.CertificateID)
		r.Refresh.RequestRefresh(cert.Spec.ServiceRef.ServiceName, svc)
	}
	controllerutil.RemoveFinalizer(cert, iplbv1alpha1.Finalizer)
	return ctrl.Result{}, r.Update(ctx, cert)
}

func (r *CertificateReconciler) finish(ctx context.Context, cert *iplbv1alpha1.Certificate, svc *iplb.Service, changed bool) (ctrl.Result, error) {
	if changed {
		r.Refresh.RequestRefresh(cert.Spec.ServiceRef.ServiceName, svc)
		setReady(&cert.Status.Conditions, cert.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonPendingRefresh,
			"certificate staged, waiting for refresh")
		if err := r.Status().Update(ctx, cert); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}

	pending, err := svc.HasPendingChanges()
	if err != nil {
		return r.fail(ctx, cert, err)
	}
	if pending {
		r.Refresh.RequestRefresh(cert.Spec.ServiceRef.ServiceName, svc)
		setReady(&cert.Status.Conditions, cert.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonPendingRefresh,
			"waiting for service-wide refresh")
		if err := r.Status().Update(ctx, cert); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}

	setReady(&cert.Status.Conditions, cert.Generation, metav1.ConditionTrue, iplbv1alpha1.ReasonApplied, "certificate applied")
	return ctrl.Result{}, r.Status().Update(ctx, cert)
}

func (r *CertificateReconciler) fail(ctx context.Context, cert *iplbv1alpha1.Certificate, err error) (ctrl.Result, error) {
	setReady(&cert.Status.Conditions, cert.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonError, err.Error())
	_ = r.Status().Update(ctx, cert)
	if isTerminalAPIError(err) {
		// A 4xx will keep failing until the spec/permissions change. Surface it
		// on status and via an Event, and back off instead of hot-looping.
		if r.Recorder != nil {
			r.Recorder.Event(cert, corev1.EventTypeWarning, "ConfigurationError", err.Error())
		}
		logf.FromContext(ctx).Info("OVHcloud API rejected the request; retrying slowly until reconfigured",
			"error", err.Error())
		return ctrl.Result{RequeueAfter: requeueTerminal}, nil
	}
	return ctrl.Result{}, err
}

func fingerprintOf(crt, key string) string {
	sum := sha256.Sum256([]byte(crt + "\x00" + key))
	return hex.EncodeToString(sum[:])
}

// SetupWithManager sets up the controller with the Manager.
func (r *CertificateReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&iplbv1alpha1.Certificate{}).
		Named("certificate").
		Complete(r)
}
