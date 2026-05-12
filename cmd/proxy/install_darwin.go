//go:build darwin

package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/install"
)

// runInstall computes a Plan and either applies it (when root) or prints a
// human summary plus a sudo snippet for the user to run themselves (when not
// root). Re-running after the manual sudo block performs the verification:
// an empty plan exits 0 with "already up-to-date".
func runInstall(args []string) int {
	opts, err := parseInstallFlags("install", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	plan, err := install.ComputeInstallPlan(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "computing plan: %v\n", err)
		return 1
	}
	plan.PrintWhat(os.Stdout)
	if plan.IsEmpty() {
		fmt.Println()
		install.ComputeStatus(opts, defaultDaemonHTTP).Print(os.Stdout, opts)
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Println()
		plan.PrintShellCommands(os.Stdout)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "(install requires root; nothing was changed.)")
		return 1
	}
	if err := plan.Apply(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "apply: %v\n", err)
		return 1
	}
	fmt.Println()
	install.ComputeStatus(opts, defaultDaemonHTTP).Print(os.Stdout, opts)
	return 0
}

// runStatus prints the install + daemon status. No privilege required;
// I/O is read-only.
func runStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	clusterDomain := fs.String("cluster-domain", install.DefaultClusterDomain, "Cluster domain (looks up /etc/resolver/<domain>)")
	dnsPort := fs.Int("dns-port", install.DefaultDNSPort, "Expected DNS port (compared against /etc/resolver/<domain>'s port line)")
	poolCIDR := fs.String("pool-cidr", install.DefaultPoolCIDR, "Expected loopback /24 CIDR")
	poolSize := fs.Int("pool-size", install.DefaultPoolSize, "Expected number of addresses aliased")
	iface := fs.String("interface", install.DefaultLoopbackIface, "Loopback interface to inspect")
	daemonHTTP := fs.String("daemon-http", defaultDaemonHTTP, "Daemon HTTP control address to probe")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: k8s-service-proxy status [flags]")
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	opts := install.Options{
		ClusterDomain: *clusterDomain,
		DNSPort:       *dnsPort,
		PoolCIDR:      *poolCIDR,
		PoolSize:      *poolSize,
		LoopbackIface: *iface,
	}.WithDefaults()
	if err := opts.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid arguments: %v\n", err)
		return 2
	}
	install.ComputeStatus(opts, *daemonHTTP).Print(os.Stdout, opts)
	return 0
}

// defaultDaemonHTTP is the daemon's HTTP control address by convention. Same
// as the daemon's HTTPListen default.
const defaultDaemonHTTP = "127.0.0.1:11616"

// runUninstall mirrors runInstall for the reverse direction.
func runUninstall(args []string) int {
	opts, err := parseInstallFlags("uninstall", args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	plan, err := install.ComputeUninstallPlan(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "computing plan: %v\n", err)
		return 1
	}
	plan.PrintWhat(os.Stdout)
	if plan.IsEmpty() {
		return 0
	}
	if os.Geteuid() != 0 {
		fmt.Println()
		plan.PrintShellCommands(os.Stdout)
		fmt.Fprintln(os.Stderr)
		fmt.Fprintln(os.Stderr, "(uninstall requires root; nothing was changed.)")
		return 1
	}
	if err := plan.Apply(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "apply: %v\n", err)
		return 1
	}
	return 0
}

// parseInstallFlags parses the shared flag set used by install and uninstall.
func parseInstallFlags(name string, args []string) (install.Options, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)

	clusterDomain := fs.String("cluster-domain", install.DefaultClusterDomain, "Cluster domain to install /etc/resolver/<domain> for")
	dnsPort := fs.Int("dns-port", install.DefaultDNSPort, "DNS port the daemon listens on; written into the resolver file")
	poolCIDR := fs.String("pool-cidr", install.DefaultPoolCIDR, "Loopback /24 CIDR to pre-alias")
	poolSize := fs.Int("pool-size", install.DefaultPoolSize, "Number of addresses from the start of the CIDR to alias (1..255)")
	iface := fs.String("interface", install.DefaultLoopbackIface, "Loopback interface to alias VIPs onto")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: k8s-service-proxy %s [flags]\n\n", name)
		fmt.Fprintln(os.Stderr, "Flags:")
		fs.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\nReverse with: k8s-service-proxy %s\n",
			map[string]string{"install": "uninstall", "uninstall": "install"}[name])
	}

	if err := fs.Parse(args); err != nil {
		return install.Options{}, err
	}

	opts := install.Options{
		ClusterDomain: *clusterDomain,
		DNSPort:       *dnsPort,
		PoolCIDR:      *poolCIDR,
		PoolSize:      *poolSize,
		LoopbackIface: *iface,
	}.WithDefaults()

	if err := opts.Validate(); err != nil {
		return install.Options{}, fmt.Errorf("invalid arguments: %w", err)
	}
	return opts, nil
}
