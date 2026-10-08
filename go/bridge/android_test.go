package bridge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

const remnawaveStyleJSON = `[{
  "remarks": "🇩🇪 Germany",
  "dns": {"servers": ["8.8.8.8"]},
  "inbounds": [{"tag": "socks", "port": 10808, "listen": "0.0.0.0", "protocol": "socks"}],
  "outbounds": [
    {"tag": "proxy", "protocol": "vless",
     "settings": {"address": "de.example.com", "port": 443, "id": "11111111-2222-3333-4444-555555555555", "encryption": "none", "flow": "xtls-rprx-vision"},
     "streamSettings": {"network": "tcp", "security": "reality",
       "realitySettings": {"serverName": "www.example.org", "publicKey": "jNXHt1yRo0vDuchQlIP6Z0ZvjT3KtzVI-T4E7RoLJS0", "shortId": "ab"},
       "sockopt": {"dialerProxy": "fragment", "interface": "wlan0", "mark": 7, "tcpNoDelay": true}},
     "mux": {"enabled": false, "concurrency": -1, "unknown": 1}},
    {"tag": "fragment", "protocol": "freedom",
     "settings": {"fragment": {"packets": "tlshello", "length": "100-200", "interval": "10-20"}, "redirect": "127.0.0.1:22"},
     "streamSettings": {"sockopt": {"tcpNoDelay": true, "interface": "rmnet0"}}},
    {"tag": "direct", "protocol": "freedom"},
    {"tag": "block", "protocol": "blackhole"},
    {"tag": "dns-out", "protocol": "dns"}
  ],
  "routing": {"domainStrategy": "IPIfNonMatch", "rules": [
    {"type": "field", "inboundTag": ["api"], "outboundTag": "api"},
    {"type": "field", "port": "53", "outboundTag": "dns-out"},
    {"type": "field", "domain": ["geosite:category-ru", "ext:custom.dat:x", "full:ya.ru"], "outboundTag": "direct"},
    {"type": "field", "ip": ["geoip:private", "geoip:ru"], "outboundTag": "direct"},
    {"type": "field", "protocol": ["bittorrent"], "outboundTag": "block"},
    {"type": "field", "domain": ["ext:only.dat:x"], "outboundTag": "block"},
    {"type": "field", "process": ["com.x"], "outboundTag": "direct"},
    {"type": "field", "domain": ["geosite:openai"], "outboundTag": "proxy"},
    {"type": "field", "network": "tcp,udp", "outboundTag": "proxy"}
  ]}
}]`

func TestXrayJSONImportsServerChainAndRules(t *testing.T) {
	skipped := 0
	profiles, err := parseSubscriptionWithStats([]byte(remnawaveStyleJSON), &skipped)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 1 {
		t.Fatalf("expected one server, got %d", len(profiles))
	}
	profile := profiles[0]
	if profile.Format != "json" {
		t.Fatalf("JSON server not marked: %q", profile.Format)
	}
	if profile.Name != "🇩🇪 Germany" || profile.address != "de.example.com" || profile.port != 443 {
		t.Fatalf("flat VLESS endpoint was not imported: %q %s:%d", profile.Name, profile.address, profile.port)
	}
	stream := profile.outbound["streamSettings"].(map[string]any)
	sockopt := stream["sockopt"].(map[string]any)
	if _, exists := sockopt["interface"]; exists {
		t.Fatal("host-level sockopt was imported")
	}
	if _, exists := sockopt["mark"]; exists {
		t.Fatal("socket mark was imported")
	}
	if len(profile.chain) != 1 || sockopt["dialerProxy"] != profile.chain[0]["tag"] {
		t.Fatalf("fragment dialer chain was lost: %#v / %#v", sockopt, profile.chain)
	}
	hopSettings := profile.chain[0]["settings"].(map[string]any)
	if _, exists := hopSettings["redirect"]; exists || hopSettings["fragment"] == nil {
		t.Fatalf("freedom hop was not sanitized: %#v", hopSettings)
	}
	mux := profile.outbound["mux"].(map[string]any)
	if _, exists := mux["unknown"]; exists || mux["concurrency"] != float64(-1) {
		t.Fatalf("mux was not sanitized: %#v", mux)
	}
	if profile.domainStrategy != "IPIfNonMatch" {
		t.Fatalf("domain strategy lost: %q", profile.domainStrategy)
	}
	want := []string{"direct", "direct", "block", "proxy"}
	if len(profile.rules) != len(want) {
		t.Fatalf("unexpected rules: %#v", profile.rules)
	}
	for index, rule := range profile.rules {
		if rule["outboundTag"] != want[index] {
			t.Fatalf("rule %d targets %v, want %s", index, rule["outboundTag"], want[index])
		}
	}
	domains := profile.rules[0]["domain"].([]string)
	if strings.Join(domains, ",") != "geosite:category-ru,full:ya.ru" {
		t.Fatalf("ext: values must be dropped: %v", domains)
	}
}

func TestXrayJSONBalancerImportsEveryMember(t *testing.T) {
	config := `{"remarks":"Auto","outbounds":[
	  {"tag":"auto-1","protocol":"trojan","settings":{"servers":[{"address":"a.example","port":443,"password":"x"}]}},
	  {"tag":"auto-2","protocol":"shadowsocks","settings":{"address":"b.example","port":8388,"method":"aes-128-gcm","password":"y"}},
	  {"tag":"chained","protocol":"vless","settings":{"address":"c.example","port":443,"id":"z"},"streamSettings":{"sockopt":{"dialerProxy":"frag"}}},
	  {"tag":"frag","protocol":"freedom","settings":{"fragment":{"packets":"tlshello"}}},
	  {"tag":"direct","protocol":"freedom"}],
	  "routing":{"balancers":[{"tag":"b","selector":["auto-"]}]}}`
	profiles, err := parseSubscription([]byte(config))
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 || profiles[0].Name != "Auto · auto-1" || profiles[1].address != "b.example" {
		t.Fatalf("balancer members were not imported: %#v", profiles)
	}
	// Chaining through another server is rejected rather than silently skipped.
	if _, err := profileFromOutbound("x", map[string]any{"protocol": "vless", "settings": map[string]any{"address": "c", "port": 1, "id": "z"},
		"streamSettings": map[string]any{"sockopt": map[string]any{"dialerProxy": "p"}}},
		map[string]map[string]any{"p": {"protocol": "vless"}}); err == nil {
		t.Fatal("server-to-server chain was accepted")
	}
}

func TestWireGuardEndpointIsResolvable(t *testing.T) {
	profiles, err := parseSubscription([]byte(`{"tag":"warp","protocol":"wireguard","settings":{"secretKey":"k","address":["172.16.0.2/32"],"peers":[{"publicKey":"p","endpoint":"engage.cloudflareclient.com:2408"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	profile := profiles[0]
	if profile.address != "engage.cloudflareclient.com" || profile.port != 2408 || profile.Transport != "udp" {
		t.Fatalf("unexpected WireGuard endpoint: %s:%d %s", profile.address, profile.port, profile.Transport)
	}
	resolved, err := profileWithEndpoint(profile, "162.159.192.1", "engage.cloudflareclient.com")
	if err != nil {
		t.Fatal(err)
	}
	peer := resolved.outbound["settings"].(map[string]any)["peers"].([]any)[0].(map[string]any)
	if peer["endpoint"] != "162.159.192.1:2408" {
		t.Fatalf("endpoint was not replaced: %v", peer["endpoint"])
	}
}

func TestShareLinksVMessShadowsocksAndBrokenLines(t *testing.T) {
	vmess, _ := json.Marshal(map[string]any{"v": "2", "ps": "VM", "add": "vm.example", "port": "443", "id": "uuid",
		"net": "ws", "host": "cdn.example", "path": "/ws", "tls": "tls", "fp": "chrome"})
	ssLegacy := base64.StdEncoding.EncodeToString([]byte("aes-256-gcm:pa:ss@ss.example:8388"))
	lines := strings.Join([]string{
		"vmess://" + base64.StdEncoding.EncodeToString(vmess),
		"ss://" + base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:secret")) + "@1.2.3.4:8388#SS%20One",
		"ss://2022-blake3-aes-128-gcm:a2V5@5.6.7.8:443#SS2022",
		"ss://" + ssLegacy + "#Legacy",
		"vless://broken",
		"tuic://u:p@x.example:443",
		"trojan://pass@t.example:443?security=tls&sni=t.example#Trojan",
	}, "\n")
	skipped := 0
	profiles, err := parseSubscriptionWithStats([]byte(lines), &skipped)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 5 || skipped != 2 {
		t.Fatalf("expected 5 servers and 2 skipped lines, got %d and %d", len(profiles), skipped)
	}
	vm := profiles[0]
	if vm.Format != "" {
		t.Fatal("share links must not be marked as JSON")
	}
	stream := vm.outbound["streamSettings"].(map[string]any)
	if vm.Name != "VM" || vm.Transport != "ws" || stream["security"] != "tls" ||
		stream["tlsSettings"].(map[string]any)["serverName"] != "cdn.example" {
		t.Fatalf("unexpected VMess profile: %#v", vm.outbound)
	}
	if profiles[1].Name != "SS One" || profiles[1].address != "1.2.3.4" {
		t.Fatalf("unexpected SIP002 profile: %#v", profiles[1])
	}
	server := profiles[2].outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if server["password"] != "a2V5" || server["method"] != "2022-blake3-aes-128-gcm" {
		t.Fatalf("plain SIP002 userinfo parsed incorrectly: %#v", server)
	}
	legacy := profiles[3].outbound["settings"].(map[string]any)["servers"].([]any)[0].(map[string]any)
	if legacy["password"] != "pa:ss" || profiles[3].address != "ss.example" {
		t.Fatalf("legacy ss:// parsed incorrectly: %#v", legacy)
	}
	if _, err := parseShareLink("ss://YWVzLTEyOC1nY206eA@1.1.1.1:1?plugin=obfs-local"); err == nil {
		t.Fatal("ss:// plugin was accepted")
	}
}

func mustProfile(t *testing.T, raw string) Profile {
	t.Helper()
	profiles, err := parseSubscription([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return profiles[0]
}

func TestAndroidConfigLoadsInXray(t *testing.T) {
	profile := mustProfile(t, strings.Replace(remnawaveStyleJSON, "de.example.com", "203.0.113.10", 1))
	// Drop geo rules so the config builds without data files.
	profile.rules = []map[string]any{
		{"type": "field", "domain": []string{"full:ya.ru"}, "outboundTag": "direct"},
		{"type": "field", "protocol": []any{"bittorrent"}, "outboundTag": "block"},
	}
	_, dns, _ := selectedDNSWithDoH("google", nil, "")
	routing, err := newRoutingOptions("bypass", []string{"example.com"}, "", "")
	if err != nil {
		t.Fatal(err)
	}
	config, err := makeConfigWithRoutingAndDoH(profile, "google", nil, "", false, "", routing)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := androidRuntimeConfig(config, androidRuntimeOptions{
		MTU: 1360, DNS: dns, RoutingMode: "bypass", SubscriptionRules: true, Profiles: []Profile{profile},
		GeoData: func(bool, bool) (geoDataInfo, error) {
			t.Fatal("GeoData requested without geo rules")
			return geoDataInfo{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.LoadConfig("json", bytes.NewReader(encoded)); err != nil {
		t.Fatalf("Xray rejected the Android config: %v\n%s", err, encoded)
	}
	var document map[string]any
	_ = json.Unmarshal(encoded, &document)
	if inbounds := document["inbounds"].([]any); len(inbounds) != 1 {
		t.Fatalf("only the TUN inbound may remain, got %d", len(inbounds))
	}
	if document["stats"] != nil || document["policy"] != nil {
		t.Fatal("stats were not removed")
	}
	if level := document["log"].(map[string]any)["loglevel"]; level != "warning" {
		t.Fatalf("unexpected log level %v", level)
	}
	dnsConfig := document["dns"].(map[string]any)
	if servers := dnsConfig["servers"].([]any); len(servers) != 2 || servers[0] != "8.8.8.8" || dnsConfig["queryStrategy"] != "UseIPv4" {
		t.Fatalf("DNS provider was not applied: %#v", dnsConfig)
	}
	routingConfig := document["routing"].(map[string]any)
	rules := routingConfig["rules"].([]any)
	if rules[0].(map[string]any)["outboundTag"] != "dns-out" || len(rules) != 4 {
		t.Fatalf("unexpected rule order: %#v", rules)
	}
	if routingConfig["domainStrategy"] != "AsIs" {
		t.Fatalf("domain strategy must stay AsIs without IP rules, got %v", routingConfig["domainStrategy"])
	}
	found := false
	for _, raw := range document["outbounds"].([]any) {
		if raw.(map[string]any)["tag"] == profile.chain[0]["tag"] {
			found = true
		}
	}
	if !found {
		t.Fatal("dialer chain outbound is missing")
	}
}

func TestSubscriptionRulesFollowModeAndRequestGeoData(t *testing.T) {
	profile := mustProfile(t, remnawaveStyleJSON)
	raw, _ := json.Marshal(map[string]any{
		"inbounds":  []any{map[string]any{"tag": "tun-in", "protocol": "tun", "settings": map[string]any{}}},
		"outbounds": []any{map[string]any{"tag": "auto-x", "protocol": "freedom"}},
		"routing": map[string]any{"rules": []any{
			map[string]any{"type": "field", "inboundTag": []string{"tun-in"}, "balancerTag": "shadow-auto"},
		}},
	})
	build := func(mode string) (map[string]any, [2]bool) {
		var asked [2]bool
		encoded, err := androidRuntimeConfig(raw, androidRuntimeOptions{
			DNS: dnsPresets["cloudflare"], IPv6: true, RoutingMode: mode, SubscriptionRules: true, Auto: true,
			Profiles: []Profile{profile},
			GeoData: func(ip, site bool) (geoDataInfo, error) {
				asked = [2]bool{ip, site}
				return geoDataInfo{Directory: "/data/geo", Codes: map[string]map[string]bool{
					"geoip.dat":   {"PRIVATE": true, "RU": true},
					"geosite.dat": {"OPENAI": true},
				}}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		var document map[string]any
		_ = json.Unmarshal(encoded, &document)
		return document, asked
	}

	document, asked := build("full")
	if asked != [2]bool{true, true} || document["env"].(map[string]any)["XRAY_LOCATION_ASSET"] != "/data/geo" {
		t.Fatalf("GeoData was not prepared: %v %#v", asked, document["env"])
	}
	routing := document["routing"].(map[string]any)
	rules := routing["rules"].([]any)
	// dns hijack, 4 subscription rules, then the Auto catch-all last.
	if len(rules) != 6 || rules[5].(map[string]any)["balancerTag"] != "shadow-auto" {
		t.Fatalf("catch-all must stay last: %#v", rules)
	}
	// geosite:category-ru is absent from this GeoSite: only that value goes.
	if domains := rules[1].(map[string]any)["domain"].([]any); len(domains) != 1 || domains[0] != "full:ya.ru" {
		t.Fatalf("missing code was not dropped: %v", domains)
	}
	proxyRule := rules[4].(map[string]any)
	if proxyRule["balancerTag"] != "shadow-auto" || proxyRule["outboundTag"] != nil {
		t.Fatalf("proxy rule must target the Auto balancer: %#v", proxyRule)
	}
	if routing["domainStrategy"] != "IPIfNonMatch" {
		t.Fatalf("IP rules need IPIfNonMatch, got %v", routing["domainStrategy"])
	}
	if dns := document["dns"].(map[string]any); dns["queryStrategy"] != "UseIP" || len(dns["servers"].([]any)) != 4 {
		t.Fatalf("IPv6 DNS servers missing: %#v", dns)
	}

	document, _ = build("proxy_only")
	for _, raw := range document["routing"].(map[string]any)["rules"].([]any) {
		if raw.(map[string]any)["balancerTag"] == "shadow-auto" && raw.(map[string]any)["domain"] != nil {
			t.Fatal("subscription proxy rule widened proxy_only mode")
		}
	}
}

func TestProfileCacheRoundTrip(t *testing.T) {
	directory := t.TempDir()
	profiles, err := parseSubscription([]byte(remnawaveStyleJSON))
	if err != nil {
		t.Fatal(err)
	}
	profiles[0].SourceIndex = manualSourceIndex
	if err := saveProfileCache(directory, profiles); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(directory, profileCacheFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("cache must be private: %v %v", info, err)
	}
	loaded, err := loadProfileCache(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 1 || loaded[0].ID != profiles[0].ID || loaded[0].SourceIndex != manualSourceIndex || loaded[0].Format != "json" ||
		len(loaded[0].chain) != 1 || len(loaded[0].rules) != 4 || loaded[0].address != "de.example.com" {
		t.Fatalf("cache lost data: %#v", loaded)
	}
	if missing, err := loadProfileCache(t.TempDir()); err != nil || missing != nil {
		t.Fatalf("missing cache must not fail: %v", err)
	}
}

func TestGeoDataCacheHitSkipsNetwork(t *testing.T) {
	directory := t.TempDir()
	geo := filepath.Join(directory, "geodata")
	_ = os.MkdirAll(geo, 0o700)
	_ = os.WriteFile(filepath.Join(geo, "geosite.dat"), []byte("cached"), 0o600)
	meta, _ := json.Marshal(map[string]geoDataEntry{"geosite.dat": {URL: "https://unreachable.invalid/geosite.dat", Size: 6, Updated: time.Now(), Codes: []string{"YOUTUBE"}}})
	_ = os.WriteFile(filepath.Join(geo, "meta.json"), meta, 0o600)
	result, err := ensureGeoData(t.Context(), directory, false, true, "", "https://unreachable.invalid/geosite.dat")
	if err != nil || result.Directory != geo || !result.has("geosite.dat", "youtube") {
		t.Fatalf("fresh cache must be used without downloading: %+v %v", result, err)
	}
	// A stale but valid copy survives a failed refresh.
	stale, _ := json.Marshal(map[string]geoDataEntry{"geosite.dat": {URL: "https://unreachable.invalid/geosite.dat", Size: 6, Updated: time.Now().Add(-30 * 24 * time.Hour), Codes: []string{"YOUTUBE"}}})
	_ = os.WriteFile(filepath.Join(geo, "meta.json"), stale, 0o600)
	if _, err := ensureGeoData(t.Context(), directory, false, true, "", "https://unreachable.invalid/geosite.dat"); err != nil {
		t.Fatalf("stale cache must be kept when refresh fails: %v", err)
	}
	if _, err := ensureGeoData(t.Context(), directory, true, false, "https://unreachable.invalid/geoip.dat", ""); err == nil {
		t.Fatal("missing GeoIP without network must fail")
	}
}

func TestAutoCandidatesFallBack(t *testing.T) {
	profiles := []Profile{
		{ID: "a", Name: "DE", SourceIndex: 0},
		{ID: "b", Name: "RU Moscow", SourceIndex: 1},
		{ID: "c", Name: "NL", SourceIndex: 1},
		{ID: "n", Name: "Naive", Protocol: "naive", SourceIndex: 1},
	}
	ids := func(list []Profile) string {
		result := []string{}
		for _, profile := range list {
			result = append(result, profile.ID)
		}
		return strings.Join(result, ",")
	}
	if got := ids(autoCandidates(profiles, mobileOptions{AutoProfileIDs: []string{"gone"}, PreferredSource: -1}, autoProfileID)); got != "a,b,c" {
		t.Fatalf("stale Auto selection must fall back to all servers, got %s", got)
	}
	if got := ids(autoCandidates(profiles, mobileOptions{PreferredSource: 1}, autoNoRUProfileID)); got != "c" {
		t.Fatalf("network source and no-RU filter not applied, got %s", got)
	}
	if got := ids(autoCandidates(profiles, mobileOptions{PreferredSource: 5}, autoProfileID)); got != "a,b,c" {
		t.Fatalf("empty source must fall back, got %s", got)
	}
}

func TestImportSourcesManualOnlyAndConfigureWhileConnected(t *testing.T) {
	directory := t.TempDir()
	if err := SetDataDir(directory); err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]any{"manual": remnawaveStyleJSON + "\n"})
	result, err := ImportSources(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	var decoded importResult
	_ = json.Unmarshal([]byte(result), &decoded)
	if len(decoded.Profiles) != 3 || decoded.Profiles[2].SourceIndex != manualSourceIndex || decoded.Sources[0].Index != manualSourceIndex {
		t.Fatalf("manual configs were not imported: %s", result)
	}
	mobile.Lock()
	mobile.profiles = nil
	mobile.Unlock()
	cached, err := LoadCachedProfiles()
	if err != nil || !strings.Contains(cached, "Germany") {
		t.Fatalf("cache was not restored: %q %v", cached, err)
	}
	if err := Configure(`{"dnsId":"custom","customDns":["9.9.9.9"],"routingMode":"bypass","routingRules":["geosite:youtube"],"subscriptionRules":true}`); err != nil {
		t.Fatalf("geosite rule must validate with default GeoData URLs: %v", err)
	}
	if !mobile.options.SubscriptionRules || mobile.options.PreferredSource != -1 {
		t.Fatalf("options not stored: %#v", mobile.options)
	}
}

func TestAndroidAutoConfigLoadsInXray(t *testing.T) {
	first := mustProfile(t, strings.Replace(remnawaveStyleJSON, "de.example.com", "203.0.113.10", 1))
	second := mustProfile(t, `trojan://pass@198.51.100.7:443?security=tls&sni=t.example#Trojan`)
	first.rules = first.rules[2:] // bittorrent block + proxy rule, no GeoData needed
	first.rules[1]["domain"] = []string{"full:chat.openai.com"}
	routing, _ := newRoutingOptions("full", nil, "", "")
	config, err := makeAutoConfigWithRoutingAndDoH([]Profile{first, second}, "cloudflare", nil, "", true, "", routing)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := androidRuntimeConfig(config, androidRuntimeOptions{
		DNS: dnsPresets["cloudflare"], RoutingMode: "full", SubscriptionRules: true, Auto: true,
		Profiles: []Profile{first, second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.LoadConfig("json", bytes.NewReader(encoded)); err != nil {
		t.Fatalf("Xray rejected the Auto config: %v\n%s", err, encoded)
	}
}

func TestMissingGeoCodeInUserRuleIsReported(t *testing.T) {
	rules := []any{
		map[string]any{"outboundTag": "dns-out"},
		map[string]any{"domain": []any{"geosite:nope"}, "outboundTag": "direct"},
	}
	info := geoDataInfo{Codes: map[string]map[string]bool{"geosite.dat": {"YOUTUBE": true}}}
	if _, err := dropMissingGeoCodes(rules, map[int]bool{1: true}, info); err == nil {
		t.Fatal("unknown user geosite must be reported")
	}
	kept, err := dropMissingGeoCodes(rules, map[int]bool{}, info)
	if err != nil || len(kept) != 1 {
		t.Fatalf("unknown subscription geosite must be dropped: %v %v", kept, err)
	}
}

// Set GEODATA_DIR to a directory with real geoip.dat and geosite.dat to check
// the wire-format scanner against published files.
func TestScanRealGeoData(t *testing.T) {
	directory := os.Getenv("GEODATA_DIR")
	if directory == "" {
		t.Skip("GEODATA_DIR not set")
	}
	for file, want := range map[string][]string{"geoip.dat": {"RU", "PRIVATE"}, "geosite.dat": {"CATEGORY-RU", "YOUTUBE", "OPENAI", "CATEGORY-ADS-ALL"}} {
		data, err := os.ReadFile(filepath.Join(directory, file))
		if err != nil {
			t.Fatal(err)
		}
		codes, err := scanGeoData(data)
		if err != nil {
			t.Fatal(err)
		}
		set := map[string]bool{}
		for _, code := range codes {
			set[code] = true
		}
		for _, code := range want {
			if !set[code] {
				t.Fatalf("%s lacks %s (%d codes)", file, code, len(codes))
			}
		}
		t.Logf("%s: %d codes", file, len(codes))
	}
	if _, err := scanGeoData([]byte("not a protobuf")); err == nil {
		t.Fatal("garbage accepted")
	}
}

func TestRealGeoDataConfigLoadsInXray(t *testing.T) {
	directory := os.Getenv("GEODATA_DIR")
	if directory == "" {
		t.Skip("GEODATA_DIR not set")
	}
	info := geoDataInfo{Directory: directory, Codes: map[string]map[string]bool{}}
	for _, file := range []string{"geoip.dat", "geosite.dat"} {
		data, _ := os.ReadFile(filepath.Join(directory, file))
		codes, err := scanGeoData(data)
		if err != nil {
			t.Fatal(err)
		}
		info.Codes[file] = map[string]bool{}
		for _, code := range codes {
			info.Codes[file][code] = true
		}
	}
	profile := mustProfile(t, strings.Replace(remnawaveStyleJSON, "de.example.com", "203.0.113.10", 1))
	routing, err := newRoutingOptions("bypass", []string{"geosite:youtube", "geoip:ru"}, defaultGeoIPURL, defaultGeoSiteURL)
	if err != nil {
		t.Fatal(err)
	}
	config, err := makeConfigWithRoutingAndDoH(profile, "cloudflare", nil, "", false, "", routing)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := androidRuntimeConfig(config, androidRuntimeOptions{
		DNS: dnsPresets["cloudflare"], RoutingMode: "bypass", SubscriptionRules: true, Profiles: []Profile{profile},
		GeoData: func(bool, bool) (geoDataInfo, error) { return info, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := core.LoadConfig("json", bytes.NewReader(encoded)); err != nil {
		t.Fatalf("Xray rejected GeoData rules: %v", err)
	}
}

func TestSubscriptionGeoRulesDoNotBlockConnecting(t *testing.T) {
	profile := mustProfile(t, remnawaveStyleJSON)
	raw, _ := json.Marshal(map[string]any{
		"inbounds":  []any{map[string]any{"tag": "tun-in", "protocol": "tun", "settings": map[string]any{}}},
		"outbounds": []any{map[string]any{"tag": "proxy", "protocol": "freedom"}},
	})
	failing := func(bool, bool) (geoDataInfo, error) { return geoDataInfo{}, errors.New("offline") }
	encoded, err := androidRuntimeConfig(raw, androidRuntimeOptions{
		DNS: dnsPresets["cloudflare"], RoutingMode: "full", SubscriptionRules: true, Profiles: []Profile{profile}, GeoData: failing,
	})
	if err != nil {
		t.Fatalf("subscription geo rules must not block connecting: %v", err)
	}
	var document map[string]any
	_ = json.Unmarshal(encoded, &document)
	if document["env"] != nil {
		t.Fatal("no GeoData directory may be set")
	}
	text := string(encoded)
	if strings.Contains(text, "geoip:") || strings.Contains(text, "geosite:") || !strings.Contains(text, "192.168.0.0/16") {
		t.Fatalf("geo values must be dropped and geoip:private expanded: %s", text)
	}
	if _, err := core.LoadConfig("json", bytes.NewReader(encoded)); err != nil {
		t.Fatalf("Xray rejected the config: %v", err)
	}

	// The user's own geo rule, however, needs the lists.
	routing, _ := newRoutingOptions("bypass", []string{"geosite:youtube"}, defaultGeoIPURL, defaultGeoSiteURL)
	single := mustProfile(t, "trojan://p@198.51.100.7:443?security=tls#T")
	config, _ := makeConfigWithRoutingAndDoH(single, "cloudflare", nil, "", false, "", routing)
	if _, err := androidRuntimeConfig(config, androidRuntimeOptions{DNS: dnsPresets["cloudflare"], RoutingMode: "bypass", GeoData: failing}); err == nil {
		t.Fatal("user geo rules without GeoData must fail")
	}
}
