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

// CertificateSpec defines the desired state of Certificate, an SSL certificate
// uploaded to an OVHcloud IP Load Balancer.
type CertificateSpec struct {
	// serviceRef selects the IPLB service and credentials.
	// +required
	ServiceRef ServiceRef `json:"serviceRef"`

	// displayName is the human-readable name of the certificate. Used to adopt
	// a pre-existing certificate with the same name. Defaults to the resource
	// name when empty.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// tlsSecretRef references a Kubernetes TLS Secret (type kubernetes.io/tls)
	// containing the keys "tls.crt" (certificate, optionally with chain) and
	// "tls.key" (private key). Mutually exclusive with freeCertificate; exactly
	// one of the two must be set.
	// +optional
	TLSSecretRef *SecretReference `json:"tlsSecretRef,omitempty"`

	// freeCertificate requests a free, auto-renewed Let's Encrypt certificate
	// from OVHcloud for the given FQDN(s) instead of uploading material from a
	// Secret. Mutually exclusive with tlsSecretRef. The FQDNs must resolve to the
	// load balancer so OVHcloud can complete ACME validation.
	// +optional
	FreeCertificate *FreeCertificateSpec `json:"freeCertificate,omitempty"`
}

// FreeCertificateSpec requests a managed Let's Encrypt certificate.
type FreeCertificateSpec struct {
	// fqdn is the list of fully-qualified domain names the certificate covers.
	// The first entry is the certificate subject; any others become SANs.
	// +kubebuilder:validation:MinItems=1
	// +required
	FQDN []string `json:"fqdn"`
}

// CertificateStatus defines the observed state of Certificate.
type CertificateStatus struct {
	// INSERT ADDITIONAL STATUS FIELD - define observed state of cluster
	// Important: Run "make" to regenerate code after modifying this file

	// For Kubernetes API conventions, see:
	// https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#typical-status-properties

	// conditions represent the current state of the Certificate resource.
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

	// certificateId is the numeric identifier assigned by the OVHcloud API.
	// +optional
	CertificateID *int64 `json:"certificateId,omitempty"`

	// fingerprint of the certificate material last pushed, used to detect
	// rotation of the referenced TLS Secret.
	// +optional
	Fingerprint string `json:"fingerprint,omitempty"`

	// freeCertificateRequested records that a Let's Encrypt issuance has been
	// requested for this resource, so the operator does not request it again
	// while waiting for the asynchronous issuance to complete.
	// +optional
	FreeCertificateRequested bool `json:"freeCertificateRequested,omitempty"`

	// observedGeneration is the .metadata.generation last reconciled.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Service",type=string,JSONPath=`.spec.serviceRef.serviceName`
// +kubebuilder:printcolumn:name="CertID",type=integer,JSONPath=`.status.certificateId`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`

// Certificate is the Schema for the certificates API
type Certificate struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Certificate
	// +required
	Spec CertificateSpec `json:"spec"`

	// status defines the observed state of Certificate
	// +optional
	Status CertificateStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// CertificateList contains a list of Certificate
type CertificateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Certificate `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Certificate{}, &CertificateList{})
		return nil
	})
}
