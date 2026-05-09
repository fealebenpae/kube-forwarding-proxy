package install

import (
	"strings"
	"testing"
)

func TestOptionsWithDefaults(t *testing.T) {
	o := Options{}.WithDefaults()
	if o.ClusterDomain != DefaultClusterDomain || o.DNSPort != DefaultDNSPort ||
		o.PoolCIDR != DefaultPoolCIDR || o.PoolSize != DefaultPoolSize ||
		o.LoopbackIface != DefaultLoopbackIface || o.ResolverDir != DefaultResolverDir {
		t.Fatalf("zero-value WithDefaults didn't fill all fields: %+v", o)
	}
	// Pre-set fields are preserved.
	o2 := Options{ClusterDomain: "svc.example.com", DNSPort: 5354}.WithDefaults()
	if o2.ClusterDomain != "svc.example.com" || o2.DNSPort != 5354 {
		t.Fatalf("WithDefaults clobbered set fields: %+v", o2)
	}
}

func TestOptionsValidate(t *testing.T) {
	good := Options{}.WithDefaults()
	if err := good.Validate(); err != nil {
		t.Fatalf("default Options invalid: %v", err)
	}
	cases := []struct {
		name string
		mut  func(*Options)
		want string
	}{
		{"empty domain", func(o *Options) { o.ClusterDomain = "" }, "ClusterDomain"},
		{"port -1", func(o *Options) { o.DNSPort = -1 }, "DNSPort"},
		{"port 70000", func(o *Options) { o.DNSPort = 70000 }, "DNSPort"},
		{"pool size 300", func(o *Options) { o.PoolSize = 300 }, "PoolSize"},
		{"bad CIDR", func(o *Options) { o.PoolCIDR = "not-a-cidr" }, "invalid CIDR"},
		{"non /24 CIDR", func(o *Options) { o.PoolCIDR = "127.50.0.0/16" }, "/24"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := Options{}.WithDefaults()
			tc.mut(&o)
			err := o.Validate()
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestResolverPathAndBody(t *testing.T) {
	o := Options{
		ClusterDomain: "svc.cluster.local",
		DNSPort:       11617,
		ResolverDir:   "/etc/resolver",
	}.WithDefaults()
	if got := o.ResolverPath(); got != "/etc/resolver/svc.cluster.local" {
		t.Errorf("ResolverPath = %q", got)
	}
	if got := o.ResolverBody(); got != "nameserver 127.0.0.1\nport 11617\n" {
		t.Errorf("ResolverBody = %q", got)
	}
	// FQDN with trailing dot still produces a clean filename.
	o2 := o
	o2.ClusterDomain = "svc.cluster.local."
	if got := o2.ResolverPath(); got != "/etc/resolver/svc.cluster.local" {
		t.Errorf("ResolverPath FQDN = %q", got)
	}
}

func TestPoolIPs(t *testing.T) {
	ips, err := PoolIPs("127.50.0.0/24", 5)
	if err != nil {
		t.Fatalf("PoolIPs: %v", err)
	}
	if len(ips) != 5 {
		t.Fatalf("len = %d", len(ips))
	}
	want := []string{"127.50.0.1", "127.50.0.2", "127.50.0.3", "127.50.0.4", "127.50.0.5"}
	for i, ip := range ips {
		if ip.String() != want[i] {
			t.Errorf("ips[%d] = %s; want %s", i, ip, want[i])
		}
	}

	// 255 = full /24 minus .0
	ips, err = PoolIPs("127.50.0.0/24", 255)
	if err != nil {
		t.Fatalf("PoolIPs(255): %v", err)
	}
	if len(ips) != 255 {
		t.Fatalf("len = %d", len(ips))
	}
	if ips[0].String() != "127.50.0.1" || ips[254].String() != "127.50.0.255" {
		t.Errorf("boundary: first=%s last=%s", ips[0], ips[254])
	}
}

func TestPoolIPs_Errors(t *testing.T) {
	cases := []struct {
		cidr string
		size int
		want string
	}{
		{"not-a-cidr", 5, "invalid CIDR"},
		{"127.50.0.0/16", 5, "/24"},
		{"127.50.0.5/24", 5, "must end in .0"},
		{"127.50.0.0/24", 0, "size must"},
		{"127.50.0.0/24", 300, "size must"},
		{"::1/64", 5, "/24"},
	}
	for _, tc := range cases {
		t.Run(tc.cidr+"_"+itoa(tc.size), func(t *testing.T) {
			_, err := PoolIPs(tc.cidr, tc.size)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
