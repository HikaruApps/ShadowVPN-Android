package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

// countingForwarder sits in front of a server and counts the connections
// that pass through it, to see which way the traffic went.
func countingForwarder(t *testing.T, targetPort int) (int, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	var count atomic.Int32
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			count.Add(1)
			go func() {
				defer conn.Close()
				upstream, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", itoa(targetPort)))
				if err != nil {
					return
				}
				defer upstream.Close()
				go func() { _, _ = io.Copy(upstream, conn) }()
				_, _ = io.Copy(conn, upstream)
			}()
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port, &count
}

func itoa(value int) string {
	b, _ := json.Marshal(value)
	return string(b)
}

// The chosen server is reached through the Russian one: a request through
// the connection succeeds and passes the relay; without the relay it does not.
func TestRelayChainCarriesTraffic(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	relayPort, relayed := countingForwarder(t, startShadowsocksServer(t, "relay-password"))
	mainPort, direct := countingForwarder(t, startShadowsocksServer(t, "main-password"))
	relay := shadowsocksProfile(t, "relay", relayPort, "relay-password")
	relay.Name = "🇷🇺 Россия | Москва"
	main := shadowsocksProfile(t, "main", mainPort, "main-password")
	main.Name = "🇩🇪 Germany"
	profiles := []Profile{main, relay}

	request := func(options mobileOptions) (string, error) {
		config, naive, relayName, err := buildSession("main", sessionInputs{options: options, profiles: profiles})
		if err != nil {
			t.Fatal(err)
		}
		instance, _, err := startDetached(config, naive)
		if err != nil {
			t.Fatalf("Xray rejected the chained config: %v", err)
		}
		defer instance.Close()
		client, done := instanceHTTPClient(instance, 10*time.Second)
		defer done()
		response, err := client.Get(target.URL)
		if err == nil {
			response.Body.Close()
		}
		return relayName, err
	}

	relayName, err := request(mobileOptions{Relay: &relayOptions{}})
	if err != nil {
		t.Fatalf("no traffic through the chain: %v", err)
	}
	if relayName != relay.Name {
		t.Fatalf("relay name %q, want %q", relayName, relay.Name)
	}
	if relayed.Load() == 0 {
		t.Fatal("the connection did not go through the relay")
	}
	if direct.Load() == 0 {
		t.Fatal("the chosen server was not reached")
	}

	relayed.Store(0)
	if relayName, err := request(mobileOptions{}); err != nil || relayName != "" {
		t.Fatalf("direct connection: %q %v", relayName, err)
	}
	// The relay check knocked on the relay's port before; nothing else may.
	if relayed.Load() != 0 {
		t.Fatal("traffic went through the relay with chaining off")
	}
}

func TestRelayChoiceAndLimits(t *testing.T) {
	open, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	go func() {
		for {
			conn, err := open.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	openPort := open.Addr().(*net.TCPAddr).Port
	closed := closedAddress(t)
	_, closedPortText, _ := net.SplitHostPort(closed)
	var closedPort int
	_ = json.Unmarshal([]byte(closedPortText), &closedPort)

	alive := shadowsocksProfile(t, "ru-alive", openPort, "x")
	alive.Name = "RU | alive"
	dead := shadowsocksProfile(t, "ru-dead", closedPort, "x")
	dead.Name = "Россия dead"
	foreign := shadowsocksProfile(t, "nl", openPort, "x")
	foreign.Name = "Netherlands"

	ctx := context.Background()
	if relay, ok, _ := pickRelay(ctx, []Profile{foreign, dead, alive}, &relayOptions{}); !ok || relay.ID != "ru-alive" {
		t.Fatalf("the fastest answering Russian server must be picked, got %q %v", relay.ID, ok)
	}
	if _, ok, reason := pickRelay(ctx, []Profile{foreign}, &relayOptions{}); ok || !strings.Contains(reason, "нет российских") {
		t.Fatalf("no Russian servers must be reported, got %v %q", ok, reason)
	}
	if relay, ok, _ := pickRelay(ctx, []Profile{alive, dead}, &relayOptions{ProfileID: "ru-dead"}); !ok || relay.ID != "ru-dead" {
		t.Fatalf("a chosen relay must be used as is, got %q", relay.ID)
	}
	if relay, ok, _ := pickRelay(ctx, []Profile{alive, dead}, &relayOptions{ProfileID: "gone"}); !ok || relay.ID != "ru-alive" {
		t.Fatalf("a relay that left the subscription falls back to the fastest, got %q", relay.ID)
	}

	for _, c := range []struct {
		profile Profile
		ok      bool
	}{
		{foreign, true},
		{alive, false},
		{Profile{Name: "DE", Protocol: "hysteria"}, false},
		{Profile{Name: "DE", Protocol: "naive"}, false},
		{Profile{Name: "DE", Protocol: "wireguard", Transport: "udp"}, false},
	} {
		if ok, _ := chainable(c.profile); ok != c.ok {
			t.Errorf("chainable(%s %s) = %v, want %v", c.profile.Name, c.profile.Protocol, ok, c.ok)
		}
	}
}

// An Xray JSON server with its own fragment hop: on the chain, its hop is
// dropped and the relay's own one kept, and Xray accepts the config.
func TestChainThroughXrayJSONLoadsInXray(t *testing.T) {
	main := mustProfile(t, strings.Replace(remnawaveStyleJSON, "de.example.com", "203.0.113.10", 1))
	main.rules = nil
	relay := mustProfile(t, strings.Replace(strings.Replace(remnawaveStyleJSON, "de.example.com", "198.51.100.7", 1), "🇩🇪 Germany", "🇷🇺 Russia", 1))
	relay.rules = nil
	if len(main.chain) == 0 || len(relay.chain) == 0 {
		t.Fatal("the fixture must have dialer hops")
	}
	chained, err := chainThrough(main, relay, true)
	if err != nil {
		t.Fatal(err)
	}
	_, dns, _ := selectedDNSWithDoH("google", nil, "")
	config, err := makeConfigWithRoutingAndDoH(chained, "google", nil, "", false, "", routingOptions{Mode: "full"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := androidRuntimeConfig(config, androidRuntimeOptions{MTU: 1400, DNS: dns, RoutingMode: "full", Profiles: []Profile{chained}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.LoadConfig("json", bytes.NewReader(encoded)); err != nil {
		t.Fatalf("Xray rejected the chain: %v\n%s", err, encoded)
	}
	var document map[string]any
	_ = json.Unmarshal(encoded, &document)
	byTag := map[string]map[string]any{}
	for _, raw := range document["outbounds"].([]any) {
		outbound := raw.(map[string]any)
		byTag[stringValue(outbound["tag"])] = outbound
	}
	if dialerTag(byTag["proxy"]) != relayTag {
		t.Fatalf("the server must dial through the relay, dials through %q", dialerTag(byTag["proxy"]))
	}
	relayHop := dialerTag(byTag[relayTag])
	if relayHop == "" || byTag[relayHop] == nil {
		t.Fatal("the relay lost its own dialer hop")
	}
	if mainHop := main.chain[0]["tag"].(string); mainHop != relayHop && byTag[mainHop] != nil {
		t.Fatal("the server's own dialer hop must be dropped from the chain")
	}
}

// relayFixture: a Russian relay and a foreign server, both real Xray
// Shadowsocks servers behind counting forwarders, and a local check target.
func relayFixture(t *testing.T) (relay, main Profile, relayed, reached *atomic.Int32) {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(target.Close)
	previous := realPingURLs
	realPingURLs = []string{target.URL + "/generate_204"}
	t.Cleanup(func() { realPingURLs = previous })
	checkRelay.Lock()
	checkRelay.key = ""
	checkRelay.Unlock()

	relayPort, relayed := countingForwarder(t, startShadowsocksServer(t, "relay-password"))
	mainPort, reached := countingForwarder(t, startShadowsocksServer(t, "main-password"))
	relay = shadowsocksProfile(t, "relay", relayPort, "relay-password")
	relay.Name = "🇷🇺 Россия | Москва"
	main = shadowsocksProfile(t, "main", mainPort, "main-password")
	main.Name = "🇩🇪 Germany"
	return relay, main, relayed, reached
}

// With the chain on, the server list checks a server through the relay.
func TestPingGoesThroughRelay(t *testing.T) {
	relay, main, relayed, reached := relayFixture(t)
	if _, err := realPingThroughRelay(t.Context(), main, relay, false); err != nil {
		t.Fatalf("the check through the relay failed: %v", err)
	}
	if relayed.Load() == 0 || reached.Load() == 0 {
		t.Fatalf("the check must pass the relay (%d) and reach the server (%d)", relayed.Load(), reached.Load())
	}

	// A broken relay makes the server unreachable from it.
	broken := relay
	broken.outbound = shadowsocksProfile(t, "relay", relay.port, "wrong-password").outbound
	if _, err := realPingThroughRelay(t.Context(), main, broken, false); err == nil {
		t.Fatal("a check through a relay that refuses must fail")
	}

	// The streamed check used by the server list takes the same path.
	mobile.Lock()
	previousProfiles, previousOptions := mobile.profiles, mobile.options
	mobile.profiles = []Profile{main, relay}
	mobile.options = mobileOptions{Relay: &relayOptions{}}
	mobile.Unlock()
	defer func() {
		mobile.Lock()
		mobile.profiles, mobile.options = previousProfiles, previousOptions
		mobile.Unlock()
	}()
	relayed.Store(0)
	result, err := PingProfile("main", "tcp")
	if err != nil || !strings.Contains(result, `"available":true`) || strings.Contains(result, "direct") {
		t.Fatalf("PingProfile through the relay: %s %v", result, err)
	}
	if relayed.Load() == 0 {
		t.Fatal("even the quick method must go through the relay")
	}
	// The relay itself is checked directly.
	if result, err := PingProfile("relay", "real"); err != nil || !strings.Contains(result, `"available":true`) {
		t.Fatalf("the relay's own check: %s %v", result, err)
	}

	// Diagnostics show the relay as a hop of its own.
	report, err := Diagnose("main")
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Relay *struct {
			OK           bool
			Name, Detail string
		} `json:"relay"`
		Server struct {
			OK     bool
			Detail string
		} `json:"server"`
	}
	_ = json.Unmarshal([]byte(report), &parsed)
	if parsed.Relay == nil || !parsed.Relay.OK || parsed.Relay.Name != relay.Name || !parsed.Server.OK {
		t.Fatalf("diagnostics with the chain: %s", report)
	}
}
