package bridge

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
	"golang.org/x/net/proxy"
)

var testProxy = localProxy{SocksPort: 10808, HTTPPort: 10809, Username: "user", Password: "password1", LAN: true}

func TestLocalProxyConfigLoadsInXray(t *testing.T) {
	profile := mustProfile(t, "trojan://pass@198.51.100.7:443?security=tls&sni=t.example#Trojan")
	_, dns, _ := selectedDNSWithDoH("cloudflare", nil, "")
	routing, _ := newRoutingOptions("full", nil, "", "")
	config, err := makeConfigWithRoutingAndDoH(profile, "cloudflare", nil, "", false, "", routing)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := androidRuntimeConfig(config, androidRuntimeOptions{
		DNS: dns, RoutingMode: "full", Profiles: []Profile{profile}, BlockUDP: true, LocalProxy: testProxy,
	})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := core.LoadConfig("json", bytes.NewReader(encoded))
	if err != nil {
		t.Fatalf("Xray rejected the config: %v\n%s", err, encoded)
	}
	instance, err := core.New(parsed)
	if err != nil {
		t.Fatal(err)
	}
	_ = instance.Close()

	var document struct {
		Inbounds []map[string]any `json:"inbounds"`
		Routing  struct {
			DomainStrategy string           `json:"domainStrategy"`
			Rules          []map[string]any `json:"rules"`
		} `json:"routing"`
	}
	_ = json.Unmarshal(encoded, &document)
	if len(document.Inbounds) != 3 || document.Inbounds[0]["tag"] != "tun-in" {
		t.Fatalf("TUN must stay first, then the proxies: %v", document.Inbounds)
	}
	socks := document.Inbounds[1]
	if socks["listen"] != "0.0.0.0" || socks["settings"].(map[string]any)["auth"] != "password" {
		t.Fatalf("SOCKS must require a password: %v", socks)
	}
	if document.Routing.DomainStrategy != "AsIs" {
		t.Fatalf("the loopback guard must not force domain resolution, got %s", document.Routing.DomainStrategy)
	}
	rules := document.Routing.Rules
	if rules[0]["outboundTag"] != "block" || !strings.Contains(fmt.Sprint(rules[0]["ip"]), "127.0.0.0/8") {
		t.Fatalf("loopback guard must come first: %v", rules[0])
	}
	for _, rule := range rules[1:] {
		tags := fmt.Sprint(rule["inboundTag"])
		if strings.Contains(tags, "tun-in") && !(strings.Contains(tags, "socks-in") && strings.Contains(tags, "http-in")) {
			t.Fatalf("rule written for the TUN must cover the proxies: %v", rule)
		}
	}
}

func TestLocalProxyValidation(t *testing.T) {
	for _, bad := range []localProxy{
		{SocksPort: 10808, Username: "user", Password: "short"},
		{SocksPort: 10808, Password: "password1"},
		{SocksPort: 80, Username: "user", Password: "password1"},
		{SocksPort: 10808, HTTPPort: 10808, Username: "user", Password: "password1"},
	} {
		if bad.validate() == nil {
			t.Fatalf("%+v must be rejected", bad)
		}
	}
	if (localProxy{}).validate() != nil || testProxy.validate() != nil {
		t.Fatal("valid settings rejected")
	}
}

func nonLoopbackIP(t *testing.T) string {
	addresses, _ := net.InterfaceAddrs()
	for _, address := range addresses {
		if prefix, ok := address.(*net.IPNet); ok && prefix.IP.To4() != nil && !prefix.IP.IsLoopback() {
			return prefix.IP.String()
		}
	}
	t.Skip("no non-loopback address to reach through the proxy")
	return ""
}

// The proxy-only mode end to end: authenticated SOCKS5 and HTTP work, wrong
// or missing credentials and the phone's loopback do not, and a server
// switch keeps the ports.
func TestProxyOnlySession(t *testing.T) {
	host := nonLoopbackIP(t)
	listener, err := net.Listen("tcp", host+":0")
	if err != nil {
		t.Fatal(err)
	}
	target := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	target.Listener = listener
	target.Start()
	defer target.Close()
	loopback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer loopback.Close()

	settings := localProxy{SocksPort: freePort(t), HTTPPort: freePort(t), Username: "user", Password: "password1"}
	raw, _ := json.Marshal(map[string]any{"localProxy": settings})
	if err := Configure(string(raw)); err != nil {
		t.Fatal(err)
	}
	defer Configure("")
	build := func(string, sessionInputs) ([]byte, *naiveRuntime, error) {
		sniffing := map[string]any{"enabled": true, "destOverride": []string{"http", "tls"}}
		document := map[string]any{
			"log":       map[string]any{"loglevel": "none"},
			"inbounds":  append([]any{map[string]any{"tag": "tun-in", "protocol": "socks", "listen": "127.0.0.1", "port": freePort(t), "settings": map[string]any{}}}, settings.inbounds(sniffing)...),
			"outbounds": []any{map[string]any{"tag": "proxy", "protocol": "freedom"}, map[string]any{"tag": "block", "protocol": "blackhole"}},
			"routing":   map[string]any{"rules": applyLocalProxyRules(nil, settings)},
		}
		config, err := json.Marshal(document)
		return config, nil, err
	}
	previousBuild, previousProbe := buildSessionFunc, probeURL
	buildSessionFunc, probeURL = build, target.URL+"/generate_204"
	defer func() { buildSessionFunc, probeURL = previousBuild, previousProbe }()
	defer Stop()

	if err := Prepare("a"); err != nil {
		t.Fatal(err)
	}
	if err := StartProxy(); err != nil {
		t.Fatal(err)
	}

	socksGet := func(auth *proxy.Auth, address string) error {
		dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", settings.SocksPort), auth, &net.Dialer{Timeout: 3 * time.Second})
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Dial: dialer.Dial}}
		response, err := client.Get(address)
		if err != nil {
			return err
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			return fmt.Errorf("HTTP %d", response.StatusCode)
		}
		return nil
	}
	httpStatus := func(user *url.Userinfo) (int, error) {
		proxyURL := &url.URL{Scheme: "http", Host: fmt.Sprintf("127.0.0.1:%d", settings.HTTPPort), User: user}
		client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}
		response, err := client.Get(target.URL)
		if err != nil {
			return 0, err
		}
		response.Body.Close()
		return response.StatusCode, nil
	}
	check := func(stage string) {
		t.Helper()
		if err := socksGet(&proxy.Auth{User: "user", Password: "password1"}, target.URL); err != nil {
			t.Fatalf("%s: SOCKS5 with credentials: %v", stage, err)
		}
		if socksGet(&proxy.Auth{User: "user", Password: "wrong-pass"}, target.URL) == nil || socksGet(nil, target.URL) == nil {
			t.Fatalf("%s: SOCKS5 must reject wrong or missing credentials", stage)
		}
		if socksGet(&proxy.Auth{User: "user", Password: "password1"}, loopback.URL) == nil {
			t.Fatalf("%s: the phone's loopback must not be reachable through the proxy", stage)
		}
		if status, err := httpStatus(url.UserPassword("user", "password1")); err != nil || status != http.StatusNoContent {
			t.Fatalf("%s: HTTP proxy with credentials: %d %v", stage, status, err)
		}
		if status, _ := httpStatus(nil); status != http.StatusProxyAuthRequired {
			t.Fatalf("%s: HTTP proxy without credentials must answer 407, got %d", stage, status)
		}
	}
	check("start")
	if err := SwitchProfile("b"); err != nil {
		t.Fatal(err)
	}
	check("after switch")
	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if listening(settings.SocksPort) || listening(settings.HTTPPort) {
		t.Fatal("Stop must close the proxy ports")
	}
}
