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
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/unversioned/cache"
	gce "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
	"k8s.io/kubernetes/pkg/util"

	"github.com/golang/glog"
)

const (
	urlMapPort             = 8082
	defaultPort            = 80
	defaultPortRange       = "80"
	defaultHttpHealthCheck = "default-health-check"

	// A single target proxy/urlmap/forwarding rule is created per loadbalancer.
	// Tagged with the name of the IngressPoint.
	targetProxyPrefix    = "k8-tp"
	forwardingRulePrefix = "k8-fw"
	urlMapPrefix         = "k8-um"

	// A backend is created per nodePort, tagged with the nodeport.
	// This allows sharing of backends across loadbalancers.
	backendPrefix = "k8-bg"

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
	ClusterName    string
	defaultIg      *compute.InstanceGroup
	defaultBackend *compute.BackendService
	backendPool    *Backends
	l7Pool         *L7s
	// TODO: Include default health check
	cloud *gce.GCECloud
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
	cluster.backendPool = NewBackendPool(&cluster)
	cluster.l7Pool = NewL7Pool(&cluster)

	ig, err := cluster.instanceGroup()
	if err != nil {
		return nil, err
	}
	cluster.defaultIg = ig

	def := fmt.Sprintf("%v-%v", backendPrefix, "default")
	be, _ := cloud.GetBackend(def)
	if be == nil {
		be, err = cluster.backendPool.create(
			ig, &compute.NamedPort{Port: defaultPort, Name: "default"}, def)
		if err != nil {
			return nil, err
		}
	}
	cluster.defaultBackend = be

	return &cluster, nil
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

func (c *ClusterManager) AddNodes(nodeNames []string) error {
	glog.Infof("Adding nodes %v", nodeNames)
	return c.cloud.AddInstancesToInstanceGroup(c.defaultIg.Name, nodeNames)
}

func (c *ClusterManager) RemoveNodes(nodeNames []string) error {
	glog.Infof("Removing nodes %v", nodeNames)
	return c.cloud.RemoveInstancesFromInstanceGroup(c.defaultIg.Name, nodeNames)
}

func (c *ClusterManager) GetNodes() (util.StringSet, error) {
	nodeNames := util.NewStringSet()
	instances, err := c.cloud.ListInstancesInInstanceGroup(
		c.defaultIg.Name, allInstances)
	if err != nil {
		return nodeNames, err
	}
	for _, ins := range instances.Items {
		// TODO: If round trips weren't so slow one would be inclided
		// to GetInstance using this url and get the name.
		parts := strings.Split(ins.Instance, "/")
		nodeNames.Insert(parts[len(parts)-1])
	}
	return nodeNames, nil
}

func (c *ClusterManager) instanceGroup() (*compute.InstanceGroup, error) {
	igName := fmt.Sprintf("%v-%v", instanceGroupPrefix, c.ClusterName)

	ig, err := c.cloud.GetInstanceGroup(igName)
	if ig != nil {
		glog.Infof("Instance group %v already exists", ig.Name)
		return ig, nil
	}

	// TODO: We need a get->delete->add thing here.
	glog.Infof("Creating instance group %v", igName)
	ig, err = c.cloud.CreateInstanceGroup(igName)
	if err != nil {
		return nil, err
	}
	return ig, err
}

// getNameForPathMatcher returns a name for a pathMatcher based on the given host rule.
// The host rule can be a regex, the path matcher name used to associate the 2 cannot.
func getNameForPathMatcher(hostRule string) string {
	hasher := md5.New()
	hasher.Write([]byte(hostRule))
	return fmt.Sprintf("%v%v", hostRulePrefix, hex.EncodeToString(hasher.Sum(nil)))
}

// runServer is a debug method.
// Eg invocation add: curl http://localhost:8082 -X POST -d '{"foo.bar.com":{"/test/*": 31778}}'
// Eg invocation del: curl http://localhost:8082?type=del -X POST -d '{"foo.bar.com":{"/test/*": 31778}}'
func RunTestServer(c *ClusterManager, lbName string) {

	lbc := loadBalancerController{clusterManager: c}
	lbc.pmQueue = NewTaskQueue(lbc.sync)
	lbc.pmLister.Store = cache.NewStore(keyFunc)

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {

		decoder := json.NewDecoder(req.Body)
		subdomainToUrlMap := map[string]map[string]int64{}
		err := decoder.Decode(&subdomainToUrlMap)
		if err != nil {
			glog.Fatalf("Failed to decode urlmap %v", err)
		}
		eventType := req.URL.Query().Get("type")

		urlMap := map[string]api.UrlMap{}
		for subdomain, um := range subdomainToUrlMap {
			pathMap := api.UrlMap{}
			for path, port := range um {
				pathMap[path] = &api.ServiceRef{
					Service: api.ObjectReference{},
					Port:    api.ServicePort{NodePort: int(port)},
				}
			}
			urlMap[subdomain] = pathMap
		}
		pm := &api.PathMap{
			ObjectMeta: api.ObjectMeta{
				Name:      lbName,
				Namespace: api.NamespaceDefault,
			},
			Spec: api.PathMapSpec{
				Host:    "foo",
				PathMap: urlMap,
			},
		}
		if err := lbc.pmLister.Store.Add(pm); err != nil {
			glog.Fatalf("%v", err)
		}
		key, err := keyFunc(pm)
		if err != nil {
			glog.Fatalf("%v", err)
		}
		lbc.sync(key)
		if eventType != deleteType {
			return
		}
		if err := lbc.pmLister.Store.Delete(pm); err != nil {
			glog.Fatalf("%v", err)
		}
		lbc.sync(key)
	})
	glog.Infof("Listening on 8082 for urlmap")
	glog.Fatal(http.ListenAndServe(fmt.Sprintf("0.0.0.0:%v", urlMapPort), nil))
}

func bgName(port int64) string {
	return fmt.Sprintf("%v-%v", backendPrefix, port)
}

// compareSelfLinks returns true if the 2 self links are equal.
func compareSelfLinks(l1, l2 string) bool {
	// TODO: These can be partial links
	return l1 == l2
}
