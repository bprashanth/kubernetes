/*
Copyright 2017 The Kubernetes Authors.
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

package upgrades

import (
	. "github.com/onsi/ginkgo"
	"net/http"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
)

// IngressUpgradeTest adapts the Ingress e2e for upgrade testing
type IngressUpgradeTest struct {
	gceController *framework.GCEIngressController
	jig           *framework.IngressTestJig
	httpClient    *http.Client
	ip            string
}

func (t *IngressUpgradeTest) Setup(f *framework.Framework) {
	// jig handles all Kubernetes testing logic
	jig := framework.NewIngressTestJig(f.ClientSet)

	// gceController handles all cloud testing logic
	gceController := &framework.GCEIngressController{
		Ns:     f.Namespace.Name,
		Client: jig.Client,
		Cloud:  framework.TestContext.CloudConfig,
	}

	t.gceController = gceController
	t.jig = jig
	t.httpClient = framework.BuildInsecureClient(framework.IngressReqTimeout)

	// Allocate a static-ip for the Ingress, this IP is cleaned up via CleanupGCE
	t.ip = t.gceController.CreateStaticIP(ns)

	// Create a working basic Ingress
	By(fmt.Sprintf("allocated static ip %v: %v through the GCE cloud provider", ns, t.ip))
	jig.CreateIngress(filepath.Join(framework.IngressManifestPath, "static-ip"), ns, map[string]string{
		"kubernetes.io/ingress.global-static-ip-name": ns,
		"kubernetes.io/ingress.allow-http":            "false",
	})

	By("waiting for Ingress to come up with ip: " + t.ip)
	framework.ExpectNoError(framework.PollURL(fmt.Sprintf("https://%v/", t.ip), "", framework.LoadBalancerPollTimeout, jig.PollInterval, httpClient, false))
}

func (t *IngressUpgradeTest) Test(f *framework.Framework, done <-chan struct{}, upgrade UpgradeType) {
	switch upgrade {
	case MasterUpgrade:
		// Restarting the ingress controller shouldn't disrupt a steady state
		// Ingress. Restarting the ingress controller and deleting ingresses
		// while it's down will leak cloud resources, because the ingress
		// controller doesn't checkpoint to disk.
		t.verify(f, done, true)
	default:
		// Currently ingress gets disrupted across node upgrade, because endpoints
		// get killed and we don't have any guarantees that 2 nodes don't overlap
		// their upgrades (even on cloud platforms like GCE, because VM level
		// rolling upgrades are not Kubernets aware).
		t.verify(f, done, false)
	}
}

func (t *IngressUpgradeTest) Teardown(f *framework.Framework) {
	if CurrentGinkgoTestDescription().Failed {
		framework.DescribeIng(ns)
	}
	if t.jig.Ingress == nil {
		By("No ingress created, no cleanup necessary")
		return
	}
	By("Deleting ingress")
	t.jig.DeleteIngress()

	By("Cleaning up cloud resources")
	framework.CleanupGCE(t.gceController)
}

func (t *IngressUpgradeTest) verify(f *framework.Framework, done <-chan struct{}, testDuringDisruption bool) {
	if testDuringDisruption {
		By("continuously hitting the Ingress IP")
		framework.ExpectNoError(framework.PollURL(fmt.Sprintf("https://%v/", t.ip), "", framework.LoadBalancerPollTimeout, jig.PollInterval, t.httpClient, false))
	} else {
		By("waiting for upgrade to finish without checking if Ingress remains up")
		<-done
	}
	By("hitting the Ingress IP " + t.ip)
	framework.ExpectNoError(framework.PollURL(fmt.Sprintf("https://%v/", t.ip), "", framework.LoadBalancerPollTimeout, jig.PollInterval, t.httpClient, false))
}

func (t *IngressUpgradeTest) restart() {

}