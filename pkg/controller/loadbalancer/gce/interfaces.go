/*
Copyright 2014 The Kubernetes Authors All rights reserved.

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

package lb

import (
	compute "google.golang.org/api/compute/v1"
)

// NodePool is an interface to manage a pool of kubernetes nodes synced with vm instances in the cloud
// through the InstanceGroups interface.
type NodePool interface {
	Add(nodeNames []string) error
	Remove(nodeNames []string) error
	Sync(nodeNames []string) error
	// TODO: Get leaks the cloud abstraction.
	Get(name string) (*compute.InstanceGroup, error)
}

// InstanceGroups is an abstract interface for managing gce instances groups, and the instances therein.
type InstanceGroups interface {
	GetInstanceGroup(name string) (*compute.InstanceGroup, error)
	CreateInstanceGroup(name string) (*compute.InstanceGroup, error)
	DeleteInstanceGroup(name string) error

	// It's slightly awkward that this interface violates the unix philosophy.
	ListInstancesInInstanceGroup(name string, state string) (*compute.InstanceGroupsListInstances, error)
	AddInstancesToInstanceGroup(name string, instanceNames []string) error
	RemoveInstancesFromInstanceGroup(name string, instanceName []string) error
	AddPortToInstanceGroup(ig *compute.InstanceGroup, port int64) (*compute.NamedPort, error)
}
