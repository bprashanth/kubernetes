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
	gce "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
	"k8s.io/kubernetes/pkg/util"

	"github.com/golang/glog"
)

const (
	urlMapPort  = 8082
	defaultPort = 80

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
	// TODO: Include default health check
	cloud *gce.GCECloud
}

// GetBackend returns a single backend
func (c *ClusterManager) GetBackend(port int64) (*compute.BackendService, error) {
	return c.cloud.GetBackend(bgName(port))
}

// Backend will return a backend for the given port. If one doesn't already exist, it
// will create it. If the port isn't one of the named ports in the instance group, it
// will add it. It returns a backend ready for insertion into a urlmap.
func (c *ClusterManager) Backend(port int64) (*compute.BackendService, error) {
	if c.defaultIg == nil {
		return nil, fmt.Errorf("Cannot create backend without an instance group.")
	}
	namedPort, err := c.cloud.AddPortToInstanceGroup(c.defaultIg, port)
	if err != nil {
		return nil, err
	}
	bg, err := c.GetBackend(port)
	if bg != nil {
		glog.Infof("Backend %v already exists", bg.Name)
		return bg, nil
	}
	glog.Infof("Creating backend for instance group %v and port %v", c.defaultIg.Name, port)
	return c.cloud.CreateBackendForPort(c.defaultIg, namedPort, bgName(port))
}

func (c *ClusterManager) DeleteBackend(bg *compute.BackendService) error {
	return c.cloud.DeleteBackend(bg.Name)
}

// NewClusterManager creates a cluster manager for shared resources.
func NewClusterManager(name string) (*ClusterManager, error) {
	cloud, err := gce.NewGCECloud(nil)
	if err != nil {
		return nil, err
	}
	cluster := ClusterManager{ClusterName: name, cloud: cloud}
	ig, err := cluster.instanceGroup()
	if err != nil {
		return nil, err
	}
	cluster.defaultIg = ig
	bg, err := cluster.Backend(defaultPort)
	if err != nil {
		return nil, err
	}
	cluster.defaultBackend = bg
	return &cluster, nil
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

func (c *ClusterManager) Cleanup() error {
	if c.defaultBackend != nil {
		if err := c.DeleteBackend(c.defaultBackend); err != nil {
			return err
		}
		c.defaultBackend = nil
	}
	if c.defaultIg != nil {
		if err := c.cloud.DeleteInstanceGroup(c.defaultIg.Name); err != nil {
			return err
		}
		c.defaultIg = nil
	}
	return nil
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

// NewL7 creates an L7 using shared resources created by the cluster manager.
func (c *ClusterManager) NewL7(name string) *L7 {
	l := &L7{
		LbName:         name,
		cloud:          c.cloud,
		defaultBackend: c.defaultBackend,
		UpdateBus:      make(chan map[string]*compute.BackendService),
	}
	l.urlMap(c.defaultBackend).proxy().forwardingRule()
	return l
}

// L7 represents a single L7 loadbalancer.
type L7 struct {
	LbName         string
	instances      []string
	cloud          *gce.GCECloud
	um             *compute.UrlMap
	tp             *compute.TargetHttpProxy
	fw             *compute.ForwardingRule
	defaultBackend *compute.BackendService
	healthCheck    *compute.HttpHealthCheck
	Errors         []error
	UpdateBus      chan map[string]*compute.BackendService
}

// GetIP returns the ip associated with the forwarding rule for this l7.
func (l *L7) GetIP() string {
	return l.fw.IPAddress
}

func (l *L7) urlMap(backend *compute.BackendService) *L7 {
	// TODO: Expose this so users can specify a default if no paths match.
	if l.defaultBackend == nil {
		l.Errors = append(l.Errors, fmt.Errorf("Cannot create urlmap without default backend."))
		return l
	}

	urlMapName := fmt.Sprintf("%v-%v", urlMapPrefix, l.LbName)
	urlMap, err := l.cloud.GetUrlMap(urlMapName)
	if urlMap != nil {
		glog.Infof("Instance group %v already exists", urlMap.Name)
		l.um = urlMap
		return l
	}

	glog.Infof("Creating url map for backend %v", l.defaultBackend.Name)
	urlMap, err = l.cloud.CreateUrlMap(l.defaultBackend, urlMapName)
	if err != nil {
		l.Errors = append(l.Errors, err)
	} else {
		l.um = urlMap
	}
	return l
}

func (l *L7) proxy() *L7 {
	if l.um == nil {
		l.Errors = append(l.Errors, fmt.Errorf("Cannot create proxy without urlmap."))
		return l
	}

	proxyName := fmt.Sprintf("%v-%v", targetProxyPrefix, l.LbName)
	proxy, err := l.cloud.GetProxy(proxyName)
	if proxy != nil {
		glog.Infof("Proxy %v already exists", proxy.Name)
		l.tp = proxy
		return l
	}

	glog.Infof("Creating new http proxy for urlmap %v", l.um.Name)
	proxy, err = l.cloud.CreateProxy(l.um, proxyName)
	if err != nil {
		l.Errors = append(l.Errors, err)
	} else {
		l.tp = proxy
	}
	return l
}

func (l *L7) forwardingRule() *L7 {
	if l.proxy == nil {
		l.Errors = append(l.Errors, fmt.Errorf("Cannot create forwarding rule without proxy."))
		return l
	}

	forwardingRuleName := fmt.Sprintf("%v-%v", forwardingRulePrefix, l.LbName)
	fw, err := l.cloud.GetGlobalForwardingRule(forwardingRuleName)
	if fw != nil {
		glog.Infof("Forwarding rule %v already exists", fw.Name)
		l.fw = fw
		return l
	}

	glog.Infof("Creating forwarding rule for proxy %v", l.tp.Name)
	fw, err = l.cloud.CreateGlobalForwardingRule(l.tp, forwardingRuleName)
	if err != nil {
		l.Errors = append(l.Errors, err)
	} else {
		l.fw = fw
	}
	return l
}

// UpdateUrlMap translates the given hostname: endpoint->port mapping into a gce url map.
//
// The GCE url map allows multiple hosts to share url->backend mappings without duplication, eg:
//   Host: foo(PathMatcher1), bar(PathMatcher1,2)
//   PathMatcher1:
//     /a -> b1
//     /b -> b2
//   PathMatcher2:
//     /c -> b1
// This leads to a lot of complexity in the common case, where all we want is a mapping of
// host->{/path: backend}.
//
// Consider some alternatives:
// 1. Using a single backend per PathMatcher:
//   Host: foo(PathMatcher1,3) bar(PathMatcher1,2,3)
//   PathMatcher1:
//     /a -> b1
//   PathMatcher2:
//     /c -> b1
//   PathMatcher3:
//     /b -> b2
// 2. Using a single host per PathMatcher:
//   Host: foo(PathMatcher1)
//   PathMatcher1:
//     /a -> b1
//     /b -> b2
//   Host: bar(PathMatcher2)
//   PathMatcher2:
//     /a -> b1
//     /b -> b2
//     /c -> b1
// In the context of kubernetes services, 2 makes more sense, because we
// rarely want to lookup backends (service:nodeport). When a service is
// deleted, we need to find all host PathMatchers that have the backend
// and remove the mapping. When a new path is added to a host (happens
// more frequently than service deletion) we just need to lookup the 1
// pathmatcher of the host.
func (l *L7) UpdateUrlMap(subdomainToBackendUrlMap map[string]map[string]*compute.BackendService) error {
	if l.um == nil {
		return fmt.Errorf("Cannot add url without an urlmap.")
	}

	for hostname, urlToBackend := range subdomainToBackendUrlMap {
		// Find the hostrule
		// Find the path matcher
		// Add all given endpoint:backends to pathRules in path matcher
		var hostRule *compute.HostRule
		pmName := getNameForPathMatcher(hostname)
		for _, hr := range l.um.HostRules {
			// TODO: Hostnames must be exact match?
			if hr.Hosts[0] == hostname {
				hostRule = hr
				break
			}
		}
		if hostRule == nil {
			// This is a new host
			hostRule = &compute.HostRule{
				Hosts:       []string{hostname},
				PathMatcher: pmName,
			}
			l.um.HostRules = append(l.um.HostRules, hostRule)
		}
		var pathMatcher *compute.PathMatcher
		for _, pm := range l.um.PathMatchers {
			if pm.Name == hostRule.PathMatcher {
				pathMatcher = pm
				break
			}
		}
		if pathMatcher == nil {
			// This is a dangling or new host
			pathMatcher = &compute.PathMatcher{
				Name:           pmName,
				DefaultService: l.um.DefaultService,
			}
			l.um.PathMatchers = append(l.um.PathMatchers, pathMatcher)
		}
		// Clobber existing path rules.
		pathMatcher.PathRules = []*compute.PathRule{}
		for ep, bg := range urlToBackend {
			pathMatcher.PathRules = append(pathMatcher.PathRules, &compute.PathRule{[]string{ep}, bg.SelfLink})
		}
	}
	if um, err := l.cloud.UpdateUrlMap(l.um); err != nil {
		return err
	} else {
		l.um = um
	}
	return nil
}

// Cleanup deletes resources specific to this l7 in the right order.
// forwarding rule -> target proxy -> url map
// This leaves backends and health checks, which are shared across loadbalancers.
func (l *L7) Cleanup() error {
	if l.fw != nil {
		glog.Infof("Deleting global forwarding rule %v", l.fw.Name)
		if err := l.cloud.DeleteGlobalForwardingRule(l.fw.Name); err != nil {
			return err
		}
		l.fw = nil
	}
	if l.tp != nil {
		glog.Infof("Deleting target proxy %v", l.tp.Name)
		if err := l.cloud.DeleteProxy(l.tp.Name); err != nil {
			return err
		}
		l.tp = nil
	}
	if l.um != nil {
		glog.Infof("Deleting url map %v", l.um.Name)
		if err := l.cloud.DeleteUrlMap(l.um.Name); err != nil {
			return err
		}
		l.um = nil
	}
	return nil
}

// getNameForPathMatcher returns a name for a pathMatcher based on the given host rule.
// The host rule can be a regex, the path matcher name used to associate the 2 cannot.
func getNameForPathMatcher(hostRule string) string {
	hasher := md5.New()
	hasher.Write([]byte(hostRule))
	return fmt.Sprintf("%v%v", hostRulePrefix, hex.EncodeToString(hasher.Sum(nil)))
}

// runServer is a debug method that runs a server listening for urlmaps for a single loadbalancer.
// Eg invocation add: curl http://localhost:8082 -X POST -d '{"foo.bar.com":{"/test/*": 31778}}'
// Eg invocation del: curl http://localhost:8082?type=del -X POST -d '{"foo.bar.com":{"/test/*": 31778}}'
func RunTestServer(c *ClusterManager, lbName string) {
	l := c.NewL7(lbName)
	if len(l.Errors) != 0 {
		glog.Fatalf("Failed to create L7: %+v", l.Errors)
	}
	glog.Infof("Forwarding rule has ip %v", l.GetIP())

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		decoder := json.NewDecoder(req.Body)
		subdomainToUrlMap := map[string]map[string]int64{}
		err := decoder.Decode(&subdomainToUrlMap)
		if err != nil {
			glog.Infof("Failed to decode urlmap %v", err)
			return
		}
		eventType := req.URL.Query().Get("type")

		// Map ports to existing backends. If a backend doesn't exist, create it.
		// This is literally the code in the controller, except for the backend creation.
		subdomainToUrlBackend := map[string]map[string]*compute.BackendService{}
		backends := []*compute.BackendService{}
		for subdomain, urlMap := range subdomainToUrlMap {
			urlToBackend := map[string]*compute.BackendService{}
			for endpoint, port := range urlMap {
				bg, err := c.Backend(port)
				if err != nil {
					glog.Infof("Could not get backend for port %v: %v. Ignoring endpoint %v.",
						port, err, endpoint)
					continue
				}
				urlToBackend[endpoint] = bg
				backends = append(backends, bg)
			}
			subdomainToUrlBackend[subdomain] = urlToBackend
		}
		glog.Infof("Processing update for urlmap %+v", subdomainToUrlMap)
		if err := l.UpdateUrlMap(subdomainToUrlBackend); err != nil {
			glog.Infof("Failed to add urlmap %v", err)
		} else {
			glog.Infof("Updated %v with urlmap", l.fw.IPAddress)
		}

		if eventType != deleteType {
			return
		}
		glog.Infof("Deleting loadbalancer %v", l.LbName)
		if err := l.Cleanup(); err != nil {
			glog.Infof("Failed to cleanup loadbalancer resources: %v", err)
		}
		glog.Infof("Deleted private loadbalancer resources")
		for _, bg := range backends {
			if err := c.DeleteBackend(bg); err != nil {
				glog.Infof("Failed to delete backend %v", err)
			}
			glog.Infof("Deleted backend %v", bg.Name)
		}
	})
	glog.Infof("Listening on 8082 for urlmap")
	glog.Fatal(http.ListenAndServe(fmt.Sprintf("0.0.0.0:%v", urlMapPort), nil))
}

func bgName(port int64) string {
	return fmt.Sprintf("%v-%v", backendPrefix, port)
}
