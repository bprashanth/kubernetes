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
	"log"
	"testing"

	compute "google.golang.org/api/compute/v1"
)

type fakeLoadBalancers struct {
	fw   []*compute.ForwardingRule
	um   []*compute.UrlMap
	tp   []*compute.TargetHttpProxy
	name string
}

// TODO: There is some duplication between these functions and the name mungers in
// loadbalancer file.
func (f *fakeLoadBalancers) fwName() string {
	return fmt.Sprintf("%v-%v", forwardingRulePrefix, f.name)
}

func (f *fakeLoadBalancers) umName() string {
	return fmt.Sprintf("%v-%v", urlMapPrefix, f.name)
}

func (f *fakeLoadBalancers) tpName() string {
	return fmt.Sprintf("%v-%v", targetProxyPrefix, f.name)
}

func (f *fakeLoadBalancers) String() string {
	msg := fmt.Sprintf(
		"Loadbalancer %v,\nforwarding rules:\n", f.name)
	for _, fw := range f.fw {
		msg += fmt.Sprintf("%v\n", fw.Name)
	}
	msg += fmt.Sprintf("Target proxies\n")
	for _, tp := range f.tp {
		msg += fmt.Sprintf("%v\n", tp.Name)
	}
	msg += fmt.Sprintf("UrlMaps\n")
	for _, um := range f.um {
		msg += fmt.Sprintf("%v\n", um.Name)
	}
	return msg
}

// Forwarding Rule fakes
func (f *fakeLoadBalancers) GetGlobalForwardingRule(name string) (*compute.ForwardingRule, error) {
	for i, _ := range f.fw {
		if f.fw[i].Name == name {
			return f.fw[i], nil
		}
	}
	return nil, fmt.Errorf("Forwarding rule %v not found", name)
}

func (f *fakeLoadBalancers) CreateGlobalForwardingRule(proxy *compute.TargetHttpProxy, name string, portRange string) (*compute.ForwardingRule, error) {

	rule := &compute.ForwardingRule{
		Name:       name,
		Target:     proxy.SelfLink,
		PortRange:  portRange,
		IPProtocol: "TCP",
		SelfLink:   f.fwName(),
	}
	f.fw = append(f.fw, rule)
	return rule, nil
}

func (f *fakeLoadBalancers) SetProxyForGlobalForwardingRule(fw *compute.ForwardingRule, proxy *compute.TargetHttpProxy) error {
	for i, _ := range f.fw {
		if f.fw[i].Name == fw.Name {
			f.fw[i].Target = proxy.SelfLink
		}
	}
	return nil
}

func (f *fakeLoadBalancers) DeleteGlobalForwardingRule(name string) error {
	fw := []*compute.ForwardingRule{}
	for i, _ := range f.fw {
		if f.fw[i].Name != name {
			fw = append(fw, f.fw[i])
		}
	}
	f.fw = fw
	return nil
}

// UrlMaps fakes
func (f *fakeLoadBalancers) GetUrlMap(name string) (*compute.UrlMap, error) {
	for i, _ := range f.um {
		if f.um[i].Name == name {
			return f.um[i], nil
		}
	}
	return nil, fmt.Errorf("Url Map %v not found", name)
}

func (f *fakeLoadBalancers) CreateUrlMap(backend *compute.BackendService, name string) (*compute.UrlMap, error) {
	urlMap := &compute.UrlMap{
		Name:           name,
		DefaultService: backend.SelfLink,
		SelfLink:       f.umName(),
	}
	f.um = append(f.um, urlMap)
	return urlMap, nil
}

func (f *fakeLoadBalancers) UpdateUrlMap(urlMap *compute.UrlMap) (*compute.UrlMap, error) {
	for i, _ := range f.um {
		if f.um[i].Name == urlMap.Name {
			f.um[i] = urlMap
			return urlMap, nil
		}
	}
	return nil, nil
}

func (f *fakeLoadBalancers) DeleteUrlMap(name string) error {
	um := []*compute.UrlMap{}
	for i, _ := range f.um {
		if f.um[i].Name != name {
			um = append(um, f.um[i])
		}
	}
	f.um = um
	return nil
}

// TargetProxies fakes
func (f *fakeLoadBalancers) GetTargetHttpProxy(name string) (*compute.TargetHttpProxy, error) {
	for i, _ := range f.tp {
		if f.tp[i].Name == name {
			return f.tp[i], nil
		}
	}
	return nil, fmt.Errorf("Targetproxy %v not found", name)
}

func (f *fakeLoadBalancers) CreateTargetHttpProxy(urlMap *compute.UrlMap, name string) (*compute.TargetHttpProxy, error) {
	proxy := &compute.TargetHttpProxy{
		Name:     name,
		UrlMap:   urlMap.SelfLink,
		SelfLink: f.tpName(),
	}
	f.tp = append(f.tp, proxy)
	return proxy, nil
}

func (f *fakeLoadBalancers) DeleteTargetHttpProxy(name string) error {
	tp := []*compute.TargetHttpProxy{}
	for i, _ := range f.tp {
		if f.tp[i].Name != name {
			tp = append(tp, f.tp[i])
		}
	}
	f.tp = tp
	return nil
}
func (f *fakeLoadBalancers) SetUrlMapForTargetHttpProxy(proxy *compute.TargetHttpProxy, urlMap *compute.UrlMap) error {
	for i, _ := range f.tp {
		if f.tp[i].Name == proxy.Name {
			f.tp[i].UrlMap = urlMap.SelfLink
		}
	}
	return nil
}

// newFakeLoadBalancers creates a fake cloud client. Name is the name
// inserted into the selfLink of the associated resources for testing.
// eg: forwardingRule.SelfLink == k8-fw-name.
func newFakeLoadBalancers(name string) *fakeLoadBalancers {
	return &fakeLoadBalancers{
		fw:   []*compute.ForwardingRule{},
		name: name,
	}
}

func newLoadBalancerPool(f LoadBalancers, t *testing.T) LoadBalancerPool {
	return NewLoadBalancerPool(
		f,
		&compute.BackendService{
			SelfLink: defaultBackendName,
		},
	)
}

func TestCreateLoadBalancer(t *testing.T) {
	lbName := "test"
	f := newFakeLoadBalancers(lbName)
	pool := newLoadBalancerPool(f, t)
	pool.Add(lbName)
	l7, err := pool.Get(lbName)
	if err != nil || l7 == nil {
		t.Fatalf("Expected l7 not created")
	}
	um, err := f.GetUrlMap(f.umName())
	if err != nil ||
		um.DefaultService != pool.(*L7s).defaultBackend.SelfLink {
		t.Fatalf("%v", err)
	}
	tp, err := f.GetTargetHttpProxy(f.tpName())
	if err != nil || tp.UrlMap != um.SelfLink {
		t.Fatalf("%v", err)
	}
	fw, err := f.GetGlobalForwardingRule(f.fwName())
	if err != nil || fw.Target != tp.SelfLink {
		t.Fatalf("%v", err)
	}

	log.Printf("Done")
}
