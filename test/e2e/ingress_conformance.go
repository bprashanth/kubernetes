/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package e2e

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/apis/extensions"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/fields"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/util/wait"
	utilyaml "k8s.io/kubernetes/pkg/util/yaml"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// Conformance suite for Ingress Controllers. This test does the following:
// * Starts an Ingress controller
// * Creates some frontend services, rcs and ingresses
// * Retrieves a list of frontend services
// * Retrieves a list of Ingress paths to each service
// * Run some standard HTTP tests against each path
// * Rinse and repeat for next controller
//
// Functionality exercised by this conformance suite should serve as a
// baseline for Ingress controllers, outlining implicit user expectations
// when they create an Ingress. It includes the following tests:
//
// * GET: Performs a simple HTTP GET against all Service routes returned by the
//   IngressControllerTest, for each Service returned by the IngressTest. Polls
//   till the GET returns 200 response, or a timeout is hit. Uses the `Host`
//   field of the Ingress as the Host header of the GET, if any is specified.
//
// * RegExp paths: This is currently tested inline with GET. If the lookuper
//   notices a url with a regex in the Ingress, it inserts the right full
//   endpoint such that the GET test would fail if the loadbalancer didn't
//   properly handle the regexp rule.
//
// * Readiness: Flips all endpoints to a Not Ready state, confirms that they
//   are unreachable via the loadbalancer, flips them all to Ready, confirms
//   the loadbalancer brings them back online.
//
// * Add/Delete Services: Doesn't need an explicity test, it happens when the
//   main loop of the test invokes start/stop methods on the IngressTester.
//   In addition to this, each IngressControllerTester should fail if resources
//   are leaked after the final deletion of all Ingress/Services/RCs etc.
//
// * Default Backend: Make requests against non-existent urls/hosts and confirm
//   the the loadbalancer serves a user defined 404 page (This test is TODO).
//
// In the future this list will include HTTPS tests.
//
// HOWTO:
// * Add a new HTTPTest that runs against all controllers
//   - Add your implementation of HTTPTest to conformanceTests.
// * Add HTTPTest to a single controller
//   - Add the test to that controller's private test list.
// * Add a new Ingress for testing all existing controllers
//   - Add your implementation of IngressTester to ingressTests.
// * Add a new controller that is tested against existing Ingress and HTTPTest
//	 - Add your implementation of IngressControllerTester to controllerTests.

const (
	// Cloud loadbalancers have a large one time starup cost (~6m on GCE)
	lbHealthCheckTimeout = 15 * time.Minute
	lbPollInterval       = 30 * time.Second
	// The kubelet checks readiness ~5-10s.
	readinessPollInterval = 5 * time.Second
	// default host port of Service endpoints used in readiness tests.
	readinessHostPort = 8080
	// If true, run all tests in the default namespace. Used to debug locally.
	runInDefaultNs = true
)

// Tests

// controllerTests returns a list of IngressControllerTesters.
func controllerTests(repoRoot string, client *client.Client) []IngressControllerTester {
	return []IngressControllerTester{
		&haproxyControllerTest{
			name:            "haproxy",
			cfg:             filepath.Join(repoRoot, "test", "e2e", "testing-manifests", "haproxy", "rc.yaml"),
			client:          client,
			address:         []string{},
			privateTestList: []HTTPTest{},
		},
		&gceControllerTest{
			name:            "glbc",
			cfg:             filepath.Join(repoRoot, "test", "e2e", "testing-manifests", "glbc", "rc.yaml"),
			defaultSvc:      filepath.Join(repoRoot, "test", "e2e", "testing-manifests", "glbc", "default-svc.yaml"),
			defaultSvcRc:    filepath.Join(repoRoot, "test", "e2e", "testing-manifests", "glbc", "default-svc-rc.yaml"),
			client:          client,
			privateTestList: []HTTPTest{},
		},
	}
}

// ingressTests returns a list of IngressTesters.
func ingressTests(repoRoot string, client *client.Client) []IngressTester {
	return []IngressTester{
		&ingManager{
			name:        "ngninx",
			rcCfgPaths:  []string{filepath.Join(repoRoot, "test", "e2e", "testing-manifests", "nginxtest", "rc.yaml")},
			svcCfgPaths: []string{filepath.Join(repoRoot, "test", "e2e", "testing-manifests", "nginxtest", "svc.yaml")},
			ingCfgPaths: []string{filepath.Join(repoRoot, "test", "e2e", "testing-manifests", "nginxtest", "ingress.yaml")},
			svcNames:    []string{},
			client:      client,
		},
	}
}

// conformanceTests returns a list of tests.
func conformanceTests() []HTTPTest {
	return []HTTPTest{
		GETTest,
		readinessTest,
	}
}

// Interfaces

// IngressControllerTester is a test for an Ingress controller.
type IngressControllerTester interface {
	IngressControllerStartStopper
	Lookuper
	fmt.Stringer
	getPrivateTestList() []HTTPTest
}

// IngressTester is a test for a single Ingress.
type IngressTester interface {
	IngressStartStopper
	fmt.Stringer
	// getServiceNames returns the list of Service names started by this Tester.
	getServiceNames() []string
}

// Lookuper looks up how to route traffic to a given Service from available Ingress.
type Lookuper interface {
	// lookup returns a list of paths associated with the service from all
	// available Ingress at the time of invocation. Each path is tested, eg:
	// with a simple GET.
	lookup(serviceName string) ([]route, error)
}

// IngressControllerStartStopper is an interface used to test Ingress controllers.
type IngressControllerStartStopper interface {
	// start starts the Ingress controller in the given namespace. It could
	// also create additional Services and RCs, for default backends.
	start(namespace string) error
	// stop stops the Ingress controller, and deletes resources not created
	// by an IngressTest, such as default backends. This method is responsible
	// for all cleanup, it should return an error if external resources are
	// leaked after the controller has been deleted.
	stop() error
}

// IngressStartStopper is capable of creating an Ingress from a manifest file.
type IngressStartStopper interface {
	// start should create an Ingress and all associated RCs, Services etc. When
	// it returns, the cluster should be capable of ingress, and the Ingress
	// Controller should understand how to route traffic to a given Service.
	start(namespace string) error
	// stop should delete all resources created by start.
	stop() error
}

// readynessFlipper flips readiness for endpoints backing a path.
type readinessFlipper struct {
	// ready flips all endpoints of a Service into Ready.
	ready func() error
	// unready flips all endpoints of a Service into Not Ready.
	unready func() error
	//TODO: Teach these functions to only flip a subset of endpoints.
}

// HTTPTest is a http test. It isn't aware of Ingress. It takes routes and
// and conducts some L7 tests.
type HTTPTest func(routes []route) error

// route has all the information to access a Service via a path.
type route struct {
	host           string
	path           string
	outputContains string
	// No readiness tests are run if r is nil. It's up to the
	// IngressControllerTester to populate it during lookup().
	r *readinessFlipper
}

func testController(t IngressControllerTester, ns, repoRoot string, client *client.Client) {
	By(fmt.Sprintf("Starting loadbalancer controller %v in namespace %v", t, ns))
	Expect(t.start(ns)).NotTo(HaveOccurred())

	for _, s := range ingressTests(repoRoot, client) {
		By(fmt.Sprintf("Starting ingress manager %v in namespace %v", s, ns))
		Expect(s.start(ns)).NotTo(HaveOccurred())

		for _, sName := range s.getServiceNames() {
			routes, err := t.lookup(sName)
			Expect(err).NotTo(HaveOccurred())

			By(fmt.Sprintf("Testing paths %v for Service %v", routes, sName))
			for _, httpTest := range append(conformanceTests(), t.getPrivateTestList()...) {
				Expect(httpTest(routes)).NotTo(HaveOccurred())
			}
		}

		Expect(s.stop()).NotTo(HaveOccurred())
	}

	Expect(t.stop()).NotTo(HaveOccurred())
}

var _ = Describe("IngressControllers", func() {
	// These variables are initialized after framework's beforeEach.
	var ns string
	var repoRoot string
	var client *client.Client

	framework := Framework{BaseName: "ingress"}

	BeforeEach(func() {
		framework.beforeEach()
		client = framework.Client
		ns = framework.Namespace.Name
		if runInDefaultNs {
			ns = "default"
		}
		repoRoot = testContext.RepoRoot
	})

	AfterEach(func() {
		framework.afterEach()
	})

	It("Ingresshaproxy", func() {
		for _, t := range controllerTests(repoRoot, client) {
			if t.String() == "haproxy" {
				testController(t, ns, repoRoot, client)
			}
		}
	})

	It("Ingressglbc", func() {
		for _, t := range controllerTests(repoRoot, client) {
			if t.String() == "glbc" {
				testController(t, ns, repoRoot, client)
			}
		}
	})
})

// Haproxy LBCTester

type haproxyControllerTest struct {
	client          *client.Client
	cfg             string
	rcName          string
	rcNamespace     string
	name            string
	address         []string
	privateTestList []HTTPTest
}

func (h *haproxyControllerTest) getPrivateTestList() []HTTPTest {
	return h.privateTestList
}

func (h *haproxyControllerTest) String() string {
	return h.name
}

func (h *haproxyControllerTest) start(namespace string) (err error) {

	// Create a replication controller with the given configuration.
	rc := rcFromManifest(h.cfg)
	rc.Namespace = namespace
	rc.Spec.Template.Labels["name"] = rc.Name

	// Add the --namespace arg.
	// TODO: Remove this when we have proper namespace support.
	for i, c := range rc.Spec.Template.Spec.Containers {
		rc.Spec.Template.Spec.Containers[i].Args = append(
			c.Args, fmt.Sprintf("--namespace=%v", namespace))
		Logf("Container args %+v", rc.Spec.Template.Spec.Containers[i].Args)
	}

	Logf("Creating rc %+v", rc)
	rc, err = h.client.ReplicationControllers(rc.Namespace).Create(rc)
	if err != nil {
		return
	}
	Logf("Waiting for pods of rc %v", rc.Name)
	if err = waitForRCPodsRunning(h.client, namespace, rc.Name); err != nil {
		return
	}
	h.rcName = rc.Name
	h.rcNamespace = rc.Namespace
	// Find the pods of the rc we just created.
	h.address, err = getPodNodeIPs(
		h.client, h.rcNamespace, map[string]string{"name": h.rcName})
	if err != nil {
		return
	}
	if len(h.address) == 0 {
		return fmt.Errorf("No external ips found for loadbalancer %v", h)
	}
	return nil
}

func (h *haproxyControllerTest) stop() error {
	Logf("Deleting rc %v", h.rcName)
	return DeleteRC(h.client, h.rcNamespace, h.rcName)
}

func (h *haproxyControllerTest) lookup(svcName string) ([]route, error) {
	// The address of a service is the address of the lb/servicename, currently.
	ro := route{
		host:           "",
		path:           fmt.Sprintf("http://%v/%v", h.address[0], svcName),
		outputContains: "",
		r:              getReadinessFlipper(h.client, h.rcNamespace, svcName, readinessHostPort),
	}
	return []route{ro}, nil
}

// GCE LBCTester

type gceControllerTest struct {
	client                *client.Client
	cfg                   string
	defaultSvc            string
	defaultSvcRc          string
	rcName                string
	rcNamespace           string
	name                  string
	defaultBackendManager *ingManager
	privateTestList       []HTTPTest
}

func (g *gceControllerTest) getPrivateTestList() []HTTPTest {
	return g.privateTestList
}

func (g *gceControllerTest) String() string {
	return g.name
}

func (g *gceControllerTest) start(namespace string) (err error) {

	// First start the default service for the controller and get its node port.
	g.defaultBackendManager = &ingManager{
		rcCfgPaths:  []string{g.defaultSvcRc},
		svcCfgPaths: []string{g.defaultSvc},
		client:      g.client,
	}
	Logf("Starting default svc and rc for glbc")
	if err = g.defaultBackendManager.start(namespace); err != nil {
		return
	}
	var nodePort int
	if nodePort, err = getSvcNodePort(
		g.client, namespace, g.defaultBackendManager.svcNames[0], 80); err != nil {
		return
	}
	Logf("Default backend for glbc has node port %v", nodePort)

	// Create a replication controller for glbc, plug in the default node
	// port from above.
	rc := rcFromManifest(g.cfg)
	rc.Namespace = namespace
	rc.Spec.Template.Labels["name"] = rc.Name

	rc.Spec.Template.Spec.Containers[0].Args = append(
		rc.Spec.Template.Spec.Containers[0].Args,
		fmt.Sprintf("--default-backend-node-port=%v", nodePort))

	Logf("Starting glbc pod, container args %+v",
		rc.Spec.Template.Spec.Containers[0].Args)

	rc, err = g.client.ReplicationControllers(rc.Namespace).Create(rc)
	if err != nil {
		return
	}
	if err = waitForRCPodsRunning(g.client, namespace, rc.Name); err != nil {
		return
	}
	g.rcName = rc.Name
	g.rcNamespace = rc.Namespace
	return
}

func (g *gceControllerTest) stop() (err error) {
	// Deleting the rc will take a while because the pod has a long
	// terminationGracePeriod.
	if err = DeleteRC(g.client, g.rcNamespace, g.rcName); err != nil {
		return
	}
	Logf("Deleting default backend manager")
	return g.defaultBackendManager.stop()
}

// lookup returns a list of fully formed urls that can be used to access the
// given service.
func (g *gceControllerTest) lookup(svcName string) ([]route, error) {
	routes := []route{}
	ings, err := g.client.Extensions().Ingress(g.rcNamespace).List(
		labels.Everything(), fields.Everything())
	if err != nil {
		return routes, err
	}

	for _, ing := range ings.Items {
		start := time.Now()
		// Waiting here even if the Ingress doesn't match works out because the
		// cost is amortized. If you created an Ingress you probably want its
		// IP sometime.
		address, err := waitForIngressAddress(g.client, ing.Namespace, ing.Name, 5*time.Minute)
		if err != nil {
			return []route{}, err
		}
		Logf("Found address %v for ingress %v, took %v to come online",
			address, ing.Name, time.Since(start))

		for _, rules := range ing.Spec.Rules {
			if rules.IngressRuleValue.HTTP == nil {
				continue
			}
			route := route{host: rules.Host}
			for _, p := range rules.IngressRuleValue.HTTP.Paths {
				if p.Backend.ServiceName == svcName {
					url := p.Path
					// By default test pods should echo hostname, which should
					// contain the name of the Service.
					// TODO: This feels magical, replace it with something explicit.
					outputContains := svcName

					// TODO: Handle regex better.
					if strings.Contains(url, "*") {
						url = strings.Replace(url, "*", "args?foo=bar", -1)
						outputContains = "bar"
					}
					route.path = fmt.Sprintf("http://%v%v", address, url)
					route.outputContains = outputContains
					route.r = getReadinessFlipper(
						g.client, g.rcNamespace, svcName, readinessHostPort)
					routes = append(routes, route)
				}
			}
		}
	}
	return routes, nil
}

// HTTPTest implementations

// GETTest just performs a get on each route.
func GETTest(routes []route) error {
	for _, route := range routes {
		url := fmt.Sprintf("%v", route.path)

		Logf("Testing route %v with simple GET", route)
		start := time.Now()

		var lastBody string
		pollErr := wait.Poll(lbPollInterval, lbHealthCheckTimeout, func() (bool, error) {
			var err error
			lastBody, err = simpleGET(http.DefaultClient, url, route.host)
			if err != nil {
				Logf("%v: %v", url, err)
				return false, nil
			}
			return true, nil
		})
		Logf("Last response body for %v:\n%v", url, lastBody)
		if pollErr != nil {
			return pollErr
		}
		Logf("Url %v took %v to come online", url, time.Since(start))
		if route.outputContains != "" && !strings.Contains(
			lastBody, route.outputContains) {
			return fmt.Errorf("Body expected to contain: %v, but has %v",
				route.outputContains, lastBody)
		}
	}
	return nil
}

// readinessTest flips endpoints to unready, makes sure requests fail, flips
// them to ready, makes sure request pass.
func readinessTest(routes []route) error {
	for _, route := range routes {
		url := fmt.Sprintf("%v", route.path)

		if route.r == nil {
			Logf("Route %+v has not readinessFlipper, skipping", route)
			continue
		}
		Logf("Testing route %+v for readiness", route)

		// Wait till all endpoints are removed from the Service.
		if err := route.r.unready(); err != nil {
			return fmt.Errorf("Could not flip endpoints to unready: %v", err)
		}
		Logf("Url %v should fail GET", url)

		if pollErr := wait.Poll(readinessPollInterval, lbHealthCheckTimeout, func() (bool, error) {
			if body, err := simpleGET(http.DefaultClient, url, route.host); err == nil {
				Logf("Passed GET with url %v, body %v: expected failure since endpoints not ready", body, url)
				return false, nil
			}
			return true, nil
		}); pollErr != nil {
			return fmt.Errorf(
				"Not-Ready endpoints continue responding on %v: %v", url, pollErr)
		}

		// Wait till at least one endpoint is up.
		if err := route.r.ready(); err != nil {
			return fmt.Errorf("Could not flip endpoints to ready: %v", err)
		}
		// Poll till we can reach the Service through the loadbalancer.
		Logf("Url %v, host %v should respond to GET", url, route.host)

		if pollErr := wait.Poll(readinessPollInterval, lbHealthCheckTimeout, func() (bool, error) {
			if body, err := simpleGET(http.DefaultClient, url, route.host); err != nil {
				Logf("Failed GET with url %v: %v, body %v, though at least one endpoint is ready",
					url, err, body)
				return false, nil
			}
			return true, nil
		}); pollErr != nil {
			return fmt.Errorf(
				"Ready endpoints did not respond on %v: %v", url, pollErr)
		}
	}
	return nil
}

// ingManager implements IngressTester

type ingManager struct {
	rcCfgPaths  []string
	svcCfgPaths []string
	ingCfgPaths []string
	name        string
	namespace   string
	client      *client.Client
	svcNames    []string
	rcNames     []string
	ingNames    []string
}

func (s *ingManager) String() string {
	return s.name
}

func (s *ingManager) start(namespace string) (err error) {
	// Create rcs
	for _, rcPath := range s.rcCfgPaths {
		rc := rcFromManifest(rcPath)
		rc.Namespace = namespace
		rc.Spec.Template.Labels["name"] = rc.Name
		rc, err = s.client.ReplicationControllers(rc.Namespace).Create(rc)
		if err != nil {
			return
		}
		if err = waitForRCPodsRunning(s.client, rc.Namespace, rc.Name); err != nil {
			return
		}
		s.rcNames = append(s.rcNames, rc.Name)
	}
	// Create services.
	// Note that it's up to the caller to make sure the service actually matches
	// the pods of the rc.
	for _, svcPath := range s.svcCfgPaths {
		svc := svcFromManifest(svcPath)
		svc.Namespace = namespace
		svc, err = s.client.Services(svc.Namespace).Create(svc)
		if err != nil {
			return
		}
		s.svcNames = append(s.svcNames, svc.Name)
	}
	// Create Ingress.
	for _, ingPath := range s.ingCfgPaths {
		ing := ingFromManifest(ingPath)
		ing.Namespace = namespace
		ing, err = s.client.Extensions().Ingress(ing.Namespace).Create(ing)
		if err != nil {
			return
		}
		s.ingNames = append(s.ingNames, ing.Name)
	}
	s.namespace = namespace
	return nil
}

func (s *ingManager) getServiceNames() []string {
	return s.svcNames
}

func (s *ingManager) stop() error {
	// We can't just rely on the ns deletion to nuke resources because we
	// test multiple loadbalancers in the same e2e, so each one has to
	// cleanup after itself.
	for _, name := range s.ingNames {
		Logf("Deleting Ingress %v", name)
		if err := s.client.Extensions().Ingress(
			s.namespace).Delete(name, nil); err != nil {
			Logf("Error during stop: %v", err)
		}
	}
	for _, name := range s.svcNames {
		Logf("Deleting Svc %v", name)
		if err := s.client.Services(s.namespace).Delete(name); err != nil {
			Logf("Error during stop: %v", err)
		}
	}
	for _, name := range s.rcNames {
		Logf("Deleting RC %v", name)
		if err := DeleteRC(s.client, s.namespace, name); err != nil {
			Logf("Error during stop: %v", err)
		}
	}
	return nil
}

// Helper functions

// getReadinessFlipper returns a readinessFlipper for the given svcUrl. The
// readinessFlipper returned by this function can only flip endpoints that export
// a `/ready?status=httpcode` endpoint. hostPort is the host port of the endpoints
// can use to toggle readiness, since the endpoint will not be part of the service
// once it fails a readiness check.
func getReadinessFlipper(c *client.Client, svcNs, svcName string, hostPort int) *readinessFlipper {
	return &readinessFlipper{
		ready: func() error {
			svc, err := c.Services(svcNs).Get(svcName)
			if err != nil {
				return err
			}
			startEps, err := getPodNodeIPs(c, svcNs, svc.Spec.Selector)
			if err != nil {
				return err
			}
			readyUrls := []string{}
			for _, ep := range startEps {
				readyUrls = append(
					readyUrls, fmt.Sprintf("http://%v:%d/ready?status=200", ep, hostPort))
			}

			Logf("Polling on readiness urls: %+v", readyUrls)
			return wait.Poll(lbPollInterval, lbHealthCheckTimeout, func() (bool, error) {
				// Hit all the ready?status=200 urls at least once
				for _, readyUrl := range readyUrls {
					body, err := simpleGET(
						http.DefaultClient, readyUrl, "")
					if err != nil {
						// Don't waste time here if we don't have a /ready endpoint.
						return false, err
					}
					Logf("Readiness status of %v, %v", readyUrl, body)
				}
				eps, err := getReadyEndpoints(c, svcNs, svcName)
				if err != nil {
					Logf("Error retrieving endpoints %v", err)
					return false, nil
				}
				// Wait till at least the number of endpoints we started with come up.
				if len(eps) == len(startEps) {
					return true, nil
				}
				Logf("Waiting for current ready endpoints: %v, and original set %v to converge", eps, startEps)
				return false, nil
			})
		},
		unready: func() error {
			svc, err := c.Services(svcNs).Get(svcName)
			if err != nil {
				return err
			}
			startEps, err := getPodNodeIPs(c, svcNs, svc.Spec.Selector)
			if err != nil {
				return err
			}
			readyUrls := []string{}
			for _, ep := range startEps {
				readyUrls = append(
					readyUrls, fmt.Sprintf("http://%v:%d/ready?status=500", ep, hostPort))
			}

			Logf("Polling on readiness urls: %+v", readyUrls)
			return wait.Poll(lbPollInterval, lbHealthCheckTimeout, func() (bool, error) {
				// Hit all the ready?status=500 urls at least once
				for _, readyUrl := range readyUrls {
					body, err := simpleGET(
						http.DefaultClient, readyUrl, "")
					if err != nil {
						// Don't waste time here if we don't have a /ready endpoint.
						return false, err
					}
					Logf("Readiness status of %v, %v", readyUrl, body)
				}
				eps, err := getReadyEndpoints(c, svcNs, svcName)
				if err != nil {
					Logf("Error retrieving endpoints %v", err)
					return false, nil
				}
				// Wait till the Service has no endpoints.
				if len(eps) == 0 {
					return true, nil
				}
				Logf("Waiting for %v to fail readiness probe", eps)
				return false, nil
			})
		},
	}
}

// simpleGET executes a get on the given url, returns error if non-200 returned.
func simpleGET(c *http.Client, url string, host string) (string, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Host = host
	res, err := c.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	rawBody, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return "", err
	}
	body := string(rawBody)
	if res.StatusCode != http.StatusOK {
		err = fmt.Errorf(
			"GET returned http error %v", res.StatusCode)
	}
	return body, err
}

// rcFromManifest reads a .json/yaml file and returns the rc in it.
func rcFromManifest(fileName string) *api.ReplicationController {
	var controller api.ReplicationController
	Logf("Parsing rc from %v", fileName)
	data, err := ioutil.ReadFile(fileName)
	Expect(err).NotTo(HaveOccurred())

	json, err := utilyaml.ToJSON(data)
	Expect(err).NotTo(HaveOccurred())

	Expect(api.Scheme.DecodeInto(json, &controller)).NotTo(HaveOccurred())
	return &controller
}

// svcFromManifest reads a .json/yaml file and returns the rc in it.
func svcFromManifest(fileName string) *api.Service {
	var svc api.Service
	Logf("Parsing service from %v", fileName)
	data, err := ioutil.ReadFile(fileName)
	Expect(err).NotTo(HaveOccurred())

	json, err := utilyaml.ToJSON(data)
	Expect(err).NotTo(HaveOccurred())

	Expect(api.Scheme.DecodeInto(json, &svc)).NotTo(HaveOccurred())
	return &svc
}

// ingFromManifest reads a .json/yaml file and returns the rc in it.
func ingFromManifest(fileName string) *extensions.Ingress {
	var ing extensions.Ingress
	Logf("Parsing ingress from %v", fileName)
	data, err := ioutil.ReadFile(fileName)
	Expect(err).NotTo(HaveOccurred())

	json, err := utilyaml.ToJSON(data)
	Expect(err).NotTo(HaveOccurred())

	Expect(api.Scheme.DecodeInto(json, &ing)).NotTo(HaveOccurred())
	return &ing
}
