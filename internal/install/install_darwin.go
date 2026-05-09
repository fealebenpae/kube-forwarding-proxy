//go:build darwin

// Privileged macOS host setup: writing /etc/resolver/<cluster-domain> and
// adding/removing IP aliases on the loopback interface via `ifconfig`.
// Read-only Status computation (resolver presence, pool listing, process
// lookup, kubeconfig context probing, render to text) lives in
// install_status.go and is portable so the daemon can serve the same shape
// from /status on any platform.

package install

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Plan describes what `install` would change on this host. Plans are computed
// from on-disk and kernel state via ComputeInstallPlan; they are pure data
// once produced and can be inspected, printed as a human summary, or rendered
// as a copy-pasteable sudo snippet.
type Plan struct {
	Opts Options

	ResolverPath        string
	CurrentResolverBody string // empty when the file is missing
	DesiredResolverBody string

	PoolToAdd []net.IP // pool addresses missing from the loopback interface
}

// UninstallPlan describes what `uninstall` would remove.
type UninstallPlan struct {
	Opts Options

	ResolverPath    string
	ResolverPresent bool

	PoolToRemove []net.IP // pool addresses currently aliased on the loopback interface
}

// ComputeInstallPlan reads the current host state (resolver file + lo0 aliases)
// and returns the diff against the desired state implied by opts.
func ComputeInstallPlan(opts Options) (Plan, error) {
	opts = opts.WithDefaults()
	if err := opts.Validate(); err != nil {
		return Plan{}, err
	}

	p := Plan{
		Opts:                opts,
		ResolverPath:        opts.ResolverPath(),
		DesiredResolverBody: opts.ResolverBody(),
	}

	if existing, err := os.ReadFile(p.ResolverPath); err == nil {
		p.CurrentResolverBody = string(existing)
	}

	ips, err := PoolIPs(opts.PoolCIDR, opts.PoolSize)
	if err != nil {
		return Plan{}, err
	}
	present, err := loopbackAddresses(opts.LoopbackIface)
	if err != nil {
		return Plan{}, fmt.Errorf("listing %s addresses: %w", opts.LoopbackIface, err)
	}
	for _, ip := range ips {
		if _, ok := present[ip.String()]; !ok {
			p.PoolToAdd = append(p.PoolToAdd, ip)
		}
	}
	return p, nil
}

// ComputeUninstallPlan returns the diff for the reverse direction.
func ComputeUninstallPlan(opts Options) (UninstallPlan, error) {
	opts = opts.WithDefaults()
	if err := opts.Validate(); err != nil {
		return UninstallPlan{}, err
	}

	p := UninstallPlan{
		Opts:         opts,
		ResolverPath: opts.ResolverPath(),
	}
	if _, err := os.Stat(p.ResolverPath); err == nil {
		p.ResolverPresent = true
	}

	ips, err := PoolIPs(opts.PoolCIDR, opts.PoolSize)
	if err != nil {
		return UninstallPlan{}, err
	}
	present, err := loopbackAddresses(opts.LoopbackIface)
	if err != nil {
		return UninstallPlan{}, fmt.Errorf("listing %s addresses: %w", opts.LoopbackIface, err)
	}
	for _, ip := range ips {
		if _, ok := present[ip.String()]; ok {
			p.PoolToRemove = append(p.PoolToRemove, ip)
		}
	}
	return p, nil
}

// resolverNeedsWrite is true when the file is missing or its bytes don't match
// the desired body exactly.
func (p Plan) resolverNeedsWrite() bool {
	return p.CurrentResolverBody != p.DesiredResolverBody
}

// IsEmpty reports whether applying this plan would change anything.
func (p Plan) IsEmpty() bool {
	return !p.resolverNeedsWrite() && len(p.PoolToAdd) == 0
}

// IsEmpty reports whether applying this uninstall plan would change anything.
func (p UninstallPlan) IsEmpty() bool {
	return !p.ResolverPresent && len(p.PoolToRemove) == 0
}

// PrintWhat writes a short human-readable summary of the planned changes.
func (p Plan) PrintWhat(w io.Writer) {
	if p.IsEmpty() {
		fmt.Fprintln(w, "Already up to date — nothing to do.")
		return
	}
	fmt.Fprintln(w, "The following privileged changes are needed:")
	if p.resolverNeedsWrite() {
		action := "create"
		if p.CurrentResolverBody != "" {
			action = "rewrite"
		}
		fmt.Fprintf(w, "  • %s %s (port %d, two lines)\n", action, p.ResolverPath, p.Opts.DNSPort)
	}
	if n := len(p.PoolToAdd); n > 0 {
		fmt.Fprintf(w, "  • alias %d address(es) to %s (range: %s … %s)\n",
			n, p.Opts.LoopbackIface, p.PoolToAdd[0], p.PoolToAdd[len(p.PoolToAdd)-1])
	}
}

// PrintWhat writes a short human-readable summary of the uninstall plan.
func (p UninstallPlan) PrintWhat(w io.Writer) {
	if p.IsEmpty() {
		fmt.Fprintln(w, "Already uninstalled — nothing to do.")
		return
	}
	fmt.Fprintln(w, "The following privileged changes are needed:")
	if p.ResolverPresent {
		fmt.Fprintf(w, "  • remove %s\n", p.ResolverPath)
	}
	if n := len(p.PoolToRemove); n > 0 {
		fmt.Fprintf(w, "  • un-alias %d address(es) from %s (range: %s … %s)\n",
			n, p.Opts.LoopbackIface, p.PoolToRemove[0], p.PoolToRemove[len(p.PoolToRemove)-1])
	}
}

// PrintShellCommands writes a copy-pasteable sudo snippet that performs the
// install actions, followed by a re-exec of the binary (without sudo) so the
// user can verify the resulting state.
func (p Plan) PrintShellCommands(w io.Writer) {
	if p.IsEmpty() {
		return
	}
	fmt.Fprintln(w, "Run with sudo:")
	fmt.Fprintln(w)
	// Inner heredoc lines are written verbatim into the resolver file, so the
	// snippet body is unindented. The inner closing tag must also sit at col 0.
	fmt.Fprintln(w, "sudo /bin/sh <<'KFP_INSTALL'")
	fmt.Fprintln(w, "set -e")
	if p.resolverNeedsWrite() {
		fmt.Fprintf(w, "mkdir -p %s\n", filepath.Dir(p.ResolverPath))
		fmt.Fprintf(w, "cat > %s <<'KFP_RESOLVER'\n", p.ResolverPath)
		for _, line := range strings.Split(strings.TrimRight(p.DesiredResolverBody, "\n"), "\n") {
			fmt.Fprintln(w, line)
		}
		fmt.Fprintln(w, "KFP_RESOLVER")
	}
	writePoolLoop(w, p.Opts.LoopbackIface, "alias", p.PoolToAdd)
	fmt.Fprintln(w, "KFP_INSTALL")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Then verify (no sudo):")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s install\n", reExecPath())
}

// PrintShellCommands for uninstall.
func (p UninstallPlan) PrintShellCommands(w io.Writer) {
	if p.IsEmpty() {
		return
	}
	fmt.Fprintln(w, "Run with sudo:")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "sudo /bin/sh <<'KFP_UNINSTALL'")
	fmt.Fprintln(w, "set -e")
	if p.ResolverPresent {
		fmt.Fprintf(w, "rm -f %s\n", p.ResolverPath)
	}
	writePoolLoop(w, p.Opts.LoopbackIface, "-alias", p.PoolToRemove)
	fmt.Fprintln(w, "KFP_UNINSTALL")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Then verify (no sudo):")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  %s uninstall\n", reExecPath())
}

// Apply executes the plan against the host. Caller must already be root;
// this function does not check euid.
func (p Plan) Apply(out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if p.IsEmpty() {
		fmt.Fprintln(out, "[skip] nothing to do")
		return nil
	}
	if p.resolverNeedsWrite() {
		if err := writeResolverFile(p.ResolverPath, p.DesiredResolverBody); err != nil {
			return fmt.Errorf("resolver: %w", err)
		}
		fmt.Fprintf(out, "[install] %s\n", p.ResolverPath)
	}
	for _, ip := range p.PoolToAdd {
		if err := ifconfigAlias(p.Opts.LoopbackIface, "alias", ip); err != nil {
			return err
		}
	}
	if n := len(p.PoolToAdd); n > 0 {
		fmt.Fprintf(out, "[pool] added=%d (range: %s … %s)\n", n, p.PoolToAdd[0], p.PoolToAdd[n-1])
	}
	return nil
}

// Apply executes the uninstall plan. Caller must already be root.
func (p UninstallPlan) Apply(out io.Writer) error {
	if out == nil {
		out = io.Discard
	}
	if p.IsEmpty() {
		fmt.Fprintln(out, "[skip] nothing to do")
		return nil
	}
	if p.ResolverPresent {
		if err := os.Remove(p.ResolverPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", p.ResolverPath, err)
		}
		fmt.Fprintf(out, "[remove] %s\n", p.ResolverPath)
	}
	removed := 0
	for _, ip := range p.PoolToRemove {
		if err := ifconfigAlias(p.Opts.LoopbackIface, "-alias", ip); err != nil {
			// keep going so a partial pool gets cleaned as much as possible
			continue
		}
		removed++
	}
	if len(p.PoolToRemove) > 0 {
		fmt.Fprintf(out, "[pool] removed=%d (range: %s … %s)\n",
			removed, p.PoolToRemove[0], p.PoolToRemove[len(p.PoolToRemove)-1])
	}
	return nil
}

// writeResolverFile writes body to path via temp-file + rename. Idempotency is
// the caller's job (we always overwrite).
func writeResolverFile(path, body string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }() // no-op if rename succeeded
	if _, err := tmp.WriteString(body); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

// writePoolLoop emits a single bash `for` loop that runs `ifconfig <iface> <op>`
// over a contiguous range derived from the first and last addresses of ips.
// No-op when ips is empty.
//
// PoolToAdd / PoolToRemove are always contiguous in practice: the daemon's
// pool is sequential (.1..N) and the diff against the live interface preserves
// order, producing a contiguous suffix at most. If a user has manually
// removed an interior alias, the snippet's loop will try to re-alias it and
// `set -e` will abort partway — re-running `install` then produces a fresh
// plan covering only the still-missing addresses.
func writePoolLoop(w io.Writer, iface, op string, ips []net.IP) {
	if len(ips) == 0 {
		return
	}
	first := ips[0].To4()
	last := ips[len(ips)-1].To4()
	prefix := fmt.Sprintf("%d.%d.%d", first[0], first[1], first[2])
	fmt.Fprintf(w, "for i in $(seq %d %d); do ifconfig %s %s %s.$i; done\n",
		first[3], last[3], iface, op, prefix)
}

// ifconfigAlias runs `ifconfig <iface> <op> <ip>`. op is "alias" (add) or
// "-alias" (remove).
func ifconfigAlias(iface, op string, ip net.IP) error {
	cmd := exec.Command("ifconfig", iface, op, ip.String())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s %s %s: %s: %w", iface, op, ip, string(out), err)
	}
	return nil
}
