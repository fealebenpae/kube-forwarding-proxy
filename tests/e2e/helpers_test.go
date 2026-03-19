// Package e2e contains end-to-end tests for the k8s-service-proxy.
//
// Each test creates its own in-process proxy instance and kind cluster(s), so
// the full suite can be run with a plain `go test ./tests/e2e/` — no Docker
// Compose stack or external process is required.
package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	dockerimage "github.com/docker/docker/api/types/image"
	dockerclient "github.com/docker/docker/client"
	mdns "github.com/miekg/dns"
	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/kind/pkg/cluster"

	xproxy "golang.org/x/net/proxy"

	"github.com/fealebenpae/kube-forwarding-proxy/app"
)

const nginxFixtureImage = "nginx:alpine"

// ---------------------------------------------------------------------------
// Test suite entry point
// ---------------------------------------------------------------------------

// TestMain pulls nginx:alpine into the local Docker daemon once for the whole
// test suite. Each call to createKindCluster then loads the image from the
// local daemon into the kind node with `kind load docker-image`, so no cluster
// ever needs to contact Docker Hub during a test run.
func TestMain(m *testing.M) {
	ctx := context.Background()
	cli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: creating docker client: %v\n", err)
	} else {
		defer cli.Close()
		rc, err := cli.ImagePull(ctx, nginxFixtureImage, dockerimage.PullOptions{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: docker pull nginx:alpine: %v\n", err)
		} else {
			// Drain the JSON-progress stream — ImagePull returns as soon as the
			// daemon accepts the request, not when the pull is complete.
			_, _ = io.Copy(io.Discard, rc)
			rc.Close()
		}
	}
	os.Exit(m.Run())
}

// ---------------------------------------------------------------------------
// Proxy lifecycle helpers
// ---------------------------------------------------------------------------

// startProxy creates and starts an in-process proxy bound to loopback on
// OS-assigned ports. A cleanup function is registered on t to stop the proxy.
// The returned *app.Server has HTTPAddr, DNSAddr, and SOCKSAddr populated.
func startProxy(t *testing.T) *app.Server {
	t.Helper()
	cfg := app.Config{
		Interface:     "127.0.0.1",
		VIPCIDR:       "127.0.0.0/8",
		ClusterDomain: "svc.cluster.local",
		LogLevel:      "debug",
		HTTPListen:    "127.0.0.1:0",
		DNSListen:     "127.0.0.1:0",
		SOCKSListen:   "127.0.0.1:0",
	}
	logger, _ := zap.NewDevelopment()
	srv := app.NewServer(cfg, logger.Sugar(), true /*dns*/, true /*socks*/)
	if err := srv.Start(); err != nil {
		t.Fatalf("starting proxy: %v", err)
	}
	t.Logf("proxy started  http=%s  dns=%s  socks5=%s", srv.HTTPAddr, srv.DNSAddr, srv.SOCKSAddr)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Logf("warning: stopping proxy: %v", err)
		}
	})
	return srv
}

// ---------------------------------------------------------------------------
// kind cluster helpers
// ---------------------------------------------------------------------------

// createKindCluster creates a kind cluster and returns the raw kubeconfig YAML.
// The kubeconfig's API server points to 127.0.0.1:<nodePort>, which is
// reachable directly from the host process without any Docker networking.
func createKindCluster(t *testing.T, name string) string {
	t.Helper()
	p := cluster.NewProvider()

	t.Logf("creating kind cluster %q", name)
	if err := p.Create(name, cluster.CreateWithWaitForReady(5*time.Minute)); err != nil {
		t.Fatalf("creating kind cluster %s: %v", name, err)
	}
	t.Cleanup(func() {
		t.Logf("deleting kind cluster %q", name)
		if err := p.Delete(name, ""); err != nil {
			t.Logf("warning: deleting kind cluster %s: %v", name, err)
		}
	})

	// Load nginx:alpine from the local Docker daemon into every cluster node so
	// pods never need to pull from Docker Hub during the test. ImageSave streams
	// a Docker-format tar; we pipe it straight into `ctr images import` on the
	// node. We call ctr directly (not via nodeutils.LoadImageArchive) because
	// that helper hardcodes --all-platforms which is incompatible with the
	// Docker-format archive produced by ImageSave.
	if clusterNodes, err := p.ListInternalNodes(name); err != nil {
		t.Logf("warning: listing nodes for %s: %v", name, err)
	} else if dockerCli, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation()); err != nil {
		t.Logf("warning: creating docker client: %v", err)
	} else {
		defer dockerCli.Close()
		for _, node := range clusterNodes {
			rc, err := dockerCli.ImageSave(context.Background(), []string{nginxFixtureImage})
			if err != nil {
				t.Logf("warning: docker save %s: %v", nginxFixtureImage, err)
				break
			}
			// overlayfs is the default containerd snapshotter for Linux kind nodes.
			if err := node.Command("ctr", "--namespace=k8s.io", "images", "import",
				"--digests", "--snapshotter=overlayfs", "-").SetStdin(rc).Run(); err != nil {
				t.Logf("warning: loading %s into node %s: %v", nginxFixtureImage, node, err)
			}
			rc.Close()
		}
	}

	raw, err := p.KubeConfig(name, false)
	if err != nil {
		t.Fatalf("getting kubeconfig for %s: %v", name, err)
	}

	// // Wait for the default service account to exist before returning, so
	// // callers can immediately create resources without a separate wait step.
	// cs := clientsetFromKubeconfig(t, raw)
	// saCtx, saCancel := context.WithTimeout(context.Background(), 60*time.Second)
	// defer saCancel()
	// if err := wait.PollUntilContextCancel(saCtx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
	// 	_, err := cs.CoreV1().ServiceAccounts("default").Get(ctx, "default", metav1.GetOptions{})
	// 	return err == nil, nil
	// }); err != nil {
	// 	t.Fatalf("cluster %s: default service account not ready within 60s", name)
	// }

	return raw
}

// clientsetFromKubeconfig creates a kubernetes.Interface from raw kubeconfig YAML.
func clientsetFromKubeconfig(t *testing.T, rawKubeconfig string) kubernetes.Interface {
	t.Helper()
	cfg, err := clientcmd.RESTConfigFromKubeConfig([]byte(rawKubeconfig))
	if err != nil {
		t.Fatalf("building REST config: %v", err)
	}
	cfg.TLSClientConfig.Insecure = true
	cfg.TLSClientConfig.CAData = nil
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatalf("creating clientset: %v", err)
	}
	return cs
}

// ---------------------------------------------------------------------------
// Proxy HTTP API helpers
// ---------------------------------------------------------------------------

func proxyHTTPURL(httpAddr, path string) string {
	return fmt.Sprintf("http://%s%s", httpAddr, path)
}

func putKubeconfig(t *testing.T, httpAddr, kubeconfig string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, proxyHTTPURL(httpAddr, "/kubeconfig"), strings.NewReader(kubeconfig))
	if err != nil {
		t.Fatalf("creating PUT request: %v", err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT /kubeconfig: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PUT /kubeconfig: status %d, body: %s", resp.StatusCode, body)
	}
}

func patchKubeconfig(t *testing.T, httpAddr, kubeconfig string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, proxyHTTPURL(httpAddr, "/kubeconfig"), strings.NewReader(kubeconfig))
	if err != nil {
		t.Fatalf("creating PATCH request: %v", err)
	}
	req.Header.Set("Content-Type", "application/yaml")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH /kubeconfig: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("PATCH /kubeconfig: status %d, body: %s", resp.StatusCode, body)
	}
}

func deleteKubeconfig(t *testing.T, httpAddr string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodDelete, proxyHTTPURL(httpAddr, "/kubeconfig"), nil)
	if err != nil {
		t.Fatalf("creating DELETE request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE /kubeconfig: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("DELETE /kubeconfig: status %d, body: %s", resp.StatusCode, body)
	}
}

// ---------------------------------------------------------------------------
// Kubernetes resource helpers
// ---------------------------------------------------------------------------

// deployNginx creates an nginx pod, a ClusterIP service, and a headless
// service in the given namespace. It waits until the pod is Ready.
// svcPrefix is prepended to service names to allow multiple deployments
// in the same namespace (e.g. "nginx" -> "nginx-clusterip", "nginx-headless").
func deployNginx(t *testing.T, cs kubernetes.Interface, namespace, svcPrefix string) {
	t.Helper()
	ctx := context.Background()

	podName := svcPrefix + "-0"
	headlessSvc := svcPrefix + "-headless"

	// Create namespace if not default.
	if namespace != "default" {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		if _, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating namespace %s: %v", namespace, err)
		}
		t.Cleanup(func() {
			_ = cs.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
		})
	}

	labels := map[string]string{"app": svcPrefix}

	// Pod.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Hostname:  podName,
			Subdomain: headlessSvc,
			Containers: []corev1.Container{{
				Name:  "nginx",
				Image: nginxFixtureImage,
				Ports: []corev1.ContainerPort{{ContainerPort: 80}},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating pod %s/%s: %v", namespace, podName, err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods(namespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
	})

	// ClusterIP Service.
	clusterIPSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcPrefix + "-clusterip",
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(80),
			}},
		},
	}
	if _, err := cs.CoreV1().Services(namespace).Create(ctx, clusterIPSvc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating ClusterIP service: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Services(namespace).Delete(context.Background(), svcPrefix+"-clusterip", metav1.DeleteOptions{})
	})

	// Headless Service.
	headless := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      headlessSvc,
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			ClusterIP: "None",
			Selector:  labels,
			Ports: []corev1.ServicePort{{
				Port:       80,
				TargetPort: intstr.FromInt32(80),
			}},
		},
	}
	if _, err := cs.CoreV1().Services(namespace).Create(ctx, headless, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating headless service: %v", err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Services(namespace).Delete(context.Background(), headlessSvc, metav1.DeleteOptions{})
	})

	// Wait for pod to be ready.
	waitForPodReady(t, cs, namespace, podName, 120*time.Second)
}

// waitForPodReady polls until the named pod is in Ready condition.
func waitForPodReady(t *testing.T, cs kubernetes.Interface, namespace, name string, timeout time.Duration) {
	t.Helper()
	t.Logf("waiting for pod %s/%s to be ready (timeout %s)", namespace, name, timeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	err := wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		pod, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, nil // retry
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		t.Fatalf("pod %s/%s not ready within %s: %v", namespace, name, timeout, err)
	}
	t.Logf("pod %s/%s is ready", namespace, name)
}

// ---------------------------------------------------------------------------
// HTTP request helpers
// ---------------------------------------------------------------------------

// httpGetViaDNSVIP performs an HTTP GET resolving hostnames via the proxy's
// DNS server at dnsAddr (which returns VIPs). Retries up to 10 times.
func httpGetViaDNSVIP(t *testing.T, dnsAddr, url string) string {
	t.Helper()
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 5 * time.Second}
			return d.DialContext(ctx, network, dnsAddr)
		},
	}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		DialContext: (&net.Dialer{
			Timeout:  30 * time.Second,
			Resolver: resolver,
		}).DialContext,
	}
	client := &http.Client{
		Timeout:   30 * time.Second,
		Transport: transport,
	}

	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			t.Logf("attempt %d: GET %s: %v", attempt, url, err)
			time.Sleep(3 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return string(body)
		}
		lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, body)
		t.Logf("attempt %d: GET %s: %v", attempt, url, lastErr)
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("GET %s failed after retries: %v", url, lastErr)
	return ""
}

// httpGetViaSOCKS5 performs an HTTP GET through the proxy's SOCKS5 server at
// socksAddr. Retries up to 10 times.
func httpGetViaSOCKS5(t *testing.T, socksAddr, url string) string {
	t.Helper()
	dialer, err := xproxy.SOCKS5("tcp", socksAddr, nil, xproxy.Direct)
	if err != nil {
		t.Fatalf("creating SOCKS5 dialer: %v", err)
	}

	client := &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			t.Logf("attempt %d: SOCKS5 GET %s: %v", attempt, url, err)
			time.Sleep(3 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return string(body)
		}
		lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, body)
		t.Logf("attempt %d: SOCKS5 GET %s: %v", attempt, url, lastErr)
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("SOCKS5 GET %s failed after retries: %v", url, lastErr)
	return ""
}

// ---------------------------------------------------------------------------
// Shared test setup
// ---------------------------------------------------------------------------

// setupSingleCluster creates a kind cluster, pushes its kubeconfig to the
// proxy, and deploys nginx in the default namespace. It returns the kind
// context name (e.g. "kind-<clusterName>") for use in context-suffix tests.
func setupSingleCluster(t *testing.T, srv *app.Server, clusterName, svcPrefix string) string {
	t.Helper()

	kubeconfig := createKindCluster(t, clusterName)
	putKubeconfig(t, srv.HTTPAddr, kubeconfig)

	cs := clientsetFromKubeconfig(t, kubeconfig)
	deployNginx(t, cs, "default", svcPrefix)

	cfg, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		t.Fatalf("loading kubeconfig: %v", err)
	}
	return cfg.CurrentContext
}

// setupSingleClusterMultiPort creates a kind cluster, pushes its kubeconfig to
// the proxy, and deploys a multi-port nginx setup in the default namespace.
func setupSingleClusterMultiPort(t *testing.T, srv *app.Server, clusterName, svcPrefix string) {
	t.Helper()

	kubeconfig := createKindCluster(t, clusterName)
	putKubeconfig(t, srv.HTTPAddr, kubeconfig)

	cs := clientsetFromKubeconfig(t, kubeconfig)
	deployNginxMultiPort(t, cs, "default", svcPrefix)
}

// deployNginxMultiPort creates a pod with two nginx containers — one serving on
// port 80 (default config) and one serving on port 8080 (custom config via
// ConfigMap) — together with a single ClusterIP service that exposes both ports.
func deployNginxMultiPort(t *testing.T, cs kubernetes.Interface, namespace, svcPrefix string) {
	t.Helper()
	ctx := context.Background()

	podName := svcPrefix + "-0"
	cmName := svcPrefix + "-nginx-cfg"

	// Create namespace if not default.
	if namespace != "default" {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
		if _, err := cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{}); err != nil {
			t.Fatalf("creating namespace %s: %v", namespace, err)
		}
		t.Cleanup(func() {
			_ = cs.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
		})
	}

	labels := map[string]string{"app": svcPrefix}

	// ConfigMap providing a minimal nginx config that listens on port 8080.
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
		},
		Data: map[string]string{
			"nginx8080.conf": `pid /tmp/nginx8080.pid;
events {}
http {
  server {
    listen 8080;
    location / {
      root /usr/share/nginx/html;
      index index.html index.htm;
    }
  }
}
`,
		},
	}
	if _, err := cs.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating ConfigMap %s/%s: %v", namespace, cmName, err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().ConfigMaps(namespace).Delete(context.Background(), cmName, metav1.DeleteOptions{})
	})

	// Pod with two nginx containers sharing the same network namespace.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx-80",
					Image: nginxFixtureImage,
					Ports: []corev1.ContainerPort{{ContainerPort: 80}},
				},
				{
					Name:  "nginx-8080",
					Image: nginxFixtureImage,
					// Override the entrypoint to use the custom config file.
					Command: []string{"nginx"},
					Args:    []string{"-c", "/etc/nginx/custom/nginx8080.conf", "-g", "daemon off;"},
					Ports:   []corev1.ContainerPort{{ContainerPort: 8080}},
					VolumeMounts: []corev1.VolumeMount{{
						Name:      "nginx-cfg",
						MountPath: "/etc/nginx/custom",
					}},
				},
			},
			Volumes: []corev1.Volume{{
				Name: "nginx-cfg",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
					},
				},
			}},
		},
	}
	if _, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating pod %s/%s: %v", namespace, podName, err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Pods(namespace).Delete(context.Background(), podName, metav1.DeleteOptions{})
	})

	// ClusterIP Service exposing both ports.
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      svcPrefix + "-clusterip",
			Namespace: namespace,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Name:       "http-80",
					Port:       80,
					TargetPort: intstr.FromInt32(80),
				},
				{
					Name:       "http-8080",
					Port:       8080,
					TargetPort: intstr.FromInt32(8080),
				},
			},
		},
	}
	if _, err := cs.CoreV1().Services(namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating service %s/%s-clusterip: %v", namespace, svcPrefix, err)
	}
	t.Cleanup(func() {
		_ = cs.CoreV1().Services(namespace).Delete(context.Background(), svcPrefix+"-clusterip", metav1.DeleteOptions{})
	})

	// Wait until both containers in the pod are ready.
	waitForPodReady(t, cs, namespace, podName, 120*time.Second)
}

// ---------------------------------------------------------------------------
// Raw DNS helpers
// ---------------------------------------------------------------------------

// dnsLookupRaw sends a DNS query of the given type to dnsAddr (e.g. "127.0.0.1:5353")
// and returns the full response. It retries on transport errors but returns
// immediately for any DNS-level response (including NXDOMAIN). Returns nil and
// fails the test if no response is received after retries.
func dnsLookupRaw(t *testing.T, dnsAddr, name string, qtype uint16) *mdns.Msg {
	t.Helper()
	c := new(mdns.Client)
	c.Net = "udp"
	m := new(mdns.Msg)
	m.SetQuestion(mdns.Fqdn(name), qtype)
	m.RecursionDesired = true
	// Advertise a 4096-byte UDP payload size so the server can return
	// responses that include Extra records (TXT, SRV) without truncation.
	m.SetEdns0(4096, false)

	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		resp, _, err := c.Exchange(m, dnsAddr)
		if err != nil {
			lastErr = err
			t.Logf("attempt %d: DNS %s %s: %v", attempt, mdns.TypeToString[qtype], name, err)
			time.Sleep(2 * time.Second)
			continue
		}
		return resp
	}
	t.Fatalf("DNS %s %s: no response after retries: %v", mdns.TypeToString[qtype], name, lastErr)
	return nil
}

// dnsLookupAExpect retries dnsLookupRaw for a TypeA query until at least
// wantCount A records appear in the response Answer section, up to a 60-second
// deadline. It is used in multi-cluster tests where endpoint-slice propagation
// may trail pod-ready status by a few seconds.
func dnsLookupAExpect(t *testing.T, dnsAddr, name string, wantCount int) *mdns.Msg {
	t.Helper()
	var last *mdns.Msg
	for attempt := 1; attempt <= 20; attempt++ {
		msg := dnsLookupRaw(t, dnsAddr, name, mdns.TypeA)
		if msg != nil {
			n := countARecords(msg)
			if n >= wantCount {
				return msg
			}
			last = msg
			t.Logf("attempt %d: got %d A record(s) for %s, want %d; retrying", attempt, n, name, wantCount)
		}
		time.Sleep(3 * time.Second)
	}
	got := 0
	if last != nil {
		got = countARecords(last)
	}
	t.Fatalf("DNS A %s: got %d record(s), want at least %d", name, got, wantCount)
	return nil
}

// countARecords returns the number of A records in msg.Answer.
func countARecords(msg *mdns.Msg) int {
	n := 0
	for _, rr := range msg.Answer {
		if _, ok := rr.(*mdns.A); ok {
			n++
		}
	}
	return n
}

// extractARecordIPs returns the IPv4 addresses from A records in msg.Answer.
func extractARecordIPs(msg *mdns.Msg) []net.IP {
	var ips []net.IP
	for _, rr := range msg.Answer {
		if a, ok := rr.(*mdns.A); ok {
			ips = append(ips, a.A)
		}
	}
	return ips
}

// extractTXTStrings returns all TXT string values from the given RR slice.
func extractTXTStrings(rrs []mdns.RR) []string {
	var out []string
	for _, rr := range rrs {
		if txt, ok := rr.(*mdns.TXT); ok {
			out = append(out, txt.Txt...)
		}
	}
	return out
}

// querySRVDirect sends a TypeSRV DNS query directly to dnsAddr and returns the
// SRV records from the Answer section. It does not retry; use dnsLookupRaw with
// mdns.TypeSRV when retry logic is needed.
func querySRVDirect(t *testing.T, dnsAddr, qname string) []*mdns.SRV {
	t.Helper()
	msg := dnsLookupRaw(t, dnsAddr, qname, mdns.TypeSRV)
	if msg == nil {
		return nil
	}
	var srvs []*mdns.SRV
	for _, rr := range msg.Answer {
		if srv, ok := rr.(*mdns.SRV); ok {
			srvs = append(srvs, srv)
		}
	}
	return srvs
}

// dnsLookupSRVExpect retries a TypeSRV query until at least wantCount SRV
// records appear in the Answer section, up to a 60-second deadline.
func dnsLookupSRVExpect(t *testing.T, dnsAddr, name string, wantCount int) []*mdns.SRV {
	t.Helper()
	for attempt := 1; attempt <= 20; attempt++ {
		msg := dnsLookupRaw(t, dnsAddr, name, mdns.TypeSRV)
		srvs := make([]*mdns.SRV, 0)
		if msg != nil {
			for _, rr := range msg.Answer {
				if srv, ok := rr.(*mdns.SRV); ok {
					srvs = append(srvs, srv)
				}
			}
			if len(srvs) >= wantCount {
				return srvs
			}
			t.Logf("attempt %d: got %d SRV record(s) for %s, want %d; retrying", attempt, len(srvs), name, wantCount)
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("DNS SRV %s: did not get %d record(s) within deadline", name, wantCount)
	return nil
}

// extractSRVFromExtra returns SRV records from the Extra section of a DNS message.
func extractSRVFromExtra(msg *mdns.Msg) []*mdns.SRV {
	var srvs []*mdns.SRV
	for _, rr := range msg.Extra {
		if srv, ok := rr.(*mdns.SRV); ok {
			srvs = append(srvs, srv)
		}
	}
	return srvs
}

// httpGetToVIP does an HTTP GET to ip:port, returning the response body.
// It retries up to 10 times with 3-second back-off.
func httpGetToVIP(t *testing.T, ip net.IP, port int) string {
	t.Helper()
	url := fmt.Sprintf("http://%s:%d/", ip.String(), port)
	client := &http.Client{Timeout: 30 * time.Second}
	var lastErr error
	for attempt := 1; attempt <= 10; attempt++ {
		resp, err := client.Get(url)
		if err != nil {
			lastErr = err
			t.Logf("attempt %d: GET %s: %v", attempt, url, err)
			time.Sleep(3 * time.Second)
			continue
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return string(body)
		}
		lastErr = fmt.Errorf("status %d: %s", resp.StatusCode, body)
		t.Logf("attempt %d: GET %s: %v", attempt, url, lastErr)
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("GET %s failed after retries: %v", url, lastErr)
	return ""
}
