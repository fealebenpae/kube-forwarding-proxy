// Status computation for the install model. Portable so the daemon can
// serve the same shape from /status that the `status` subcommand prints.
// Mutating ops live in install_darwin.go.

package install

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	netUrl "net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"time"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/fealebenpae/kube-forwarding-proxy/internal/api"
)

// Status captures the live state relevant to the install model: what's on
// disk, what's aliased on the loopback interface, and whether the daemon is
// running on its configured HTTP control port. JSON-tagged so the daemon's
// /status endpoint can round-trip it as the wire shape.
type Status struct {
	// Resolver file
	ResolverPath    string `json:"resolver_path"`
	ResolverPresent bool   `json:"resolver_present"`
	ResolverPort    int    `json:"resolver_port,omitempty"`

	// Loopback alias pool
	PoolTotal   int    `json:"pool_total"`
	PoolPresent int    `json:"pool_present"`
	PoolFirst   string `json:"pool_first,omitempty"`
	PoolLast    string `json:"pool_last,omitempty"`

	// Daemon HTTP control plane
	DaemonHTTP      string              `json:"daemon_http"`
	DaemonReachable bool                `json:"daemon_reachable"`
	DaemonReady     bool                `json:"daemon_ready"`
	DaemonSettings  *api.StatusResponse `json:"daemon_settings,omitempty"`
	DaemonPID       int                 `json:"daemon_pid,omitempty"`
	DaemonCommand   string              `json:"daemon_command,omitempty"`
	DaemonUptime    string              `json:"daemon_uptime,omitempty"`

	// Per-context view of the daemon's merged kubeconfig.
	Contexts        []ContextStatus `json:"contexts"`
	CurrentContext  string          `json:"current_context,omitempty"`
	KubeconfigError string          `json:"kubeconfig_error,omitempty"`
}

// ContextStatus describes one entry from the daemon's merged kubeconfig and
// the TCP-level reachability of its proxy-url.
type ContextStatus struct {
	Name        string `json:"name"`
	ClusterName string `json:"cluster"`
	Server      string `json:"server,omitempty"`
	ProxyURL    string `json:"proxy_url,omitempty"`
	User        string `json:"user,omitempty"`
	Namespace   string `json:"namespace,omitempty"`
	IsCurrent   bool   `json:"is_current,omitempty"`
	ProxyOK     bool   `json:"proxy_ok,omitempty"`
	ProxyError  string `json:"proxy_error,omitempty"`
}

// ServerInternals captures the runtime state the daemon already knows about
// itself, so ComputeStatusForServer can build a Status without HTTP-probing.
type ServerInternals struct {
	HTTPAddr       string
	Settings       *api.StatusResponse
	Kubeconfig     *clientcmdapi.Config
	CurrentContext string
	StartTime      time.Time
	Ready          bool
	PID            int
	Command        string
}

// ComputeStatus reads local install state (resolver file, lo0 aliases) and
// asks the daemon at daemonHTTP for the rest via a single GET /status.
// Best-effort: an unreachable daemon yields a Status with DaemonReachable=false
// and local fields filled in.
func ComputeStatus(opts Options, daemonHTTP string) Status {
	opts = opts.WithDefaults()
	s := Status{ResolverPath: opts.ResolverPath(), DaemonHTTP: daemonHTTP}
	readResolverInto(&s)
	readPoolInto(&s, opts)

	client := &http.Client{Timeout: 1 * time.Second}
	resp, err := client.Get("http://" + daemonHTTP + "/status")
	if err != nil {
		return s
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return s
	}
	var srv Status
	if err := json.NewDecoder(resp.Body).Decode(&srv); err != nil {
		return s
	}
	s.DaemonReachable = true
	s.DaemonReady = srv.DaemonReady
	s.DaemonSettings = srv.DaemonSettings
	s.DaemonPID = srv.DaemonPID
	s.DaemonCommand = srv.DaemonCommand
	s.DaemonUptime = srv.DaemonUptime
	s.Contexts = srv.Contexts
	s.CurrentContext = srv.CurrentContext
	s.KubeconfigError = srv.KubeconfigError
	return s
}

// ComputeStatusForServer is the daemon-side equivalent: reads install state
// from disk directly and uses the supplied internals for daemon fields.
func ComputeStatusForServer(opts Options, in ServerInternals) Status {
	opts = opts.WithDefaults()
	s := Status{
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

// contextsFromAPIConfig builds a sorted []ContextStatus from an in-memory
// kubeconfig and TCP-probes each unique proxy-url at most once.
func contextsFromAPIConfig(cfg *clientcmdapi.Config, currentCtx string) (contexts []ContextStatus, current, errMsg string) {
	if cfg == nil {
		return nil, currentCtx, ""
	}
	type probeResult struct {
		ok  bool
		err string
	}
	cache := map[string]probeResult{}
	probe := func(url string) probeResult {
		if r, ok := cache[url]; ok {
			return r
		}
		var r probeResult
		hostPort, perr := proxyHostPort(url)
		if perr != nil {
			r.err = perr.Error()
			cache[url] = r
			return r
		}
		conn, dialErr := net.DialTimeout("tcp", hostPort, 500*time.Millisecond)
		if dialErr != nil {
			r.err = dialErr.Error()
		} else {
			r.ok = true
			_ = conn.Close()
		}
		cache[url] = r
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

// humanUptime formats d in the same [[dd-]hh:]mm:ss shape as `ps etime=`.
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

// reExecPath returns a copy-pasteable path to the current binary.
func reExecPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "./k8s-service-proxy"
	}
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(cwd, exe); err == nil && !strings.HasPrefix(rel, "..") {
			return "./" + rel
		}
	}
	return exe
}

// envOverrides returns the `KEY=VALUE` env vars needed to make the daemon
// match this install's settings (only fields that differ from defaults).
func (o Options) envOverrides() []string {
	var out []string
	if o.LoopbackIface != DefaultLoopbackIface {
		out = append(out, fmt.Sprintf("INTERFACE=%s", o.LoopbackIface))
	}
	if o.PoolCIDR != DefaultPoolCIDR {
		out = append(out, fmt.Sprintf("VIP_CIDR=%s", o.PoolCIDR))
	}
	if o.DNSPort != DefaultDNSPort {
		out = append(out, fmt.Sprintf("DNS_LISTEN=127.0.0.1:%d", o.DNSPort))
	}
	return out
}

// Print writes the multi-section status report. State markers ([ok]/[--]/[!!])
// stay ASCII for terminal portability. opts supplies the install-side "expected"
// values used to flag drift.
func (s Status) Print(w io.Writer, opts Options) {
	opts = opts.WithDefaults()
	if err := statusTmpl.Execute(w, renderView{Status: s, Opts: opts, Bin: reExecPath()}); err != nil {
		fmt.Fprintf(w, "[render error: %v]\n", err)
	}
}

// renderView is the data the template receives. Keeping helpers as methods on
// renderView keeps the template body declarative.
type renderView struct {
	Status
	Opts Options
	Bin  string
}

// Render helpers — all called from statusTmpl. They return short lines/strings
// the template stitches together so the structure stays visible.

func (v renderView) ResolverLine() string {
	switch {
	case !v.ResolverPresent:
		return fmt.Sprintf("[--] resolver: %s missing — run `%s install`", v.ResolverPath, v.Bin)
	case v.ResolverPort == v.Opts.DNSPort:
		return fmt.Sprintf("[ok] resolver: %s (port %d)", v.ResolverPath, v.ResolverPort)
	default:
		return fmt.Sprintf("[!!] resolver: %s (port %d, expected %d) — re-run `%s install`",
			v.ResolverPath, v.ResolverPort, v.Opts.DNSPort, v.Bin)
	}
}

func (v renderView) PoolLine() string {
	switch {
	case v.PoolTotal == 0:
		return "[!!] pool: configured size is 0"
	case v.PoolPresent == v.PoolTotal:
		return fmt.Sprintf("[ok] pool: %d/%d aliased on %s (%s..%s)",
			v.PoolPresent, v.PoolTotal, v.Opts.LoopbackIface, v.PoolFirst, v.PoolLast)
	default:
		return fmt.Sprintf("[--] pool: %d/%d aliased on %s, %d missing — run `%s install`",
			v.PoolPresent, v.PoolTotal, v.Opts.LoopbackIface, v.PoolTotal-v.PoolPresent, v.Bin)
	}
}

func (v renderView) DaemonHeader() string {
	attrs := func(state string) string {
		if v.DaemonPID == 0 {
			return "(" + state + ")"
		}
		return fmt.Sprintf("(pid %d, %s)", v.DaemonPID, state)
	}
	switch {
	case v.DaemonReachable && v.DaemonReady:
		return fmt.Sprintf("[ok] running on %s %s", v.DaemonHTTP, attrs("ready"))
	case v.DaemonReachable:
		return fmt.Sprintf("[..] running on %s %s", v.DaemonHTTP, attrs("starting; /readyz not yet 200"))
	default:
		return fmt.Sprintf("[--] not running on %s", v.DaemonHTTP)
	}
}

// SettingsRows returns label/value pairs for the daemon's effective config.
func (v renderView) SettingsRows() [][2]string {
	d := v.DaemonSettings
	if d == nil {
		return nil
	}
	rows := [][2]string{
		{"interface", d.Interface},
		{"vip_cidr", d.VIPCIDR},
		{"vip_alias_mode", d.VIPAliasMode},
	}
	if d.VIPIdleTimeout != "" && d.VIPIdleTimeout != "0s" {
		rows = append(rows, [2]string{"vip_idle_timeout", d.VIPIdleTimeout})
	}
	rows = append(rows,
		[2]string{"cluster_domain", d.ClusterDomain},
		[2]string{"log_level", d.LogLevel},
		[2]string{"http_listen", d.HTTPListen},
	)
	switch {
	case d.DNSEnabled && d.DNSListen != "":
		rows = append(rows, [2]string{"dns_listen", d.DNSListen + " (enabled)"})
	case d.DNSEnabled:
		rows = append(rows, [2]string{"dns_listen", "(enabled but address not bound)"})
	default:
		rows = append(rows, [2]string{"dns_listen", "(disabled)"})
	}
	switch {
	case d.SOCKSEnabled && d.SOCKSListen != "":
		rows = append(rows, [2]string{"socks_listen", d.SOCKSListen + " (enabled — point ALL_PROXY at socks5://" + d.SOCKSListen + ")"})
	case d.SOCKSEnabled:
		rows = append(rows, [2]string{"socks_listen", "(enabled but address not bound)"})
	default:
		rows = append(rows, [2]string{"socks_listen", "(disabled — start daemon with --socks to expose SOCKS5)"})
	}
	return rows
}

// StartLines returns the env-prefixed start hint shown when the daemon is down.
func (v renderView) StartLines() []string {
	overrides := v.Opts.envOverrides()
	if len(overrides) == 0 {
		return []string{fmt.Sprintf("Start: %s --dns", v.Bin)}
	}
	out := []string{"Start:"}
	for _, o := range overrides {
		out = append(out, "  "+o+" \\")
	}
	out = append(out, "  "+v.Bin+" --dns")
	return out
}

func (v renderView) ContextsHeader() string {
	if v.CurrentContext != "" {
		return "Registered contexts (from /kubeconfig; current: " + v.CurrentContext + ")"
	}
	return "Registered contexts (from /kubeconfig)"
}

// ContextLine returns the first line for a context block, including its marker.
func (c ContextStatus) Line() string {
	marker := "[ok]"
	suffix := ""
	if c.ProxyURL != "" && !c.ProxyOK {
		marker = "[--]"
		suffix = " (proxy unreachable)"
	}
	if c.IsCurrent {
		suffix += " (current)"
	}
	return fmt.Sprintf("%s %s%s", marker, c.Name, suffix)
}

// Rows returns label/value pairs for a context's detail block.
func (c ContextStatus) Rows() [][2]string {
	var rows [][2]string
	if c.ClusterName != "" && c.ClusterName != c.Name {
		rows = append(rows, [2]string{"cluster:", c.ClusterName})
	}
	if c.Server != "" {
		rows = append(rows, [2]string{"server:", c.Server})
	}
	switch {
	case c.ProxyURL != "" && c.ProxyOK:
		rows = append(rows, [2]string{"proxy-url:", c.ProxyURL + " (reachable)"})
	case c.ProxyURL != "" && c.ProxyError != "":
		rows = append(rows, [2]string{"proxy-url:", c.ProxyURL + " (" + c.ProxyError + ")"})
	case c.ProxyURL != "":
		rows = append(rows, [2]string{"proxy-url:", c.ProxyURL})
	default:
		rows = append(rows, [2]string{"proxy-url:", "(none — direct)"})
	}
	if c.User != "" {
		rows = append(rows, [2]string{"user:", c.User})
	}
	if c.Namespace != "" {
		rows = append(rows, [2]string{"namespace:", c.Namespace})
	}
	return rows
}

var statusTmpl = template.Must(template.New("status").Funcs(template.FuncMap{
	"settingsRow": func(p [2]string) string { return fmt.Sprintf("       %-17s %s", p[0], p[1]) },
	"contextRow":  func(p [2]string) string { return fmt.Sprintf("         %-10s %s", p[0], p[1]) },
}).Parse(`Install
  {{.ResolverLine}}
  {{.PoolLine}}

Daemon
  {{.DaemonHeader}}
{{- if .DaemonReachable}}
{{- if .DaemonCommand}}
       {{printf "%-17s %s" "process" .DaemonCommand}}
       {{printf "%-17s %s" "uptime" (or .DaemonUptime "?")}}
{{- end}}
{{- range .SettingsRows}}
{{settingsRow .}}
{{- end}}
{{- else}}
{{- range .StartLines}}
       {{.}}
{{- end}}
{{- end}}
{{if .DaemonReachable}}
{{.ContextsHeader}}
{{- if .KubeconfigError}}
  [!!] {{.KubeconfigError}}
{{- else if not .Contexts}}
  none registered (POST /kubeconfig to add)
{{- else}}
{{- range $i, $c := .Contexts}}
{{if $i}}
{{end}}  {{$c.Line}}
{{- range $c.Rows}}
{{contextRow .}}
{{- end}}
{{- end}}
{{- end}}
{{- end}}
`))
