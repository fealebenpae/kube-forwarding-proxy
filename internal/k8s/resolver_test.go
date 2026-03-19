package k8s

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

// newTestManager creates a ClientManager pre-seeded with a single fake
// clientset under the context name "test". Returns the manager and the
// underlying fake clientset so test helpers can populate it with fixtures.
func newTestManager(objects ...corev1.Service) (*ClientManager, kubernetes.Interface) {
	cs := fake.NewClientset()
	for i := range objects {
		_, _ = cs.CoreV1().Services(objects[i].Namespace).Create(
			context.Background(), &objects[i], metav1.CreateOptions{})
	}
	cm := &ClientManager{
		clients:    map[string]*contextClient{"test": {clientset: cs}},
		currentCtx: "test",
		logger:     zap.NewNop().Sugar(),
	}
	return cm, cs
}

func createTestService(ns, name string, port, targetPort int32) corev1.Service {
	return corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       port,
					TargetPort: intstr.FromInt32(targetPort),
					Protocol:   corev1.ProtocolTCP,
				},
			},
		},
	}
}

func createTestEndpoints(cs kubernetes.Interface, ns, name, podName, ip string) {
	ready := true
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"kubernetes.io/service-name": name},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints: []discoveryv1.Endpoint{
			{
				Addresses:  []string{ip},
				Conditions: discoveryv1.EndpointConditions{Ready: &ready},
				TargetRef: &corev1.ObjectReference{
					Kind: "Pod",
					Name: podName,
				},
			},
		},
	}
	_, _ = cs.DiscoveryV1().EndpointSlices(ns).Create(context.Background(), es, metav1.CreateOptions{})
}

func createTestEndpointsMulti(cs kubernetes.Interface, ns, name string, pods []struct{ name, ip string }) {
	ready := true
	eps := make([]discoveryv1.Endpoint, 0, len(pods))
	for _, p := range pods {
		eps = append(eps, discoveryv1.Endpoint{
			Addresses:  []string{p.ip},
			Conditions: discoveryv1.EndpointConditions{Ready: &ready},
			TargetRef: &corev1.ObjectReference{
				Kind: "Pod",
				Name: p.name,
			},
		})
	}
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"kubernetes.io/service-name": name},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   eps,
	}
	_, _ = cs.DiscoveryV1().EndpointSlices(ns).Create(context.Background(), es, metav1.CreateOptions{})
}

func TestResolver_Resolve_Success(t *testing.T) {
	svc := createTestService("default", "my-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpoints(cs, "default", "my-svc", "pod-1", "10.0.0.1")

	r := NewResolver(manager, 15*time.Second)

	resolved, err := r.Resolve(context.Background(), "", "default", "my-svc", "")
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if len(resolved.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(resolved.Endpoints))
	}

	if resolved.Endpoints[0].PodName != "pod-1" {
		t.Errorf("PodName = %q, want pod-1", resolved.Endpoints[0].PodName)
	}

	if resolved.Endpoints[0].IP != "10.0.0.1" {
		t.Errorf("IP = %q, want 10.0.0.1", resolved.Endpoints[0].IP)
	}

	if len(resolved.Ports) != 1 {
		t.Fatalf("expected 1 port, got %d", len(resolved.Ports))
	}

	if resolved.Ports[0].Port != 80 || resolved.Ports[0].TargetPort != 8080 {
		t.Errorf("Port = %d/%d, want 80/8080", resolved.Ports[0].Port, resolved.Ports[0].TargetPort)
	}
}

func TestResolver_Resolve_NoEndpoints(t *testing.T) {
	svc := createTestService("default", "empty-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	es := &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "empty-svc",
			Namespace: "default",
			Labels:    map[string]string{"kubernetes.io/service-name": "empty-svc"},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   []discoveryv1.Endpoint{},
	}
	_, _ = cs.DiscoveryV1().EndpointSlices("default").Create(context.Background(), es, metav1.CreateOptions{})

	r := NewResolver(manager, 15*time.Second)
	_, err := r.Resolve(context.Background(), "", "default", "empty-svc", "")
	if err == nil {
		t.Fatal("expected error for service with no endpoints, got nil")
	}
}

func TestResolver_Resolve_ServiceNotFound(t *testing.T) {
	manager, _ := newTestManager()
	r := NewResolver(manager, 15*time.Second)
	_, err := r.Resolve(context.Background(), "", "default", "nonexistent", "")
	if err == nil {
		t.Fatal("expected error for nonexistent service, got nil")
	}
}

func TestResolver_Caching(t *testing.T) {
	svc := createTestService("default", "cached-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpoints(cs, "default", "cached-svc", "pod-1", "10.0.0.1")

	r := NewResolver(manager, 1*time.Hour)

	res1, err := r.Resolve(context.Background(), "", "default", "cached-svc", "")
	if err != nil {
		t.Fatalf("first Resolve failed: %v", err)
	}

	res2, err := r.Resolve(context.Background(), "", "default", "cached-svc", "")
	if err != nil {
		t.Fatalf("second Resolve failed: %v", err)
	}

	if res1 != res2 {
		t.Error("expected cached result to be the same pointer")
	}
}

func TestResolver_CacheExpiry(t *testing.T) {
	svc := createTestService("default", "expiring-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpoints(cs, "default", "expiring-svc", "pod-1", "10.0.0.1")

	r := NewResolver(manager, 1*time.Millisecond)

	res1, err := r.Resolve(context.Background(), "", "default", "expiring-svc", "")
	if err != nil {
		t.Fatalf("first Resolve failed: %v", err)
	}

	time.Sleep(10 * time.Millisecond)

	res2, err := r.Resolve(context.Background(), "", "default", "expiring-svc", "")
	if err != nil {
		t.Fatalf("second Resolve failed: %v", err)
	}

	if res1 == res2 {
		t.Error("expected fresh result after cache expiry")
	}
}

func TestResolver_Invalidate(t *testing.T) {
	svc := createTestService("default", "inv-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpoints(cs, "default", "inv-svc", "pod-1", "10.0.0.1")

	r := NewResolver(manager, 1*time.Hour)
	res1, _ := r.Resolve(context.Background(), "", "default", "inv-svc", "")
	r.Invalidate("", "default", "inv-svc")
	res2, _ := r.Resolve(context.Background(), "", "default", "inv-svc", "")

	if res1 == res2 {
		t.Error("expected different result after invalidation")
	}
}

func TestResolver_FindPort(t *testing.T) {
	svc := createTestService("default", "port-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpoints(cs, "default", "port-svc", "pod-1", "10.0.0.1")

	r := NewResolver(manager, 15*time.Second)
	resolved, _ := r.Resolve(context.Background(), "", "default", "port-svc", "")

	target, err := r.FindPort(resolved, 80)
	if err != nil {
		t.Fatalf("FindPort failed: %v", err)
	}
	if target != 8080 {
		t.Errorf("FindPort returned %d, want 8080", target)
	}

	_, err = r.FindPort(resolved, 9999)
	if err == nil {
		t.Fatal("expected error for unknown port, got nil")
	}
}

func TestResolver_PickEndpoint(t *testing.T) {
	svc := createTestService("default", "pick-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpoints(cs, "default", "pick-svc", "pod-1", "10.0.0.1")

	r := NewResolver(manager, 15*time.Second)
	resolved, _ := r.Resolve(context.Background(), "", "default", "pick-svc", "")

	ep := r.PickEndpoint(resolved)
	if ep.PodName != "pod-1" {
		t.Errorf("PickEndpoint returned pod %q, want pod-1", ep.PodName)
	}
}

func TestResolver_ResolveForPod_Success(t *testing.T) {
	svc := createTestService("default", "headless-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpointsMulti(cs, "default", "headless-svc", []struct{ name, ip string }{
		{"pod-0", "10.0.0.1"},
		{"pod-1", "10.0.0.2"},
		{"pod-2", "10.0.0.3"},
	})

	r := NewResolver(manager, 15*time.Second)

	for _, tc := range []struct {
		podName string
		wantIP  string
	}{
		{"pod-0", "10.0.0.1"},
		{"pod-1", "10.0.0.2"},
		{"pod-2", "10.0.0.3"},
	} {
		t.Run(tc.podName, func(t *testing.T) {
			resolved, err := r.Resolve(context.Background(), "", "default", "headless-svc", tc.podName)
			if err != nil {
				t.Fatalf("ResolveForPod failed: %v", err)
			}
			if len(resolved.Endpoints) != 1 {
				t.Fatalf("expected 1 endpoint, got %d", len(resolved.Endpoints))
			}
			if resolved.Endpoints[0].PodName != tc.podName {
				t.Errorf("PodName = %q, want %q", resolved.Endpoints[0].PodName, tc.podName)
			}
			if resolved.Endpoints[0].IP != tc.wantIP {
				t.Errorf("IP = %q, want %q", resolved.Endpoints[0].IP, tc.wantIP)
			}
			if len(resolved.Ports) != 1 || resolved.Ports[0].Port != 80 {
				t.Errorf("unexpected ports: %+v", resolved.Ports)
			}
		})
	}
}

func TestResolver_ResolveForPod_NotFound(t *testing.T) {
	svc := createTestService("default", "headless-svc", 80, 8080)
	manager, cs := newTestManager(svc)
	createTestEndpoints(cs, "default", "headless-svc", "pod-0", "10.0.0.1")

	r := NewResolver(manager, 15*time.Second)

	_, err := r.Resolve(context.Background(), "", "default", "headless-svc", "pod-99")
	if err == nil {
		t.Fatal("expected error for unknown pod name, got nil")
	}
}

func TestResolver_ResolveForPod_ServiceNotFound(t *testing.T) {
	manager, _ := newTestManager()
	r := NewResolver(manager, 15*time.Second)

	_, err := r.Resolve(context.Background(), "", "default", "nonexistent", "pod-0")
	if err == nil {
		t.Fatal("expected error for nonexistent service, got nil")
	}
}
