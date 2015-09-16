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
	"fmt"
	"testing"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/kubernetes/pkg/util/sets"
)

const (
	Add = iota
	Remove
	Sync
	Get
	Create
	Update
	Delete
	AddInstances
	RemoveInstances
	defaultIgName = "defaultIgName"
	project       = "project"
	zone          = "zone"
)

// getInstanceList returns an instance list based on the given names.
// The names cannot contain a '.', the real gce api validates against this.
func getInstanceList(nodeNames sets.String) *compute.InstanceGroupsListInstances {
	instanceNames := nodeNames.List()
	computeInstances := []*compute.InstanceWithNamedPorts{}
	for _, name := range instanceNames {
		instanceLink := fmt.Sprintf(
			"https://www.googleapis.com/compute/v1/projects/%s/zones/%s/instances/%s",
			project, zone, name)
		computeInstances = append(
			computeInstances, &compute.InstanceWithNamedPorts{
				Instance: instanceLink})
	}
	return &compute.InstanceGroupsListInstances{
		Items: computeInstances,
	}
}

type fakeInstanceGroups struct {
	instances     sets.String
	instanceGroup string
	ports         []int64
	getResult     *compute.InstanceGroup
	listResult    *compute.InstanceGroupsListInstances
	calls         []int
}

func (f *fakeInstanceGroups) GetInstanceGroup(name string) (*compute.InstanceGroup, error) {
	f.calls = append(f.calls, Get)
	return f.getResult, nil
}

func (f *fakeInstanceGroups) CreateInstanceGroup(name string) (*compute.InstanceGroup, error) {
	f.instanceGroup = name
	return &compute.InstanceGroup{}, nil
}

func (f *fakeInstanceGroups) DeleteInstanceGroup(name string) error {
	f.instanceGroup = ""
	return nil
}

func (f *fakeInstanceGroups) ListInstancesInInstanceGroup(name string, state string) (*compute.InstanceGroupsListInstances, error) {
	return f.listResult, nil
}

func (f *fakeInstanceGroups) AddInstancesToInstanceGroup(name string, instanceNames []string) error {
	f.calls = append(f.calls, AddInstances)
	f.instances.Insert(instanceNames...)
	return nil
}

func (f *fakeInstanceGroups) RemoveInstancesFromInstanceGroup(name string, instanceNames []string) error {
	f.calls = append(f.calls, RemoveInstances)
	f.instances.Delete(instanceNames...)
	return nil
}

func (f *fakeInstanceGroups) AddPortToInstanceGroup(ig *compute.InstanceGroup, port int64) (*compute.NamedPort, error) {
	f.ports = append(f.ports, port)
	return &compute.NamedPort{Name: bgName(port), Port: port}, nil
}

func newFakeInstanceGroups(nodes sets.String) *fakeInstanceGroups {
	return &fakeInstanceGroups{
		instances:  nodes,
		listResult: getInstanceList(nodes),
	}
}

func newNodePool(f InstanceGroups, defaultIgName string, t *testing.T) NodePool {
	pool, err := NewNodePool(f, defaultIgName)
	if err != nil || pool == nil {
		t.Fatalf("%v", err)
	}
	return pool
}

func TestNewNodePoolCreate(t *testing.T) {
	f := newFakeInstanceGroups(sets.NewString())
	newNodePool(f, defaultIgName, t)

	// Test that creating a node pool creates a default instance group
	// after checking that it doesn't already exist.
	if f.instanceGroup != defaultIgName {
		t.Fatalf("Default instance group not created, got %v expected %v.",
			f.instanceGroup, defaultIgName)
	}

	if f.calls[0] != Get {
		t.Fatalf("Default instance group was created without existence check.")
	}

	f.getResult = &compute.InstanceGroup{}
	f.instanceGroup = ""
	pool := newNodePool(f, "newDefaultIgName", t)
	for _, call := range f.calls {
		if call == Create {
			t.Fatalf("Tried to create instance group when one already exists.")
		}
	}
	if pool.(*Instances).defaultIg != f.getResult {
		t.Fatalf("Default instance group not created, got %v expected %v.",
			f.instanceGroup, defaultIgName)
	}
}

func TestNodePoolSync(t *testing.T) {

	f := newFakeInstanceGroups(sets.NewString(
		[]string{"n1", "n2"}...))
	pool := newNodePool(f, defaultIgName, t)

	// KubeNodes: n1
	// GCENodes: n1, n2
	// Remove n2 from the instance group.

	f.calls = []int{}
	kubeNodes := sets.NewString([]string{"n1"}...)
	pool.Sync(kubeNodes.List())
	if len(f.calls) != 1 || f.calls[0] != RemoveInstances ||
		f.instances.Len() != kubeNodes.Len() ||
		!kubeNodes.IsSuperset(f.instances) {
		t.Fatalf(
			"Expected %v with instances %v, got %v with instances %+v",
			RemoveInstances, kubeNodes, f.calls, f.instances)
	}

	// KubeNodes: n1, n2
	// GCENodes: n1
	// Try to add n2 to the instance group. If n2 doesn't exist this will fail.

	f = newFakeInstanceGroups(sets.NewString([]string{"n1"}...))
	pool = newNodePool(f, defaultIgName, t)

	f.calls = []int{}
	kubeNodes = sets.NewString([]string{"n1", "n2"}...)
	pool.Sync(kubeNodes.List())
	if len(f.calls) != 1 || f.calls[0] != AddInstances ||
		f.instances.Len() != kubeNodes.Len() ||
		!kubeNodes.IsSuperset(f.instances) {
		t.Fatalf(
			"Expected %v with instances %v, got %v with instances %+v",
			RemoveInstances, kubeNodes, f.calls, f.instances)
	}

	// KubeNodes: n1, n2
	// GCENodes: n1, n2
	// Do nothing.

	f = newFakeInstanceGroups(sets.NewString([]string{"n1", "n2"}...))
	pool = newNodePool(f, defaultIgName, t)

	f.calls = []int{}
	kubeNodes = sets.NewString([]string{"n1", "n2"}...)
	pool.Sync(kubeNodes.List())
	if len(f.calls) != 0 {
		t.Fatalf(
			"Did not expect any calls, got %+v", f.calls)
	}
}
