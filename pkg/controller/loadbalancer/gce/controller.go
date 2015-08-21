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
	"reflect"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/client/unversioned/cache"
	"k8s.io/kubernetes/pkg/client/unversioned/record"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/workqueue"
	"k8s.io/kubernetes/pkg/watch"

	"github.com/golang/glog"
)

var (
	keyFunc          = framework.DeletionHandlingMetaNamespaceKeyFunc
	resyncPeriod     = 60 * time.Second
	lbControllerName = "lbcontroller"
)

// taskQueue manages a work queue through an independent worker that
// invokes the given sync function for every work item inserted.
type taskQueue struct {
	queue *workqueue.Type
	sync  func(string)
}

func (t *taskQueue) run(period time.Duration, stopCh <-chan struct{}) {
	util.Until(t.worker, period, stopCh)
}

// enqueue enqueues ns/name of the given api object in the task queue.
func (t *taskQueue) enqueue(obj interface{}) {
	key, err := keyFunc(obj)
	if err != nil {
		glog.Infof("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	t.queue.Add(key)
}

// worker processes work in the queue through sync.
func (t *taskQueue) worker() {
	for {
		key, _ := t.queue.Get()
		glog.Infof("Syncing %v", key)
		t.sync(key.(string))
		t.queue.Done(key)
	}
}

// NewTaskQueue creates a new task queue with the given sync function.
// The sync function is called for every element inserted into the queue.
func NewTaskQueue(syncFn func(string)) *taskQueue {
	return &taskQueue{
		queue: workqueue.New(),
		sync:  syncFn,
	}
}

// loadBalancerController watches the kubernetes api and adds/removes services
// from the loadbalancer, via loadBalancerConfig.
type loadBalancerController struct {
	client         *client.Client
	pmController   *framework.Controller
	nodeController *framework.Controller
	pmLister       cache.StoreToPathMapLister
	nodeLister     cache.StoreToNodeLister
	clusterManager *ClusterManager
	loadBalancers  map[string]*L7
	backends       map[int]*compute.BackendService
	recorder       record.EventRecorder
	nodeQueue      *taskQueue
	pmQueue        *taskQueue
}

// NewLoadBalancerController creates a controller for gce loadbalancers.
func NewLoadBalancerController(kubeClient *client.Client, createClusterManager bool) (*loadBalancerController, error) {
	var clusterManager *ClusterManager
	if createClusterManager {
		nodes, err := getNodeNames(kubeClient)
		if err != nil {
			return nil, err
		}
		glog.Infof("Creating loadbalancer cluster manager %v with nodes %v", lbControllerName, nodes)
		clusterManager, err = NewClusterManager(lbControllerName)
		if err != nil {
			return nil, err
		}
		if err := clusterManager.AddNodes(nodes); err != nil {
			return nil, err
		}
	}
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(kubeClient.Events(""))

	lbc := loadBalancerController{
		client:         kubeClient,
		loadBalancers:  map[string]*L7{},
		backends:       map[int]*compute.BackendService{},
		clusterManager: clusterManager,
		recorder:       eventBroadcaster.NewRecorder(api.EventSource{Component: "loadbalancer-controller"}),
	}
	lbc.nodeQueue = NewTaskQueue(lbc.syncNodes)
	lbc.pmQueue = NewTaskQueue(lbc.sync)

	pathHandlers := framework.ResourceEventHandlerFuncs{
		AddFunc:    lbc.pmQueue.enqueue,
		DeleteFunc: lbc.pmQueue.enqueue,
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				lbc.pmQueue.enqueue(cur)
			}
		},
	}
	lbc.pmLister.Store, lbc.pmController = framework.NewInformer(
		cache.NewListWatchFromClient(
			lbc.client, "pathMaps", api.NamespaceAll, fields.Everything()),
		&api.PathMap{}, resyncPeriod, pathHandlers)
	glog.Infof("Created new loadbalancer controller")

	nodeHandlers := framework.ResourceEventHandlerFuncs{
		AddFunc:    lbc.nodeQueue.enqueue,
		DeleteFunc: lbc.nodeQueue.enqueue,
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				lbc.nodeQueue.enqueue(cur)
			}
		},
	}

	lbc.nodeLister.Store, lbc.nodeController = framework.NewInformer(
		&cache.ListWatch{
			ListFunc: func() (runtime.Object, error) {
				return lbc.client.Get().
					Resource("nodes").
					FieldsSelectorParam(fields.Everything()).
					Do().
					Get()
			},
			WatchFunc: func(resourceVersion string) (watch.Interface, error) {
				return lbc.client.Get().
					Prefix("watch").
					Resource("nodes").
					FieldsSelectorParam(fields.Everything()).
					Param("resourceVersion", resourceVersion).Watch()
			},
		},
		&api.Node{}, resyncPeriod, nodeHandlers)
	glog.Infof("Created new loadbalancer controller")

	return &lbc, nil
}

// Run starts the loadbalancer controller.
func (lbc *loadBalancerController) Run() {
	glog.Infof("Starting loadbalancer controller")
	go lbc.pmController.Run(util.NeverStop)
	go lbc.nodeController.Run(util.NeverStop)
	lbc.pmQueue.run(time.Second, util.NeverStop)
	lbc.nodeQueue.run(time.Second, util.NeverStop)
}

// syncBackends deletes GCE backends without any paths, and creates backends for new paths.
func (lbc *loadBalancerController) syncBackends() error {
	glog.Infof("Syncing backends")
	paths, err := lbc.pmLister.List()
	if err != nil {
		return err
	}
	// gc backends without a path
	for port, backend := range lbc.backends {
		if len(getPathsToNodePort(paths, port)) == 0 {
			glog.Infof("No paths found for port %v, deleting backend %v", port, backend.Name)
		}
		if err := lbc.clusterManager.DeleteBackend(backend); err != nil {
			return err
		}
		glog.Infof("Deleted backend %v", backend.Name)
	}
	// Create backends with a new path. Though this is stored as a nested struct all we need
	// is a flat list of nodePorts for backends.
	for _, pm := range paths.Items {
		for _, subdomainToUrlMap := range pm.Spec.PathMap {
			for _, svcRef := range subdomainToUrlMap {
				port := svcRef.Port.NodePort
				if _, ok := lbc.backends[port]; !ok && port != 0 {
					if lbc.clusterManager == nil {
						glog.Infof("In test mode, would've created backend for port %v", port)
						continue
					}
					backend, err := lbc.clusterManager.Backend(int64(port))
					if err != nil {
						return err
					}
					lbc.backends[port] = backend
				}
			}
		}
	}
	return nil
}

// syncPathMap updates gce's urlmap according to the kubernetes pathmap.
// This method assumes the appropriate backends already exist.
func (lbc *loadBalancerController) syncPathMap(pm api.PathMap) error {

	// Convert the pathmap from a subdomain: {path: kubernetes service} mapping
	// to a subdomain: {path: gce backend for kubernetes service} mapping.
	subdomainToUrlBackend := map[string]map[string]*compute.BackendService{}
	for subdomain, urlMap := range pm.Spec.PathMap {
		urlToBackend := map[string]*compute.BackendService{}
		for endpoint, svcRef := range urlMap {
			port := svcRef.Port.NodePort
			if port == 0 {
				glog.Errorf("Ignoring path map %v, service %v doesn't have nodePort",
					pm.Name, svcRef.Service.Name)
				continue
			}
			if portBackend, ok := lbc.backends[port]; !ok {
				return fmt.Errorf("No backend for pathmap %v, port %v", pm.Name, port)
			} else {
				urlToBackend[endpoint] = portBackend
			}
		}
		subdomainToUrlBackend[subdomain] = urlToBackend
	}

	// Get the loadbalancer for the given pathmap and update its urls.
	key, err := keyFunc(&pm)
	if err != nil {
		return err
	}
	l, lbExists := lbc.loadBalancers[key]
	if !lbExists {
		l = lbc.clusterManager.NewL7(pm.Name)
		if len(l.Errors) != 0 {
			return fmt.Errorf("%+v", l.Errors)
		} else {
			lbc.loadBalancers[key] = l
		}
		lbc.recorder.Eventf(&pm, "Created", "Created loadbalancer: %v", l.GetIP())
	}
	if err := l.UpdateUrlMap(subdomainToUrlBackend); err != nil {
		return err
	}

	paths := []string{}
	for subdomain, urlToBackend := range subdomainToUrlBackend {
		for path, _ := range urlToBackend {
			paths = append(paths, path)
		}
		lbc.recorder.Eventf(&pm, "Updated",
			"Updated loadbalancer: %v subdomain %v with paths: %v",
			l.GetIP(), subdomain, strings.Join(paths, ","))
	}
	return nil
}

// sync manages the syncing of backends and pathmaps.
func (lbc *loadBalancerController) sync(key string) {
	obj, pmExists, err := lbc.pmLister.Store.GetByKey(key)
	if err != nil {
		glog.Errorf("requeuing %v: %v", key, err)
		// TODO: don't break the enqueue abstraction
		lbc.pmQueue.queue.Add(key)
		return
	}

	// Cleaning cluster resources of deleted pathmaps involves
	// deleting forwarding rules, proxies and urlmaps but not
	// backends. Backends are freed when no pathmaps reference
	// them, in syncBackends.
	if !pmExists {
		glog.Infof("Pathmap %v deleted", key)
		l, lbExists := lbc.loadBalancers[key]
		if !lbExists {
			glog.Infof("Pathmap %v doesn't have loadbalancer", key)
		} else if err := l.Cleanup(); err != nil {
			glog.Errorf("requeuing %v: %v", key, err)
			lbc.pmQueue.queue.Add(key)
			return
		}
		delete(lbc.loadBalancers, key)
	}

	// Backends are shared across loadbalancers for efficiency. Syncing of
	// backends should happen after deleting cluster resources of deleted
	// pathmaps (because the reference graph is private loadbalancer
	// resources -> backends -> instance group), but before we create
	// cluster resources for new pathmaps.
	if err := lbc.syncBackends(); err != nil {
		glog.Errorf("requeuing %v: %v", key, err)
		lbc.pmQueue.enqueue(obj)
		return
	}
	if lbc.clusterManager == nil || !pmExists {
		return
	}

	// Pathmap syncing involves updating the urlmap associated with a
	// forwarding rule for a loadbalancer, with the backends created
	// in syncBackends.
	if err := lbc.syncPathMap(*obj.(*api.PathMap)); err != nil {
		glog.Errorf("requeuing %v: %v", key, err)
		lbc.pmQueue.enqueue(obj)
		return
	}
	glog.Infof("Finished syncing %v", key)
}

// syncNodes manages the syncing of kubernetes nodes to gce instance groups.
// The instancegroups are referenced by loadbalancer backends.
func (lbc *loadBalancerController) syncNodes(key string) {
	obj, nodeExists, err := lbc.nodeLister.Store.GetByKey(key)
	if err != nil {
		glog.Errorf("requeuing %v: %v", key, err)
		lbc.nodeQueue.queue.Add(key)
		return
	}
	if !nodeExists {
		glog.Infof("Node %v deleted", key)
		return
	}

	// Add/Delete nodes to cluster instance group using kubernetes
	// nodes as the source of truth.
	oldNodes, err := lbc.clusterManager.GetNodes()
	if err != nil {
		lbc.nodeQueue.enqueue(obj)
	}
	curNodes, err := lbc.nodeLister.List()
	if err != nil {
		lbc.nodeQueue.enqueue(obj)
	}
	// TODO: delete unhealthy kubernetes nodes from cluster?
	deleteNodes := []string{}
	addNodes := []string{}
	for _, n := range curNodes.Items {
		if !oldNodes.Has(n.Name) {
			deleteNodes = append(deleteNodes, n.Name)
		} else {
			addNodes = append(addNodes, n.Name)
		}
	}
	if err := lbc.clusterManager.AddNodes(addNodes); err != nil {
		glog.Infof("Failed to add nodes %v to cluster instance group, requeuing", addNodes)
		// TODO: Try to delete anyway?
		lbc.nodeQueue.enqueue(obj)
		return
	}
	if err := lbc.clusterManager.RemoveNodes(deleteNodes); err != nil {
		glog.Infof("Failed to delete nodes %v from cluster instance group, requeuing", deleteNodes)
		lbc.nodeQueue.enqueue(obj)
		return
	}
}

func getPathsToNodePort(pathMaps api.PathMapList, port int) []api.PathMap {
	pms := []api.PathMap{}
	// get a flat list of pathmaps that have the given nodeport.
	for _, pm := range pathMaps.Items {
		for _, urlMap := range pm.Spec.PathMap {
			for _, svcRef := range urlMap {
				if svcRef.Port.NodePort == port {
					pms = append(pms, pm)
					break
				}
			}
		}
	}
	return pms
}

func getNodeNames(client *client.Client) (nodes []string, err error) {
	nodeList, err := client.Nodes().List(labels.Everything(), fields.Everything())
	if err != nil {
		return
	}
	for i := range nodeList.Items {
		nodes = append(nodes, nodeList.Items[i].Name)
	}
	return
}
