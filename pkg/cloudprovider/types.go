/*
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

package cloudprovider

import (
	"context"
	"errors"
	"fmt"

	"github.com/samber/lo"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/clock"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/aws/karpenter-core/pkg/apis/v1alpha5"
	"github.com/aws/karpenter-core/pkg/events"
	"github.com/aws/karpenter-core/pkg/scheduling"
	"github.com/aws/karpenter-core/pkg/utils/resources"
)

// Context is injected into CloudProvider's factories
type Context struct {
	context.Context

	RESTConfig          *rest.Config
	KubernetesInterface kubernetes.Interface
	KubeClient          client.Client
	EventRecorder       events.Recorder
	Clock               clock.Clock
	// StartAsync is a channel that is closed when leader election has been won.  This is a signal to start any  async
	// processing that should only occur while the cloud provider is the leader.
	StartAsync <-chan struct{}
}

// CloudProvider interface is implemented by cloud providers to support provisioning.
type CloudProvider interface {
	// Create launches a machine with the given resource requests and requirements and returns a hydrated
	// machine back with resolved machine labels for the launched machine
	Create(context.Context, *v1alpha5.Machine) (*v1alpha5.Machine, error)
	// Delete removes a machine from the cloudprovider by its machine name
	Delete(context.Context, *v1alpha5.Machine) error
	// Get retrieves a machine from the cloudprovider by its machine name
	Get(context.Context, string, string) (*v1alpha5.Machine, error)
	// GetInstanceTypes returns instance types supported by the cloudprovider.
	// Availability of types or zone may vary by provisioner or over time.  Regardless of
	// availability, the GetInstanceTypes method should always return all instance types,
	// even those with no offerings available.
	GetInstanceTypes(context.Context, *v1alpha5.Provisioner) ([]*InstanceType, error)
	// IsMachineDrifted returns whether a machine has drifted from the provisioning requirements
	// it is tied to.
	IsMachineDrifted(context.Context, *v1alpha5.Machine) (bool, error)
	// Name returns the CloudProvider implementation name.
	Name() string
}

// InstanceType describes the properties of a potential node (either concrete attributes of an instance of this type
// or supported options in the case of arrays)
type InstanceType struct {
	// Name of the instance type, must correspond to v1.LabelInstanceTypeStable
	Name string
	// Requirements returns a flexible set of properties that may be selected
	// for scheduling. Must be defined for every well known label, even if empty.
	Requirements scheduling.Requirements
	// Note that though this is an array it is expected that all the Offerings are unique from one another
	Offerings Offerings
	// Resources are the full resource capacities for this instance type
	Capacity v1.ResourceList
	// Overhead is the amount of resource overhead expected to be used by kubelet and any other system daemons outside
	// of Kubernetes.
	Overhead *InstanceTypeOverhead
}

func (i *InstanceType) Allocatable() v1.ResourceList {
	return resources.Subtract(i.Capacity, i.Overhead.Total())
}

type InstanceTypeOverhead struct {
	// KubeReserved returns the default resources allocated to kubernetes system daemons by default
	KubeReserved v1.ResourceList
	// SystemReserved returns the default resources allocated to the OS system daemons by default
	SystemReserved v1.ResourceList
	// EvictionThreshold returns the resources used to maintain a hard eviction threshold
	EvictionThreshold v1.ResourceList
}

func (i InstanceTypeOverhead) Total() v1.ResourceList {
	return resources.Merge(i.KubeReserved, i.SystemReserved, i.EvictionThreshold)
}

// An Offering describes where an InstanceType is available to be used, with the expectation that its properties
// may be tightly coupled (e.g. the availability of an instance type in some zone is scoped to a capacity type)
type Offering struct {
	CapacityType string
	Zone         string
	Price        float64
	// Available is added so that Offerings can return all offerings that have ever existed for an instance type,
	// so we can get historical pricing data for calculating savings in consolidation
	Available bool
}

type Offerings []Offering

// Get gets the offering from an offering slice that matches the
// passed zone and capacity type
func (ofs Offerings) Get(ct, zone string) (Offering, bool) {
	return lo.Find(ofs, func(of Offering) bool {
		return of.CapacityType == ct && of.Zone == zone
	})
}

// Available filters the available offerings from the returned offerings
func (ofs Offerings) Available() Offerings {
	return lo.Filter(ofs, func(o Offering, _ int) bool {
		return o.Available
	})
}

// Requirements filters the offerings based on the passed requirements
func (ofs Offerings) Requirements(reqs scheduling.Requirements) Offerings {
	return lo.Filter(ofs, func(offering Offering, _ int) bool {
		return (!reqs.Has(v1.LabelTopologyZone) || reqs.Get(v1.LabelTopologyZone).Has(offering.Zone)) &&
			(!reqs.Has(v1alpha5.LabelCapacityType) || reqs.Get(v1alpha5.LabelCapacityType).Has(offering.CapacityType))
	})
}

// Cheapest returns the cheapest offering from the returned offerings
func (ofs Offerings) Cheapest() Offering {
	return lo.MinBy(ofs, func(a, b Offering) bool {
		return a.Price < b.Price
	})
}

// MachineNotFoundError is an error type returned by CloudProviders when the reason for failure is NotFound
type MachineNotFoundError struct {
	Err error
}

func NewMachineNotFoundError(err error) *MachineNotFoundError {
	return &MachineNotFoundError{
		Err: err,
	}
}

func (e *MachineNotFoundError) Error() string {
	return fmt.Sprintf("machine not found, %s", e.Err)
}

func IsMachineNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	var mnfErr *MachineNotFoundError
	return errors.As(err, &mnfErr)
}

func IgnoreMachineNotFoundError(err error) error {
	if IsMachineNotFoundError(err) {
		return nil
	}
	return err
}
