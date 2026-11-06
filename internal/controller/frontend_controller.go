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
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/iplb"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
	"github.com/ovhcloud/iplb-operator/internal/refresh"
)

// FrontendReconciler reconciles a Frontend object
type FrontendReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Factory  ovhclient.Factory
	Refresh  *refresh.Coordinator
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=frontends,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=frontends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=frontends/finalizers,verbs=update
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=farms,verbs=get;list;watch

func (r *FrontendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var fe iplbv1alpha1.Frontend
	if err := r.Get(ctx, req.NamespacedName, &fe); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	svc, err := serviceFor(ctx, r.Factory, fe.Spec.ServiceRef, fe.Namespace)
	if err != nil {
		setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonError, err.Error())
		_ = r.Status().Update(ctx, &fe)
		return ctrl.Result{}, err
	}

	if !fe.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &fe, svc)
	}

	if controllerutil.AddFinalizer(&fe, iplbv1alpha1.Finalizer) {
		if err := r.Update(ctx, &fe); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Reject spec combinations the OVHcloud API forbids, before any API call, so
	// the error is a clear condition/Event rather than a remote 400 loop.
	if err := validateFrontendSpec(&fe); err != nil {
		setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonError, err.Error())
		if r.Recorder != nil {
			r.Recorder.Eventf(&fe, nil, corev1.EventTypeWarning, "InvalidConfiguration", eventActionReconcile, "%s", err.Error())
		}
		// A spec fix re-triggers reconciliation; no point requeueing.
		return ctrl.Result{}, r.Status().Update(ctx, &fe)
	}

	// Resolve the default farm id (from a Farm CR reference or a raw id).
	farmID, ready, err := r.resolveDefaultFarm(ctx, &fe)
	if err != nil {
		return r.fail(ctx, &fe, err)
	}
	if !ready {
		setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonDependencyNotReady,
			fmt.Sprintf("referenced farm %q not ready", fe.Spec.DefaultFarmRef))
		if err := r.Status().Update(ctx, &fe); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}
	fe.Status.ResolvedFarmID = farmID

	// Resolve the default SSL certificate (from a Certificate CR or a raw id).
	sslID, sslReady, err := r.resolveDefaultSSL(ctx, &fe)
	if err != nil {
		return r.fail(ctx, &fe, err)
	}
	if !sslReady {
		setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonDependencyNotReady,
			fmt.Sprintf("referenced certificate %q not ready", fe.Spec.DefaultCertificateRef))
		if err := r.Status().Update(ctx, &fe); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}
	fe.Status.ResolvedSslID = sslID

	proto := fe.Spec.Protocol
	desired := buildFrontend(&fe, farmID, sslID)
	changed := false

	if fe.Status.FrontendID == nil {
		id, err := r.ensureFrontend(ctx, svc, &fe, desired)
		if err != nil {
			return r.fail(ctx, &fe, err)
		}
		fe.Status.FrontendID = &id
		changed = true
	} else {
		current, err := svc.GetFrontend(proto, *fe.Status.FrontendID)
		if isNotFound(err) {
			log.Info("stored frontend no longer exists; recreating", "frontendId", *fe.Status.FrontendID)
			fe.Status.FrontendID = nil
			fe.Status.RouteIDs = nil
			if err := r.Status().Update(ctx, &fe); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{RequeueAfter: requeueRecreate}, nil
		}
		if err != nil {
			return r.fail(ctx, &fe, fmt.Errorf("getting frontend %d: %w", *fe.Status.FrontendID, err))
		}
		if frontendDrift(current, desired) {
			desired.FrontendID = *fe.Status.FrontendID
			if err := svc.UpdateFrontend(proto, *fe.Status.FrontendID, desired); err != nil {
				return r.fail(ctx, &fe, fmt.Errorf("updating frontend %d: %w", *fe.Status.FrontendID, err))
			}
			log.Info("updated frontend", "frontendId", *fe.Status.FrontendID)
			changed = true
		}
	}

	// Reconcile HTTP routes (http frontends only).
	routeChanged, err := r.reconcileRoutes(ctx, svc, &fe)
	if err != nil {
		return r.fail(ctx, &fe, err)
	}
	changed = changed || routeChanged

	fe.Status.ObservedGeneration = fe.Generation
	return r.finish(ctx, &fe, svc, changed)
}

// resolveDefaultFarm returns the default farm id for the frontend. The bool is
// false when a referenced Farm CR is missing or not yet reconciled (no farmId).
func (r *FrontendReconciler) resolveDefaultFarm(ctx context.Context, fe *iplbv1alpha1.Frontend) (*int64, bool, error) {
	if fe.Spec.DefaultFarmID != nil {
		return fe.Spec.DefaultFarmID, true, nil
	}
	if fe.Spec.DefaultFarmRef == "" {
		return nil, true, nil // no default farm configured
	}
	var farm iplbv1alpha1.Farm
	key := types.NamespacedName{Namespace: fe.Namespace, Name: fe.Spec.DefaultFarmRef}
	if err := r.Get(ctx, key, &farm); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, false, nil // not created yet; wait
		}
		return nil, false, err
	}
	if farm.Status.FarmID == nil {
		return nil, false, nil // exists but not reconciled yet
	}
	return farm.Status.FarmID, true, nil
}

// resolveDefaultSSL returns the default SSL id for the frontend. The bool is
// false when a referenced Certificate CR is missing or not yet reconciled.
func (r *FrontendReconciler) resolveDefaultSSL(ctx context.Context, fe *iplbv1alpha1.Frontend) (*int64, bool, error) {
	if fe.Spec.DefaultSslID != nil {
		return fe.Spec.DefaultSslID, true, nil
	}
	if fe.Spec.DefaultCertificateRef == "" {
		return nil, true, nil // no certificate configured
	}
	var cert iplbv1alpha1.Certificate
	key := types.NamespacedName{Namespace: fe.Namespace, Name: fe.Spec.DefaultCertificateRef}
	if err := r.Get(ctx, key, &cert); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return nil, false, nil // not created yet; wait
		}
		return nil, false, err
	}
	if cert.Status.CertificateID == nil {
		return nil, false, nil // exists but not uploaded yet
	}
	return cert.Status.CertificateID, true, nil
}

func (r *FrontendReconciler) reconcileDelete(ctx context.Context, fe *iplbv1alpha1.Frontend, svc *iplb.Service) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(fe, iplbv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	// Delete managed routes first (they reference the frontend).
	for name, id := range fe.Status.RouteIDs {
		if err := svc.DeleteRoute(id); err != nil && !isNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting route %d: %w", id, err)
		}
		delete(fe.Status.RouteIDs, name)
	}
	if fe.Status.FrontendID != nil {
		if err := svc.DeleteFrontend(fe.Spec.Protocol, *fe.Status.FrontendID); err != nil && !isNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting frontend %d: %w", *fe.Status.FrontendID, err)
		}
		log.Info("deleted frontend", "frontendId", *fe.Status.FrontendID)
		r.Refresh.RequestRefresh(fe.Spec.ServiceRef.ServiceName, svc)
	}
	controllerutil.RemoveFinalizer(fe, iplbv1alpha1.Finalizer)
	return ctrl.Result{}, r.Update(ctx, fe)
}

func (r *FrontendReconciler) ensureFrontend(ctx context.Context, svc *iplb.Service, fe *iplbv1alpha1.Frontend, desired *iplb.Frontend) (int64, error) {
	log := logf.FromContext(ctx)
	ids, err := svc.ListFrontends(fe.Spec.Protocol)
	if err != nil {
		return 0, fmt.Errorf("listing frontends: %w", err)
	}
	for _, id := range ids {
		existing, err := svc.GetFrontend(fe.Spec.Protocol, id)
		if err != nil {
			return 0, fmt.Errorf("getting frontend %d: %w", id, err)
		}
		if existing.DisplayName == desired.DisplayName {
			log.Info("adopted existing frontend", "frontendId", id, "displayName", desired.DisplayName)
			if frontendDrift(existing, desired) {
				if err := svc.UpdateFrontend(fe.Spec.Protocol, id, desired); err != nil {
					return 0, fmt.Errorf("updating adopted frontend %d: %w", id, err)
				}
			}
			return id, nil
		}
	}
	created, err := svc.CreateFrontend(fe.Spec.Protocol, desired)
	if err != nil {
		return 0, fmt.Errorf("creating frontend: %w", err)
	}
	log.Info("created frontend", "frontendId", created.FrontendID, "displayName", desired.DisplayName)
	return created.FrontendID, nil
}

func (r *FrontendReconciler) finish(ctx context.Context, fe *iplbv1alpha1.Frontend, svc *iplb.Service, changed bool) (ctrl.Result, error) {
	if changed {
		r.Refresh.RequestRefresh(fe.Spec.ServiceRef.ServiceName, svc)
		setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonPendingRefresh,
			"configuration staged, waiting for refresh")
		if err := r.Status().Update(ctx, fe); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}

	pending, err := svc.HasPendingChanges()
	if err != nil {
		return r.fail(ctx, fe, err)
	}
	if pending {
		r.Refresh.RequestRefresh(fe.Spec.ServiceRef.ServiceName, svc)
		setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonPendingRefresh,
			"waiting for service-wide refresh")
		if err := r.Status().Update(ctx, fe); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}

	setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionTrue, iplbv1alpha1.ReasonApplied, "frontend applied")
	return ctrl.Result{}, r.Status().Update(ctx, fe)
}

func (r *FrontendReconciler) fail(ctx context.Context, fe *iplbv1alpha1.Frontend, err error) (ctrl.Result, error) {
	setReady(&fe.Status.Conditions, fe.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonError, err.Error())
	_ = r.Status().Update(ctx, fe)
	if isTerminalAPIError(err) {
		// A 4xx will keep failing until the spec/permissions change. Surface it
		// on status and via an Event, and back off instead of hot-looping.
		if r.Recorder != nil {
			r.Recorder.Eventf(fe, nil, corev1.EventTypeWarning, "ConfigurationError", eventActionReconcile, "%s", err.Error())
		}
		logf.FromContext(ctx).Info("OVHcloud API rejected the request; retrying slowly until reconfigured",
			"error", err.Error())
		return ctrl.Result{RequeueAfter: requeueTerminal}, nil
	}
	return ctrl.Result{}, err
}

// validateFrontendSpec rejects spec combinations the OVHcloud API forbids. UDP
// frontends ignore source ACLs entirely, so the check does not apply to them.
func validateFrontendSpec(fe *iplbv1alpha1.Frontend) error {
	if fe.Spec.Protocol == protocolUDP {
		return nil
	}
	if len(fe.Spec.AllowedSource) > 0 && len(fe.Spec.DeniedSource) > 0 {
		return fmt.Errorf("allowedSource and deniedSource are mutually exclusive; set only one")
	}
	return nil
}

func buildFrontend(fe *iplbv1alpha1.Frontend, farmID, sslID *int64) *iplb.Frontend {
	f := &iplb.Frontend{
		DisplayName:   defaultStr(fe.Spec.DisplayName, fe.Name),
		Zone:          fe.Spec.Zone,
		Port:          fe.Spec.Port,
		DefaultFarmID: farmID,
		Disabled:      fe.Spec.Disabled,
	}
	// UDP frontends support none of the TLS / source-restriction fields.
	if fe.Spec.Protocol == protocolUDP {
		return f
	}
	f.AllowedSource = fe.Spec.AllowedSource
	f.DeniedSource = fe.Spec.DeniedSource
	f.SSL = fe.Spec.SSL
	f.DefaultSslID = sslID
	f.ForceSni = fe.Spec.ForceSni
	f.HSTS = fe.Spec.HSTS
	f.RedirectLocation = fe.Spec.RedirectLocation
	// A configured certificate implies TLS termination unless explicitly set.
	if sslID != nil && f.SSL == nil {
		f.SSL = boolp(true)
	}
	return f
}

func frontendDrift(current, desired *iplb.Frontend) bool {
	if desired.DisplayName != "" && current.DisplayName != desired.DisplayName {
		return true
	}
	if desired.Zone != "" && current.Zone != desired.Zone {
		return true
	}
	if desired.Port != "" && current.Port != desired.Port {
		return true
	}
	if desired.DefaultFarmID != nil && !int64pEqual(current.DefaultFarmID, desired.DefaultFarmID) {
		return true
	}
	if desired.SSL != nil && !boolpEqual(current.SSL, desired.SSL) {
		return true
	}
	if desired.DefaultSslID != nil && !int64pEqual(current.DefaultSslID, desired.DefaultSslID) {
		return true
	}
	if desired.ForceSni != nil && !boolpEqual(current.ForceSni, desired.ForceSni) {
		return true
	}
	if desired.HSTS != nil && !boolpEqual(current.HSTS, desired.HSTS) {
		return true
	}
	if desired.RedirectLocation != "" && current.RedirectLocation != desired.RedirectLocation {
		return true
	}
	if desired.Disabled != nil && !boolpEqual(current.Disabled, desired.Disabled) {
		return true
	}
	if desired.AllowedSource != nil && !strSliceEqual(current.AllowedSource, desired.AllowedSource) {
		return true
	}
	if desired.DeniedSource != nil && !strSliceEqual(current.DeniedSource, desired.DeniedSource) {
		return true
	}
	return false
}

// reconcileRoutes brings the frontend's HTTP routes (and their rules) in line
// with the spec. Routes are only valid on http frontends. Managed route ids are
// tracked in status.routeIDs so routes dropped from the spec can be deleted.
func (r *FrontendReconciler) reconcileRoutes(ctx context.Context, svc *iplb.Service, fe *iplbv1alpha1.Frontend) (bool, error) {
	if fe.Spec.Protocol != protocolHTTP || fe.Status.FrontendID == nil {
		return false, nil
	}
	log := logf.FromContext(ctx)
	frontendID := *fe.Status.FrontendID
	if fe.Status.RouteIDs == nil {
		fe.Status.RouteIDs = map[string]int64{}
	}
	changed := false

	desired := map[string]iplbv1alpha1.Route{}
	for _, rt := range fe.Spec.Routes {
		desired[rt.Name] = rt
	}

	for name, rt := range desired {
		action, err := r.buildRouteAction(ctx, fe, rt.Action)
		if err != nil {
			return changed, err
		}
		want := &iplb.Route{DisplayName: name, FrontendID: &frontendID, Weight: rt.Weight, Action: *action}

		id, ok := fe.Status.RouteIDs[name]
		if ok {
			if _, err := svc.GetRoute(id); isNotFound(err) {
				// Stored route vanished; recreate it below.
				delete(fe.Status.RouteIDs, name)
				ok = false
			}
		}
		if !ok {
			newID, c, err := r.ensureRoute(ctx, svc, want)
			if err != nil {
				return changed, err
			}
			id = newID
			fe.Status.RouteIDs[name] = id
			changed = changed || c
		} else {
			current, err := svc.GetRoute(id)
			if err != nil {
				return changed, fmt.Errorf("getting route %d: %w", id, err)
			}
			if routeDrift(current, want) {
				want.RouteID = id
				if err := svc.UpdateRoute(id, want); err != nil {
					return changed, fmt.Errorf("updating route %d: %w", id, err)
				}
				log.Info("updated route", "routeId", id, "name", name)
				changed = true
			}
		}

		ruleChanged, err := r.reconcileRouteRules(ctx, svc, id, rt.Rules)
		if err != nil {
			return changed, err
		}
		changed = changed || ruleChanged
	}

	// Delete routes no longer desired.
	for name, id := range fe.Status.RouteIDs {
		if _, ok := desired[name]; ok {
			continue
		}
		if err := svc.DeleteRoute(id); err != nil && !isNotFound(err) {
			return changed, fmt.Errorf("deleting route %d: %w", id, err)
		}
		delete(fe.Status.RouteIDs, name)
		log.Info("deleted route", "routeId", id, "name", name)
		changed = true
	}
	return changed, nil
}

// buildRouteAction maps a spec action to the API action, resolving a Farm CR
// reference to its farm id for "farm" actions.
func (r *FrontendReconciler) buildRouteAction(ctx context.Context, fe *iplbv1alpha1.Frontend, a iplbv1alpha1.RouteAction) (*iplb.RouteAction, error) {
	out := &iplb.RouteAction{Type: a.Type, Target: a.Target, Status: a.Status}
	if a.Type == "farm" && a.TargetFarmRef != "" {
		var farm iplbv1alpha1.Farm
		key := types.NamespacedName{Namespace: fe.Namespace, Name: a.TargetFarmRef}
		if err := r.Get(ctx, key, &farm); err != nil {
			return nil, fmt.Errorf("route target farm %q: %w", a.TargetFarmRef, err)
		}
		if farm.Status.FarmID == nil {
			return nil, fmt.Errorf("route target farm %q not ready", a.TargetFarmRef)
		}
		out.Target = strconv.FormatInt(*farm.Status.FarmID, 10)
	}
	return out, nil
}

// ensureRoute adopts a route with the same displayName on the frontend, or
// creates one. Returns the route id and whether anything changed.
func (r *FrontendReconciler) ensureRoute(ctx context.Context, svc *iplb.Service, want *iplb.Route) (int64, bool, error) {
	log := logf.FromContext(ctx)
	ids, err := svc.ListRoutes()
	if err != nil {
		return 0, false, fmt.Errorf("listing routes: %w", err)
	}
	for _, id := range ids {
		existing, err := svc.GetRoute(id)
		if err != nil {
			return 0, false, fmt.Errorf("getting route %d: %w", id, err)
		}
		if existing.DisplayName == want.DisplayName && int64pEqual(existing.FrontendID, want.FrontendID) {
			if routeDrift(existing, want) {
				want.RouteID = id
				if err := svc.UpdateRoute(id, want); err != nil {
					return 0, false, fmt.Errorf("updating adopted route %d: %w", id, err)
				}
				return id, true, nil
			}
			return id, false, nil
		}
	}
	created, err := svc.CreateRoute(want)
	if err != nil {
		return 0, false, fmt.Errorf("creating route: %w", err)
	}
	log.Info("created route", "routeId", created.RouteID, "name", want.DisplayName)
	return created.RouteID, true, nil
}

func routeDrift(current, desired *iplb.Route) bool {
	if desired.DisplayName != "" && current.DisplayName != desired.DisplayName {
		return true
	}
	if desired.Weight != nil && !intpEqual(current.Weight, desired.Weight) {
		return true
	}
	if current.Action.Type != desired.Action.Type || current.Action.Target != desired.Action.Target {
		return true
	}
	if desired.Action.Status != nil && !intpEqual(current.Action.Status, desired.Action.Status) {
		return true
	}
	return false
}

// reconcileRouteRules makes a route's rules match the spec. Rules have no stable
// natural key and are cheap, so when the set differs they are replaced wholesale.
func (r *FrontendReconciler) reconcileRouteRules(ctx context.Context, svc *iplb.Service, routeID int64, desired []iplbv1alpha1.RouteRule) (bool, error) {
	ids, err := svc.ListRouteRules(routeID)
	if err != nil {
		return false, fmt.Errorf("listing rules of route %d: %w", routeID, err)
	}
	current := make([]*iplb.RouteRule, 0, len(ids))
	for _, id := range ids {
		rule, err := svc.GetRouteRule(routeID, id)
		if err != nil {
			return false, fmt.Errorf("getting rule %d of route %d: %w", id, routeID, err)
		}
		current = append(current, rule)
	}
	if rulesEqual(current, desired) {
		return false, nil
	}
	for _, id := range ids {
		if err := svc.DeleteRouteRule(routeID, id); err != nil && !isNotFound(err) {
			return true, fmt.Errorf("deleting rule %d of route %d: %w", id, routeID, err)
		}
	}
	for i := range desired {
		d := desired[i]
		if _, err := svc.CreateRouteRule(routeID, &iplb.RouteRule{
			Field:    d.Field,
			Match:    d.Match,
			Pattern:  d.Pattern,
			SubField: d.SubField,
			Negate:   d.Negate,
		}); err != nil {
			return true, fmt.Errorf("creating rule on route %d: %w", routeID, err)
		}
	}
	return true, nil
}

// rulesEqual compares the current API rules with the desired spec rules,
// order-independent (rules are ANDed).
func rulesEqual(current []*iplb.RouteRule, desired []iplbv1alpha1.RouteRule) bool {
	if len(current) != len(desired) {
		return false
	}
	key := func(field, match, pattern, subField string, negate *bool) string {
		neg := false
		if negate != nil {
			neg = *negate
		}
		return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%t", field, match, pattern, subField, neg)
	}
	cur := make([]string, 0, len(current))
	for _, c := range current {
		cur = append(cur, key(c.Field, c.Match, c.Pattern, c.SubField, c.Negate))
	}
	des := make([]string, 0, len(desired))
	for _, d := range desired {
		des = append(des, key(d.Field, d.Match, d.Pattern, d.SubField, d.Negate))
	}
	sort.Strings(cur)
	sort.Strings(des)
	for i := range cur {
		if cur[i] != des[i] {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *FrontendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&iplbv1alpha1.Frontend{}).
		Named("frontend").
		Complete(r)
}
