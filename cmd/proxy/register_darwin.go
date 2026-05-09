//go:build darwin

package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/install"
	"github.com/fealebenpae/kube-forwarding-proxy/internal/k8s"
)

// runRegister reads a kubeconfig file from disk and submits it to the daemon's
// /kubeconfig endpoint. On success it prints a confirmation line and the full
// status block (so the user sees reachability of every newly-registered
// proxy-url). The default method is PATCH (merge with overwrite) — re-running
// against the same file is idempotent.
func runRegister(args []string) int {
	fs := flag.NewFlagSet("register", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	daemonHTTP := fs.String("daemon-http", defaultDaemonHTTP, "Daemon HTTP control address")
	method := fs.String("method", http.MethodPatch, "HTTP method: PATCH (merge), PUT (replace), or POST (append)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: k8s-service-proxy register [flags] <path-to-kubeconfig>")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rest := fs.Args()
	if len(rest) != 1 {
		fs.Usage()
		return 2
	}
	path := rest[0]

	switch *method {
	case http.MethodPatch, http.MethodPut, http.MethodPost:
	default:
		fmt.Fprintf(os.Stderr, "register: unsupported -method %q (want PATCH, PUT, or POST)\n", *method)
		return 2
	}

	body, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: reading %s: %v\n", path, err)
		return 1
	}

	cfg, err := clientcmd.Load(body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: %s is not a valid kubeconfig: %v\n", path, err)
		return 1
	}

	url := "http://" + *daemonHTTP + "/kubeconfig"
	req, err := http.NewRequest(*method, url, bytes.NewReader(body))
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: building request: %v\n", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/yaml")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "register: %s %s: %v\n", *method, url, err)
		fmt.Fprintln(os.Stderr, "(is the daemon running? try 'k8s-service-proxy status')")
		return 1
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNoContent {
		fmt.Fprintf(os.Stderr, "register: daemon responded %s: %s\n", resp.Status, bytes.TrimSpace(respBody))
		return 1
	}

	// Preview the names the daemon keys entries by — the registrant rewrite
	// suffixes each context/cluster/user with the cluster's proxy-url port
	// so contexts from different worktrees (different tunnel ports) stay
	// independently addressable. Re-registering the same context name on
	// the same tunnel port overwrites the previous entry in place.
	// When no proxy-url is set, the rewrite is a no-op and
	// submitted == registered.
	type pair struct{ submitted, registered string }
	pairs := make([]pair, 0, len(cfg.Contexts))
	for name, ctx := range cfg.Contexts {
		registered := name
		if ctx != nil {
			if cluster, ok := cfg.Clusters[ctx.Cluster]; ok {
				if s := k8s.DerivePortSuffix(cluster); s != "" && !strings.HasSuffix(name, "-"+s) {
					registered = name + "-" + s
				}
			}
		}
		pairs = append(pairs, pair{name, registered})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].submitted < pairs[j].submitted })

	fmt.Printf("Registered %s via %s %s\n", path, *method, url)
	fmt.Printf("  contexts (submitted -> registered): %d\n", len(pairs))
	for _, p := range pairs {
		if p.submitted == p.registered {
			fmt.Printf("    - %s\n", p.submitted)
		} else {
			fmt.Printf("    - %s -> %s\n", p.submitted, p.registered)
		}
	}
	fmt.Println()

	opts := install.Options{}.WithDefaults()
	install.ComputeStatus(opts, *daemonHTTP).Print(os.Stdout)
	return 0
}
