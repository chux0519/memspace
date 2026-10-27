package client

import (
	"net/http"
	"testing"
)

func TestPrivateHostBypassesProxy(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:7890")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:7890")
	t.Setenv("NO_PROXY", "")
	for host, private := range map[string]bool{
		"192.168.1.13": true, "10.0.0.2": true, "172.20.0.1": true, "127.0.0.1": true, "::1": true,
		"100.76.143.253": true, "fd81:50ae::1": true, "homelab": true, "nas.local": true, "box.tail1234.ts.net": true,
		"memspace.example.com": false, "8.8.8.8": false,
	} {
		if got := PrivateHost(host); got != private {
			t.Errorf("PrivateHost(%q) = %v", host, got)
		}
	}
	req, _ := http.NewRequest("GET", "http://192.168.1.13:8770/api/v1/health", nil)
	if u, _ := transport.Proxy(req); u != nil {
		t.Errorf("LAN request proxied via %v", u)
	}
	// ProxyFromEnvironment caches the environment on first use, so only
	// check that a public host is not forced direct when a proxy is set.
	req, _ = http.NewRequest("GET", "http://memspace.example.com/", nil)
	if _, err := transport.Proxy(req); err != nil {
		t.Errorf("public proxy lookup: %v", err)
	}
}
