/*
 * Copyright 2026 InfAI (CC SES)
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

// Package scripthttp is the http client behind the httpGet of both script
// runtimes. It reaches public addresses only, optionally only allowlisted hosts,
// and hands a script at most MaxBodyBytes.
package scripthttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/SENERGY-Platform/moses/lib/util"
)

// MaxBodyBytes is the largest response body a script receives; a larger one is
// a failed request, never a truncated value.
const MaxBodyBytes = 1 << 20

// MaxRedirects is how many redirects one request follows.
const MaxRedirects = 3

// maxHeaderBytes bounds the response headers the transport reads.
const maxHeaderBytes = 64 << 10

// nonPublic are the ranges outside what netip's predicates cover that must not
// be reached: shared, reserved and documentation space, IPv4-compatible and
// Teredo addresses.
var nonPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/32"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

// A well-known NAT64 (RFC 6052, /96) or 6to4 address is translated to the IPv4
// address it embeds, so it is as public as that address. The local-use NAT64
// prefix is refused outright: its deployments may embed the IPv4 elsewhere.
var (
	nat64      = netip.MustParsePrefix("64:ff9b::/96")
	nat64Local = netip.MustParsePrefix("64:ff9b:1::/48")
	sixToFour  = netip.MustParsePrefix("2002::/16")
)

// IsPublic reports whether a script may connect to addr. It is the check every
// production client applies to each connection it opens.
func IsPublic(addr netip.Addr) bool {
	//a zoned address never matches a prefix, and a mapped one never an IPv4 prefix
	addr = addr.Unmap().WithZone("")
	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsMulticast() {
		return false
	}
	for _, prefix := range nonPublic {
		if prefix.Contains(addr) {
			return false
		}
	}
	if nat64Local.Contains(addr) {
		return false
	}
	bytes := addr.As16()
	switch {
	case nat64.Contains(addr):
		return IsPublic(netip.AddrFrom4([4]byte{bytes[12], bytes[13], bytes[14], bytes[15]}))
	case sixToFour.Contains(addr):
		return IsPublic(netip.AddrFrom4([4]byte{bytes[2], bytes[3], bytes[4], bytes[5]}))
	}
	return true
}

// Client fetches what a script asks for. A nil Client refuses every request.
type Client struct {
	allowed map[string]bool
	http    *http.Client
}

// Option changes how New builds a Client.
type Option func(*options)

type options struct {
	permit func(netip.Addr) bool
}

// WithAddressCheck replaces IsPublic as the check on every connection. Only
// tests use it, to reach a server on the loopback interface.
func WithAddressCheck(permit func(netip.Addr) bool) Option {
	return func(this *options) {
		this.permit = permit
	}
}

// New returns the client for allowedHosts, a comma separated list of host names
// (empty: any host). An entry that is no host name or IP address is an error.
func New(allowedHosts string, opts ...Option) (*Client, error) {
	allowed, err := parseHosts(allowedHosts)
	if err != nil {
		return nil, err
	}
	settings := options{permit: IsPublic}
	for _, opt := range opts {
		opt(&settings)
	}
	permit := settings.permit
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
		//runs on the resolved address of every connection, so neither a DNS answer
		//nor a redirect can lead to an address a host name check would not see
		Control: func(network string, address string, _ syscall.RawConn) error {
			return checkDial(permit, network, address)
		},
	}
	transport := &http.Transport{
		//a proxy would be dialed instead of the target, past the address check
		Proxy:                  nil,
		DialContext:            dialer.DialContext,
		ForceAttemptHTTP2:      true,
		MaxIdleConns:           100,
		IdleConnTimeout:        90 * time.Second,
		TLSHandshakeTimeout:    10 * time.Second,
		ExpectContinueTimeout:  time.Second,
		MaxResponseHeaderBytes: maxHeaderBytes,
	}
	this := &Client{allowed: allowed}
	this.http = &http.Client{Transport: transport, CheckRedirect: this.checkRedirect}
	return this, nil
}

// Get returns the body of a GET to endpoint. Every failure is logged here and
// returned as one of this package's fixed messages plus the host: url parsing and
// net/http quote their input, which may carry a secret in the query or userinfo.
func (this *Client) Get(ctx context.Context, endpoint string) (string, error) {
	if this == nil {
		err := refusal("no http client is configured for scripts")
		util.Logger.Warn("httpGet refused", "reason", err)
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		err = failure("the address is not a url")
		util.Logger.Warn("httpGet failed", "reason", err)
		return "", err
	}
	host := req.URL.Host
	body, err := this.get(req)
	if err != nil {
		var refused refusal
		if errors.As(err, &refused) {
			util.Logger.Warn("httpGet refused", "reason", err, "host", host)
		} else {
			util.Logger.Warn("httpGet failed", "reason", err, "host", host)
		}
		return "", err
	}
	return body, nil
}

func (this *Client) get(req *http.Request) (string, error) {
	if err := this.checkTarget(req.URL); err != nil {
		return "", err
	}
	resp, err := this.http.Do(req)
	if err != nil {
		return "", classify(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ContentLength > MaxBodyBytes {
		return "", errBodyTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxBodyBytes+1))
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return "", failure("timeout while reading the body")
		}
		return "", failure("reading the body failed")
	}
	if len(body) > MaxBodyBytes {
		return "", errBodyTooLarge
	}
	return string(body), nil
}

var errBodyTooLarge = failure(fmt.Sprintf("the response body exceeds %d bytes", MaxBodyBytes))

// classify turns an error of net/http into a fixed message. A refusal is this
// package's own text; everything else is named by its kind only.
func classify(err error) error {
	var refused refusal
	var dnsErr *net.DNSError
	var opErr *net.OpError
	var certErr *tls.CertificateVerificationError
	switch {
	case errors.As(err, &refused):
		return refused
	case errors.Is(err, context.DeadlineExceeded):
		return failure("timeout")
	case errors.Is(err, context.Canceled):
		return failure("canceled")
	case errors.As(err, &dnsErr):
		return failure("the host name did not resolve")
	case errors.As(err, &certErr):
		return failure("the certificate was not accepted")
	case errors.As(err, &opErr):
		return failure("network error during " + opErr.Op)
	default:
		return failure("the request failed")
	}
}

// checkTarget applies the scheme and the allowlist, to the first request and to
// every redirect.
func (this *Client) checkTarget(target *url.URL) error {
	if target.Scheme != "http" && target.Scheme != "https" {
		return refusal("only http and https urls are allowed")
	}
	//the transport dials the IDNA form of a non-ASCII name, which lower casing
	//cannot predict: "gİthub" lowers to "github" but dials "xn--github-qyd"
	if !isASCII(target.Hostname()) {
		return refusal("the host is not ascii")
	}
	host := normalizeHost(target.Hostname())
	if host == "" {
		return refusal("the url has no host")
	}
	if len(this.allowed) > 0 && !this.allowed[host] {
		return refusal("the host is not on the allowlist")
	}
	return nil
}

func isASCII(text string) bool {
	for i := 0; i < len(text); i++ {
		if text[i] >= 0x80 {
			return false
		}
	}
	return true
}

func (this *Client) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > MaxRedirects {
		return refusal(fmt.Sprintf("more than %d redirects", MaxRedirects))
	}
	if err := this.checkTarget(req.URL); err != nil {
		return refusal(fmt.Sprintf("redirect: %v", err))
	}
	return nil
}

// failure is a request that was made and did not succeed.
type failure string

func (this failure) Error() string {
	return string(this)
}

func checkDial(permit func(netip.Addr) bool, network string, address string) error {
	if network != "tcp4" && network != "tcp6" {
		return refusal(fmt.Sprintf("network %q is not allowed", network))
	}
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return refusal("the dialed address is not an ip address")
	}
	if !permit(addrPort.Addr()) {
		return refusal(fmt.Sprintf("address %v is not public", addrPort.Addr()))
	}
	return nil
}

// refusal is a request moses would not make, as opposed to one that failed.
type refusal string

func (this refusal) Error() string {
	return string(this)
}

func normalizeHost(host string) string {
	return strings.TrimSuffix(strings.ToLower(host), ".")
}

// parseHosts reads the allowlist. An entry is matched against the host of the
// url exactly, after lower casing and dropping a trailing dot, on any port.
func parseHosts(list string) (map[string]bool, error) {
	result := map[string]bool{}
	for _, entry := range strings.Split(list, ",") {
		host := normalizeHost(strings.TrimSpace(entry))
		if host == "" {
			continue
		}
		if !validHost(host) {
			return nil, fmt.Errorf("script http allowed hosts: %q is not a host name or ip address", entry)
		}
		result[host] = true
	}
	return result, nil
}

func validHost(host string) bool {
	if _, err := netip.ParseAddr(host); err == nil {
		return true
	}
	for _, c := range host {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '.' && c != '-' && c != '_' {
			return false
		}
	}
	return true
}
