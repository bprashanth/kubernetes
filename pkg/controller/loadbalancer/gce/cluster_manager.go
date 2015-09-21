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

	compute "google.golang.org/api/compute/v1"
	gce "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
)

const (
	defaultPort            = 80
	defaultPortRange       = "80"
	defaultHttpHealthCheck = "default-health-check"

	// A single instance-group is created per cluster manager.
	// Tagged with the name of the controller.
	instanceGroupPrefix = "k8-ig"

	// A backend is created per nodePort, tagged with the nodeport.
	// This allows sharing of backends across loadbalancers.
	backendPrefix      = "k8-be"
	defaultBackendName = "k8-be-default"

	// A single target proxy/urlmap/forwarding rule is created per loadbalancer.
	// Tagged with the name of the IngressPoint.
	targetProxyPrefix    = "k8-tp"
	forwardingRulePrefix = "k8-fw"
	urlMapPrefix         = "k8-um"

	// The gce api uses the name of a path rule to match a host rule.
	// In the current implementation,
	hostRulePrefix = "host"

	// State string required by gce library to list all instances.
	allInstances = "ALL"

	// Used in the test RunServer method to denote a delete request.
	deleteType = "del"
)

// ClusterManager manages L7s at a cluster level.
type ClusterManager struct {
	ClusterName  string
	cloud        *gce.GCECloud
	instancePool NodePool
	backendPool  BackendPool
	l7Pool       LoadBalancerPool
}

// NewClusterManager creates a cluster manager for shared resources.
func NewClusterManager(name string) (*ClusterManager, error) {
	cloud, err := gce.NewGCECloud(nil)
	if err != nil {
		return nil, err
	}
	cluster := ClusterManager{
		ClusterName: name,
		cloud:       cloud,
	}

	// Why do we need so many defaults?
	// Default IG: We add all instances to a single ig, and
	// every service that requires loadbalancing opens up
	// a nodePort on the cluster, which translates to a node
	// on this default ig.
	//
	// Default Backend: We need a default backend to create
	// every urlmap, even if the user doesn't specify one.
	// This is the backend that gets requests if no paths match.
	// Note that this backend doesn't actually occupy a port
	// on the instance group.
	//
	// Default Health Check: Needs investigation. Currently
	// we just plug something in there for the api.

	defaultIgName := fmt.Sprintf("%v-%v", instanceGroupPrefix, name)
	cluster.instancePool, err = NewNodePool(
		cloud, defaultIgName)
	if err != nil {
		return nil, err
	}
	// TODO: We're roud tripping for a resource we just created.
	defaultIg, err := cluster.instancePool.Get(defaultIgName)
	if err != nil {
		return nil, err
	}
	defaultHc, err := cloud.GetHttpHealthCheck(defaultHttpHealthCheck)
	if err != nil {
		return nil, err
	}
	cluster.backendPool, err = NewBackendPool(
		cloud,
		defaultBeName,
		defaultIg,
		defaultHc,
		cloud)
	if err != nil {
		return nil, err
	}
	// TODO: Don't cast, the problem here is the default backend doesn't have
	// a port and the interface only allows backend access via port.
	cluster.l7Pool = NewLoadBalancerPool(
		cloud, cluster.backendPool.(*Backends).defaultBackend)
	return &cluster, nil
}

func (c *ClusterManager) AddNodes(nodeNames []string) error {
	return c.instancePool.Add(nodeNames)
}

func (c *ClusterManager) RemoveNodes(nodeNames []string) error {
	return c.instancePool.Remove(nodeNames)
}

func (c *ClusterManager) SyncNodes(nodeNames []string) error {
	return c.instancePool.Sync(nodeNames)
}

func (c *ClusterManager) AddBackend(port int64) error {
	return c.backendPool.Add(port)
}

func (c *ClusterManager) GetBackend(port int64) (*compute.BackendService, error) {
	return c.backendPool.Get(port)
}

func (c *ClusterManager) DeleteBackend(port int64) error {
	return c.backendPool.Delete(port)
}

func (c *ClusterManager) SyncBackends(ports []int64) error {
	return c.backendPool.Sync(ports)
}

func (c *ClusterManager) AddL7(name string) error {
	return c.l7Pool.Add(name)
}

func (c *ClusterManager) GetL7(name string) (*L7, error) {
	return c.l7Pool.Get(name)
}

func (c *ClusterManager) DeleteL7(name string) error {
	return c.l7Pool.Delete(name)
}

func (c *ClusterManager) SyncL7s(names []string) error {
	return c.l7Pool.Sync(names)
}
