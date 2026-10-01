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

package scripthttp

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SENERGY-Platform/moses/lib/util"
)

// countingServer answers "body" and counts the requests that reached it.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	hits := &atomic.Int64{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("body"))
	}))
	t.Cleanup(server.Close)
	return server, hits
}

func permitLoopback(addr netip.Addr) bool {
	return addr.Unmap().IsLoopback() || IsPublic(addr)
}

// permitOnly127 lets a test server on 127.0.0.1 through and refuses everything
// else the production check refuses.
func permitOnly127(addr netip.Addr) bool {
	return addr.Unmap() == netip.MustParseAddr("127.0.0.1") || IsPublic(addr)
}

func newTestClient(t *testing.T, allowed string, permit func(netip.Addr) bool) *Client {
	t.Helper()
	client, err := New(allowed, WithAddressCheck(permit))
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func get(client *Client, endpoint string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return client.Get(ctx, endpoint)
}

func isRefusal(err error) bool {
	var refused refusal
	return errors.As(err, &refused)
}

// captureLog routes util.Logger into a buffer for the test.
func captureLog(t *testing.T) *lockedBuffer {
	t.Helper()
	buffer := &lockedBuffer{}
	previous := util.Logger
	util.Logger = slog.New(slog.NewTextHandler(buffer, &slog.HandlerOptions{Level: slog.LevelDebug}))
	t.Cleanup(func() { util.Logger = previous })
	return buffer
}

type lockedBuffer struct {
	mux    sync.Mutex
	buffer bytes.Buffer
}

func (this *lockedBuffer) Write(p []byte) (int, error) {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.buffer.Write(p)
}

func (this *lockedBuffer) String() string {
	this.mux.Lock()
	defer this.mux.Unlock()
	return this.buffer.String()
}

func TestIsPublic(t *testing.T) {
	refused := []string{
		"127.0.0.1", "127.255.255.254", "::1",
		"10.0.0.1", "172.16.0.1", "172.31.255.255", "192.168.1.1", "fc00::1", "fd12:3456::1",
		"169.254.169.254", "169.254.0.1", "fe80::1", "fe80::1%eth0",
		"100.64.0.1", "100.127.255.255",
		"0.0.0.0", "0.1.2.3", "::",
		"224.0.0.1", "239.255.255.250", "ff02::1", "ff05::2",
		"255.255.255.255", "240.0.0.1",
		"192.0.0.8", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.1",
		"::ffff:127.0.0.1", "::ffff:10.0.0.1", "::ffff:169.254.169.254", "::ffff:100.64.0.1", "::ffff:0.0.0.0",
		"::127.0.0.1", "64:ff9b::127.0.0.1", "64:ff9b::a9fe:a9fe", "64:ff9b::10.0.0.1", "64:ff9b:1::1", "64:ff9b:1::a9fe:a9fe", "64:ff9b:1::808:808",
		"2002:7f00:1::1", "2002:a00:1::1", "2002:a9fe:a9fe::1", "2001::1", "2001:db8::1",
		"fec0::1", "100::1",
		"64:ff9b::7f00:1%eth0",
	}
	for _, text := range refused {
		if IsPublic(netip.MustParseAddr(text)) {
			t.Errorf("%s must not be public", text)
		}
	}
	if IsPublic(netip.Addr{}) {
		t.Error("the zero address must not be public")
	}
	public := []string{"8.8.8.8", "1.1.1.1", "100.128.0.1", "172.32.0.1", "::ffff:8.8.8.8", "2606:4700:4700::1111", "2a00:1450:4001::1",
		"64:ff9b::8.8.8.8", "2002:808:808::1"}
	for _, text := range public {
		if !IsPublic(netip.MustParseAddr(text)) {
			t.Errorf("%s must be public", text)
		}
	}
}

// The production client refuses a server on 127.0.0.1 before connecting, so the
// test seam is the only way to reach one.
func TestNewRefusesLoopback(t *testing.T) {
	server, hits := countingServer(t)
	client, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	for _, endpoint := range []string{
		server.URL,
		"http://localhost:" + port + "/",
		"http://[::ffff:127.0.0.1]:" + port + "/",
		"http://[::1]:" + port + "/",
	} {
		body, err := get(client, endpoint)
		if body != "" || !isRefusal(err) {
			t.Errorf("%s: expected a refusal, got body %q and %v", endpoint, body, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("the loopback server was reached %d times", hits.Load())
	}
	//the control: the same server through the test seam
	if body, err := get(newTestClient(t, "", permitLoopback), server.URL); err != nil || body != "body" {
		t.Fatalf("expected the body through the test seam, got %q and %v", body, err)
	}
}

// Internal addresses are refused when the connection is dialed, before any
// packet leaves; none of these needs a listener.
func TestInternalAddressesAreRefusedAtDial(t *testing.T) {
	client, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{
		"http://10.0.0.1/",
		"http://169.254.169.254/latest/meta-data/",
		"http://[fe80::1]/",
		"http://100.64.0.1/",
		"http://0.0.0.0/",
		"http://[::ffff:169.254.169.254]/",
		"https://192.168.0.1/",
	} {
		started := time.Now()
		body, err := get(client, endpoint)
		if body != "" || !isRefusal(err) {
			t.Errorf("%s: expected a refusal, got body %q and %v", endpoint, body, err)
		}
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Errorf("%s: the refusal took %v, it must not wait for a connection", endpoint, elapsed)
		}
	}
}

func TestOnlyHttpAndHttpsAreAllowed(t *testing.T) {
	client := newTestClient(t, "", permitLoopback)
	for _, endpoint := range []string{"file:///etc/passwd", "ftp://example.com/", "gopher://example.com/", "data:text/plain,x", "", "/relative", "http:///nohost"} {
		body, err := get(client, endpoint)
		if body != "" || !isRefusal(err) {
			t.Errorf("%q: expected a refusal, got body %q and %v", endpoint, body, err)
		}
	}
	if _, err := get(client, "http://a b/"); err == nil || isRefusal(err) {
		t.Errorf("an address that is no url must fail as a request error, got %v", err)
	}
}

// A redirect is checked like the first request: an allowed server cannot send
// the client on to an internal address or another scheme.
func TestRedirectsAreCheckedOnEveryHop(t *testing.T) {
	client := newTestClient(t, "", permitOnly127)
	for _, location := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1:8080/",
		"http://[::ffff:10.0.0.1]/",
		"file:///etc/passwd",
	} {
		redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, location, http.StatusFound)
		}))
		body, err := get(client, redirecting.URL)
		redirecting.Close()
		if body != "" || !isRefusal(err) {
			t.Errorf("redirect to %s: expected a refusal, got body %q and %v", location, body, err)
		}
	}
}

func TestRedirectsAreLimited(t *testing.T) {
	client := newTestClient(t, "", permitLoopback)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		left, _ := strconv.Atoi(r.URL.Query().Get("left"))
		if left > 0 {
			http.Redirect(w, r, fmt.Sprintf("%s/?left=%d", server.URL, left-1), http.StatusFound)
			return
		}
		_, _ = w.Write([]byte("arrived"))
	}))
	t.Cleanup(server.Close)
	if body, err := get(client, fmt.Sprintf("%s/?left=%d", server.URL, MaxRedirects)); err != nil || body != "arrived" {
		t.Fatalf("%d redirects: expected the body, got %q and %v", MaxRedirects, body, err)
	}
	if body, err := get(client, fmt.Sprintf("%s/?left=%d", server.URL, MaxRedirects+1)); body != "" || !isRefusal(err) {
		t.Fatalf("%d redirects: expected a refusal, got %q and %v", MaxRedirects+1, body, err)
	}
}

// A body past the limit is a failed request, whether the server announces its
// length, streams it, or compresses it.
func TestBodyOverTheLimitFails(t *testing.T) {
	client := newTestClient(t, "", permitLoopback)
	serve := func(size int, announce bool, compress bool) string {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			payload := bytes.Repeat([]byte("x"), size)
			if compress {
				var packed bytes.Buffer
				writer := gzip.NewWriter(&packed)
				_, _ = writer.Write(payload)
				_ = writer.Close()
				payload = packed.Bytes()
				w.Header().Set("Content-Encoding", "gzip")
			}
			if announce {
				w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
			}
			_, _ = w.Write(payload)
		}))
		t.Cleanup(server.Close)
		return server.URL
	}
	cases := []struct {
		name     string
		endpoint string
		fails    bool
	}{
		{"exactly the limit, announced", serve(MaxBodyBytes, true, false), false},
		{"exactly the limit, streamed", serve(MaxBodyBytes, false, false), false},
		{"one byte over, announced", serve(MaxBodyBytes+1, true, false), true},
		{"one byte over, streamed", serve(MaxBodyBytes+1, false, false), true},
		{"compressed below, expanded over", serve(4*MaxBodyBytes, true, true), true},
	}
	for _, c := range cases {
		body, err := get(client, c.endpoint)
		switch {
		case c.fails && (err == nil || body != ""):
			t.Errorf("%s: expected a failure and no body, got %d bytes and %v", c.name, len(body), err)
		case !c.fails && (err != nil || len(body) != MaxBodyBytes):
			t.Errorf("%s: expected the whole body, got %d bytes and %v", c.name, len(body), err)
		}
	}
}

// The allowlist admits its hosts only, on every hop, and never relaxes the
// address check.
func TestAllowlist(t *testing.T) {
	server, hits := countingServer(t)
	port := server.URL[strings.LastIndex(server.URL, ":")+1:]
	allowed := "http://localhost:" + port + "/"
	client := newTestClient(t, " LocalHost. , example.org", permitLoopback)

	if body, err := get(client, allowed); err != nil || body != "body" {
		t.Fatalf("allowlisted host: expected the body, got %q and %v", body, err)
	}
	if err := client.checkTarget(&url.URL{Scheme: "https", Host: "EXAMPLE.org.:8443"}); err != nil {
		t.Fatalf("allowlisted host in another spelling: expected it to pass, got %v", err)
	}
	before := hits.Load()
	if body, err := get(client, server.URL); body != "" || !isRefusal(err) {
		t.Fatalf("host not on the allowlist: expected a refusal, got %q and %v", body, err)
	}
	if hits.Load() != before {
		t.Fatal("a host not on the allowlist was reached")
	}

	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, server.URL, http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	redirectPort := redirecting.URL[strings.LastIndex(redirecting.URL, ":")+1:]
	if body, err := get(client, "http://localhost:"+redirectPort+"/"); body != "" || !isRefusal(err) {
		t.Fatalf("redirect off the allowlist: expected a refusal, got %q and %v", body, err)
	}
	if hits.Load() != before {
		t.Fatal("a redirect reached a host not on the allowlist")
	}

	strict, err := New("localhost")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := get(strict, allowed); body != "" || !isRefusal(err) {
		t.Fatalf("allowlisted host on loopback with the production check: expected a refusal, got %q and %v", body, err)
	}
}

func TestAllowlistEntriesAreValidated(t *testing.T) {
	for _, list := range []string{"http://example.org", "example.org/path", "user@example.org", "exa mple.org", "example.org:443", "[::1]", "bücher.example"} {
		if _, err := New(list); err == nil {
			t.Errorf("%q: expected an error", list)
		}
	}
	hosts, err := parseHosts(" Example.ORG. ,1.2.3.4,, ::1 ,under_score.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 4 || !hosts["example.org"] || !hosts["1.2.3.4"] || !hosts["::1"] || !hosts["under_score.example"] {
		t.Fatalf("unexpected hosts %v", hosts)
	}
	if hosts, err := parseHosts(" , "); err != nil || len(hosts) != 0 {
		t.Fatalf("an empty list must allow any host, got %v and %v", hosts, err)
	}
}

// A proxy from the environment would be dialed instead of the target, past the
// address check, so the client never uses one.
func TestProxyFromTheEnvironmentIsIgnored(t *testing.T) {
	client := newTestClient(t, "", permitLoopback)
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("the transport must not have a proxy function")
	}
	proxyHits := &atomic.Int64{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		_, _ = w.Write([]byte("proxied"))
	}))
	t.Cleanup(proxy.Close)
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	//a name that never resolves: only a proxy could answer it
	if body, err := get(client, "http://moses-scripthttp-test.invalid/"); err == nil || body != "" {
		t.Fatalf("expected the lookup to fail, got %q and %v", body, err)
	}
	if proxyHits.Load() != 0 {
		t.Fatal("the request went through the proxy from the environment")
	}
}

// The caller's deadline ends a request whose server never answers.
func TestGetEndsAtTheContextDeadline(t *testing.T) {
	release := make(chan struct{})
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(stalled.Close)
	t.Cleanup(func() { close(release) })
	client := newTestClient(t, "", permitLoopback)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	body, err := client.Get(ctx, stalled.URL)
	if err == nil || body != "" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected a failure at the deadline, got %q and %v", body, err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the request ended after %v", elapsed)
	}
}

func TestNilClientRefuses(t *testing.T) {
	var client *Client
	if body, err := get(client, "http://example.org/"); body != "" || !isRefusal(err) {
		t.Fatalf("expected a refusal, got %q and %v", body, err)
	}
}

// What is logged names the host, never the query, which may carry a secret.
func TestLogsNameTheHostButNotTheQuery(t *testing.T) {
	logs := captureLog(t)
	server, _ := countingServer(t)
	strict, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/?token=SECRET-REDIRECT", http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	host := strings.TrimPrefix(server.URL, "http://")

	_, _ = get(strict, server.URL+"/path?token=SECRET-REFUSED")
	_, _ = get(strict, "http://a b/?token=SECRET-UNPARSABLE")
	_, _ = get(newTestClient(t, "", permitOnly127), redirecting.URL+"/?token=SECRET-FIRST")
	_, _ = get(newTestClient(t, "only.example", permitLoopback), server.URL+"/?token=SECRET-ALLOWLIST")

	out := logs.String()
	if strings.Contains(out, "SECRET") {
		t.Fatalf("a query reached the log:\n%s", out)
	}
	if !strings.Contains(out, "httpGet refused") || !strings.Contains(out, host) {
		t.Fatalf("expected the refusal with the host %s in the log:\n%s", host, out)
	}
}

// classify names an error by its kind and never passes its text on.
func TestClassifyKeepsNoInput(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{&url.Error{Op: "Get", URL: "http://x/?token=SECRET", Err: errors.New("failed to parse Location header \"http://x/%zz?token=SECRET\"")}, "the request failed"},
		{&url.Error{Op: "Get", URL: "http://x/?token=SECRET", Err: context.DeadlineExceeded}, "timeout"},
		{&url.Error{Op: "Get", URL: "http://x/?token=SECRET", Err: &net.DNSError{Name: "SECRET.example", Err: "no such host"}}, "the host name did not resolve"},
		{&url.Error{Op: "Get", URL: "http://x/?token=SECRET", Err: &net.OpError{Op: "dial", Err: errors.New("SECRET")}}, "network error during dial"},
		{&url.Error{Op: "Get", URL: "http://x/?token=SECRET", Err: &net.OpError{Op: "dial", Err: refusal("address 10.0.0.1 is not public")}}, "address 10.0.0.1 is not public"},
	}
	for _, c := range cases {
		if got := classify(c.err); got.Error() != c.want || strings.Contains(got.Error(), "SECRET") {
			t.Errorf("classify(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// A non-ASCII host is dialed in its IDNA form, which lower casing does not
// predict, so it is refused on every hop rather than matched against the allowlist.
func TestNonASCIIHostsAreRefused(t *testing.T) {
	client := newTestClient(t, "api.github.com, kelvin.example", permitLoopback)
	for _, endpoint := range []string{
		"http://api.g\u0130thub.com/",
		"http://api.g%C4%B0thub.com/",
		"http://\u212Aelvin.example/",
		"https://\u212Aelvin.example/",
	} {
		if body, err := get(client, endpoint); body != "" || !isRefusal(err) {
			t.Errorf("%s: expected a refusal, got %q and %v", endpoint, body, err)
		}
	}
	open := newTestClient(t, "", permitLoopback)
	redirecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://api.g\u0130thub.com/")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(redirecting.Close)
	if body, err := get(open, redirecting.URL); body != "" || !isRefusal(err) {
		t.Errorf("redirect to a non-ascii host: expected a refusal, got %q and %v", body, err)
	}
}

// Neither an unparsable Location header nor a password in the url reaches the
// log or the returned error.
func TestRawErrorTextIsNeverPassedOn(t *testing.T) {
	logs := captureLog(t)
	client := newTestClient(t, "", permitLoopback)
	badLocation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "http://x/%zz?token=SECRET-LOC")
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(badLocation.Close)
	for _, endpoint := range []string{badLocation.URL, "http://svc:S3cr/et@example.com/"} {
		body, err := get(client, endpoint)
		if body != "" || err == nil {
			t.Fatalf("%s: expected a failure, got %q and %v", endpoint, body, err)
		}
		if strings.Contains(err.Error(), "SECRET-LOC") || strings.Contains(err.Error(), "S3cr") {
			t.Fatalf("%s: the returned error carries the input: %v", endpoint, err)
		}
	}
	if out := logs.String(); strings.Contains(out, "SECRET-LOC") || strings.Contains(out, "S3cr") {
		t.Fatalf("the input reached the log:\n%s", out)
	}
}

func TestCheckDialRefusesOtherNetworksAndUnparsableAddresses(t *testing.T) {
	if err := checkDial(IsPublic, "udp4", "8.8.8.8:53"); !isRefusal(err) {
		t.Errorf("udp: expected a refusal, got %v", err)
	}
	if err := checkDial(IsPublic, "tcp4", "example.org:80"); !isRefusal(err) {
		t.Errorf("a name: expected a refusal, got %v", err)
	}
	if err := checkDial(IsPublic, "tcp6", "[fe80::1%eth0]:80"); !isRefusal(err) {
		t.Errorf("a zoned link-local address: expected a refusal, got %v", err)
	}
	if err := checkDial(IsPublic, "tcp4", "8.8.8.8:80"); err != nil {
		t.Errorf("a public address: expected no error, got %v", err)
	}
}
