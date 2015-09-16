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

	gce "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
)

const (
	defaultPort            = 80
	defaultPortRange       = "80"
	defaultHttpHealthCheck = "default-health-check"

	// A single instance-group is created per cluster manager.
	// Tagged with the name of the controller.
	instanceGroupPrefix = "k8-ig"

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
	cluster.instancePool, err = NewNodePool(
		cloud, fmt.Sprintf("%v-%v", instanceGroupPrefix, name))
	if err != nil {
		return nil, err
	}
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
