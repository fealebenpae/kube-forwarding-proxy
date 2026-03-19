package k8s

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Endpoint represents a resolved pod backing a Kubernetes Service.
type Endpoint struct {
	PodName   string
	Namespace string
	IP        string
}

// ServicePort represents a port exposed by a Kubernetes Service.
type ServicePort struct {
	Name       string
	Port       int32
	TargetPort int32
	Protocol   string
}

// ResolvedService contains the resolved endpoints and ports for a service.
type ResolvedService struct {
	Endpoints []Endpoint
	Ports     []ServicePort
	CachedAt  time.Time
}

// Resolver resolves Kubernetes service names to pod endpoints.
type Resolver struct {
	manager          *ClientManager
	testContextNames []string // non-nil when created via NewResolverForTest
	cache            map[string]*ResolvedService
	cacheTTL         time.Duration
	mu               sync.RWMutex
}

// NewResolverForTest creates a Resolver with no live Kubernetes client. It is
// intended solely for unit tests that do not need real API access. contextNames
// is the list returned by AllContextNames(); pre-seeded entries can be added
// immediately via Seed.
func NewResolverForTest(contextNames []string) *Resolver {
	r := &Resolver{
		cache:    make(map[string]*ResolvedService),
		cacheTTL: 24 * time.Hour, // long TTL so seeded entries don't expire mid-test
	}
	r.testContextNames = contextNames
	return r
}

// NewResolver creates a new service resolver with the given cache TTL.
func NewResolver(manager *ClientManager, cacheTTL time.Duration) *Resolver {
	return &Resolver{
		manager:  manager,
		cache:    make(map[string]*ResolvedService),
		cacheTTL: cacheTTL,
	}
}

// Resolve looks up the endpoints and ports for a Kubernetes service.
// Results are cached for cacheTTL duration.
// contextName selects which kubeconfig context to use; an empty string means
// the current-context from the merged kubeconfig.
// When podName is non-empty only the endpoint for that pod is returned;
// an error is returned if the pod is not found among the service's endpoints.
func (r *Resolver) Resolve(ctx context.Context, contextName, namespace, serviceName, podName string) (*ResolvedService, error) {
	if contextName == "" {
		if r.manager != nil {
			contextName = r.manager.CurrentContextName()
		}
	}

	key := contextName + "/" + namespace + "/" + serviceName

	r.mu.RLock()
	if cached, ok := r.cache[key]; ok && time.Since(cached.CachedAt) < r.cacheTTL {
		r.mu.RUnlock()
		return filterByPod(cached, namespace, serviceName, podName)
	}
	r.mu.RUnlock()

	if r.manager == nil {
		return nil, fmt.Errorf("no live client: service %s/%s not in test cache", namespace, serviceName)
	}

	cc, ok := r.manager.clientForContext(contextName)
	if !ok {
		return nil, fmt.Errorf("unknown kubeconfig context %q", contextName)
	}
	cs := cc.clientset

	svc, err := cs.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("getting service %s/%s: %w", namespace, serviceName, err)
	}

	var ports []ServicePort
	for _, p := range svc.Spec.Ports {
		ports = append(ports, ServicePort{
			Name:       p.Name,
			Port:       p.Port,
			TargetPort: p.TargetPort.IntVal,
			Protocol:   string(p.Protocol),
		})
	}

	sliceList, err := cs.DiscoveryV1().EndpointSlices(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "kubernetes.io/service-name=" + serviceName,
	})
	if err != nil {
		return nil, fmt.Errorf("getting endpoint slices for %s/%s: %w", namespace, serviceName, err)
	}

	var endpoints []Endpoint
	for _, slice := range sliceList.Items {
		for _, ep := range slice.Endpoints {
			if ep.Conditions.Ready != nil && !*ep.Conditions.Ready {
				continue
			}
			epPodName := ""
			if ep.TargetRef != nil {
				epPodName = ep.TargetRef.Name
			}
			for _, addr := range ep.Addresses {
				endpoints = append(endpoints, Endpoint{
					PodName:   epPodName,
					Namespace: namespace,
					IP:        addr,
				})
			}
		}
	}

	if len(endpoints) == 0 {
		return nil, fmt.Errorf("no ready endpoints for service %s/%s", namespace, serviceName)
	}

	resolved := &ResolvedService{
		Endpoints: endpoints,
		Ports:     ports,
		CachedAt:  time.Now(),
	}

	r.mu.Lock()
	r.cache[key] = resolved
	r.mu.Unlock()

	return filterByPod(resolved, namespace, serviceName, podName)
}

// filterByPod returns resolved unchanged when podName is empty.
// When podName is non-empty it returns a copy containing only the matching
// endpoint, or an error if the pod is not present in the service endpoints.
func filterByPod(resolved *ResolvedService, namespace, serviceName, podName string) (*ResolvedService, error) {
	if podName == "" {
		return resolved, nil
	}
	for _, ep := range resolved.Endpoints {
		if ep.PodName == podName {
			return &ResolvedService{
				Endpoints: []Endpoint{ep},
				Ports:     resolved.Ports,
				CachedAt:  resolved.CachedAt,
			}, nil
		}
	}
	return nil, fmt.Errorf("pod %s not found in endpoints for service %s/%s", podName, namespace, serviceName)
}

// PickEndpoint selects a random endpoint from the resolved service.
func (r *Resolver) PickEndpoint(resolved *ResolvedService) Endpoint {
	return resolved.Endpoints[rand.Intn(len(resolved.Endpoints))]
}

// FindPort finds the target port for a given service port number.
func (r *Resolver) FindPort(resolved *ResolvedService, servicePort int32) (int32, error) {
	for _, p := range resolved.Ports {
		if p.Port == servicePort {
			if p.TargetPort != 0 {
				return p.TargetPort, nil
			}
			return p.Port, nil
		}
	}
	return 0, fmt.Errorf("port %d not found in service", servicePort)
}

// AllContextNames returns the names of all known kubeconfig contexts.
func (r *Resolver) AllContextNames() []string {
	if r.testContextNames != nil {
		return r.testContextNames
	}
	return r.manager.AllContextNames()
}

// Seed inserts a pre-built ResolvedService into the resolver's cache under the
// given contextName/namespace/serviceName key. Intended for use in tests that
// need to exercise code paths which call Resolve without a live Kubernetes API.
func (r *Resolver) Seed(contextName, namespace, serviceName string, svc *ResolvedService) {
	key := contextName + "/" + namespace + "/" + serviceName
	r.mu.Lock()
	r.cache[key] = svc
	r.mu.Unlock()
}

// Invalidate removes a cached entry for the given service and context.
// An empty contextName uses the current-context.
func (r *Resolver) Invalidate(contextName, namespace, serviceName string) {
	if contextName == "" {
		contextName = r.manager.CurrentContextName()
	}
	key := contextName + "/" + namespace + "/" + serviceName
	r.mu.Lock()
	delete(r.cache, key)
	r.mu.Unlock()
}
