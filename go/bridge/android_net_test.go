package bridge

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
)

// trustTestServer makes the direct transport trust server's certificate.
func trustTestServer(t *testing.T, server *httptest.Server) {
	previous := directTLSConfig
	directTLSConfig = server.Client().Transport.(*http.Transport).TLSClientConfig
	t.Cleanup(func() { directTLSConfig = previous })
}

func withTunnel(t *testing.T, tunnel http.RoundTripper, headStart time.Duration) {
	previousTunnel, previousHead := subscriptionTunnel, tunnelHeadStart
	subscriptionTunnel = func() http.RoundTripper { return tunnel }
	tunnelHeadStart = headStart
	t.Cleanup(func() { subscriptionTunnel, tunnelHeadStart = previousTunnel, previousHead })
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// closedAddress returns an address nothing listens on.
func closedAddress(t *testing.T) string {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	return address
}

// Under mobile whitelists the panel answers directly while the VPN server's
// exit cannot reach it: the direct answer must win even though the tunnel,
// as Xray does, accepts the connection and only then fails.
func TestSubscriptionDirectWinsOverBrokenTunnel(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("vless://11111111-2222-3333-4444-555555555555@203.0.113.7:443?security=tls&sni=example.com#PL"))
	}))
	defer server.Close()
	trustTestServer(t, server)
	tunnelUsed := make(chan struct{}, 1)
	withTunnel(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		tunnelUsed <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}), 0)
	profiles, _, _, err := fetchSubscription(t.Context(), server.URL+"/sub")
	if err != nil || len(profiles) != 1 {
		t.Fatalf("direct subscription must load: %v %d", err, len(profiles))
	}
}

// A panel blocked directly still loads through the tunnel, right after the
// direct attempts fail rather than after a fixed wait.
func TestSubscriptionFallsBackToTunnel(t *testing.T) {
	withTunnel(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		_, _ = recorder.WriteString("vless://11111111-2222-3333-4444-555555555555@203.0.113.7:443?security=tls&sni=example.com#DE")
		return recorder.Result(), nil
	}), time.Minute)
	started := time.Now()
	profiles, _, _, err := fetchSubscription(t.Context(), "https://"+closedAddress(t)+"/sub")
	if err != nil || len(profiles) != 1 {
		t.Fatalf("the tunnel must carry a subscription the direct route cannot reach: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatalf("the tunnel waited for its head start after the direct route failed: %v", elapsed)
	}
}

func TestSubscriptionReportsServerAnswer(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	trustTestServer(t, server)
	withTunnel(t, nil, 0)
	_, _, _, err := fetchSubscription(t.Context(), server.URL+"/sub")
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") {
		t.Fatalf("the server's own answer must be reported: %v", err)
	}
}

// With a dead system resolver (Private DNS blocked by a whitelist) names
// still resolve through the fallback DNS.
func TestLookupFallsBackWhenSystemDNSFails(t *testing.T) {
	packet, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &dns.Server{PacketConn: packet, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, request *dns.Msg) {
		reply := new(dns.Msg)
		reply.SetReply(request)
		if request.Question[0].Qtype == dns.TypeA {
			reply.Answer = append(reply.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: request.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("203.0.113.9"),
			})
		}
		_ = w.WriteMsg(reply)
	})}
	go func() { _ = server.ActivateAndServe() }()
	defer server.Shutdown()
	previousServer, previousSystem := fallbackDNSServer, systemLookup
	fallbackDNSServer = packet.LocalAddr().String()
	systemLookup = func(context.Context, string) ([]net.IPAddr, error) { return nil, errors.New("no DNS") }
	defer func() { fallbackDNSServer, systemLookup = previousServer, previousSystem }()

	addresses, err := lookupIPWithFallback(t.Context(), "panel.example")
	if err != nil || len(addresses) == 0 || addresses[0].IP.String() != "203.0.113.9" {
		t.Fatalf("fallback DNS was not used: %v %v", addresses, err)
	}
	if _, _, err := resolveProfileEndpoint(t.Context(), Profile{address: "panel.example", port: 443,
		outbound: map[string]any{"protocol": "vless", "settings": map[string]any{"address": "panel.example", "port": 443}}}); err != nil {
		t.Fatalf("servers must resolve through the fallback too: %v", err)
	}
}

func TestWhitelistDetection(t *testing.T) {
	open := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusFound)
	}))
	defer open.Close()
	trustTestServer(t, open)
	closed := "https://" + closedAddress(t) + "/"
	previousAllowed, previousControl := whitelistAllowedSites, whitelistControlSites
	defer func() { whitelistAllowedSites, whitelistControlSites = previousAllowed, previousControl }()

	cases := []struct {
		name             string
		allowed, control []string
		want             whitelistResult
	}{
		{"whitelist", []string{open.URL, closed}, []string{closed, closed}, whitelistResult{Whitelist: true, Allowed: 1}},
		{"open internet", []string{open.URL}, []string{closed, open.URL}, whitelistResult{Allowed: 1, Control: 1}},
		{"offline", []string{closed}, []string{closed}, whitelistResult{Offline: true}},
	}
	for _, c := range cases {
		whitelistAllowedSites, whitelistControlSites = c.allowed, c.control
		if got := probeWhitelist(t.Context()); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// A server whose check through it fails but whose port answers is shown as
// reachable directly; one whose port is closed stays unavailable.
func TestPingFallsBackToDirectTCP(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	link := func(address, name string) string {
		return "vless://11111111-2222-3333-4444-555555555555@" + address + "?security=none&type=tcp#" + name
	}
	profiles, err := parseSubscription([]byte(link(listener.Addr().String(), "open") + "\n" + link(closedAddress(t), "closed")))
	if err != nil || len(profiles) != 2 {
		t.Fatalf("profiles: %v %d", err, len(profiles))
	}
	mobile.Lock()
	previous := mobile.profiles
	mobile.profiles = profiles
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.profiles = previous; mobile.Unlock() }()

	for index, want := range []string{`"available":true`, `"available":false`} {
		result, err := PingProfile(profiles[index].ID, "real")
		if err != nil || !strings.Contains(result, want) {
			t.Fatalf("%s: %s %v", profiles[index].Name, result, err)
		}
		if direct := strings.Contains(result, `"direct":true`); direct != (index == 0) {
			t.Fatalf("%s: direct must mark only the server answering directly: %s", profiles[index].Name, result)
		}
	}
}
