// Package k8s provides Kubernetes client management, port-forwarding, and service resolution.
package k8s

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.uber.org/zap"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// contextClient holds an eagerly-built Kubernetes client for a single kubeconfig context.
type contextClient struct {
	clientset  kubernetes.Interface
	restConfig *rest.Config
}

// ConflictError is returned by Post when the input kubeconfig contains cluster,
// context, or auth-info names that already exist in the merged config.
type ConflictError struct {
	Names []string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("kubeconfig conflicts with existing entries: %s", strings.Join(e.Names, ", "))
}

// ClientManager manages a set of Kubernetes clients, one per kubeconfig context.
// It merges a static file-based kubeconfig (optional) with an in-memory dynamic
// config that can be updated via the /kubeconfig HTTP endpoint.
// Clientsets are built eagerly whenever the config is mutated.
type ClientManager struct {
	dynamicConfig *clientcmdapi.Config      // in-memory config contributed via the HTTP API; merged with default config on every change
	clients       map[string]*contextClient // context name -> eagerly built client
	currentCtx    string
	logger        *zap.SugaredLogger
	mu            sync.RWMutex
}

// NewClientManager creates a ClientManager by loading the default kubeconfig.
func NewClientManager(logger *zap.SugaredLogger) (*ClientManager, error) {
	cm := &ClientManager{
		logger: logger,
	}
	if err := cm.rebuild(); err != nil {
		return nil, err
	}
	return cm, nil
}

// clientForContext returns the pre-built client for the named context.
// Returns (nil, false) if the context is unknown.
func (cm *ClientManager) clientForContext(name string) (*contextClient, bool) {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	c, ok := cm.clients[name]
	return c, ok
}

// CurrentContextName returns the current-context from the merged kubeconfig.
func (cm *ClientManager) CurrentContextName() string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.currentCtx
}

// AllContextNames returns a sorted slice of all known context names.
func (cm *ClientManager) AllContextNames() []string {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	names := make([]string, 0, len(cm.clients))
	for name := range cm.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Kubeconfig returns the dynamic config.
func (cm *ClientManager) Kubeconfig() *clientcmdapi.Config {
	cm.mu.RLock()
	defer cm.mu.RUnlock()
	return cm.dynamicConfig
}

// Reset replaces the dynamic config entirely with cfg.
func (cm *ClientManager) Reset(cfg *clientcmdapi.Config) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()
	cm.dynamicConfig = cfg
	return cm.rebuild()
}

// Add merges cfg into the dynamic config without overwriting existing
// entries. Returns *ConflictError if any cluster, context, or auth-info name
// in cfg already exists in the merged config.
func (cm *ClientManager) Add(cfg *clientcmdapi.Config) error {
	parsed := cfg
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.dynamicConfig != nil {
		var conflicts []string
		for name := range parsed.Clusters {
			if _, exists := cm.dynamicConfig.Clusters[name]; exists {
				conflicts = append(conflicts, "cluster:"+name)
			}
		}
		for name := range parsed.Contexts {
			if _, exists := cm.dynamicConfig.Contexts[name]; exists {
				conflicts = append(conflicts, "context:"+name)
			}
		}
		for name := range parsed.AuthInfos {
			if _, exists := cm.dynamicConfig.AuthInfos[name]; exists {
				conflicts = append(conflicts, "user:"+name)
			}
		}
		if len(conflicts) > 0 {
			sort.Strings(conflicts)
			return &ConflictError{Names: conflicts}
		}

		mergeConfigs(cm.dynamicConfig, parsed, false)
	} else {
		cm.dynamicConfig = parsed
	}

	return cm.rebuild()
}

// MergeAndOverwrite merges cfg into the dynamic config, overwriting any existing
// entries with the same name.
func (cm *ClientManager) MergeAndOverwrite(cfg *clientcmdapi.Config) error {
	parsed := cfg
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.dynamicConfig == nil {
		cm.dynamicConfig = clientcmdapi.NewConfig()
	}
	mergeConfigs(cm.dynamicConfig, parsed, true)
	return cm.rebuild()
}

// rebuild constructs clientsets for every context in the merged config.
// Must be called while holding the write-lock (or from NewClientManager
// before the object is shared).
func (cm *ClientManager) rebuild() error {
	merged, err := cm.mergedConfig()
	if err != nil {
		return err
	}

	clients := make(map[string]*contextClient, len(merged.Contexts))
	for ctxName := range merged.Contexts {
		cc := clientcmd.NewDefaultClientConfig(*merged, &clientcmd.ConfigOverrides{
			CurrentContext: ctxName,
		})
		restConfig, err := cc.ClientConfig()
		if err != nil {
			cm.logger.Warnw("skipping context: failed to build REST config",
				"context", ctxName,
				"error", err,
			)
			continue
		}
		cs, err := kubernetes.NewForConfig(restConfig)
		if err != nil {
			cm.logger.Warnw("skipping context: failed to create clientset",
				"context", ctxName,
				"error", err,
			)
			continue
		}
		clients[ctxName] = &contextClient{clientset: cs, restConfig: restConfig}
	}

	cm.clients = clients
	cm.currentCtx = merged.CurrentContext
	cm.logger.Infow("client manager rebuilt",
		"contexts", len(cm.clients),
		"current_context", cm.currentCtx,
	)
	return nil
}

// mergedConfig returns the merged api.Config (file + dynamic) without locking.
// Callers must hold the appropriate lock.
func (cm *ClientManager) mergedConfig() (*clientcmdapi.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{})
	rawConfig, err := loader.RawConfig()
	if err != nil {
		return nil, err
	}

	if cm.dynamicConfig != nil {
		mergeConfigs(&rawConfig, cm.dynamicConfig, true)
	}

	return &rawConfig, nil
}

// mergeConfigs copies clusters, contexts, and auth-infos from src into dst.
// When overwrite is true existing dst entries are replaced; when false they
// are kept. The current-context from src is applied when overwrite is true
// and src.CurrentContext is non-empty, or when dst.CurrentContext is empty.
func mergeConfigs(dst, src *clientcmdapi.Config, overwrite bool) {
	for name, cluster := range src.Clusters {
		if _, exists := dst.Clusters[name]; !exists || overwrite {
			clusterCopy := *cluster
			dst.Clusters[name] = &clusterCopy
		}
	}
	for name, ctx := range src.Contexts {
		if _, exists := dst.Contexts[name]; !exists || overwrite {
			ctxCopy := *ctx
			dst.Contexts[name] = &ctxCopy
		}
	}
	for name, auth := range src.AuthInfos {
		if _, exists := dst.AuthInfos[name]; !exists || overwrite {
			authCopy := *auth
			dst.AuthInfos[name] = &authCopy
		}
	}
	if src.CurrentContext != "" && (overwrite || dst.CurrentContext == "") {
		dst.CurrentContext = src.CurrentContext
	}
}
