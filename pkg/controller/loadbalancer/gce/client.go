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

package gcelb

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	compute "google.golang.org/api/compute/v1"
	gce "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"

	"github.com/golang/glog"
)

const (
	urlMapPort  = 8082
	defaultPort = 80
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

// NewClusterManager creates a cluster manager for shared resources.
func NewClusterManager(nodes []string, name string) (*ClusterManager, error) {
	cloud, err := gce.NewGCECloud(nil)
	if err != nil {
		return nil, err
	}
	cluster := ClusterManager{ClusterName: name, cloud: cloud}
	ig, err := cluster.instanceGroup(nodes)
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

func (c *ClusterManager) instanceGroup(instances []string) (*compute.InstanceGroup, error) {
	igName := fmt.Sprintf("ig-%v", c.ClusterName)

	ig, err := c.cloud.GetInstanceGroup(igName)
	if ig != nil {
		glog.Infof("Instance group %v already exists", ig.Name)
		return ig, nil
	}

	glog.Infof("Creating instance group for minions %v", instances)
	ig, err = c.cloud.CreateInstanceGroup(instances, igName)
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

	urlMapName := fmt.Sprintf("um-%v", l.LbName)
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

	proxyName := fmt.Sprintf("tp-%v", l.LbName)
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

	forwardingRuleName := fmt.Sprintf("fw-%v", l.LbName)
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
//	   /a -> b1
//	   /b -> b2
//   PathMatcher2:
//	   /c -> b1
// This leads to a lot of complexity in the common case, where all we want is a mapping of
// host->{/path: backend}.
//
// Consider some alternatives:
// 1. Using a single backend per PathMatcher:
//   Host: foo(PathMatcher1,3) bar(PathMatcher1,2,3)
//   PathMatcher1:
//	   /a -> b1
//   PathMatcher2:
//     /c -> b1
//   PathMatcher3:
//     /b -> b2
// 2. Using a single host per PathMatcher:
//   Host: foo(PathMatcher1), bar(PathMatcher2)
//   PathMatcher1:
//     /a -> b1
//     /b -> b2
//     /c -> b1
//   PathMatcher2:
//     /a -> b1
//     /b -> b2
// In the context of kubernetes services, 2 makes more sense, because we
// rarely want to lookup backends (service:nodeport). When a service is
// deleted, we need to find all host PathMatchers that have the backend
// and remove the mapping. When a new path is added to a host (happens
// more frequently than service deletion) we just need to lookup the 1
// pathmatcher of the host.
func (l *L7) UpdateUrlMap(hostname string, urlToBackend map[string]*compute.BackendService) error {
	if l.um == nil {
		return fmt.Errorf("Cannot add url without an urlmap.")
	}

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
	// TODO: Support patch via subresource?
	pathMatcher.PathRules = []*compute.PathRule{}
	for ep, bg := range urlToBackend {
		pathMatcher.PathRules = append(pathMatcher.PathRules, &compute.PathRule{[]string{ep}, bg.SelfLink})
	}
	if um, err := l.cloud.UpdateUrlMap(l.um); err != nil {
		return err
	} else {
		l.um = um
	}
	return nil
}

// getNameForPathMatcher returns a name for a pathMatcher based on the given host rule.
// The host rule can be a regex, the path matcher name used to associate the 2 cannot.
func getNameForPathMatcher(hostRule string) string {
	hasher := md5.New()
	hasher.Write([]byte(hostRule))
	return fmt.Sprintf("host%v", hex.EncodeToString(hasher.Sum(nil)))
}

// runServer is a debug method that runs a server listening for urlmaps for a single loadbalancer.
// Eg invocation: curl http://localhost:8082 -X POST -d '{"/test/*": 31778}'
func RunServer(c *ClusterManager, lbName string) {
	l := c.NewL7(lbName)
	if len(l.Errors) != 0 {
		glog.Fatalf("Failed to create L7: %+v", l.Errors)
	}
	glog.Infof("Forwarding rule has ip %v", l.GetIP())

	http.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		decoder := json.NewDecoder(req.Body)
		urlMap := map[string]int64{}
		err := decoder.Decode(&urlMap)
		if err != nil {
			glog.Infof("Failed to decode urlmap %v", err)
			return
		}

		// Map ports to existing backends. If a backend doesn't exist, create it.
		urlToBackend := map[string]*compute.BackendService{}
		for endpoint, port := range urlMap {
			bg, err := c.Backend(port)
			if err != nil {
				glog.Infof("Could not get backend for port %v: %v. Ignoring endpoint %v.", port, err, endpoint)
				continue
			}
			urlToBackend[endpoint] = bg
		}
		glog.Infof("Processing update for urlmap %+v", urlMap)
		if err := l.UpdateUrlMap("*", urlToBackend); err != nil {
			glog.Infof("Failed to add urlmap %v", err)
		} else {
			glog.Infof("Updated %v with urlmap", l.fw.IPAddress)
		}
	})
	glog.Infof("Listening on 8082 for urlmap")
	glog.Fatal(http.ListenAndServe(fmt.Sprintf("0.0.0.0:%v", urlMapPort), nil))
}

func bgName(port int64) string {
	return fmt.Sprintf("bg-%v", port)
}
