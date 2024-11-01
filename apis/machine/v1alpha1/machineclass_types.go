/*
Copyright 2022 The Crossplane Authors.

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
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	xpv1 "github.com/crossplane/crossplane-runtime/apis/common/v1"
)

type MachineClassResources struct {
	CPU          uint   `json:"cpu"`
	Memory       uint   `json:"memory"`
	DiskSize     uint   `json:"diskSize"`
	Architecture string `json:"architecture"`
}

type MachineClassAutoprovision struct {
	ProviderID string                `json:"providerID"`
	KernelArgs []string              `json:"kernelArgs"`
	Resources  MachineClassResources `json:"resources"`
}

// MachineClassParameters are the configurable fields of a MachineClass.
type MachineClassParameters struct {
	Name          string                    `json:"name"`
	AutoProvision MachineClassAutoprovision `json:"autoprovision"`
}

// MachineClassObservation are the observable fields of a MachineClass.
type MachineClassObservation struct {
	Version uint `json:"version,omitempty"`
}

// A MachineClassSpec defines the desired state of a MachineClass.
type MachineClassSpec struct {
	xpv1.ResourceSpec `json:",inline"`
	ForProvider       MachineClassParameters `json:"forProvider"`
}

// A MachineClassStatus represents the observed state of a MachineClass.
type MachineClassStatus struct {
	xpv1.ResourceStatus `json:",inline"`
	AtProvider          MachineClassObservation `json:"atProvider,omitempty"`
}

// +kubebuilder:object:root=true

// A MachineClass is an example API type.
// +kubebuilder:printcolumn:name="READY",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="SYNCED",type="string",JSONPath=".status.conditions[?(@.type=='Synced')].status"
// +kubebuilder:printcolumn:name="EXTERNAL-NAME",type="string",JSONPath=".metadata.annotations.crossplane\\.io/external-name"
// +kubebuilder:printcolumn:name="AGE",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,categories={crossplane,managed,omni}
type MachineClass struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MachineClassSpec   `json:"spec"`
	Status MachineClassStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// MachineClassList contains a list of MachineClass
type MachineClassList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []MachineClass `json:"items"`
}

// MachineClass type metadata.
var (
	MachineClassKind             = reflect.TypeOf(MachineClass{}).Name()
	MachineClassGroupKind        = schema.GroupKind{Group: Group, Kind: MachineClassKind}.String()
	MachineClassKindAPIVersion   = MachineClassKind + "." + SchemeGroupVersion.String()
	MachineClassGroupVersionKind = SchemeGroupVersion.WithKind(MachineClassKind)
)

func init() {
	SchemeBuilder.Register(&MachineClass{}, &MachineClassList{})
}
