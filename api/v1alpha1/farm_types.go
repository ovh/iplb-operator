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

// FarmSpec defines the desired state of Farm, a server cluster (backend pool)
// on an OVHcloud IP Load Balancer.
type FarmSpec struct {
	// serviceRef selects the IPLB service and credentials.
	// +required
	ServiceRef ServiceRef `json:"serviceRef"`

	// protocol is the farm protocol. It must match the protocol of any
	// frontend that targets this farm. UDP farms do not support balance,
	// stickiness or a probe, nor per-server weight/ssl/backup/probe.
	// +kubebuilder:validation:Enum=http;tcp;udp
	// +required
	Protocol string `json:"protocol"`

	// displayName is the human-readable name of the farm. It is also used to
	// adopt a pre-existing farm with the same name. Defaults to the resource
	// name when empty.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// balance is the load balancing algorithm.
	// +kubebuilder:validation:Enum=first;leastconn;roundrobin;source;uri
	// +optional
	Balance string `json:"balance,omitempty"`

	// stickiness is the session stickiness mode (not valid for every protocol).
	// +kubebuilder:validation:Enum=sourceIp;cookie
	// +optional
	Stickiness string `json:"stickiness,omitempty"`

	// port is the default port backends are reached on when a server does not
	// specify its own port. 1-65535.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int `json:"port,omitempty"`

	// zone is the balancing zone the farm lives in (e.g. "gra", "all").
	// +optional
	Zone string `json:"zone,omitempty"`

	// vrackNetworkId attaches the farm to a vRack private network.
	// +optional
	VrackNetworkID *int64 `json:"vrackNetworkId,omitempty"`

	// probe configures the health check applied to the farm's servers.
	// +optional
	Probe *Probe `json:"probe,omitempty"`

	// servers is the declarative list of backend servers in this farm. The
	// operator creates, updates and deletes server members to match this list.
	// +listType=map
	// +listMapKey=address
	// +optional
	Servers []FarmServer `json:"servers,omitempty"`
}

// Probe defines a farm health check.
type Probe struct {
	// type is the probe protocol.
	// +kubebuilder:validation:Enum=http;https;tcp;mysql;pgsql;smtp;ssl;oco
	// +required
	Type string `json:"type"`

	// port is the port to probe. Defaults to the server port when omitted.
	// +optional
	Port *int `json:"port,omitempty"`

	// interval is the probe interval in seconds.
	// +optional
	Interval *int `json:"interval,omitempty"`

	// method is the HTTP method for http/https probes.
	// +optional
	Method string `json:"method,omitempty"`

	// url is the URL to request for http/https probes.
	// +optional
	URL string `json:"url,omitempty"`

	// match is the expected match condition.
	// +optional
	Match string `json:"match,omitempty"`

	// pattern is the value compared against using "match".
	// +optional
	Pattern string `json:"pattern,omitempty"`
}

// FarmServer is a single backend server member of a Farm.
type FarmServer struct {
	// address is the IPv4/IPv6 address of the backend. Used as the unique key
	// within a farm.
	// +required
	Address string `json:"address"`

	// port is the port the backend is reached on. Falls back to the farm port
	// when omitted.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +optional
	Port *int `json:"port,omitempty"`

	// displayName is the human-readable name of the server.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// weight is the relative weight of the backend (0-256).
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=256
	// +optional
	Weight *int `json:"weight,omitempty"`

	// status enables or disables the backend.
	// +kubebuilder:validation:Enum=active;inactive
	// +kubebuilder:default=active
	// +optional
	Status string `json:"status,omitempty"`

	// backup marks the server as a backup, only used when the active pool is down.
	// +optional
	Backup *bool `json:"backup,omitempty"`

	// ssl enables TLS towards the backend.
	// +optional
	SSL *bool `json:"ssl,omitempty"`

	// probe enables the farm probe for this server.
	// +optional
	Probe *bool `json:"probe,omitempty"`
}

// FarmStatus defines the observed state of Farm.
type FarmStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Farm resource.
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

	// farmId is the numeric identifier assigned by the OVHcloud API.
	// +optional
	FarmID *int64 `json:"farmId,omitempty"`

	// serverIDs maps each server address to its numeric id in the OVHcloud API.
	// +optional
	ServerIDs map[string]int64 `json:"serverIDs,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.serviceName`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.protocol`
// +kubebuilder:printcolumn:name="FarmID",type=integer,JSONPath=`.status.farmId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// Farm is the Schema for the farms API
type Farm struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Farm
	// +required
	Spec FarmSpec `json:"spec"`

	// status defines the observed state of Farm
	// +optional
	Status FarmStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// FarmList contains a list of Farm
type FarmList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Farm `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Farm{}, &FarmList{})
		return nil
	})
}
