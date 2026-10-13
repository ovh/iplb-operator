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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
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

// FarmReconciler reconciles a Farm object
type FarmReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Factory  ovhclient.Factory
	Refresh  *refresh.Coordinator
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=farms,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=farms/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=farms/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

func (r *FarmReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var farm iplbv1alpha1.Farm
	if err := r.Get(ctx, req.NamespacedName, &farm); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	svc, err := serviceFor(ctx, r.Factory, farm.Spec.ServiceRef, farm.Namespace)
	if err != nil {
		setReady(&farm.Status.Conditions, farm.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonError, err.Error())
		_ = r.Status().Update(ctx, &farm)
		return ctrl.Result{}, err
	}

	// Deletion.
	if !farm.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &farm, svc)
	}

	// Ensure finalizer.
	if controllerutil.AddFinalizer(&farm, iplbv1alpha1.Finalizer) {
		if err := r.Update(ctx, &farm); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	proto := farm.Spec.Protocol
	changed := false

	// Reconcile the farm object itself.
	desired := buildFarm(&farm)
	if farm.Status.FarmID == nil {
		id, err := r.ensureFarm(ctx, svc, &farm, desired)
		if err != nil {
			return r.fail(ctx, &farm, err)
		}
		farm.Status.FarmID = &id
		changed = true
	} else {
		current, err := svc.GetFarm(proto, *farm.Status.FarmID)
		if isNotFound(err) {
			// The stored farm vanished on the API (e.g. a staged-but-unapplied
			// create was discarded). Forget it and recreate on the next pass.
			log.Info("stored farm no longer exists; recreating", "farmId", *farm.Status.FarmID)
			farm.Status.FarmID = nil
			farm.Status.ServerIDs = nil
			if err := r.Status().Update(ctx, &farm); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		if err != nil {
			return r.fail(ctx, &farm, fmt.Errorf("getting farm %d: %w", *farm.Status.FarmID, err))
		}
		if farmDrift(current, desired) {
			desired.FarmID = *farm.Status.FarmID
			clearProbeIfUnset(desired, proto)
			if err := svc.UpdateFarm(proto, *farm.Status.FarmID, desired); err != nil {
				return r.fail(ctx, &farm, fmt.Errorf("updating farm %d: %w", *farm.Status.FarmID, err))
			}
			log.Info("updated farm", "farmId", *farm.Status.FarmID)
			changed = true
		}
	}

	// Reconcile servers.
	serverChanged, err := r.reconcileServers(ctx, svc, &farm)
	if err != nil {
		return r.fail(ctx, &farm, err)
	}
	changed = changed || serverChanged

	farm.Status.ObservedGeneration = farm.Generation
	return r.finish(ctx, &farm, svc, changed)
}

func (r *FarmReconciler) reconcileDelete(ctx context.Context, farm *iplbv1alpha1.Farm, svc *iplb.Service) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if !controllerutil.ContainsFinalizer(farm, iplbv1alpha1.Finalizer) {
		return ctrl.Result{}, nil
	}
	if farm.Status.FarmID != nil {
		if err := svc.DeleteFarm(farm.Spec.Protocol, *farm.Status.FarmID); err != nil && !isNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting farm %d: %w", *farm.Status.FarmID, err)
		}
		log.Info("deleted farm", "farmId", *farm.Status.FarmID)
		r.Refresh.RequestRefresh(farm.Spec.ServiceRef.ServiceName, svc)
	}
	controllerutil.RemoveFinalizer(farm, iplbv1alpha1.Finalizer)
	return ctrl.Result{}, r.Update(ctx, farm)
}

// ensureFarm adopts a pre-existing farm with the same displayName, or creates a
// new one, returning its id.
func (r *FarmReconciler) ensureFarm(ctx context.Context, svc *iplb.Service, farm *iplbv1alpha1.Farm, desired *iplb.Farm) (int64, error) {
	log := logf.FromContext(ctx)
	ids, err := svc.ListFarms(farm.Spec.Protocol)
	if err != nil {
		return 0, fmt.Errorf("listing farms: %w", err)
	}
	for _, id := range ids {
		existing, err := svc.GetFarm(farm.Spec.Protocol, id)
		if err != nil {
			return 0, fmt.Errorf("getting farm %d: %w", id, err)
		}
		if existing.DisplayName == desired.DisplayName {
			log.Info("adopted existing farm", "farmId", id, "displayName", desired.DisplayName)
			if farmDrift(existing, desired) {
				clearProbeIfUnset(desired, farm.Spec.Protocol)
				if err := svc.UpdateFarm(farm.Spec.Protocol, id, desired); err != nil {
					return 0, fmt.Errorf("updating adopted farm %d: %w", id, err)
				}
			}
			return id, nil
		}
	}
	created, err := svc.CreateFarm(farm.Spec.Protocol, desired)
	if err != nil {
		return 0, fmt.Errorf("creating farm: %w", err)
	}
	log.Info("created farm", "farmId", created.FarmID, "displayName", desired.DisplayName)
	return created.FarmID, nil
}

func (r *FarmReconciler) reconcileServers(ctx context.Context, svc *iplb.Service, farm *iplbv1alpha1.Farm) (bool, error) {
	log := logf.FromContext(ctx)
	proto := farm.Spec.Protocol
	farmID := *farm.Status.FarmID

	// Current servers by address.
	ids, err := svc.ListServers(proto, farmID)
	if err != nil {
		return false, fmt.Errorf("listing servers: %w", err)
	}
	current := map[string]*iplb.Server{}
	for _, id := range ids {
		s, err := svc.GetServer(proto, farmID, id)
		if err != nil {
			return false, fmt.Errorf("getting server %d: %w", id, err)
		}
		current[s.Address] = s
	}

	desired := map[string]iplbv1alpha1.FarmServer{}
	for _, s := range farm.Spec.Servers {
		desired[s.Address] = s
	}

	if farm.Status.ServerIDs == nil {
		farm.Status.ServerIDs = map[string]int64{}
	}
	changed := false

	// Create / update.
	for addr, ds := range desired {
		want := buildServer(ds)
		if proto == protocolUDP {
			// UDP servers support only address/port/status.
			want.Weight = nil
			want.SSL = nil
			want.Backup = nil
			want.Probe = nil
		}
		cur, ok := current[addr]
		if !ok {
			created, err := svc.CreateServer(proto, farmID, want)
			if err != nil {
				return changed, fmt.Errorf("creating server %s: %w", addr, err)
			}
			farm.Status.ServerIDs[addr] = created.ServerID
			log.Info("created server", "address", addr, "serverId", created.ServerID)
			changed = true
			continue
		}
		if serverDrift(cur, want) {
			if err := svc.UpdateServer(proto, farmID, cur.ServerID, want); err != nil {
				return changed, fmt.Errorf("updating server %s: %w", addr, err)
			}
			log.Info("updated server", "address", addr, "serverId", cur.ServerID)
			changed = true
		}
		farm.Status.ServerIDs[addr] = cur.ServerID
	}

	// Delete servers no longer desired.
	for addr, cur := range current {
		if _, ok := desired[addr]; ok {
			continue
		}
		if err := svc.DeleteServer(proto, farmID, cur.ServerID); err != nil {
			return changed, fmt.Errorf("deleting server %s: %w", addr, err)
		}
		delete(farm.Status.ServerIDs, addr)
		log.Info("deleted server", "address", addr, "serverId", cur.ServerID)
		changed = true
	}

	return changed, nil
}

// finish sets the Ready condition based on whether changes were staged and
// whether the service still has pending changes, then persists status.
func (r *FarmReconciler) finish(ctx context.Context, farm *iplbv1alpha1.Farm, svc *iplb.Service, changed bool) (ctrl.Result, error) {
	if changed {
		r.Refresh.RequestRefresh(farm.Spec.ServiceRef.ServiceName, svc)
		setReady(&farm.Status.Conditions, farm.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonPendingRefresh,
			"configuration staged, waiting for refresh")
		if err := r.Status().Update(ctx, farm); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}

	pending, err := svc.HasPendingChanges()
	if err != nil {
		return r.fail(ctx, farm, err)
	}
	if pending {
		r.Refresh.RequestRefresh(farm.Spec.ServiceRef.ServiceName, svc)
		setReady(&farm.Status.Conditions, farm.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonPendingRefresh,
			"waiting for service-wide refresh")
		if err := r.Status().Update(ctx, farm); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}

	setReady(&farm.Status.Conditions, farm.Generation, metav1.ConditionTrue, iplbv1alpha1.ReasonApplied, "farm applied")
	return ctrl.Result{}, r.Status().Update(ctx, farm)
}

func (r *FarmReconciler) fail(ctx context.Context, farm *iplbv1alpha1.Farm, err error) (ctrl.Result, error) {
	setReady(&farm.Status.Conditions, farm.Generation, metav1.ConditionFalse, iplbv1alpha1.ReasonError, err.Error())
	_ = r.Status().Update(ctx, farm)
	if isTerminalAPIError(err) {
		// A 4xx will keep failing until the spec/permissions change. Surface it
		// on status and via an Event, and back off instead of hot-looping.
		if r.Recorder != nil {
			r.Recorder.Event(farm, corev1.EventTypeWarning, "ConfigurationError", err.Error())
		}
		logf.FromContext(ctx).Info("OVHcloud API rejected the request; retrying slowly until reconfigured",
			"error", err.Error())
		return ctrl.Result{RequeueAfter: requeueTerminal}, nil
	}
	return ctrl.Result{}, err
}

// ---- spec <-> API conversion ----

func buildFarm(farm *iplbv1alpha1.Farm) *iplb.Farm {
	f := &iplb.Farm{
		DisplayName:    defaultStr(farm.Spec.DisplayName, farm.Name),
		Zone:           farm.Spec.Zone,
		Balance:        farm.Spec.Balance,
		Stickiness:     farm.Spec.Stickiness,
		Port:           farm.Spec.Port,
		VrackNetworkID: farm.Spec.VrackNetworkID,
	}
	if p := farm.Spec.Probe; p != nil {
		f.Probe = &iplb.Probe{
			Type:     p.Type,
			Port:     p.Port,
			Interval: p.Interval,
			Method:   p.Method,
			URL:      p.URL,
			Match:    p.Match,
			Pattern:  p.Pattern,
		}
	}
	// UDP farms support none of these; the API rejects them.
	if farm.Spec.Protocol == protocolUDP {
		f.Balance = ""
		f.Stickiness = ""
		f.Probe = nil
	}
	return f
}

// clearProbeIfUnset forces an explicit empty probe object on the update payload
// when the spec declares no probe. A nil probe would be omitted from the request
// and leave any existing probe in place; sending "probe": {} clears it. UDP
// farms have no probe field, so this is a no-op for them.
func clearProbeIfUnset(desired *iplb.Farm, proto string) {
	if proto == protocolUDP {
		return
	}
	if desired.Probe == nil {
		desired.Probe = &iplb.Probe{}
	}
}

// farmDrift reports whether the farm must be updated. Only fields explicitly
// set in the desired spec are compared, so we never fight the API's own
// defaults for fields the user left unset.
func farmDrift(current, desired *iplb.Farm) bool {
	if desired.DisplayName != "" && current.DisplayName != desired.DisplayName {
		return true
	}
	if desired.Zone != "" && current.Zone != desired.Zone {
		return true
	}
	if desired.Balance != "" && current.Balance != desired.Balance {
		return true
	}
	if desired.Stickiness != "" && current.Stickiness != desired.Stickiness {
		return true
	}
	if desired.Port != nil && !intpEqual(current.Port, desired.Port) {
		return true
	}
	if desired.VrackNetworkID != nil && !int64pEqual(current.VrackNetworkID, desired.VrackNetworkID) {
		return true
	}
	return probeDrift(current.Probe, desired.Probe)
}

func probeDrift(current, desired *iplb.Probe) bool {
	if desired == nil || desired.Type == "" {
		// Spec declares no probe: drift if the farm currently has one, so it
		// gets cleared. (There is no OVHcloud-default probe to fight here.)
		return current != nil && current.Type != ""
	}
	if current == nil {
		return true
	}
	if desired.Type != "" && current.Type != desired.Type {
		return true
	}
	if desired.Port != nil && !intpEqual(current.Port, desired.Port) {
		return true
	}
	if desired.Interval != nil && !intpEqual(current.Interval, desired.Interval) {
		return true
	}
	return (desired.Method != "" && current.Method != desired.Method) ||
		(desired.URL != "" && current.URL != desired.URL) ||
		(desired.Match != "" && current.Match != desired.Match) ||
		(desired.Pattern != "" && current.Pattern != desired.Pattern)
}

func buildServer(s iplbv1alpha1.FarmServer) *iplb.Server {
	return &iplb.Server{
		Address:     s.Address,
		Port:        s.Port,
		DisplayName: s.DisplayName,
		Weight:      s.Weight,
		Status:      defaultStr(s.Status, "active"),
		Backup:      s.Backup,
		SSL:         s.SSL,
		Probe:       s.Probe,
	}
}

// serverDrift compares only fields the desired spec sets explicitly.
func serverDrift(current, desired *iplb.Server) bool {
	if desired.Port != nil && !intpEqual(current.Port, desired.Port) {
		return true
	}
	if desired.DisplayName != "" && current.DisplayName != desired.DisplayName {
		return true
	}
	if desired.Weight != nil && !intpEqual(current.Weight, desired.Weight) {
		return true
	}
	if desired.Status != "" && current.Status != desired.Status {
		return true
	}
	if desired.Backup != nil && !boolpEqual(current.Backup, desired.Backup) {
		return true
	}
	if desired.SSL != nil && !boolpEqual(current.SSL, desired.SSL) {
		return true
	}
	if desired.Probe != nil && !boolpEqual(current.Probe, desired.Probe) {
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *FarmReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&iplbv1alpha1.Farm{}).
		Named("farm").
		Complete(r)
}
