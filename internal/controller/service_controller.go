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
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	iplbv1alpha1 "github.com/ovhcloud/iplb-operator/api/v1alpha1"
	"github.com/ovhcloud/iplb-operator/internal/ovhclient"
)

// LoadBalancerClass is the spec.loadBalancerClass value a Service must carry to
// be managed by this operator. It mirrors the K8s-native opt-in mechanism so we
// coexist cleanly with other load-balancer controllers.
const LoadBalancerClass = "iplb.ovhcloud.com/iplb"

// Annotations on a Service that configure how it maps to OVHcloud IPLB.
const (
	// AnnServiceName is the OVHcloud IPLB serviceName (e.g. "loadbalancer-xxxxxxxx"). Required.
	AnnServiceName = "iplb.ovhcloud.com/service-name"
	// AnnCredentialsSecret is the name of the Secret holding OAuth2 credentials. Required.
	AnnCredentialsSecret = "iplb.ovhcloud.com/credentials-secret"
	// AnnCredentialsNamespace overrides the namespace of the credentials Secret (defaults to the Service namespace).
	AnnCredentialsNamespace = "iplb.ovhcloud.com/credentials-namespace"
	// AnnEndpoint selects the go-ovh API endpoint (ovh-eu, ovh-ca, ovh-us).
	AnnEndpoint = "iplb.ovhcloud.com/endpoint"
	// AnnZone pins the farms/frontends to a balancing zone (e.g. "gra", "all").
	AnnZone = "iplb.ovhcloud.com/zone"
	// AnnProtocol overrides the farm/frontend protocol (http or tcp) for every port.
	AnnProtocol = "iplb.ovhcloud.com/protocol"
	// AnnBalance overrides the farm load-balancing algorithm (default roundrobin).
	AnnBalance = "iplb.ovhcloud.com/balance"
	// AnnNodeAddressType selects which node address feeds the farm servers (InternalIP or ExternalIP).
	AnnNodeAddressType = "iplb.ovhcloud.com/node-address-type"
	// AnnIP overrides the IP written back to the Service status, skipping the API lookup.
	AnnIP = "iplb.ovhcloud.com/ip"
	// AnnVrackNetworkID sets the vRack private network id farms attach to. Required
	// (or auto-discovered) when the load balancer is attached to a vRack.
	AnnVrackNetworkID = "iplb.ovhcloud.com/vrack-network-id"
	// AnnDeniedSource is a comma-separated list of CIDRs blocked on the frontends
	// (deny-list). Complements the Service's loadBalancerSourceRanges allow-list.
	AnnDeniedSource = "iplb.ovhcloud.com/denied-source"

	// AnnTLSCertificateRef names a Certificate custom resource served as the
	// default TLS certificate on http frontends (enables SSL termination).
	AnnTLSCertificateRef = "iplb.ovhcloud.com/tls-certificate-ref"

	// AnnTLSFreeCertificate ("true") requests a free, auto-renewed Let's Encrypt
	// certificate for the FQDN(s) in AnnTLSHost and serves it on http frontends.
	AnnTLSFreeCertificate = "iplb.ovhcloud.com/tls-free-certificate"
	// AnnTLSHost is the comma-separated FQDN(s) the free certificate covers. The
	// domains must resolve to the load balancer for ACME validation.
	AnnTLSHost = "iplb.ovhcloud.com/tls-host"

	// AnnStickiness overrides session persistence (sourceIp or cookie). When unset
	// it is derived from the Service's sessionAffinity (ClientIP -> sourceIp).
	AnnStickiness = "iplb.ovhcloud.com/stickiness"

	// Health-check (farm probe) annotations. Setting any of them enables a probe;
	// the type defaults to tcp and the port defaults to the backend (node) port.
	AnnHealthCheckProtocol = "iplb.ovhcloud.com/health-check-protocol"
	AnnHealthCheckPort     = "iplb.ovhcloud.com/health-check-port"
	AnnHealthCheckInterval = "iplb.ovhcloud.com/health-check-interval"
	AnnHealthCheckURL      = "iplb.ovhcloud.com/health-check-url"
	AnnHealthCheckMethod   = "iplb.ovhcloud.com/health-check-method"

	// Per-server (backend) settings, applied uniformly to every node server.
	// AnnServerSSL enables TLS from the load balancer to the backends.
	AnnServerSSL = "iplb.ovhcloud.com/server-ssl"
	// AnnServerBackup marks the servers as backup (only used when the active pool is down).
	AnnServerBackup = "iplb.ovhcloud.com/server-backup"
	// AnnServerWeight sets the relative weight of each backend (0-256).
	AnnServerWeight = "iplb.ovhcloud.com/server-weight"
	// AnnServerHealthCheck enables the farm probe on each server. Defaults to true
	// when a health-check probe is configured.
	AnnServerHealthCheck = "iplb.ovhcloud.com/server-health-check"
)

// Labels stamped onto the Farm and Frontend resources this controller owns.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	labelService   = "iplb.ovhcloud.com/owner-service"
	managedByValue = "iplb-operator"
)

// ServiceReconciler reconciles core Service objects of type LoadBalancer that
// opt in via spec.loadBalancerClass. For each TCP port it maintains an owned
// Farm + Frontend custom resource; the Farm/Frontend reconcilers then drive the
// OVHcloud API. Backend servers are the cluster node IPs reached on the
// Service's allocated nodePort.
type ServiceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Factory  ovhclient.Factory
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=iplb.ovhcloud.com,resources=farms;frontends;certificates,verbs=get;list;watch;create;update;patch;delete

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var svc corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &svc); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Not (or no longer) ours: garbage-collect any Farm/Frontend we previously
	// created for this Service. Owner references handle deletion of the Service
	// itself; this covers the case where the class/type is changed away.
	if !manages(&svc) {
		if err := r.reconcileFreeCertificate(ctx, &svc, iplbv1alpha1.ServiceRef{}, nil); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, r.prune(ctx, &svc, nil)
	}

	ref, err := serviceRefFromAnnotations(&svc)
	if err != nil {
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "InvalidConfiguration", eventActionReconcile, "%s", err.Error())
		// A spec/annotation fix re-triggers reconciliation; no point requeueing.
		return ctrl.Result{}, nil
	}

	addrs, err := r.nodeAddresses(ctx, &svc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(addrs) == 0 {
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "NoBackends", eventActionReconcile,
			"No ready node addresses found for the selected node-address-type")
	}

	balance := annotationOr(&svc, AnnBalance, "roundrobin")

	// Resolve the balancing zone, the load balancer IP, and (for vRack-attached
	// services) the vRack network id. Each can come from an annotation;
	// otherwise we query the load balancer once and reuse that client.
	zone := svc.Annotations[AnnZone]
	ip := svc.Annotations[AnnIP]
	vrackNetworkID, err := parseVrackNetworkID(&svc)
	if err != nil {
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "InvalidConfiguration", eventActionReconcile, "%s", err.Error())
		return ctrl.Result{}, nil
	}

	needInfo := zone == "" || ip == ""
	needVrack := vrackNetworkID == nil && svc.Annotations[AnnVrackNetworkID] == ""
	if needInfo || needVrack {
		s, err := serviceFor(ctx, r.Factory, ref, svc.Namespace)
		if err != nil {
			return ctrl.Result{}, err
		}
		if needInfo {
			info, err := s.GetInfo()
			if err != nil {
				r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "LookupFailed", eventActionReconcile,
					"Could not read load balancer %q: %v", ref.ServiceName, err)
				return ctrl.Result{RequeueAfter: requeuePending}, nil
			}
			if zone == "" {
				switch len(info.Zone) {
				case 1:
					zone = info.Zone[0]
				case 0:
					// No zone advertised; let the API decide.
				default:
					r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "ZoneRequired", eventActionReconcile,
						"Load balancer %q has multiple zones (%s); set the %s annotation to pick one",
						ref.ServiceName, strings.Join(info.Zone, ", "), AnnZone)
					return ctrl.Result{RequeueAfter: requeuePending}, nil
				}
			}
			if ip == "" {
				ip = info.IP()
			}
		}
		if needVrack {
			ids, err := s.ListVrackNetworks()
			if err != nil {
				r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "LookupFailed", eventActionReconcile,
					"Could not list vRack networks of %q: %v", ref.ServiceName, err)
				return ctrl.Result{RequeueAfter: requeuePending}, nil
			}
			switch len(ids) {
			case 1:
				vrackNetworkID = &ids[0]
			case 0:
				// Not a vRack service (or none configured yet); leave unset. If the
				// LB is in a vRack, the farm create surfaces a clear error.
			default:
				r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "VrackNetworkRequired", eventActionReconcile,
					"Load balancer %q has multiple vRack networks; set the %s annotation to pick one",
					ref.ServiceName, AnnVrackNetworkID)
				return ctrl.Result{RequeueAfter: requeuePending}, nil
			}
		}
	}

	// Session persistence, the health-check probe and per-server settings apply
	// to every farm.
	stickiness := stickinessFor(&svc)
	probe, err := buildHealthCheck(&svc)
	if err != nil {
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "InvalidConfiguration", eventActionReconcile, "%s", err.Error())
		return ctrl.Result{}, nil
	}
	serverCfg, err := buildServerConfig(&svc, probe != nil)
	if err != nil {
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "InvalidConfiguration", eventActionReconcile, "%s", err.Error())
		return ctrl.Result{}, nil
	}

	// OVHcloud frontends accept an allow-list or a deny-list, not both.
	if len(svc.Spec.LoadBalancerSourceRanges) > 0 && svc.Annotations[AnnDeniedSource] != "" {
		r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "InvalidConfiguration", eventActionReconcile,
			"loadBalancerSourceRanges (allow-list) and the %s annotation (deny-list) are mutually exclusive; set only one",
			AnnDeniedSource)
		return ctrl.Result{}, nil
	}

	// TLS certificate served on http frontends: an explicit Certificate CR, or a
	// free Let's Encrypt certificate the operator requests for the given host(s).
	certRef := svc.Annotations[AnnTLSCertificateRef]
	if free, _ := parseBoolAnnotation(&svc, AnnTLSFreeCertificate); free != nil && *free {
		hosts := splitHosts(svc.Annotations[AnnTLSHost])
		if len(hosts) == 0 {
			r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "InvalidConfiguration", eventActionReconcile,
				"%s requires %s to be set with at least one FQDN", AnnTLSFreeCertificate, AnnTLSHost)
			return ctrl.Result{}, nil
		}
		certRef = freeCertName(&svc)
		if err := r.reconcileFreeCertificate(ctx, &svc, ref, hosts); err != nil {
			return ctrl.Result{}, err
		}
	} else if err := r.reconcileFreeCertificate(ctx, &svc, ref, nil); err != nil {
		return ctrl.Result{}, err
	}

	keep := make([]string, 0, len(svc.Spec.Ports))
	for _, p := range svc.Spec.Ports {
		if p.Protocol != corev1.ProtocolTCP && p.Protocol != corev1.ProtocolUDP {
			r.Recorder.Eventf(&svc, nil, corev1.EventTypeWarning, "UnsupportedProtocol", eventActionReconcile,
				"Skipping port %d: only TCP and UDP are supported by the IPLB operator", p.Port)
			continue
		}
		if p.NodePort == 0 {
			// nodePort not allocated yet; retry shortly.
			return ctrl.Result{RequeueAfter: requeuePending}, nil
		}
		proto := protocolForPort(&svc, p)
		name := resourceName(&svc, p)
		keep = append(keep, name)

		if err := r.ensureFarm(ctx, &svc, ref, proto, balance, zone, name, p, addrs, vrackNetworkID, stickiness, probe, serverCfg); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring farm %q: %w", name, err)
		}
		if err := r.ensureFrontend(ctx, &svc, ref, proto, zone, name, p, certRef); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring frontend %q: %w", name, err)
		}
	}

	if err := r.prune(ctx, &svc, keep); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.setIngressIP(ctx, &svc, ip); err != nil {
		log.Error(err, "Failed to update Service load-balancer status")
		return ctrl.Result{RequeueAfter: requeuePending}, nil
	}

	return ctrl.Result{}, nil
}

// ensureFarm creates or updates the Farm CR backing one Service port. Servers
// are the node addresses, each reached on the Service's nodePort.
func (r *ServiceReconciler) ensureFarm(ctx context.Context, svc *corev1.Service, ref iplbv1alpha1.ServiceRef,
	proto, balance, zone, name string, port corev1.ServicePort, addrs []string, vrackNetworkID *int64,
	stickiness string, probe *iplbv1alpha1.Probe, serverCfg serverConfig) error {
	log := logf.FromContext(ctx)
	farm := &iplbv1alpha1.Farm{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}
	nodePort := int(port.NodePort)

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, farm, func() error {
		setManagedLabels(farm, svc)
		farm.Spec.ServiceRef = ref
		farm.Spec.Protocol = proto
		farm.Spec.DisplayName = name
		farm.Spec.Balance = balance
		farm.Spec.Zone = zone
		farm.Spec.VrackNetworkID = vrackNetworkID
		farm.Spec.Stickiness = stickiness
		farm.Spec.Probe = probe.DeepCopy()
		// UDP farms require an explicit port at creation (TCP/HTTP infer it from
		// the servers); use the backend nodePort.
		if proto == protocolUDP {
			np := nodePort
			farm.Spec.Port = &np
		} else {
			farm.Spec.Port = nil
		}

		servers := make([]iplbv1alpha1.FarmServer, 0, len(addrs))
		for _, a := range addrs {
			p := nodePort
			servers = append(servers, iplbv1alpha1.FarmServer{
				Address: a,
				Port:    &p,
				SSL:     serverCfg.SSL,
				Backup:  serverCfg.Backup,
				Weight:  serverCfg.Weight,
				Probe:   serverCfg.Probe,
			})
		}
		farm.Spec.Servers = servers
		return controllerutil.SetControllerReference(svc, farm, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		log.Info("Reconciled Farm for Service", "farm", name, "operation", op)
	}
	return nil
}

// ensureFrontend creates or updates the Frontend CR backing one Service port.
// It listens on the Service port and targets the matching Farm CR.
func (r *ServiceReconciler) ensureFrontend(ctx context.Context, svc *corev1.Service, ref iplbv1alpha1.ServiceRef,
	proto, zone, name string, port corev1.ServicePort, certRef string) error {
	log := logf.FromContext(ctx)
	fe := &iplbv1alpha1.Frontend{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}

	op, err := controllerutil.CreateOrUpdate(ctx, r.Client, fe, func() error {
		setManagedLabels(fe, svc)
		fe.Spec.ServiceRef = ref
		fe.Spec.Protocol = proto
		fe.Spec.DisplayName = name
		fe.Spec.Port = strconv.Itoa(int(port.Port))
		fe.Spec.Zone = zone
		fe.Spec.DefaultFarmRef = name
		if len(svc.Spec.LoadBalancerSourceRanges) > 0 {
			fe.Spec.AllowedSource = append([]string(nil), svc.Spec.LoadBalancerSourceRanges...)
		} else {
			fe.Spec.AllowedSource = nil
		}
		fe.Spec.DeniedSource = splitHosts(svc.Annotations[AnnDeniedSource])
		// TLS: serve a Certificate CR as the default certificate on http frontends.
		if proto == protocolHTTP {
			fe.Spec.DefaultCertificateRef = certRef
		}
		return controllerutil.SetControllerReference(svc, fe, r.Scheme)
	})
	if err != nil {
		return err
	}
	if op != controllerutil.OperationResultNone {
		log.Info("Reconciled Frontend for Service", "frontend", name, "operation", op)
	}
	return nil
}

// freeCertName is the deterministic name of the free-certificate Certificate CR
// the operator manages for a Service.
func freeCertName(svc *corev1.Service) string {
	return resourceNameWithSuffix(svc, "cert")
}

// splitHosts parses the comma-separated tls-host annotation into FQDNs.
func splitHosts(v string) []string {
	var hosts []string
	for _, h := range strings.Split(v, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts
}

// reconcileFreeCertificate ensures the owned free (Let's Encrypt) Certificate CR
// exists for the given FQDNs, or deletes it when hosts is nil (free TLS turned
// off or the Service is no longer managed).
func (r *ServiceReconciler) reconcileFreeCertificate(ctx context.Context, svc *corev1.Service, ref iplbv1alpha1.ServiceRef, hosts []string) error {
	name := freeCertName(svc)
	if len(hosts) == 0 {
		cert := &iplbv1alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}
		if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: svc.Namespace}, cert); err != nil {
			return client.IgnoreNotFound(err)
		}
		if !metav1.IsControlledBy(cert, svc) {
			return nil
		}
		if err := r.Delete(ctx, cert); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}

	cert := &iplbv1alpha1.Certificate{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: svc.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, cert, func() error {
		setManagedLabels(cert, svc)
		cert.Spec.ServiceRef = ref
		cert.Spec.DisplayName = name
		cert.Spec.TLSSecretRef = nil
		cert.Spec.FreeCertificate = &iplbv1alpha1.FreeCertificateSpec{FQDN: hosts}
		return controllerutil.SetControllerReference(svc, cert, r.Scheme)
	})
	return err
}

// prune deletes Farm/Frontend resources owned by this Service whose names are
// not in keep (nil keep deletes them all).
func (r *ServiceReconciler) prune(ctx context.Context, svc *corev1.Service, keep []string) error {
	log := logf.FromContext(ctx)
	wanted := make(map[string]bool, len(keep))
	for _, n := range keep {
		wanted[n] = true
	}
	sel := client.MatchingLabels{labelService: svc.Name}

	var farms iplbv1alpha1.FarmList
	if err := r.List(ctx, &farms, client.InNamespace(svc.Namespace), sel); err != nil {
		return err
	}
	for i := range farms.Items {
		f := &farms.Items[i]
		if wanted[f.Name] || !metav1.IsControlledBy(f, svc) {
			continue
		}
		if err := r.Delete(ctx, f); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		log.Info("Deleted orphaned Farm", "farm", f.Name)
	}

	var fes iplbv1alpha1.FrontendList
	if err := r.List(ctx, &fes, client.InNamespace(svc.Namespace), sel); err != nil {
		return err
	}
	for i := range fes.Items {
		fe := &fes.Items[i]
		if wanted[fe.Name] || !metav1.IsControlledBy(fe, svc) {
			continue
		}
		if err := r.Delete(ctx, fe); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		log.Info("Deleted orphaned Frontend", "frontend", fe.Name)
	}
	return nil
}

// setIngressIP writes the IPLB's public IP into the Service's
// status.loadBalancer.ingress so type=LoadBalancer reports an address.
func (r *ServiceReconciler) setIngressIP(ctx context.Context, svc *corev1.Service, ip string) error {
	if ip == "" {
		return nil // nothing assigned yet
	}
	ingress := []corev1.LoadBalancerIngress{{IP: ip}}
	if loadBalancerIngressEqual(svc.Status.LoadBalancer.Ingress, ingress) {
		return nil
	}
	svc.Status.LoadBalancer.Ingress = ingress
	return r.Status().Update(ctx, svc)
}

// nodeAddresses returns the chosen address (InternalIP by default) of every
// schedulable, Ready node, sorted for stable comparison.
func (r *ServiceReconciler) nodeAddresses(ctx context.Context, svc *corev1.Service) ([]string, error) {
	addrType := corev1.NodeInternalIP
	if annotationOr(svc, AnnNodeAddressType, "") == string(corev1.NodeExternalIP) {
		addrType = corev1.NodeExternalIP
	}

	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, err
	}
	var addrs []string
	for i := range nodes.Items {
		n := &nodes.Items[i]
		if n.Spec.Unschedulable || !nodeReady(n) {
			continue
		}
		for _, a := range n.Status.Addresses {
			if a.Type == addrType && a.Address != "" {
				addrs = append(addrs, a.Address)
			}
		}
	}
	sort.Strings(addrs)
	return addrs, nil
}

// servicesForNode enqueues every managed Service when a Node changes, so farm
// membership tracks the node set.
func (r *ServiceReconciler) servicesForNode(ctx context.Context, _ client.Object) []ctrl.Request {
	var services corev1.ServiceList
	if err := r.List(ctx, &services); err != nil {
		return nil
	}
	var reqs []ctrl.Request
	for i := range services.Items {
		if manages(&services.Items[i]) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: services.Items[i].Namespace,
				Name:      services.Items[i].Name,
			}})
		}
	}
	return reqs
}

// ---- helpers ----

// manages reports whether a Service is a LoadBalancer opted in to this operator.
func manages(svc *corev1.Service) bool {
	return svc.Spec.Type == corev1.ServiceTypeLoadBalancer &&
		svc.Spec.LoadBalancerClass != nil &&
		*svc.Spec.LoadBalancerClass == LoadBalancerClass
}

// serviceRefFromAnnotations builds a ServiceRef from the Service annotations,
// erroring if a required annotation is missing.
func serviceRefFromAnnotations(svc *corev1.Service) (iplbv1alpha1.ServiceRef, error) {
	name := svc.Annotations[AnnServiceName]
	if name == "" {
		return iplbv1alpha1.ServiceRef{}, fmt.Errorf("missing required annotation %q", AnnServiceName)
	}
	secret := svc.Annotations[AnnCredentialsSecret]
	if secret == "" {
		return iplbv1alpha1.ServiceRef{}, fmt.Errorf("missing required annotation %q", AnnCredentialsSecret)
	}
	return iplbv1alpha1.ServiceRef{
		ServiceName: name,
		Endpoint:    svc.Annotations[AnnEndpoint],
		CredentialsSecretRef: iplbv1alpha1.SecretReference{
			Name:      secret,
			Namespace: svc.Annotations[AnnCredentialsNamespace],
		},
	}, nil
}

// parseVrackNetworkID reads the vRack network id annotation, if present.
// Returns (nil, nil) when unset so the caller can auto-discover it.
func parseVrackNetworkID(svc *corev1.Service) (*int64, error) {
	v := svc.Annotations[AnnVrackNetworkID]
	if v == "" {
		return nil, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("annotation %s must be an integer: %w", AnnVrackNetworkID, err)
	}
	return &n, nil
}

// stickinessFor resolves session persistence: the annotation wins, otherwise it
// is derived from the Service's sessionAffinity (ClientIP -> sourceIp).
func stickinessFor(svc *corev1.Service) string {
	if v := svc.Annotations[AnnStickiness]; v != "" {
		return v
	}
	if svc.Spec.SessionAffinity == corev1.ServiceAffinityClientIP {
		return "sourceIp"
	}
	return ""
}

// buildHealthCheck builds the farm probe from the health-check annotations.
// Returns (nil, nil) when none are set so the farm keeps no probe. The probe
// port is left unset when not specified, so the API defaults it to the backend
// (node) port.
func buildHealthCheck(svc *corev1.Service) (*iplbv1alpha1.Probe, error) {
	typ := svc.Annotations[AnnHealthCheckProtocol]
	portStr := svc.Annotations[AnnHealthCheckPort]
	intervalStr := svc.Annotations[AnnHealthCheckInterval]
	url := svc.Annotations[AnnHealthCheckURL]
	method := svc.Annotations[AnnHealthCheckMethod]

	if typ == "" && portStr == "" && intervalStr == "" && url == "" && method == "" {
		return nil, nil
	}
	if typ == "" {
		typ = "tcp"
	}
	probe := &iplbv1alpha1.Probe{Type: typ, URL: url, Method: method}
	if portStr != "" {
		n, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, fmt.Errorf("annotation %s must be an integer: %w", AnnHealthCheckPort, err)
		}
		probe.Port = &n
	}
	if intervalStr != "" {
		n, err := strconv.Atoi(intervalStr)
		if err != nil {
			return nil, fmt.Errorf("annotation %s must be an integer: %w", AnnHealthCheckInterval, err)
		}
		probe.Interval = &n
	}
	return probe, nil
}

// serverConfig holds the backend (server) settings applied uniformly to every
// node server of a farm.
type serverConfig struct {
	SSL    *bool
	Backup *bool
	Weight *int
	Probe  *bool
}

// buildServerConfig parses the per-server annotations. probeConfigured enables
// the farm probe on servers by default when a health-check is set (unless the
// annotation says otherwise).
func buildServerConfig(svc *corev1.Service, probeConfigured bool) (serverConfig, error) {
	var cfg serverConfig
	var err error

	if cfg.SSL, err = parseBoolAnnotation(svc, AnnServerSSL); err != nil {
		return cfg, err
	}
	if cfg.Backup, err = parseBoolAnnotation(svc, AnnServerBackup); err != nil {
		return cfg, err
	}
	if cfg.Probe, err = parseBoolAnnotation(svc, AnnServerHealthCheck); err != nil {
		return cfg, err
	}
	if v := svc.Annotations[AnnServerWeight]; v != "" {
		n, convErr := strconv.Atoi(v)
		if convErr != nil {
			return cfg, fmt.Errorf("annotation %s must be an integer: %w", AnnServerWeight, convErr)
		}
		cfg.Weight = &n
	}
	// The server probe is always set explicitly (never left nil) so it can be
	// turned off: enabling a probe via drift but never disabling it would mean a
	// health-check, once added, could never be removed. It defaults to enabled
	// when a health-check is configured and disabled otherwise.
	if cfg.Probe == nil {
		enabled := probeConfigured
		cfg.Probe = &enabled
	}
	return cfg, nil
}

// parseBoolAnnotation reads a boolean annotation, returning nil when unset.
func parseBoolAnnotation(svc *corev1.Service, key string) (*bool, error) {
	v := svc.Annotations[key]
	if v == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return nil, fmt.Errorf("annotation %s must be a boolean: %w", key, err)
	}
	return &b, nil
}

// protocolForPort maps a Service port to an IPLB farm/frontend protocol. The
// per-Service annotation wins; otherwise an http/https appProtocol yields
// "http" and everything else "tcp".
func protocolForPort(svc *corev1.Service, p corev1.ServicePort) string {
	if v := svc.Annotations[AnnProtocol]; v != "" {
		return v
	}
	if p.Protocol == corev1.ProtocolUDP {
		return protocolUDP
	}
	if p.AppProtocol != nil && (*p.AppProtocol == "http" || *p.AppProtocol == "https") {
		return protocolHTTP
	}
	return protocolTCP
}

func setManagedLabels(obj metav1.Object, svc *corev1.Service) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelManagedBy] = managedByValue
	labels[labelService] = svc.Name
	obj.SetLabels(labels)
}

func annotationOr(svc *corev1.Service, key, def string) string {
	if v := svc.Annotations[key]; v != "" {
		return v
	}
	return def
}

func nodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// resourceName derives a deterministic, DNS-1123-safe name for the Farm and
// Frontend backing a Service port: "<service>-<port>", hashed if too long.
func resourceName(svc *corev1.Service, p corev1.ServicePort) string {
	return resourceNameWithSuffix(svc, strconv.Itoa(int(p.Port)))
}

// resourceNameWithSuffix builds "<service>-<suffix>", hashed to stay within the
// 63-character DNS-1123 limit.
func resourceNameWithSuffix(svc *corev1.Service, suffix string) string {
	name := svc.Name + "-" + suffix
	if len(name) <= 63 {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	tail := "-" + hex.EncodeToString(sum[:])[:8]
	return svc.Name[:63-len(tail)] + tail
}

func loadBalancerIngressEqual(a, b []corev1.LoadBalancerIngress) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].IP != b[i].IP || a[i].Hostname != b[i].Hostname {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}, builder.WithPredicates(serviceManagedPredicate())).
		Owns(&iplbv1alpha1.Farm{}).
		Owns(&iplbv1alpha1.Frontend{}).
		Watches(&corev1.Node{}, handler.EnqueueRequestsFromMapFunc(r.servicesForNode)).
		Named("service").
		Complete(r)
}

// serviceManagedPredicate limits reconciliation to LoadBalancer Services,
// keeping events for either side of a type/class transition so we can clean up.
func serviceManagedPredicate() predicate.Predicate {
	isLB := func(o client.Object) bool {
		svc, ok := o.(*corev1.Service)
		return ok && svc.Spec.Type == corev1.ServiceTypeLoadBalancer
	}
	return predicate.Funcs{
		CreateFunc:  func(e event.CreateEvent) bool { return isLB(e.Object) },
		DeleteFunc:  func(e event.DeleteEvent) bool { return isLB(e.Object) },
		UpdateFunc:  func(e event.UpdateEvent) bool { return isLB(e.ObjectOld) || isLB(e.ObjectNew) },
		GenericFunc: func(e event.GenericEvent) bool { return isLB(e.Object) },
	}
}
