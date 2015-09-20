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

	//"github.com/golang/glog"
	compute "google.golang.org/api/compute/v1"
	"k8s.io/kubernetes/pkg/util/sets"
)

const defaultBeName = "defaultBeName"

type fakeBackendServices struct {
	backendServices []*compute.BackendService
	calls           []int
}

func (f *fakeBackendServices) GetBackendService(name string) (*compute.BackendService, error) {
	f.calls = append(f.calls, Get)
	for i, _ := range f.backendServices {
		if name == f.backendServices[i].Name {
			return f.backendServices[i], nil
		}
	}
	return nil, fmt.Errorf("Backend service %v not found", name)
}

func (f *fakeBackendServices) CreateBackendService(be *compute.BackendService) error {
	f.calls = append(f.calls, Create)
	f.backendServices = append(f.backendServices, be)
	return nil
}

func (f *fakeBackendServices) DeleteBackendService(name string) error {
	f.calls = append(f.calls, Delete)
	newBackends := []*compute.BackendService{}
	for i, _ := range f.backendServices {
		if name != f.backendServices[i].Name {
			newBackends = append(newBackends, f.backendServices[i])
		}
	}
	f.backendServices = newBackends
	return nil
}

func (f *fakeBackendServices) UpdateBackendService(be *compute.BackendService) error {

	f.calls = append(f.calls, Update)
	for i, _ := range f.backendServices {
		if f.backendServices[i].Name == be.Name {
			f.backendServices[i] = be
		}
	}
	return nil
}

func newFakeBackendServices() *fakeBackendServices {
	return &fakeBackendServices{
		backendServices: []*compute.BackendService{},
	}
}

func newBackendPool(f BackendServices, fakeIgs InstanceGroups, defaultBeName string, t *testing.T) BackendPool {
	pool, err := NewBackendPool(
		f,
		defaultBeName,
		&compute.InstanceGroup{
			SelfLink: "foo",
		},
		&compute.HttpHealthCheck{}, fakeIgs)
	if err != nil || pool == nil {
		t.Fatalf("%v", err)
	}
	return pool
}

func TestNewBackendPool(t *testing.T) {
	f := newFakeBackendServices()
	fakeIgs := newFakeInstanceGroups(sets.NewString())

	// Create a backend and pool, then recreate the pool and make sure
	// it reuses the existing backend. This simulates the restart scenario.
	newBackendPool(f, fakeIgs, defaultBeName, t)
	if f.backendServices[0].Name != defaultBeName {
		t.Fatalf("%v not created as expected", defaultBeName)
	}
	f.calls = []int{}
	pool := newBackendPool(f, fakeIgs, defaultBeName, t)
	for _, call := range f.calls {
		if call == Create {
			t.Fatalf("Tried to create instance group when one already exists.")
		}
	}
	got, _ := f.GetBackendService(defaultBeName)
	if pool.(*Backends).defaultBackend != got {
		t.Fatalf("Default backend service not create, got %v expected %v",
			f.backendServices, defaultBeName)
	}
}

func TestBackendPoolAdd(t *testing.T) {
	f := newFakeBackendServices()
	fakeIgs := newFakeInstanceGroups(sets.NewString())
	pool := newBackendPool(f, fakeIgs, defaultBeName, t)

	// Add a backend for a port, then re-add the same port and
	// make sure it corrects a broken link from the backend to
	// the instance group.
	nodePort := int64(8080)
	pool.Add(nodePort)
	beName := beName(nodePort)

	// Check that the new backend has the right port
	be, err := f.GetBackendService(beName)
	if err != nil {
		t.Fatalf("Did not find expected backend %v", beName)
	}
	if be.Port != nodePort {
		t.Fatalf("Backend %v has wrong port %v, expected %v", be.Name, be.Port, nodePort)
	}
	// Check that the instance group has the new port
	var found bool
	for _, port := range fakeIgs.ports {
		if port == nodePort {
			found = true
		}
	}
	if !found {
		t.Fatalf("Port %v not added to instance group", nodePort)
	}

	// Mess up the link between backend service and instance group.
	// This simulates a user doing foolish things through the UI.
	f.calls = []int{}
	be, err = f.GetBackendService(beName)
	be.Backends[0].Group = "test edge hop"
	f.UpdateBackendService(be)

	pool.Add(nodePort)
	for _, call := range f.calls {
		if call == Create {
			t.Fatalf("Unexpected create for existing backend service")
		}
	}
	got, _ := f.GetBackendService(defaultBeName)
	if got.Backends[0].Group != pool.(*Backends).defaultIg.SelfLink {
		t.Fatalf(
			"Broken instance group link: %v %v",
			got.Backends[0].Group,
			pool.(*Backends).defaultIg.SelfLink)
	}
}

func TestBackendPoolSync(t *testing.T) {
	// Call sync on a backend pool with a list of ports, make sure the pool
	// creates/deletes required ports.
	svcNodePorts := []int64{81, 82, 83}
	f := newFakeBackendServices()
	fakeIgs := newFakeInstanceGroups(sets.NewString())
	pool := newBackendPool(f, fakeIgs, defaultBeName, t)
	pool.Add(81)
	pool.Add(90)
	pool.Sync(svcNodePorts)
	if _, err := pool.Get(90); err == nil {
		t.Fatalf("Did not expect to find port 90")
	}
	for _, port := range svcNodePorts {
		if _, err := pool.Get(port); err != nil {
			t.Fatalf("Expected to find port %v", port)
		}
	}

}
