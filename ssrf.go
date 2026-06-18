package main

import (
	"errors"
	"net"
	"net/http"
	"syscall"
	"time"
)

// extraDenied covers ranges the net.IP predicates below do NOT already catch.
var extraDenied = mustCIDRs(
	"100.64.0.0/10", // CGNAT — NOT reported by IsPrivate(); common cloud-internal range
	"192.0.0.0/24",  // IETF protocol assignments
	"192.0.2.0/24",  // TEST-NET-1
	"198.18.0.0/15", // benchmarking
	"198.51.100.0/24", // TEST-NET-2
	"203.0.113.0/24",  // TEST-NET-3
	"240.0.0.0/4",     // reserved
	"::ffff:0:0/96",   // IPv4-mapped IPv6 (belt-and-suspenders; To4() also normalizes these)
	"64:ff9b::/96",    // NAT64
	"2001:db8::/32",   // documentation
)

func mustCIDRs(ss ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(ss))
	for _, s := range ss {
		_, n, err := net.ParseCIDR(s)
		if err != nil {
			panic(err)
		}
		out = append(out, n)
	}
	return out
}

// ipDisallowed reports whether ip is anything other than a public, routable
// unicast address. It is the single source of truth for "may we connect here".
func ipDisallowed(ip net.IP) bool {
	if ip == nil ||
		ip.IsLoopback() || // 127.0.0.0/8, ::1
		ip.IsPrivate() || // 10/8, 172.16/12, 192.168/16, fc00::/7
		ip.IsLinkLocalUnicast() || // 169.254/16 (cloud metadata!), fe80::/10
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsMulticast() || // 224/4, ff00::/8
		ip.IsUnspecified() { // 0.0.0.0, ::
		return true
	}
	for _, n := range extraDenied {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

var errBlockedAddr = errors.New("refused connection to non-public address")

// guardedClient returns an http.Client that validates the resolved IP of EVERY
// connection — the initial request and every redirect hop — at dial time, after
// DNS resolution but before connect. This closes the DNS-rebinding and
// redirect-SSRF windows that a "resolve, check, then fetch" approach leaves open:
// the IP that is validated is exactly the IP the socket connects to.
//
// The client attaches no credentials of its own; callers MUST NOT add an
// Authorization header to outbound requests (we never leak the Matrix token to a
// previewed site).
func guardedClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: -1}
	dialer.Control = func(network, address string, _ syscall.RawConn) error {
		switch network {
		case "tcp", "tcp4", "tcp6":
		default:
			return errBlockedAddr
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || ipDisallowed(ip) {
			return errBlockedAddr
		}
		return nil
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			Proxy:                 nil, // never tunnel through *_proxy env
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 5 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			DisableKeepAlives:     true,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("too many redirects")
			}
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return errors.New("non-http(s) redirect blocked")
			}
			req.Header.Del("Authorization") // defensive: never carry creds across a redirect
			return nil
		},
	}
}
