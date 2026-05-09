// Status computation for the install model. Portable (no privileged
// operations) so the daemon can serve the same shape from /status that the
// `status` subcommand prints. Mutating ops live in install_darwin.go.

package install

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	netUrl "net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/api"
)

// Status captures the live state relevant to the install model: what's on
// disk, what's aliased on the loopback interface, and whether the daemon is
// running on the configured HTTP control port. JSON-tagged so the daemon's
// /status endpoint can round-trip it as the wire shape.
type Status struct {
	Opts Options `json:"-"`

	// Resolver file
	ResolverPath    string `json:"resolver_path"`
	ResolverPresent bool   `json:"resolver_present"`
	ResolverPort    int    `json:"resolver_port,omitempty"`

	// lo0 alias pool
	PoolTotal   int    `json:"pool_total"`
	PoolPresent int    `json:"pool_present"`
	PoolFirst   string `json:"pool_first,omitempty"`
	PoolLast    string `json:"pool_last,omitempty"`

	// Daemon HTTP control plane
	DaemonHTTP       string              `json:"daemon_http"`
	DaemonReachable  bool                `json:"daemon_reachable"`
	DaemonReady      bool                `json:"daemon_ready"`
	DaemonProbeError string              `json:"daemon_probe_error,omitempty"`
	DaemonSettings   *api.StatusResponse `json:"daemon_settings,omitempty"`

	// Listener process — populated even when /status is unavailable, so older
	// daemons or sidecar deployments still surface a PID.
	DaemonPID     int    `json:"daemon_pid,omitempty"`
	DaemonCommand string `json:"daemon_command,omitempty"`
	DaemonUptime  string `json:"daemon_uptime,omitempty"`

	// Contexts is the per-context view of the daemon's merged kubeconfig:
	// names, server URL, proxy-url + reachability, user, namespace, and
	// whether the entry is the current-context.
	Contexts        []ContextStatus `json:"contexts"`
	CurrentContext  string          `json:"current_context,omitempty"`
	KubeconfigError string          `json:"kubeconfig_error,omitempty"`
}

// ContextStatus describes one entry from the daemon's merged kubeconfig and
// the reachability of its `proxy-url` (TCP-level only — we do not speak
// HTTP CONNECT or SOCKS5 to verify the upstream actually proxies).
type ContextStatus struct {
	Name        string `json:"name"`              // kubeconfig context name
	ClusterName string `json:"cluster"`           // the cluster this context references
	Server      string `json:"server,omitempty"`  // cluster.server
	ProxyURL    string `json:"proxy_url,omitempty"`
	User        string `json:"user,omitempty"`     // context.user
	Namespace   string `json:"namespace,omitempty"` // context.namespace
	IsCurrent   bool   `json:"is_current,omitempty"`

	// Probe — only populated when ProxyURL is non-empty.
	ProxyHostPort string `json:"proxy_host_port,omitempty"`
	ProxyOK       bool   `json:"proxy_ok,omitempty"`
	ProxyError    string `json:"proxy_error,omitempty"`
}

// ServerInternals captures the runtime state the daemon already knows about
// itself, so ComputeStatusForServer can build a Status without HTTP-probing
// its own endpoints.
type ServerInternals struct {
	HTTPAddr       string
	Settings       *api.StatusResponse  // the daemon's effective config
	Kubeconfig     *clientcmdapi.Config // merged kubeconfig
	CurrentContext string
	StartTime      time.Time
	Ready          bool
	PID            int
	Command        string
}

// ComputeStatus reads the host state and returns a populated Status. opts
// supplies expected values; daemonHTTP is the address probed for /healthz
// and /readyz (e.g. "127.0.0.1:11616"). Best-effort: I/O errors are recorded
// in DaemonProbeError but never returned.
//
// Used by the `status` subcommand from outside the daemon.
func ComputeStatus(opts Options, daemonHTTP string) Status {
	opts = opts.WithDefaults()
	s := Status{
		Opts:         opts,
		ResolverPath: opts.ResolverPath(),
		DaemonHTTP:   daemonHTTP,
	}

	readResolverInto(&s)
	readPoolInto(&s, opts)

	client := &http.Client{Timeout: 1 * time.Second}
	if resp, err := client.Get("http://" + daemonHTTP + "/healthz"); err == nil {
		s.DaemonReachable = resp.StatusCode == http.StatusOK
		_ = resp.Body.Close()
	} else {
		s.DaemonProbeError = err.Error()
	}
	if s.DaemonReachable {
		s.DaemonPID = findListenerPID(daemonHTTP)
		if s.DaemonPID > 0 {
			s.DaemonCommand = processCommand(s.DaemonPID)
			s.DaemonUptime = processUptime(s.DaemonPID)
		}
		if resp, err := client.Get("http://" + daemonHTTP + "/readyz"); err == nil {
			s.DaemonReady = resp.StatusCode == http.StatusOK
			_ = resp.Body.Close()
		}
		if resp, err := client.Get("http://" + daemonHTTP + "/status"); err == nil {
			if resp.StatusCode == http.StatusOK {
				// /status now returns a rich install.Status (the same shape
				// we're building); pluck out DaemonSettings since the rest
				// (resolver, pool, contexts) is already populated locally.
				var srv Status
				if err := json.NewDecoder(resp.Body).Decode(&srv); err == nil {
					s.DaemonSettings = srv.DaemonSettings
				}
			}
			_ = resp.Body.Close()
		}
		s.Contexts, s.CurrentContext, s.KubeconfigError = discoverContexts(daemonHTTP)
	}

	return s
}

// ComputeStatusForServer is the daemon-side equivalent of ComputeStatus. It
// reads on-disk install state directly (resolver file, lo0 aliases) and uses
// the supplied internals for daemon fields rather than probing /healthz,
// /readyz, /status, /kubeconfig over HTTP. Each unique proxy-url referenced
// by a context is TCP-probed once.
func ComputeStatusForServer(opts Options, in ServerInternals) Status {
	opts = opts.WithDefaults()
	s := Status{
		Opts:            opts,
		ResolverPath:    opts.ResolverPath(),
		DaemonHTTP:      in.HTTPAddr,
		DaemonReachable: true,
		DaemonReady:     in.Ready,
		DaemonSettings:  in.Settings,
		DaemonPID:       in.PID,
		DaemonCommand:   in.Command,
	}
	if !in.StartTime.IsZero() {
		s.DaemonUptime = humanUptime(time.Since(in.StartTime))
	}

	readResolverInto(&s)
	readPoolInto(&s, opts)

	s.Contexts, s.CurrentContext, s.KubeconfigError = contextsFromAPIConfig(in.Kubeconfig, in.CurrentContext)
	return s
}

// readResolverInto reads the per-domain resolver file and fills
// ResolverPresent/ResolverPort on s. Best-effort.
func readResolverInto(s *Status) {
	data, err := os.ReadFile(s.ResolverPath)
	if err != nil {
		return
	}
	s.ResolverPresent = true
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "port" {
			if p, err := strconv.Atoi(fields[1]); err == nil {
				s.ResolverPort = p
				break
			}
		}
	}
}

// readPoolInto fills PoolTotal/PoolPresent/PoolFirst/PoolLast on s by listing
// addresses currently aliased on opts.LoopbackIface. Best-effort.
func readPoolInto(s *Status, opts Options) {
	ips, err := PoolIPs(opts.PoolCIDR, opts.PoolSize)
	if err != nil || len(ips) == 0 {
		return
	}
	s.PoolTotal = len(ips)
	s.PoolFirst = ips[0].String()
	s.PoolLast = ips[len(ips)-1].String()
	present, err := loopbackAddresses(opts.LoopbackIface)
	if err != nil {
		return
	}
	for _, ip := range ips {
		if _, ok := present[ip.String()]; ok {
			s.PoolPresent++
		}
	}
}

// discoverContexts fetches /kubeconfig, parses it, and returns one
// ContextStatus per kubeconfig context. Each unique proxy-url is TCP-probed
// at most once and the result is cached across all contexts that use it.
//
// On HTTP/parse error returns an explanatory string in errMsg; the slice and
// current are empty in that case.
func discoverContexts(daemonHTTP string) (contexts []ContextStatus, current, errMsg string) {
	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get("http://" + daemonHTTP + "/kubeconfig")
	if err != nil {
		return nil, "", err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Sprintf("/kubeconfig returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", err.Error()
	}
	cfg, err := clientcmd.Load(body)
	if err != nil {
		return nil, "", fmt.Sprintf("parsing kubeconfig: %v", err)
	}
	return contextsFromAPIConfig(cfg, cfg.CurrentContext)
}

// contextsFromAPIConfig builds a sorted []ContextStatus from an in-memory
// kubeconfig and TCP-probes each unique proxy-url at most once.
func contextsFromAPIConfig(cfg *clientcmdapi.Config, currentCtx string) (contexts []ContextStatus, current, errMsg string) {
	if cfg == nil {
		return nil, currentCtx, ""
	}
	type probeResult struct {
		hostPort string
		ok       bool
		err      string
	}
	probeCache := map[string]probeResult{}
	probe := func(url string) probeResult {
		if r, ok := probeCache[url]; ok {
			return r
		}
		r := probeResult{}
		hostPort, perr := proxyHostPort(url)
		if perr != nil {
			r.err = perr.Error()
			probeCache[url] = r
			return r
		}
		r.hostPort = hostPort
		conn, dialErr := net.DialTimeout("tcp", hostPort, 500*time.Millisecond)
		if dialErr != nil {
			r.err = dialErr.Error()
		} else {
			r.ok = true
			_ = conn.Close()
		}
		probeCache[url] = r
		return r
	}

	cur := currentCtx
	if cur == "" {
		cur = cfg.CurrentContext
	}
	for name, ctx := range cfg.Contexts {
		if ctx == nil {
			continue
		}
		c := ContextStatus{
			Name:        name,
			ClusterName: ctx.Cluster,
			User:        ctx.AuthInfo,
			Namespace:   ctx.Namespace,
			IsCurrent:   name == cur,
		}
		if cluster := cfg.Clusters[ctx.Cluster]; cluster != nil {
			c.Server = cluster.Server
			c.ProxyURL = cluster.ProxyURL
		}
		if c.ProxyURL != "" {
			r := probe(c.ProxyURL)
			c.ProxyHostPort = r.hostPort
			c.ProxyOK = r.ok
			c.ProxyError = r.err
		}
		contexts = append(contexts, c)
	}
	sort.Slice(contexts, func(i, j int) bool { return contexts[i].Name < contexts[j].Name })
	return contexts, cur, ""
}

// proxyHostPort returns the host:port to dial for a kubeconfig proxy-url,
// applying the scheme's default port when omitted.
func proxyHostPort(raw string) (string, error) {
	u, err := netUrl.Parse(raw)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxy-url %q has no host", raw)
	}
	if strings.Contains(u.Host, ":") {
		return u.Host, nil
	}
	switch u.Scheme {
	case "https":
		return u.Host + ":443", nil
	case "socks5", "socks5h":
		return u.Host + ":1080", nil
	default:
		return u.Host + ":80", nil
	}
}

// findListenerPID returns the PID of the process listening on the TCP host:port.
// Uses lsof; returns 0 when lsof is unavailable, errors, or finds no listener.
func findListenerPID(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0
	}
	cmd := exec.Command("lsof", "-nP", "-iTCP:"+port, "-sTCP:LISTEN")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(out), "\n")[1:] { // skip header
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		if pid, err := strconv.Atoi(fields[1]); err == nil {
			return pid
		}
	}
	return 0
}

// processCommand returns the full command line for pid, or "" when ps fails.
func processCommand(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// processUptime returns the elapsed-time string (`ps etime=`) for pid, or ""
// when ps fails. Format follows ps conventions: `[[dd-]hh:]mm:ss`.
func processUptime(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "etime=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// humanUptime formats d in the same `[[dd-]hh:]mm:ss` shape as `ps etime=`,
// for parity between ComputeStatus (probes via ps) and ComputeStatusForServer
// (knows time.Since(start)).
func humanUptime(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d.Seconds())
	days := total / 86400
	total %= 86400
	hours := total / 3600
	total %= 3600
	mins := total / 60
	secs := total % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%d-%02d:%02d:%02d", days, hours, mins, secs)
	case hours > 0:
		return fmt.Sprintf("%02d:%02d:%02d", hours, mins, secs)
	default:
		return fmt.Sprintf("%02d:%02d", mins, secs)
	}
}

// loopbackAddresses returns the set of IPv4 addresses currently aliased to
// the named interface, keyed by IP string.
func loopbackAddresses(iface string) (map[string]struct{}, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return nil, err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(addrs))
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if v4 := ip.To4(); v4 != nil {
			out[v4.String()] = struct{}{}
		}
	}
	return out, nil
}

// reExecPath returns a path the user can run to re-invoke the current binary.
// Falls back to a leading "./" when os.Args[0] looks like a bare program name
// from the local directory, so the snippet is unambiguous when copy-pasted.
func reExecPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "./k8s-service-proxy"
	}
	cwd, err := os.Getwd()
	if err == nil {
		if rel, err := filepath.Rel(cwd, exe); err == nil && !strings.HasPrefix(rel, "..") {
			return "./" + rel
		}
	}
	return exe
}

// envOverrides returns the list of `KEY=VALUE` strings the user must set to
// run the daemon with this install's settings — i.e. only the env vars whose
// values differ from app.Config defaults. Same defaults as
// app/config.go:NewConfigFromEnvironment.
func (o Options) envOverrides() []string {
	const (
		defaultInterface = "lo0"
		defaultPoolCIDR  = "127.50.0.0/24"
		defaultDNSPort   = 11617
	)
	var out []string
	if o.LoopbackIface != defaultInterface {
		out = append(out, fmt.Sprintf("INTERFACE=%s", o.LoopbackIface))
	}
	if o.PoolCIDR != defaultPoolCIDR {
		out = append(out, fmt.Sprintf("VIP_CIDR=%s", o.PoolCIDR))
	}
	if o.DNSPort != defaultDNSPort {
		out = append(out, fmt.Sprintf("DNS_LISTEN=127.0.0.1:%d", o.DNSPort))
	}
	return out
}

// daemonAttrs returns the parenthesised attribute list shown after
// "running on <addr>" — e.g. "(pid 1234, ready)" — or "(<state>)" when no
// PID was discovered.
func (s Status) daemonAttrs(state string) string {
	if s.DaemonPID == 0 {
		return "(" + state + ")"
	}
	return fmt.Sprintf("(pid %d, %s)", s.DaemonPID, state)
}

// printProcessLine writes the full command line and uptime under the daemon
// header line, indented to match printDaemonSettings.
func (s Status) printProcessLine(w io.Writer) {
	if s.DaemonCommand == "" {
		return
	}
	uptime := s.DaemonUptime
	if uptime == "" {
		uptime = "?"
	}
	fmt.Fprintf(w, "       %-17s %s\n", "process", s.DaemonCommand)
	fmt.Fprintf(w, "       %-17s %s\n", "uptime", uptime)
}

// printDaemonSettings writes the daemon's effective runtime config under the
// "running on …" line, indented for readability. Quiet when /status didn't
// return — older daemons or non-darwin builds — so the report degrades
// gracefully.
func (s Status) printDaemonSettings(w io.Writer) {
	d := s.DaemonSettings
	if d == nil {
		return
	}
	row := func(label, value string) {
		fmt.Fprintf(w, "       %-17s %s\n", label, value)
	}
	row("interface", d.Interface)
	row("vip_cidr", d.VIPCIDR)
	row("vip_alias_mode", d.VIPAliasMode)
	if d.VIPIdleTimeout != "" && d.VIPIdleTimeout != "0s" {
		row("vip_idle_timeout", d.VIPIdleTimeout)
	}
	row("cluster_domain", d.ClusterDomain)
	row("log_level", d.LogLevel)
	row("http_listen", d.HTTPListen)
	switch {
	case d.DNSEnabled && d.DNSListen != "":
		row("dns_listen", d.DNSListen+" (enabled)")
	case d.DNSEnabled:
		row("dns_listen", "(enabled but address not bound)")
	default:
		row("dns_listen", "(disabled)")
	}
	switch {
	case d.SOCKSEnabled && d.SOCKSListen != "":
		row("socks_listen", d.SOCKSListen+" (enabled — point ALL_PROXY at socks5://"+d.SOCKSListen+")")
	case d.SOCKSEnabled:
		row("socks_listen", "(enabled but address not bound)")
	default:
		row("socks_listen", "(disabled — start daemon with --socks to expose SOCKS5)")
	}
}

// Print writes a multi-section status report. Each line is prefixed with a
// state marker (`[ok]`, `[--]`, `[!!]`) so output is parseable by eye in any
// terminal regardless of unicode support.
func (s Status) Print(w io.Writer) {
	fmt.Fprintln(w, "Install")
	if s.ResolverPresent {
		if s.ResolverPort == s.Opts.DNSPort {
			fmt.Fprintf(w, "  [ok] resolver: %s (port %d)\n", s.ResolverPath, s.ResolverPort)
		} else {
			fmt.Fprintf(w, "  [!!] resolver: %s (port %d, expected %d) — re-run `%s install`\n",
				s.ResolverPath, s.ResolverPort, s.Opts.DNSPort, reExecPath())
		}
	} else {
		fmt.Fprintf(w, "  [--] resolver: %s missing — run `%s install`\n",
			s.ResolverPath, reExecPath())
	}

	switch {
	case s.PoolTotal == 0:
		fmt.Fprintln(w, "  [!!] pool: configured size is 0")
	case s.PoolPresent == s.PoolTotal:
		fmt.Fprintf(w, "  [ok] pool: %d/%d aliased on %s (%s..%s)\n",
			s.PoolPresent, s.PoolTotal, s.Opts.LoopbackIface, s.PoolFirst, s.PoolLast)
	default:
		fmt.Fprintf(w, "  [--] pool: %d/%d aliased on %s, %d missing — run `%s install`\n",
			s.PoolPresent, s.PoolTotal, s.Opts.LoopbackIface, s.PoolTotal-s.PoolPresent, reExecPath())
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Daemon")
	switch {
	case s.DaemonReachable && s.DaemonReady:
		fmt.Fprintf(w, "  [ok] running on %s %s\n", s.DaemonHTTP, s.daemonAttrs("ready"))
		s.printProcessLine(w)
		s.printDaemonSettings(w)
	case s.DaemonReachable:
		fmt.Fprintf(w, "  [..] running on %s %s\n", s.DaemonHTTP, s.daemonAttrs("starting; /readyz not yet 200"))
		s.printProcessLine(w)
		s.printDaemonSettings(w)
	default:
		fmt.Fprintf(w, "  [--] not running on %s\n", s.DaemonHTTP)
		overrides := s.Opts.envOverrides()
		if len(overrides) == 0 {
			fmt.Fprintf(w, "       Start: %s --dns\n", reExecPath())
		} else {
			fmt.Fprintln(w, "       Start:")
			for _, o := range overrides {
				fmt.Fprintf(w, "         %s \\\n", o)
			}
			fmt.Fprintf(w, "         %s --dns\n", reExecPath())
		}
	}

	// Registered contexts — only meaningful when the daemon is up.
	if !s.DaemonReachable {
		return
	}
	fmt.Fprintln(w)
	header := "Registered contexts (from /kubeconfig)"
	if s.CurrentContext != "" {
		header = fmt.Sprintf("Registered contexts (from /kubeconfig; current: %s)", s.CurrentContext)
	}
	fmt.Fprintln(w, header)
	switch {
	case s.KubeconfigError != "":
		fmt.Fprintf(w, "  [!!] %s\n", s.KubeconfigError)
	case len(s.Contexts) == 0:
		fmt.Fprintln(w, "  none registered (POST /kubeconfig to add)")
	default:
		for i, c := range s.Contexts {
			if i > 0 {
				fmt.Fprintln(w)
			}
			c.print(w)
		}
	}
}

// print writes one context's details over a few aligned lines.
func (c ContextStatus) print(w io.Writer) {
	marker, suffix := contextMarker(c)
	if c.IsCurrent {
		suffix += " (current)"
	}
	fmt.Fprintf(w, "  %s %s%s\n", marker, c.Name, suffix)
	row := func(label, value string) {
		fmt.Fprintf(w, "         %-10s %s\n", label, value)
	}
	if c.ClusterName != "" && c.ClusterName != c.Name {
		row("cluster:", c.ClusterName)
	}
	if c.Server != "" {
		row("server:", c.Server)
	}
	switch {
	case c.ProxyURL != "" && c.ProxyOK:
		row("proxy-url:", c.ProxyURL+" (reachable)")
	case c.ProxyURL != "" && c.ProxyError != "":
		row("proxy-url:", c.ProxyURL+" ("+c.ProxyError+")")
	case c.ProxyURL != "":
		row("proxy-url:", c.ProxyURL)
	default:
		row("proxy-url:", "(none — direct)")
	}
	if c.User != "" {
		row("user:", c.User)
	}
	if c.Namespace != "" {
		row("namespace:", c.Namespace)
	}
}

// contextMarker returns the leading state marker for a context.
func contextMarker(c ContextStatus) (marker, suffix string) {
	switch {
	case c.ProxyURL == "":
		return "[ok]", ""
	case c.ProxyOK:
		return "[ok]", ""
	default:
		return "[--]", " (proxy unreachable)"
	}
}
