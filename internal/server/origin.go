package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

type requestOriginKey struct{}

// RequestOrigin follows the requested host and transport. Proxy overrides are
// set by the server middleware only for explicitly trusted direct peers.
func RequestOrigin(r *http.Request) string {
	if origin, ok := r.Context().Value(requestOriginKey{}).(string); ok {
		return origin
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func trustedPeer(r *http.Request, prefixes []netip.Prefix) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	for _, p := range prefixes {
		if p.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

func validOriginHost(host string) bool {
	if host == "" || strings.ContainsAny(host, " /\\?#@,\t\r\n") {
		return false
	}
	u, err := url.Parse("http://" + host)
	return err == nil && u.Host == host && u.Hostname() != "" && u.User == nil
}

func withRequestOrigin(r *http.Request, prefixes []netip.Prefix) (*http.Request, bool) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if trustedPeer(r, prefixes) {
		if values, ok := r.Header["X-Forwarded-Host"]; ok {
			if len(values) != 1 || !validOriginHost(values[0]) {
				return r, false
			}
			host = values[0]
		}
		if values, ok := r.Header["X-Forwarded-Proto"]; ok {
			if len(values) != 1 || (values[0] != "http" && values[0] != "https") {
				return r, false
			}
			scheme = values[0]
		}
	}
	if !validOriginHost(host) {
		return r, false
	}
	return r.WithContext(context.WithValue(r.Context(), requestOriginKey{}, scheme+"://"+host)), true
}
