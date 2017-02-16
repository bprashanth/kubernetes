/*
Copyright 2015 The Kubernetes Authors.

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

package framework

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"io/ioutil"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/v1"

	compute "google.golang.org/api/compute/v1"
	"google.golang.org/api/googleapi"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	utilnet "k8s.io/apimachinery/pkg/util/net"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	extensions "k8s.io/kubernetes/pkg/apis/extensions/v1beta1"
	"k8s.io/kubernetes/pkg/client/clientset_generated/clientset"
	gcecloud "k8s.io/kubernetes/pkg/cloudprovider/providers/gce"
	utilexec "k8s.io/kubernetes/pkg/util/exec"
	testutils "k8s.io/kubernetes/test/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

const (
	rsaBits  = 2048
	validFor = 365 * 24 * time.Hour

	// Ingress class annotation defined in ingress repository.
	ingressClass = "kubernetes.io/ingress.class"

	// all cloud resources created by the ingress controller start with this
	// prefix.
	k8sPrefix = "k8s-"

	// clusterDelimiter is the delimiter used by the ingress controller
	// to split uid from other naming/metadata.
	clusterDelimiter = "--"

	// Name of the default http backend service
	defaultBackendName = "default-http-backend"

	// IP src range from which the GCE L7 performs health checks.
	GCEL7SrcRange = "130.211.0.0/22"

	// Cloud resources created by the ingress controller older than this
	// are automatically purged to prevent running out of quota.
	// TODO(37335): write soak tests and bump this up to a week.
	maxAge = -48 * time.Hour

	// parent path to yaml test manifests.
	IngressManifestPath = "test/e2e/testing-manifests/ingress"

	// timeout on a single http request.
	IngressReqTimeout = 10 * time.Second

	// healthz port used to verify glbc restarted correctly on the master.
	glbcHealthzPort = 8086

	// General cloud resource poll timeout (eg: create static ip, firewall etc)
	cloudResourcePollTimeout = 5 * time.Minute

	// Name of the config-map and key the ingress controller stores its uid in.
	uidConfigMap = "ingress-uid"
	uidKey       = "uid"

	// GCE only allows names < 64 characters, and the loadbalancer controller inserts
	// a single character of padding.
	nameLenLimit = 62
)

type IngressTestJig struct {
	Client  clientset.Interface
	RootCAs map[string][]byte
	Address string
	Ingress *extensions.Ingress
	// class is the value of the annotation keyed under
	// `kubernetes.io/ingress.class`. It's added to all ingresses created by
	// this jig.
	Class string

	// The interval used to poll urls
	PollInterval time.Duration
}

type IngressConformanceTests struct {
	EntryLog string
	Execute  func()
	ExitLog  string
}

func CreateIngressComformanceTests(jig *IngressTestJig, ns string) []IngressConformanceTests {
	manifestPath := filepath.Join(IngressManifestPath, "http")
	// These constants match the manifests used in IngressManifestPath
	tlsHost := "foo.bar.com"
	tlsSecretName := "foo"
	updatedTLSHost := "foobar.com"
	updateURLMapHost := "bar.baz.com"
	updateURLMapPath := "/testurl"
	// Platform agnostic list of tests that must be satisfied by all controllers
	return []IngressConformanceTests{
		{
			fmt.Sprintf("should create a basic HTTP ingress"),
			func() { jig.CreateIngress(manifestPath, ns, map[string]string{}) },
			fmt.Sprintf("waiting for urls on basic HTTP ingress"),
		},
		{
			fmt.Sprintf("should terminate TLS for host %v", tlsHost),
			func() { jig.AddHTTPS(tlsSecretName, tlsHost) },
			fmt.Sprintf("waiting for HTTPS updates to reflect in ingress"),
		},
		{
			fmt.Sprintf("should update SSL certificate with modified hostname %v", updatedTLSHost),
			func() {
				jig.Update(func(ing *extensions.Ingress) {
					newRules := []extensions.IngressRule{}
					for _, rule := range ing.Spec.Rules {
						if rule.Host != tlsHost {
							newRules = append(newRules, rule)
							continue
						}
						newRules = append(newRules, extensions.IngressRule{
							Host:             updatedTLSHost,
							IngressRuleValue: rule.IngressRuleValue,
						})
					}
					ing.Spec.Rules = newRules
				})
				jig.AddHTTPS(tlsSecretName, updatedTLSHost)
			},
			fmt.Sprintf("Waiting for updated certificates to accept requests for host %v", updatedTLSHost),
		},
		{
			fmt.Sprintf("should update url map for host %v to expose a single url: %v", updateURLMapHost, updateURLMapPath),
			func() {
				var pathToFail string
				jig.Update(func(ing *extensions.Ingress) {
					newRules := []extensions.IngressRule{}
					for _, rule := range ing.Spec.Rules {
						if rule.Host != updateURLMapHost {
							newRules = append(newRules, rule)
							continue
						}
						existingPath := rule.IngressRuleValue.HTTP.Paths[0]
						pathToFail = existingPath.Path
						newRules = append(newRules, extensions.IngressRule{
							Host: updateURLMapHost,
							IngressRuleValue: extensions.IngressRuleValue{
								HTTP: &extensions.HTTPIngressRuleValue{
									Paths: []extensions.HTTPIngressPath{
										{
											Path:    updateURLMapPath,
											Backend: existingPath.Backend,
										},
									},
								},
							},
						})
					}
					ing.Spec.Rules = newRules
				})
				By("Checking that " + pathToFail + " is not exposed by polling for failure")
				route := fmt.Sprintf("http://%v%v", jig.Address, pathToFail)
				ExpectNoError(PollURL(route, updateURLMapHost, LoadBalancerCleanupTimeout, jig.PollInterval, &http.Client{Timeout: IngressReqTimeout}, true))
			},
			fmt.Sprintf("Waiting for path updates to reflect in L7"),
		},
	}
}

// generateRSACerts generates a basic self signed certificate using a key length
// of rsaBits, valid for validFor time.
func generateRSACerts(host string, isCA bool, keyOut, certOut io.Writer) error {
	if len(host) == 0 {
		return fmt.Errorf("Require a non-empty host for client hello")
	}
	priv, err := rsa.GenerateKey(rand.Reader, rsaBits)
	if err != nil {
		return fmt.Errorf("Failed to generate key: %v", err)
	}
	notBefore := time.Now()
	notAfter := notBefore.Add(validFor)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)

	if err != nil {
		return fmt.Errorf("failed to generate serial number: %s", err)
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:   "default",
			Organization: []string{"Acme Co"},
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	hosts := strings.Split(host, ",")
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	if isCA {
		template.IsCA = true
		template.KeyUsage |= x509.KeyUsageCertSign
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.PublicKey, priv)
	if err != nil {
		return fmt.Errorf("Failed to create certificate: %s", err)
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: derBytes}); err != nil {
		return fmt.Errorf("Failed creating cert: %v", err)
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv)}); err != nil {
		return fmt.Errorf("Failed creating keay: %v", err)
	}
	return nil
}

// buildTransport creates a transport for use in executing HTTPS requests with
// the given certs. Note that the given rootCA must be configured with isCA=true.
func buildTransport(serverName string, rootCA []byte) (*http.Transport, error) {
	pool := x509.NewCertPool()
	ok := pool.AppendCertsFromPEM(rootCA)
	if !ok {
		return nil, fmt.Errorf("Unable to load serverCA.")
	}
	return utilnet.SetTransportDefaults(&http.Transport{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: false,
			ServerName:         serverName,
			RootCAs:            pool,
		},
	}), nil
}

// BuildInsecureClient returns an insecure http client. Can be used for "curl -k".
func BuildInsecureClient(timeout time.Duration) *http.Client {
	t := &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}
	return &http.Client{Timeout: timeout, Transport: utilnet.SetTransportDefaults(t)}
}

// createSecret creates a secret containing TLS certificates for the given Ingress.
// If a secret with the same name already exists in the namespace of the
// Ingress, it's updated.
func createSecret(kubeClient clientset.Interface, ing *extensions.Ingress) (host string, rootCA, privKey []byte, err error) {
	var k, c bytes.Buffer
	tls := ing.Spec.TLS[0]
	host = strings.Join(tls.Hosts, ",")
	Logf("Generating RSA cert for host %v", host)

	if err = generateRSACerts(host, true, &k, &c); err != nil {
		return
	}
	cert := c.Bytes()
	key := k.Bytes()
	secret := &v1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: tls.SecretName,
		},
		Data: map[string][]byte{
			v1.TLSCertKey:       cert,
			v1.TLSPrivateKeyKey: key,
		},
	}
	var s *v1.Secret
	if s, err = kubeClient.Core().Secrets(ing.Namespace).Get(tls.SecretName, metav1.GetOptions{}); err == nil {
		// TODO: Retry the update. We don't really expect anything to conflict though.
		Logf("Updating secret %v in ns %v with hosts %v for ingress %v", secret.Name, secret.Namespace, host, ing.Name)
		s.Data = secret.Data
		_, err = kubeClient.Core().Secrets(ing.Namespace).Update(s)
	} else {
		Logf("Creating secret %v in ns %v with hosts %v for ingress %v", secret.Name, secret.Namespace, host, ing.Name)
		_, err = kubeClient.Core().Secrets(ing.Namespace).Create(secret)
	}
	return host, cert, key, err
}

func CleanupGCE(gceController *GCEIngressController) {
	pollErr := wait.Poll(5*time.Second, LoadBalancerCleanupTimeout, func() (bool, error) {
		if err := gceController.Cleanup(false); err != nil {
			Logf("Still waiting for glbc to cleanup:\n%v", err)
			return false, nil
		}
		return true, nil
	})

	// Static-IP allocated on behalf of the test, never deleted by the
	// controller. Delete this IP only after the controller has had a chance
	// to cleanup or it might interfere with the controller, causing it to
	// throw out confusing events.
	if ipErr := wait.Poll(5*time.Second, LoadBalancerCleanupTimeout, func() (bool, error) {
		if err := gceController.deleteStaticIPs(); err != nil {
			Logf("Failed to delete static-ip: %v\n", err)
			return false, nil
		}
		return true, nil
	}); ipErr != nil {
		// If this is a persistent error, the suite will fail when we run out
		// of quota anyway.
		By(fmt.Sprintf("WARNING: possibly leaked static IP: %v\n", ipErr))
	}

	// Always try to cleanup even if pollErr == nil, because the cleanup
	// routine also purges old leaked resources based on creation timestamp.
	if cleanupErr := gceController.Cleanup(true); cleanupErr != nil {
		By(fmt.Sprintf("WARNING: possibly leaked resources: %v\n", cleanupErr))
	} else {
		By("No resources leaked.")
	}

	// Fail if the controller didn't cleanup
	if pollErr != nil {
		Failf("L7 controller failed to delete all cloud resources on time. %v", pollErr)
	}
}

func (cont *GCEIngressController) deleteForwardingRule(del bool) string {
	msg := ""
	fwList := []compute.ForwardingRule{}
	for _, regex := range []string{fmt.Sprintf("%vfw-.*%v.*", k8sPrefix, clusterDelimiter), fmt.Sprintf("%vfws-.*%v.*", k8sPrefix, clusterDelimiter)} {
		gcloudList("forwarding-rules", regex, cont.Cloud.ProjectID, &fwList)
		if len(fwList) == 0 {
			continue
		}
		for _, f := range fwList {
			if !cont.canDelete(f.Name, f.CreationTimestamp, del) {
				continue
			}
			msg += fmt.Sprintf("%v (forwarding rule)\n", f.Name)
			if del {
				GcloudDelete("forwarding-rules", f.Name, cont.Cloud.ProjectID, "--global")
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) deleteAddresses(del bool) string {
	msg := ""
	ipList := []compute.Address{}
	regex := fmt.Sprintf("%vfw-.*%v.*", k8sPrefix, clusterDelimiter)
	gcloudList("addresses", regex, cont.Cloud.ProjectID, &ipList)
	if len(ipList) != 0 {
		for _, ip := range ipList {
			if !cont.canDelete(ip.Name, ip.CreationTimestamp, del) {
				continue
			}
			msg += fmt.Sprintf("%v (static-ip)\n", ip.Name)
			if del {
				GcloudDelete("addresses", ip.Name, cont.Cloud.ProjectID, "--global")
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) deleteTargetProxy(del bool) string {
	msg := ""
	tpList := []compute.TargetHttpProxy{}
	regex := fmt.Sprintf("%vtp-.*%v.*", k8sPrefix, clusterDelimiter)
	gcloudList("target-http-proxies", regex, cont.Cloud.ProjectID, &tpList)
	if len(tpList) != 0 {
		for _, t := range tpList {
			if !cont.canDelete(t.Name, t.CreationTimestamp, del) {
				continue
			}
			msg += fmt.Sprintf("%v (target-http-proxy)\n", t.Name)
			if del {
				GcloudDelete("target-http-proxies", t.Name, cont.Cloud.ProjectID)
			}
		}
	}
	tpsList := []compute.TargetHttpsProxy{}
	regex = fmt.Sprintf("%vtps-.*%v.*", k8sPrefix, clusterDelimiter)
	gcloudList("target-https-proxies", regex, cont.Cloud.ProjectID, &tpsList)
	if len(tpsList) != 0 {
		for _, t := range tpsList {
			if !cont.canDelete(t.Name, t.CreationTimestamp, del) {
				continue
			}
			msg += fmt.Sprintf("%v (target-https-proxy)\n", t.Name)
			if del {
				GcloudDelete("target-https-proxies", t.Name, cont.Cloud.ProjectID)
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) deleteUrlMap(del bool) (msg string) {
	gceCloud := cont.Cloud.Provider.(*gcecloud.GCECloud)
	umList, err := gceCloud.ListUrlMaps()
	if err != nil {
		if cont.isHTTPErrorCode(err, http.StatusNotFound) {
			return msg
		}
		return fmt.Sprintf("Failed to list url maps: %v", err)
	}
	if len(umList.Items) == 0 {
		return msg
	}
	for _, um := range umList.Items {
		if !cont.canDelete(um.Name, um.CreationTimestamp, del) {
			continue
		}
		msg += fmt.Sprintf("%v (url-map)\n", um.Name)
		if del {
			if err := gceCloud.DeleteUrlMap(um.Name); err != nil &&
				!cont.isHTTPErrorCode(err, http.StatusNotFound) {
				msg += fmt.Sprintf("Failed to delete url map %v\n", um.Name)
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) deleteBackendService(del bool) (msg string) {
	gceCloud := cont.Cloud.Provider.(*gcecloud.GCECloud)
	beList, err := gceCloud.ListBackendServices()
	if err != nil {
		if cont.isHTTPErrorCode(err, http.StatusNotFound) {
			return msg
		}
		return fmt.Sprintf("Failed to list backend services: %v", err)
	}
	if len(beList.Items) == 0 {
		Logf("No backend services found")
		return msg
	}
	for _, be := range beList.Items {
		if !cont.canDelete(be.Name, be.CreationTimestamp, del) {
			continue
		}
		msg += fmt.Sprintf("%v (backend-service)\n", be.Name)
		if del {
			if err := gceCloud.DeleteBackendService(be.Name); err != nil &&
				!cont.isHTTPErrorCode(err, http.StatusNotFound) {
				msg += fmt.Sprintf("Failed to delete backend service %v\n", be.Name)
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) deleteHttpHealthCheck(del bool) (msg string) {
	gceCloud := cont.Cloud.Provider.(*gcecloud.GCECloud)
	hcList, err := gceCloud.ListHttpHealthChecks()
	if err != nil {
		if cont.isHTTPErrorCode(err, http.StatusNotFound) {
			return msg
		}
		return fmt.Sprintf("Failed to list HTTP health checks: %v", err)
	}
	if len(hcList.Items) == 0 {
		return msg
	}
	for _, hc := range hcList.Items {
		if !cont.canDelete(hc.Name, hc.CreationTimestamp, del) {
			continue
		}
		msg += fmt.Sprintf("%v (http-health-check)\n", hc.Name)
		if del {
			if err := gceCloud.DeleteHttpHealthCheck(hc.Name); err != nil &&
				!cont.isHTTPErrorCode(err, http.StatusNotFound) {
				msg += fmt.Sprintf("Failed to delete HTTP health check %v\n", hc.Name)
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) deleteSSLCertificate(del bool) (msg string) {
	gceCloud := cont.Cloud.Provider.(*gcecloud.GCECloud)
	sslList, err := gceCloud.ListSslCertificates()
	if err != nil {
		if cont.isHTTPErrorCode(err, http.StatusNotFound) {
			return msg
		}
		return fmt.Sprintf("Failed to list ssl certificates: %v", err)
	}
	if len(sslList.Items) != 0 {
		for _, s := range sslList.Items {
			if !cont.canDelete(s.Name, s.CreationTimestamp, del) {
				continue
			}
			msg += fmt.Sprintf("%v (ssl-certificate)\n", s.Name)
			if del {
				if err := gceCloud.DeleteSslCertificate(s.Name); err != nil &&
					!cont.isHTTPErrorCode(err, http.StatusNotFound) {
					msg += fmt.Sprintf("Failed to delete ssl certificates: %v\n", s.Name)
				}
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) deleteInstanceGroup(del bool) (msg string) {
	gceCloud := cont.Cloud.Provider.(*gcecloud.GCECloud)
	// TODO: E2E cloudprovider has only 1 zone, but the cluster can have many.
	// We need to poll on all IGs across all zones.
	igList, err := gceCloud.ListInstanceGroups(cont.Cloud.Zone)
	if err != nil {
		if cont.isHTTPErrorCode(err, http.StatusNotFound) {
			return msg
		}
		return fmt.Sprintf("Failed to list instance groups: %v", err)
	}
	if len(igList.Items) == 0 {
		return msg
	}
	for _, ig := range igList.Items {
		if !cont.canDelete(ig.Name, ig.CreationTimestamp, del) {
			continue
		}
		msg += fmt.Sprintf("%v (instance-group)\n", ig.Name)
		if del {
			if err := gceCloud.DeleteInstanceGroup(ig.Name, cont.Cloud.Zone); err != nil &&
				!cont.isHTTPErrorCode(err, http.StatusNotFound) {
				msg += fmt.Sprintf("Failed to delete instance group %v\n", ig.Name)
			}
		}
	}
	return msg
}

// canDelete returns true if either the name ends in a suffix matching this
// controller's UID, or the creationTimestamp exceeds the maxAge and del is set
// to true. Always returns false if the name doesn't match that we expect for
// Ingress cloud resources.
func (cont *GCEIngressController) canDelete(resourceName, creationTimestamp string, delOldResources bool) bool {
	// ignore everything not created by an ingress controller.
	if !strings.HasPrefix(resourceName, k8sPrefix) || len(strings.Split(resourceName, clusterDelimiter)) != 2 {
		return false
	}
	// always delete things that are created by the current ingress controller.
	if strings.HasSuffix(resourceName, cont.UID) {
		return true
	}
	if !delOldResources {
		return false
	}
	createdTime, err := time.Parse(time.RFC3339, creationTimestamp)
	if err != nil {
		Logf("WARNING: Failed to parse creation timestamp %v for %v: %v", creationTimestamp, resourceName, err)
		return false
	}
	if createdTime.Before(time.Now().Add(maxAge)) {
		Logf("%v created on %v IS too old", resourceName, creationTimestamp)
		return true
	}
	return false
}

func (cont *GCEIngressController) GetFirewallRuleName() string {
	return fmt.Sprintf("%vfw-l7%v%v", k8sPrefix, clusterDelimiter, cont.UID)
}

func (cont *GCEIngressController) GetFirewallRule() *compute.Firewall {
	gceCloud := cont.Cloud.Provider.(*gcecloud.GCECloud)
	fwName := cont.GetFirewallRuleName()
	fw, err := gceCloud.GetFirewall(fwName)
	Expect(err).NotTo(HaveOccurred())
	return fw
}

func (cont *GCEIngressController) deleteFirewallRule(del bool) (msg string) {
	fwList := []compute.Firewall{}
	regex := fmt.Sprintf("%vfw-l7%v.*", k8sPrefix, clusterDelimiter)
	gcloudList("firewall-rules", regex, cont.Cloud.ProjectID, &fwList)
	if len(fwList) != 0 {
		for _, f := range fwList {
			if !cont.canDelete(f.Name, f.CreationTimestamp, del) {
				continue
			}
			msg += fmt.Sprintf("%v (firewall rule)\n", f.Name)
			if del {
				GcloudDelete("firewall-rules", f.Name, cont.Cloud.ProjectID)
			}
		}
	}
	return msg
}

func (cont *GCEIngressController) isHTTPErrorCode(err error, code int) bool {
	apiErr, ok := err.(*googleapi.Error)
	return ok && apiErr.Code == code
}

// Cleanup cleans up cloud resources.
// If del is false, it simply reports existing resources without deleting them.
// It always deletes resources created through it's methods, like staticIP, even
// if del is false.
func (cont *GCEIngressController) Cleanup(del bool) error {
	// Ordering is important here because we cannot delete resources that other
	// resources hold references to.
	errMsg := cont.deleteForwardingRule(del)
	// Static IPs are named after forwarding rules.
	errMsg += cont.deleteAddresses(del)

	errMsg += cont.deleteTargetProxy(del)
	errMsg += cont.deleteUrlMap(del)
	errMsg += cont.deleteBackendService(del)
	errMsg += cont.deleteHttpHealthCheck(del)

	errMsg += cont.deleteInstanceGroup(del)
	errMsg += cont.deleteFirewallRule(del)
	errMsg += cont.deleteSSLCertificate(del)

	// TODO: Verify instance-groups, issue #16636. Gcloud mysteriously barfs when told
	// to unmarshal instance groups into the current vendored gce-client's understanding
	// of the struct.
	if errMsg == "" {
		return nil
	}
	return fmt.Errorf(errMsg)
}

func (cont *GCEIngressController) Init() {
	uid, err := cont.getL7AddonUID()
	Expect(err).NotTo(HaveOccurred())
	cont.UID = uid
	// There's a name limit imposed by GCE. The controller will truncate.
	testName := fmt.Sprintf("k8s-fw-foo-app-X-%v--%v", cont.Ns, cont.UID)
	if len(testName) > nameLenLimit {
		Logf("WARNING: test name including cluster UID: %v is over the GCE limit of %v", testName, nameLenLimit)
	} else {
		Logf("Detected cluster UID %v", cont.UID)
	}
}

// CreateStaticIP allocates a random static ip with the given name. Returns a string
// representation of the ip. Caller is expected to manage cleanup of the ip by
// invoking deleteStaticIPs.
func (cont *GCEIngressController) CreateStaticIP(name string) string {
	gceCloud := cont.Cloud.Provider.(*gcecloud.GCECloud)
	ip, err := gceCloud.ReserveGlobalStaticIP(name, "")
	if err != nil {
		if delErr := gceCloud.DeleteGlobalStaticIP(name); delErr != nil {
			if cont.isHTTPErrorCode(delErr, http.StatusNotFound) {
				Logf("Static ip with name %v was not allocated, nothing to delete", name)
			} else {
				Logf("Failed to delete static ip %v: %v", name, delErr)
			}
		}
		Failf("Failed to allocated static ip %v: %v", name, err)
	}
	cont.staticIPName = ip.Name
	Logf("Reserved static ip %v: %v", cont.staticIPName, ip.Address)
	return ip.Address
}

// deleteStaticIPs delets all static-ips allocated through calls to
// CreateStaticIP.
func (cont *GCEIngressController) deleteStaticIPs() error {
	if cont.staticIPName != "" {
		if err := GcloudDelete("addresses", cont.staticIPName, cont.Cloud.ProjectID, "--global"); err == nil {
			cont.staticIPName = ""
		} else {
			return err
		}
	} else {
		e2eIPs := []compute.Address{}
		gcloudList("addresses", "e2e-.*", cont.Cloud.ProjectID, &e2eIPs)
		ips := []string{}
		for _, ip := range e2eIPs {
			ips = append(ips, ip.Name)
		}
		Logf("None of the remaining %d static-ips were created by this e2e: %v", len(ips), strings.Join(ips, ", "))
	}
	return nil
}

// gcloudList unmarshals json output of gcloud into given out interface.
func gcloudList(resource, regex, project string, out interface{}) {
	// gcloud prints a message to stderr if it has an available update
	// so we only look at stdout.
	command := []string{
		"compute", resource, "list",
		fmt.Sprintf("--regexp=%v", regex),
		fmt.Sprintf("--project=%v", project),
		"-q", "--format=json",
	}
	output, err := exec.Command("gcloud", command...).Output()
	if err != nil {
		errCode := -1
		errMsg := ""
		if exitErr, ok := err.(utilexec.ExitError); ok {
			errCode = exitErr.ExitStatus()
			errMsg = exitErr.Error()
			if osExitErr, ok := err.(*exec.ExitError); ok {
				errMsg = fmt.Sprintf("%v, stderr %v", errMsg, string(osExitErr.Stderr))
			}
		}
		Logf("Error running gcloud command 'gcloud %s': err: %v, output: %v, status: %d, msg: %v", strings.Join(command, " "), err, string(output), errCode, errMsg)
	}
	if err := json.Unmarshal([]byte(output), out); err != nil {
		Logf("Error unmarshalling gcloud output for %v: %v, output: %v", resource, err, string(output))
	}
}

func GcloudDelete(resource, name, project string, args ...string) error {
	Logf("Deleting %v: %v", resource, name)
	argList := append([]string{"compute", resource, "delete", name, fmt.Sprintf("--project=%v", project), "-q"}, args...)
	output, err := exec.Command("gcloud", argList...).CombinedOutput()
	if err != nil {
		Logf("Error deleting %v, output: %v\nerror: %+v", resource, string(output), err)
	}
	return err
}

func GcloudCreate(resource, name, project string, args ...string) error {
	Logf("Creating %v in project %v: %v", resource, project, name)
	argsList := append([]string{"compute", resource, "create", name, fmt.Sprintf("--project=%v", project)}, args...)
	Logf("Running command: gcloud %+v", strings.Join(argsList, " "))
	output, err := exec.Command("gcloud", argsList...).CombinedOutput()
	if err != nil {
		Logf("Error creating %v, output: %v\nerror: %+v", resource, string(output), err)
	}
	return err
}

// CreateIngress creates the Ingress and associated service/rc.
// Required: ing.yaml, rc.yaml, svc.yaml must exist in manifestPath
// Optional: secret.yaml, ingAnnotations
// If ingAnnotations is specified it will overwrite any annotations in ing.yaml
func (j *IngressTestJig) CreateIngress(manifestPath, ns string, ingAnnotations map[string]string) {
	mkpath := func(file string) string {
		return filepath.Join(TestContext.RepoRoot, manifestPath, file)
	}

	Logf("creating replication controller")
	RunKubectlOrDie("create", "-f", mkpath("rc.yaml"), fmt.Sprintf("--namespace=%v", ns))

	Logf("creating service")
	RunKubectlOrDie("create", "-f", mkpath("svc.yaml"), fmt.Sprintf("--namespace=%v", ns))

	if exists(mkpath("secret.yaml")) {
		Logf("creating secret")
		RunKubectlOrDie("create", "-f", mkpath("secret.yaml"), fmt.Sprintf("--namespace=%v", ns))
	}
	j.Ingress = ingFromManifest(mkpath("ing.yaml"))
	j.Ingress.Namespace = ns
	j.Ingress.Annotations = map[string]string{ingressClass: j.Class}
	for k, v := range ingAnnotations {
		j.Ingress.Annotations[k] = v
	}
	Logf(fmt.Sprintf("creating" + j.Ingress.Name + " ingress"))
	var err error
	j.Ingress, err = j.Client.Extensions().Ingresses(ns).Create(j.Ingress)
	ExpectNoError(err)
}

func (j *IngressTestJig) Update(update func(ing *extensions.Ingress)) {
	var err error
	ns, name := j.Ingress.Namespace, j.Ingress.Name
	for i := 0; i < 3; i++ {
		j.Ingress, err = j.Client.Extensions().Ingresses(ns).Get(name, metav1.GetOptions{})
		if err != nil {
			Failf("failed to get ingress %q: %v", name, err)
		}
		update(j.Ingress)
		j.Ingress, err = j.Client.Extensions().Ingresses(ns).Update(j.Ingress)
		if err == nil {
			DescribeIng(j.Ingress.Namespace)
			return
		}
		if !apierrs.IsConflict(err) && !apierrs.IsServerTimeout(err) {
			Failf("failed to update ingress %q: %v", name, err)
		}
	}
	Failf("too many retries updating ingress %q", name)
}

func (j *IngressTestJig) AddHTTPS(secretName string, hosts ...string) {
	j.Ingress.Spec.TLS = []extensions.IngressTLS{{Hosts: hosts, SecretName: secretName}}
	// TODO: Just create the secret in GetRootCAs once we're watching secrets in
	// the ingress controller.
	_, cert, _, err := createSecret(j.Client, j.Ingress)
	ExpectNoError(err)
	Logf("Updating ingress %v to use secret %v for TLS termination", j.Ingress.Name, secretName)
	j.Update(func(ing *extensions.Ingress) {
		ing.Spec.TLS = []extensions.IngressTLS{{Hosts: hosts, SecretName: secretName}}
	})
	j.RootCAs[secretName] = cert
}

func (j *IngressTestJig) GetRootCA(secretName string) (rootCA []byte) {
	var ok bool
	rootCA, ok = j.RootCAs[secretName]
	if !ok {
		Failf("Failed to retrieve rootCAs, no recorded secret by name %v", secretName)
	}
	return
}

func (j *IngressTestJig) DeleteIngress() {
	ExpectNoError(j.Client.Extensions().Ingresses(j.Ingress.Namespace).Delete(j.Ingress.Name, nil))
}

// waitForIngress waits till the ingress acquires an IP, then waits for its
// hosts/urls to respond to a protocol check (either http or https). If
// waitForNodePort is true, the NodePort of the Service is verified before
// verifying the Ingress. NodePort is currently a requirement for cloudprovider
// Ingress.
func (j *IngressTestJig) WaitForIngress(waitForNodePort bool) {
	// Wait for the loadbalancer IP.
	address, err := WaitForIngressAddress(j.Client, j.Ingress.Namespace, j.Ingress.Name, LoadBalancerPollTimeout)
	if err != nil {
		Failf("Ingress failed to acquire an IP address within %v", LoadBalancerPollTimeout)
	}
	j.Address = address
	Logf("Found address %v for ingress %v", j.Address, j.Ingress.Name)
	timeoutClient := &http.Client{Timeout: IngressReqTimeout}

	// Check that all rules respond to a simple GET.
	for _, rules := range j.Ingress.Spec.Rules {
		proto := "http"
		if len(j.Ingress.Spec.TLS) > 0 {
			knownHosts := sets.NewString(j.Ingress.Spec.TLS[0].Hosts...)
			if knownHosts.Has(rules.Host) {
				timeoutClient.Transport, err = buildTransport(rules.Host, j.GetRootCA(j.Ingress.Spec.TLS[0].SecretName))
				ExpectNoError(err)
				proto = "https"
			}
		}
		for _, p := range rules.IngressRuleValue.HTTP.Paths {
			if waitForNodePort {
				j.CurlServiceNodePort(j.Ingress.Namespace, p.Backend.ServiceName, int(p.Backend.ServicePort.IntVal))
			}
			route := fmt.Sprintf("%v://%v%v", proto, address, p.Path)
			Logf("Testing route %v host %v with simple GET", route, rules.Host)
			ExpectNoError(PollURL(route, rules.Host, LoadBalancerPollTimeout, j.PollInterval, timeoutClient, false))
		}
	}
}

// VerifyURL polls for the given iterations, in intervals, and fails if the
// given url returns a non-healthy http code even once.
func (j *IngressTestJig) VerifyURL(route, host string, iterations int, interval time.Duration, httpClient *http.Client) error {
	for i := 0; i < iterations; i++ {
		b, err := SimpleGET(httpClient, route, host)
		if err != nil {
			Logf(b)
			return err
		}
		Logf("Verfied %v with host %v %d times, sleeping for %v", route, host, i, interval)
		time.Sleep(interval)
	}
	return nil
}

func (j *IngressTestJig) CurlServiceNodePort(ns, name string, port int) {
	// TODO: Curl all nodes?
	u, err := GetNodePortURL(j.Client, ns, name, port)
	ExpectNoError(err)
	ExpectNoError(PollURL(u, "", 30*time.Second, j.PollInterval, &http.Client{Timeout: IngressReqTimeout}, false))
}

// GetIngressNodePorts returns all related backend services' nodePorts.
// Current GCE ingress controller allows traffic to the default HTTP backend
// by default, so retrieve its nodePort as well.
func (j *IngressTestJig) GetIngressNodePorts() []string {
	nodePorts := []string{}
	defaultSvc, err := j.Client.Core().Services(metav1.NamespaceSystem).Get(defaultBackendName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())
	nodePorts = append(nodePorts, strconv.Itoa(int(defaultSvc.Spec.Ports[0].NodePort)))

	backendSvcs := []string{}
	if j.Ingress.Spec.Backend != nil {
		backendSvcs = append(backendSvcs, j.Ingress.Spec.Backend.ServiceName)
	}
	for _, rule := range j.Ingress.Spec.Rules {
		for _, ingPath := range rule.HTTP.Paths {
			backendSvcs = append(backendSvcs, ingPath.Backend.ServiceName)
		}
	}
	for _, svcName := range backendSvcs {
		svc, err := j.Client.Core().Services(j.Ingress.Namespace).Get(svcName, metav1.GetOptions{})
		Expect(err).NotTo(HaveOccurred())
		nodePorts = append(nodePorts, strconv.Itoa(int(svc.Spec.Ports[0].NodePort)))
	}
	return nodePorts
}

// constructFirewallForIngress returns the expected GCE firewall rule for the ingress resource
func (j *IngressTestJig) ConstructFirewallForIngress(gceController *GCEIngressController) *compute.Firewall {
	nodeTags := GetNodeTags(j.Client, gceController.Cloud)
	nodePorts := j.GetIngressNodePorts()

	fw := compute.Firewall{}
	fw.Name = gceController.GetFirewallRuleName()
	fw.SourceRanges = []string{GCEL7SrcRange}
	fw.TargetTags = nodeTags.Items
	fw.Allowed = []*compute.FirewallAllowed{
		{
			IPProtocol: "tcp",
			Ports:      nodePorts,
		},
	}
	return &fw
}

// ingFromManifest reads a .json/yaml file and returns the rc in it.
func ingFromManifest(fileName string) *extensions.Ingress {
	var ing extensions.Ingress
	Logf("Parsing ingress from %v", fileName)
	data, err := ioutil.ReadFile(fileName)
	ExpectNoError(err)

	json, err := utilyaml.ToJSON(data)
	ExpectNoError(err)

	ExpectNoError(runtime.DecodeInto(api.Codecs.UniversalDecoder(), json, &ing))
	return &ing
}

func (cont *GCEIngressController) getL7AddonUID() (string, error) {
	Logf("Retrieving UID from config map: %v/%v", metav1.NamespaceSystem, uidConfigMap)
	cm, err := cont.Client.Core().ConfigMaps(metav1.NamespaceSystem).Get(uidConfigMap, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	if uid, ok := cm.Data[uidKey]; ok {
		return uid, nil
	}
	return "", fmt.Errorf("Could not find cluster UID for L7 addon pod")
}

func exists(path string) bool {
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	Failf("Failed to os.Stat path %v", path)
	return false
}

// GCEIngressController manages implementation details of Ingress on GCE/GKE.
type GCEIngressController struct {
	Ns           string
	rcPath       string
	UID          string
	staticIPName string
	rc           *v1.ReplicationController
	svc          *v1.Service
	Client       clientset.Interface
	Cloud        CloudConfig
}

func NewIngressTestJig(c clientset.Interface) *IngressTestJig {
	return &IngressTestJig{Client: c, RootCAs: map[string][]byte{}, PollInterval: LoadBalancerPollInterval}
}

// NginxIngressController manages implementation details of Ingress on Nginx.
type NginxIngressController struct {
	Ns         string
	rc         *v1.ReplicationController
	pod        *v1.Pod
	Client     clientset.Interface
	externalIP string
}

func (cont *NginxIngressController) Init() {
	mkpath := func(file string) string {
		return filepath.Join(TestContext.RepoRoot, IngressManifestPath, "nginx", file)
	}
	Logf("initializing nginx ingress controller")
	RunKubectlOrDie("create", "-f", mkpath("rc.yaml"), fmt.Sprintf("--namespace=%v", cont.Ns))

	rc, err := cont.Client.Core().ReplicationControllers(cont.Ns).Get("nginx-ingress-controller", metav1.GetOptions{})
	ExpectNoError(err)
	cont.rc = rc

	Logf("waiting for pods with label %v", rc.Spec.Selector)
	sel := labels.SelectorFromSet(labels.Set(rc.Spec.Selector))
	ExpectNoError(testutils.WaitForPodsWithLabelRunning(cont.Client, cont.Ns, sel))
	pods, err := cont.Client.Core().Pods(cont.Ns).List(metav1.ListOptions{LabelSelector: sel.String()})
	ExpectNoError(err)
	if len(pods.Items) == 0 {
		Failf("Failed to find nginx ingress controller pods with selector %v", sel)
	}
	cont.pod = &pods.Items[0]
	cont.externalIP, err = GetHostExternalAddress(cont.Client, cont.pod)
	ExpectNoError(err)
	Logf("ingress controller running in pod %v on ip %v", cont.pod.Name, cont.externalIP)
}