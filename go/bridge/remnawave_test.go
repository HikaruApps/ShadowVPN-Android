package bridge

import (
	"bytes"
	"testing"

	"github.com/xtls/xray-core/core"
)

// Shape of a Remnawave XRAY_JSON response: DNS objects with hosts, IP rules
// first, flat VLESS settings.
const remnawaveResponse = `[{"dns":{"hosts":{"dns.shadowvpn.io":"94.156.232.5"},"servers":[{"address":"https://dns.shadowvpn.io/dns-query","timeoutMs":1000,"queryStrategy":"UseIPv4"},{"address":"https://8.8.8.8/dns-query","timeoutMs":1000,"queryStrategy":"UseIPv4"}],"queryStrategy":"UseIPv4","enableParallelQuery":false},
"routing":{"rules":[{"ip":["1.1.1.1","1.0.0.1"],"type":"field","outboundTag":"direct"},{"ip":["::/0"],"type":"field","outboundTag":"block"}]},
"inbounds":[{"tag":"socks","port":10808,"listen":"127.0.0.1","protocol":"socks","settings":{"udp":true}}],
"outbounds":[{"tag":"proxy","protocol":"vless","settings":{"address":"203.0.113.20","port":443,"id":"11111111-2222-3333-4444-555555555555","encryption":"none","flow":"xtls-rprx-vision"},
"streamSettings":{"network":"raw","security":"reality","realitySettings":{"serverName":"www.example.org","publicKey":"jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0","shortId":"ab","fingerprint":"chrome"}}},
{"tag":"direct","protocol":"freedom"},{"tag":"block","protocol":"blackhole"}],
"remarks":"🇳🇱 Нидерланды"}]`

func TestRemnawaveXrayJSONResponse(t *testing.T) {
	profiles, err := parseSubscription([]byte(remnawaveResponse))
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 || profiles[0].address != "203.0.113.20" || profiles[0].port != 443 || profiles[0].Format != "json" {
		t.Fatalf("unexpected import: %#v", profiles)
	}
	if len(profiles[0].rules) != 2 {
		t.Fatalf("rules lost: %#v", profiles[0].rules)
	}
	routing, _ := newRoutingOptions("full", nil, "", "")
	_, dns, _ := selectedDNSWithDoH("subscription-doh", nil, "https://dns.shadowvpn.io/dns-query")
	config, err := makeConfigWithRoutingAndDoH(profiles[0], "subscription-doh", nil, "https://dns.shadowvpn.io/dns-query", false, "", routing)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := androidRuntimeConfig(config, androidRuntimeOptions{DNS: dns, RoutingMode: "full", SubscriptionRules: true, Profiles: profiles})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.LoadConfig("json", bytes.NewReader(encoded)); err != nil {
		t.Fatalf("Xray rejected the config: %v", err)
	}
}
