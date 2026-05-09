// Package api holds the wire types shared between the daemon's HTTP control
// plane and external clients (e.g. the `status` subcommand). Each type is
// intentionally a flat record with json tags — no nested abstractions, no
// behaviour — so the JSON contract is obvious from the struct.
package api

// StatusResponse is what the daemon returns from GET /status. It captures the
// effective runtime configuration (post-flag parsing, post-listener bind) so
// callers can verify what's actually in effect rather than what was requested.
type StatusResponse struct {
	Interface      string `json:"interface"`
	VIPCIDR        string `json:"vip_cidr"`
	VIPAliasMode   string `json:"vip_alias_mode"`
	VIPIdleTimeout string `json:"vip_idle_timeout"`
	ClusterDomain  string `json:"cluster_domain"`
	LogLevel       string `json:"log_level"`

	HTTPListen string `json:"http_listen"`

	DNSEnabled bool   `json:"dns_enabled"`
	DNSListen  string `json:"dns_listen,omitempty"`

	SOCKSEnabled bool   `json:"socks_enabled"`
	SOCKSListen  string `json:"socks_listen,omitempty"`
}
