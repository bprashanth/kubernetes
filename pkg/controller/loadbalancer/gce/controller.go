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
	"reflect"
	"strings"
	"time"

	compute "google.golang.org/api/compute/v1"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client"
	"k8s.io/kubernetes/pkg/client/cache"
	"k8s.io/kubernetes/pkg/client/record"
	"k8s.io/kubernetes/pkg/controller/framework"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util"
	"k8s.io/kubernetes/pkg/util/workqueue"

	"github.com/golang/glog"
)

var (
	keyFunc          = framework.DeletionHandlingMetaNamespaceKeyFunc
	resyncPeriod     = 60 * time.Second
	lbControllerName = "lbcontroller"
)

// loadBalancerController watches the kubernetes api and adds/removes services
// from the loadbalancer, via loadBalancerConfig.
type loadBalancerController struct {
	queue          *workqueue.Type
	client         *client.Client
	pmController   *framework.Controller
	svcController  *framework.Controller
	pmLister       cache.StoreToPathMapLister
	clusterManager *ClusterManager
	loadBalancers  map[string]*L7
	backends       map[int]*compute.BackendService
	recorder       record.EventRecorder
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
		clusterManager, err = NewClusterManager(nodes, lbControllerName)
		if err != nil {
			return nil, err
		}
	}
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(kubeClient.Events(""))

	lbc := loadBalancerController{
		queue:          workqueue.New(),
		client:         kubeClient,
		loadBalancers:  map[string]*L7{},
		backends:       map[int]*compute.BackendService{},
		clusterManager: clusterManager,
		recorder:       eventBroadcaster.NewRecorder(api.EventSource{Component: "loadbalancer-controller"}),
	}

	pathHandlers := framework.ResourceEventHandlerFuncs{
		AddFunc:    lbc.enqueue,
		DeleteFunc: lbc.enqueue,
		UpdateFunc: func(old, cur interface{}) {
			if !reflect.DeepEqual(old, cur) {
				lbc.enqueue(cur)
			}
		},
	}
	lbc.pmLister.Store, lbc.pmController = framework.NewInformer(
		cache.NewListWatchFromClient(
			lbc.client, "pathMaps", api.NamespaceAll, fields.Everything()),
		&api.PathMap{}, resyncPeriod, pathHandlers)
	glog.Infof("Created new loadbalancer controller")
	return &lbc, nil
}

func (lbc *loadBalancerController) Run() {
	go lbc.pmController.Run(util.NeverStop)
	glog.Infof("Starting loadbalancer controller")
	util.Until(lbc.worker, time.Second, util.NeverStop)
}

func (lbc *loadBalancerController) enqueue(obj interface{}) {
	key, err := keyFunc(obj)
	if err != nil {
		glog.Infof("Couldn't get key for object %+v: %v", obj, err)
		return
	}
	lbc.queue.Add(key)
}

func (lbc *loadBalancerController) worker() {
	for {
		key, _ := lbc.queue.Get()
		glog.Infof("Sync triggered by pathmap %v", key)
		lbc.sync(key.(string))
		lbc.queue.Done(key)
	}
}

// syncBackends deletes GCE backends without any paths, and creates backends for new paths.
func (lbc *loadBalancerController) syncBackends() error {

	paths, err := lbc.pmLister.List()
	if err != nil {
		return err
	}
	// gc backends without a path
	for port, backend := range lbc.backends {
		if len(getPathsToNodePort(paths, port)) == 0 {
			glog.Infof("No paths found for port %v, deleting backend %v", port, backend.Name)
		}
	}
	// create backends with a new path
	for _, pm := range paths.Items {
		for _, svcRef := range pm.Spec.PathMap {
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
	return nil
}

func (lbc *loadBalancerController) sync(key string) {
	obj, exists, err := lbc.pmLister.Store.GetByKey(key)
	if err != nil {
		glog.Infof("Error syncing pathmap %v, requeuing %v", err, key)
	}
	glog.Infof("Syncing backends, update key %v", key)
	if err := lbc.syncBackends(); err != nil {
		glog.Errorf("Error in syncing backends for pathmap %v, requeuing: %v", key, err)
		lbc.enqueue(obj)
		return
	}
	if !exists {
		glog.Infof("Pathmap %v has been deleted", key)
		return
	}
	if lbc.clusterManager == nil {
		glog.Infof("In test mode, not creating loadbalancer, received update for %v", key)
		return
	}

	pm := *obj.(*api.PathMap)
	urlToBackend := map[string]*compute.BackendService{}
	for endpoint, svcRef := range pm.Spec.PathMap {
		port := svcRef.Port.NodePort
		if port == 0 {
			glog.Errorf("Ignoring path map %v, service %v doesn't have nodePort",
				pm.Name, svcRef.Service.Name)
			continue
		}
		urlToBackend[endpoint] = lbc.backends[port]
	}

	l, ok := lbc.loadBalancers[pm.Name]
	if !ok {
		l = lbc.clusterManager.NewL7(pm.Name)
		if len(l.Errors) != 0 {
			glog.Infof("Error updating urlmap %v: %v", pm.Name, err)
			lbc.enqueue(pm)
		} else {
			lbc.loadBalancers[pm.Name] = l
		}
		lbc.recorder.Eventf(&pm, "Created", "Created loadbalancer: %v", l.GetIP())
	}
	l.UpdateUrlMap(pm.Spec.Host, urlToBackend)

	paths := []string{}
	for path, _ := range urlToBackend {
		paths = append(paths, path)
	}
	lbc.recorder.Eventf(&pm, "Updated", "Updated loadbalancer: %v with paths: %v", l.GetIP(), strings.Join(paths, ","))
}

func getPathsToNodePort(pathMaps api.PathMapList, port int) []api.PathMap {
	pms := []api.PathMap{}
	for _, pm := range pathMaps.Items {
		for _, svcRef := range pm.Spec.PathMap {
			if svcRef.Port.NodePort == port {
				pms = append(pms, pm)
				break
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
