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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// FrontendSpec defines the desired state of Frontend, a listener on an
// OVHcloud IP Load Balancer.
type FrontendSpec struct {
	// serviceRef selects the IPLB service and credentials.
	// +required
	ServiceRef ServiceRef `json:"serviceRef"`

	// protocol is the frontend protocol. It must match the protocol of the
	// default farm it targets. TLS and HTTP routes are only valid for http
	// frontends; UDP frontends support neither TLS nor allowedSource.
	// +kubebuilder:validation:Enum=http;tcp;udp
	// +required
	Protocol string `json:"protocol"`

	// displayName is the human-readable name of the frontend. Used to adopt a
	// pre-existing frontend with the same name. Defaults to the resource name.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// port is the listening port specification, e.g. "80", "80,443" or "8080-8090".
	// +required
	Port string `json:"port"`

	// zone is the balancing zone the frontend lives in (e.g. "gra", "all").
	// +optional
	Zone string `json:"zone,omitempty"`

	// defaultFarmRef references a Farm custom resource in the same namespace
	// whose status.farmId is used as this frontend's default farm. Mutually
	// exclusive with defaultFarmId.
	// +optional
	DefaultFarmRef string `json:"defaultFarmRef,omitempty"`

	// defaultFarmId sets the default farm by its raw OVHcloud numeric id.
	// Mutually exclusive with defaultFarmRef.
	// +optional
	DefaultFarmID *int64 `json:"defaultFarmId,omitempty"`

	// allowedSource restricts inbound traffic to the given CIDRs (allow-list).
	// +optional
	AllowedSource []string `json:"allowedSource,omitempty"`

	// deniedSource blocks inbound traffic from the given CIDRs (deny-list). Not
	// valid for udp frontends.
	// +optional
	DeniedSource []string `json:"deniedSource,omitempty"`

	// ssl enables TLS termination (SSL deciphering) on this frontend. Implied
	// when a default certificate is set. Only valid for http frontends.
	// +optional
	SSL *bool `json:"ssl,omitempty"`

	// defaultCertificateRef references a Certificate custom resource in the same
	// namespace whose uploaded SSL certificate is served as the frontend's
	// default. Mutually exclusive with defaultSslId. Setting either enables ssl.
	// +optional
	DefaultCertificateRef string `json:"defaultCertificateRef,omitempty"`

	// defaultSslId sets the default certificate by its raw OVHcloud SSL id.
	// Mutually exclusive with defaultCertificateRef.
	// +optional
	DefaultSslID *int64 `json:"defaultSslId,omitempty"`

	// forceSni blocks SSL connections that do not send an SNI. Only valid for
	// http frontends.
	// +optional
	ForceSni *bool `json:"forceSni,omitempty"`

	// hsts enables HTTP Strict Transport Security. Only valid for http frontends.
	// +optional
	HSTS *bool `json:"hsts,omitempty"`

	// redirectLocation is an HTTP redirection target (e.g. "https://www.ovh.com").
	// Only valid for http frontends.
	// +optional
	RedirectLocation string `json:"redirectLocation,omitempty"`

	// routes are HTTP routes attached to this frontend, each running an action
	// when its rules match. Only valid for http frontends.
	// +listType=map
	// +listMapKey=name
	// +optional
	Routes []Route `json:"routes,omitempty"`

	// disabled disables the frontend without deleting it.
	// +optional
	Disabled *bool `json:"disabled,omitempty"`
}

// Route is an HTTP route attached to a Frontend: an action triggered when all
// of its rules match. Only valid for http frontends.
type Route struct {
	// name identifies the route within the frontend (used as its display name).
	// +required
	Name string `json:"name"`

	// action is triggered when every rule matches.
	// +required
	Action RouteAction `json:"action"`

	// rules are ANDed together to trigger the action. An empty list matches all.
	// +optional
	Rules []RouteRule `json:"rules,omitempty"`

	// weight is the route priority (0-255, highest wins).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=255
	// +optional
	Weight *int `json:"weight,omitempty"`
}

// RouteAction is what a Route does when its rules match.
type RouteAction struct {
	// type is the action to trigger.
	// +kubebuilder:validation:Enum=farm;redirect;reject
	// +required
	Type string `json:"type"`

	// target is the raw farm id (for "farm") or a URL template (for "redirect").
	// For a "farm" action prefer targetFarmRef.
	// +optional
	Target string `json:"target,omitempty"`

	// targetFarmRef references a Farm custom resource in the same namespace whose
	// farmId is the target of a "farm" action. Mutually exclusive with target.
	// +optional
	TargetFarmRef string `json:"targetFarmRef,omitempty"`

	// status is the HTTP status code for "redirect" and "reject" actions.
	// +optional
	Status *int `json:"status,omitempty"`
}

// RouteRule is a single match condition of a Route.
type RouteRule struct {
	// field is the field to match, e.g. "host", "path" or "header".
	// +required
	Field string `json:"field"`

	// match is the matching operator.
	// +kubebuilder:validation:Enum=contains;endswith;exists;in;internal;is;matches;startswith
	// +required
	Match string `json:"match"`

	// pattern is the value matched against; its interpretation depends on match.
	// +optional
	Pattern string `json:"pattern,omitempty"`

	// subField is the sub-field when applicable, e.g. a header or cookie name.
	// +optional
	SubField string `json:"subField,omitempty"`

	// negate inverts the matching operator.
	// +optional
	Negate *bool `json:"negate,omitempty"`
}

// FrontendStatus defines the observed state of Frontend.
type FrontendStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Frontend resource.
	// Each condition has a unique type and reflects the status of a specific aspect of the resource.
	//
	// Standard condition types include:
	// - "Available": the resource is fully functional
	// - "Progressing": the resource is being created or updated
	// - "Degraded": the resource failed to reach or maintain its desired state
	//
	// The status of each condition is one of True, False, or Unknown.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// frontendId is the numeric identifier assigned by the OVHcloud API.
	// +optional
	FrontendID *int64 `json:"frontendId,omitempty"`

	// resolvedFarmId is the default farm id resolved from spec (ref or raw).
	// +optional
	ResolvedFarmID *int64 `json:"resolvedFarmId,omitempty"`

	// resolvedSslId is the default SSL id resolved from spec (cert ref or raw).
	// +optional
	ResolvedSslID *int64 `json:"resolvedSslId,omitempty"`

	// routeIDs maps each route name to its numeric id in the OVHcloud API.
	// +optional
	RouteIDs map[string]int64 `json:"routeIDs,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.serviceName`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="Port",type=string,JSONPath=`.spec.port`
// +kubebuilder:printcolumn:name="FrontendID",type=integer,JSONPath=`.status.frontendId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// Frontend is the Schema for the frontends API
type Frontend struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Frontend
	// +required
	Spec FrontendSpec `json:"spec"`

	// status defines the observed state of Frontend
	// +optional
	Status FrontendStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FrontendList contains a list of Frontend
type FrontendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Frontend `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Frontend{}, &FrontendList{})
		return nil
	})
}
