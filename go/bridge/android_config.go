package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

type androidRuntimeOptions struct {
	MTU               int
	IPv6              bool
	IPType            string // "auto", "ipv4" or "ipv6": answers DNS gives apps
	Mux               bool
	RouteOnly         bool // sniffed domains route, but connect to the original IP
	BlockUDP          bool // everything but DNS; apps fall back from QUIC to TCP
	LocalProxy        localProxy
	DNS               dnsPreset
	RoutingMode       string
	SubscriptionRules bool
	Auto              bool
	// Profiles are the servers in the config; the first one supplies the
	// subscription routing rules.
	Profiles []Profile
	LogPath  string
	// GeoData makes geoip.dat/geosite.dat available. It is called only when
	// the final rules reference them.
	GeoData func(needIP, needSite bool) (geoDataInfo, error)
}

// androidRuntimeConfig adapts the desktop config to Android: the platform
// owns addresses, routes and the DNS server, all DNS is answered by Xray
// through the tunnel with the user's provider, and nothing listens on a local
// port that other apps could find.
func androidRuntimeConfig(config []byte, options androidRuntimeOptions) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(config, &document); err != nil {
		return nil, err
	}
	if options.MTU == 0 {
		options.MTU = 1400
	}
	inbounds, _ := document["inbounds"].([]any)
	var tun map[string]any
	for _, raw := range inbounds {
		if inbound, ok := raw.(map[string]any); ok && stringValue(inbound["protocol"]) == "tun" {
			tun = inbound
			break
		}
	}
	if tun == nil {
		return nil, errors.New("Конфигурация Xray не содержит TUN")
	}
	settings, ok := tun["settings"].(map[string]any)
	if !ok {
		return nil, errors.New("Конфигурация TUN не содержит настроек")
	}
	delete(settings, "autoSystemRoutingTable")
	delete(settings, "autoOutboundsInterface")
	delete(settings, "gateway")
	delete(settings, "dns")
	settings["mtu"] = options.MTU
	sniffing, _ := tun["sniffing"].(map[string]any)
	if sniffing != nil && options.RouteOnly {
		sniffing["routeOnly"] = true
	}
	// The TUN stays first: sessions attach it separately from the rest.
	document["inbounds"] = append([]any{tun}, options.LocalProxy.inbounds(sniffing)...)

	// Traffic counters come from Android; per-connection stats and debug
	// logging only cost CPU and battery.
	delete(document, "stats")
	delete(document, "policy")
	delete(document, "geodata")
	logConfig := map[string]any{"loglevel": "warning", "dnsLog": false, "access": "none"}
	if options.LogPath != "" {
		logConfig["error"] = options.LogPath
	}
	document["log"] = logConfig

	outbounds, _ := document["outbounds"].([]any)
	tags := map[string]bool{}
	for _, raw := range outbounds {
		if outbound, ok := raw.(map[string]any); ok {
			tags[stringValue(outbound["tag"])] = true
		}
	}
	addOutbound := func(outbound map[string]any) {
		tag := stringValue(outbound["tag"])
		if tags[tag] {
			return
		}
		tags[tag] = true
		outbounds = append(outbounds, outbound)
	}
	for _, profile := range options.Profiles {
		for _, hop := range profile.chain {
			clone, err := cloneObject(hop)
			if err != nil {
				return nil, err
			}
			addOutbound(clone)
		}
	}
	addOutbound(map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}})
	addOutbound(map[string]any{"tag": "block", "protocol": "blackhole", "settings": map[string]any{}})
	addOutbound(map[string]any{"tag": "dns-out", "protocol": "dns", "settings": map[string]any{}})
	if options.Mux {
		for _, raw := range outbounds {
			if outbound, ok := raw.(map[string]any); ok {
				enableMux(outbound)
			}
		}
	}
	document["outbounds"] = outbounds
	document["dns"] = androidDNSConfig(options.DNS, options.IPv6, options.IPType)

	rules, userRules := androidRoutingRules(document, options)
	expandPrivateIPs(rules)
	delete(document, "env")
	if needIP, needSite := geoDataNeeds(rules); needIP || needSite {
		var info geoDataInfo
		err := errors.New("GeoData недоступна")
		if options.GeoData != nil {
			info, err = options.GeoData(needIP, needSite)
		}
		if err != nil {
			// The user's own geo rules cannot work without the lists, so say
			// so. Subscription rules are an extra: connect without them.
			if userNeedsGeoData(rules, userRules) {
				return nil, err
			}
			info = geoDataInfo{}
		}
		if rules, err = dropMissingGeoCodes(rules, userRules, info); err != nil {
			return nil, err
		}
		if needIP, needSite = geoDataNeeds(rules); needIP || needSite {
			document["env"] = map[string]any{"XRAY_LOCATION_ASSET": info.Directory}
		}
	}
	routing, _ := document["routing"].(map[string]any)
	if routing == nil {
		routing = map[string]any{}
	}
	if options.BlockUDP {
		// Right after the DNS hijack, so DNS keeps working and nothing
		// below can send UDP anywhere.
		block := map[string]any{"type": "field", "inboundTag": []string{"tun-in"}, "network": "udp", "outboundTag": "block"}
		rules = append(rules[:1], append([]any{block}, rules[1:]...)...)
	}
	// The loopback guard matches IPs only; it must not make every domain
	// resolve before routing, so the strategy is decided without it.
	routing["domainStrategy"] = routingDomainStrategy(rules, options)
	routing["rules"] = applyLocalProxyRules(rules, options.LocalProxy)
	document["routing"] = routing
	return json.Marshal(document)
}

// androidDNSConfig answers every DNS query of the tunnel with the selected
// provider. Queries leave through the proxy, so the ISP sees none of them.
func androidDNSConfig(preset dnsPreset, ipv6 bool, ipType string) map[string]any {
	servers := []string{}
	if preset.DoHURL != "" {
		// One DoH connection serves every query; while it is being
		// re-established after the server or proxy closed it, queries fail.
		// A plain TCP resolver of another provider answers meanwhile, also
		// through the proxy.
		fallback := "tcp://1.1.1.1"
		if strings.Contains(preset.DoHURL, "1.1.1.1") || strings.Contains(preset.DoHURL, "cloudflare") {
			fallback = "tcp://8.8.8.8"
		}
		servers = append(servers, preset.DoHURL, fallback)
	} else {
		for _, server := range preset.Servers {
			address, err := netip.ParseAddr(server)
			if err == nil && (address.Is4() || ipv6) {
				servers = append(servers, address.String())
			}
		}
	}
	if len(servers) == 0 {
		servers = append(servers, "1.1.1.1")
	}
	strategy := "UseIPv4"
	if ipv6 {
		switch ipType {
		case "ipv4":
		case "ipv6":
			strategy = "UseIPv6"
		default:
			strategy = "UseIP"
		}
	}
	return map[string]any{"servers": servers, "queryStrategy": strategy}
}

// enableMux turns on Mux.Cool for proxy protocols that support it, unless
// the subscription already configured it. XTLS Vision (a flow), XHTTP with
// its own multiplexing and QUIC-based protocols are left alone.
func enableMux(outbound map[string]any) {
	switch stringValue(outbound["protocol"]) {
	case "vmess", "vless", "trojan", "shadowsocks":
	default:
		return
	}
	if _, configured := outbound["mux"]; configured {
		return
	}
	if stream, ok := outbound["streamSettings"].(map[string]any); ok {
		switch stringValue(stream["network"]) {
		case "xhttp", "splithttp":
			return
		}
	}
	settings, _ := outbound["settings"].(map[string]any)
	if stringValue(settings["flow"]) != "" {
		return
	}
	for _, key := range []string{"vnext", "servers"} {
		servers, _ := settings[key].([]any)
		for _, raw := range servers {
			server, _ := raw.(map[string]any)
			users, _ := server["users"].([]any)
			for _, rawUser := range users {
				if user, ok := rawUser.(map[string]any); ok && stringValue(user["flow"]) != "" {
					return
				}
			}
		}
	}
	outbound["mux"] = map[string]any{"enabled": true, "concurrency": 8, "xudpConcurrency": 16, "xudpProxyUDP443": "reject"}
}

// androidRoutingRules orders the rules as: DNS hijack, the user's rules, the
// subscription's rules, then the catch-all rules of the selected mode.
func androidRoutingRules(document map[string]any, options androidRuntimeOptions) ([]any, map[int]bool) {
	routing, _ := document["routing"].(map[string]any)
	existing, _ := routing["rules"].([]any)
	rules := []any{map[string]any{
		"type": "field", "inboundTag": []string{"tun-in"},
		"network": "tcp,udp", "port": "53", "outboundTag": "dns-out",
	}}
	var catchAll []any
	userRules := map[int]bool{}
	for _, raw := range existing {
		rule, ok := raw.(map[string]any)
		if !ok || stringValue(rule["outboundTag"]) == "dns-out" {
			continue
		}
		if isCatchAllRule(rule) {
			catchAll = append(catchAll, rule)
		} else {
			userRules[len(rules)] = true
			rules = append(rules, rule)
		}
	}
	if options.SubscriptionRules && len(options.Profiles) > 0 {
		for _, source := range options.Profiles[0].rules {
			rule, err := cloneObject(source)
			if err != nil {
				continue
			}
			if stringValue(rule["outboundTag"]) == "proxy" {
				// In "only selected via VPN" mode the user's list is the
				// whole proxied set.
				if options.RoutingMode == "proxy_only" {
					continue
				}
				if options.Auto {
					delete(rule, "outboundTag")
					rule["balancerTag"] = "shadow-auto"
				}
			}
			rule["inboundTag"] = []string{"tun-in"}
			rules = append(rules, rule)
		}
	}
	rules = append(rules, catchAll...)
	return rules, userRules
}

// routingDomainStrategy resolves domains only when some rule matches on IP:
// otherwise it would cost a DNS round trip per connection for nothing.
func routingDomainStrategy(rules []any, options androidRuntimeOptions) string {
	for _, raw := range rules[1:] {
		if rule, ok := raw.(map[string]any); ok && rule["ip"] != nil {
			strategy := ""
			if options.SubscriptionRules && len(options.Profiles) > 0 {
				strategy = options.Profiles[0].domainStrategy
			}
			return firstNonEmpty(strategy, "IPIfNonMatch")
		}
	}
	return "AsIs"
}

// dropMissingGeoCodes removes geosite:/geoip: values absent from the data
// files, which would otherwise stop Xray from starting. A missing code in the
// user's own rules is reported; in subscription rules it is skipped.
func dropMissingGeoCodes(rules []any, userRules map[int]bool, info geoDataInfo) ([]any, error) {
	result := make([]any, 0, len(rules))
	for index, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		emptied := false
		for _, key := range []string{"domain", "ip"} {
			values := ruleValues(rule[key])
			if values == nil {
				continue
			}
			kept := make([]string, 0, len(values))
			for _, value := range values {
				prefix, code, found := strings.Cut(strings.TrimPrefix(value, "!"), ":")
				prefix = strings.ToLower(prefix)
				if found && (prefix == "geosite" || prefix == "geoip") {
					file := "geoip.dat"
					if prefix == "geosite" {
						file = "geosite.dat"
					}
					code, _, _ = strings.Cut(strings.TrimPrefix(code, "!"), "@")
					if !info.has(file, code) {
						if userRules[index] {
							return nil, fmt.Errorf("Список %s не найден в %s", value, file)
						}
						continue
					}
				}
				kept = append(kept, value)
			}
			if len(kept) == 0 {
				emptied = true
			}
			rule[key] = kept
		}
		if !emptied {
			result = append(result, rule)
		}
	}
	return result, nil
}

func isCatchAllRule(rule map[string]any) bool {
	for key := range rule {
		switch key {
		case "type", "inboundTag", "outboundTag", "balancerTag":
		default:
			return false
		}
	}
	return true
}

// privateCIDRs is what geoip:private covers; spelling it out avoids
// downloading an 18 MB GeoIP file for the most common rule of all.
var privateCIDRs = []string{
	"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
	"192.0.0.0/24", "192.168.0.0/16", "198.18.0.0/15", "224.0.0.0/3",
	"::1/128", "fc00::/7", "fe80::/10", "ff00::/8",
}

func expandPrivateIPs(rules []any) {
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		values := ruleValues(rule["ip"])
		if values == nil {
			continue
		}
		expanded := make([]string, 0, len(values))
		for _, value := range values {
			if strings.EqualFold(value, "geoip:private") {
				expanded = append(expanded, privateCIDRs...)
			} else {
				expanded = append(expanded, value)
			}
		}
		rule["ip"] = expanded
	}
}

func userNeedsGeoData(rules []any, userRules map[int]bool) bool {
	for index := range userRules {
		if index < len(rules) {
			if needIP, needSite := geoDataNeeds(rules[index : index+1]); needIP || needSite {
				return true
			}
		}
	}
	return false
}

func geoDataNeeds(rules []any) (needIP, needSite bool) {
	for _, raw := range rules {
		rule, _ := raw.(map[string]any)
		for _, key := range []string{"domain", "ip"} {
			for _, value := range ruleValues(rule[key]) {
				value = strings.ToLower(strings.TrimPrefix(value, "!"))
				needIP = needIP || strings.HasPrefix(value, "geoip:")
				needSite = needSite || strings.HasPrefix(value, "geosite:")
			}
		}
	}
	return needIP, needSite
}

func ruleValues(raw any) []string {
	switch value := raw.(type) {
	case []string:
		return value
	case []any:
		result := make([]string, 0, len(value))
		for _, item := range value {
			result = append(result, stringValue(item))
		}
		return result
	case string:
		return []string{value}
	}
	return nil
}
