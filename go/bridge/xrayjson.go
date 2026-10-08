package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// Client protocols that can be imported from Xray JSON as a server.
var xrayProxyProtocols = map[string]bool{
	"vless": true, "vmess": true, "trojan": true, "shadowsocks": true,
	"hysteria": true, "naive": true, "socks": true, "http": true, "wireguard": true,
}

// Xray protocols that are not servers but may appear next to them.
var xrayServiceProtocols = map[string]bool{
	"freedom": true, "blackhole": true, "dns": true, "loopback": true,
}

// Only transport tuning is accepted from subscription sockopt. Interface
// binding, marks, TPROXY and similar host-level overrides are dropped.
var allowedSockopt = map[string]bool{
	"tcpFastOpen": true, "tcpNoDelay": true, "tcpKeepAliveIdle": true,
	"tcpKeepAliveInterval": true, "tcpMptcp": true, "tcpCongestion": true,
	"tcpUserTimeout": true, "tcpMaxSeg": true, "tcpWindowClamp": true,
	"domainStrategy": true, "happyEyeballs": true, "addressPortStrategy": true,
}

var allowedRuleKeys = map[string]bool{
	"type": true, "domain": true, "ip": true, "port": true, "network": true,
	"protocol": true, "outboundTag": true, "balancerTag": true, "ruleTag": true,
}

const maxSubscriptionRules = 256

// parseXrayJSON imports servers from Xray JSON: an array of full client
// configs (the "v2ray-json" subscription format), a single config, or bare
// outbound objects. Listeners, DNS and host-level socket options are never
// taken from the subscription; routing rules are kept in sanitized form and
// applied only when the user enables them.
func parseXrayJSON(data []byte, skipped *int) ([]Profile, error) {
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return nil, errors.New("Некорректный JSON")
	}
	var items []any
	switch value := document.(type) {
	case []any:
		items = value
	case map[string]any:
		items = []any{value}
	default:
		return nil, errors.New("Некорректный JSON")
	}
	countSkip := func() {
		if skipped != nil {
			(*skipped)++
		}
	}
	profiles := []Profile{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			countSkip()
			continue
		}
		if _, isOutbound := item["protocol"]; isOutbound {
			name := firstNonEmpty(stringValue(item["remarks"]), stringValue(item["tag"]))
			profile, err := profileFromOutbound(name, item, nil)
			if err != nil {
				countSkip()
				continue
			}
			profiles = append(profiles, profile)
			continue
		}
		imported, ignored := profilesFromXrayConfig(item)
		if skipped != nil {
			*skipped += ignored
		}
		profiles = append(profiles, imported...)
	}
	return profiles, nil
}

func profilesFromXrayConfig(config map[string]any) ([]Profile, int) {
	remarks := firstNonEmpty(stringValue(config["remarks"]), stringValue(config["ps"]))
	rawOutbounds, _ := config["outbounds"].([]any)
	byTag := make(map[string]map[string]any, len(rawOutbounds))
	outbounds := make([]map[string]any, 0, len(rawOutbounds))
	for _, raw := range rawOutbounds {
		if outbound, ok := raw.(map[string]any); ok {
			outbounds = append(outbounds, outbound)
			if tag := stringValue(outbound["tag"]); tag != "" {
				byTag[tag] = outbound
			}
		}
	}

	// Outbounds used as another outbound's dialer are hops, not servers.
	dialers := map[string]bool{}
	for _, outbound := range outbounds {
		if tag := dialerTag(outbound); tag != "" {
			dialers[tag] = true
		}
	}
	ignored := 0
	candidates := make([]map[string]any, 0, len(outbounds))
	for _, outbound := range outbounds {
		protocol := stringValue(outbound["protocol"])
		if xrayProxyProtocols[protocol] && !dialers[stringValue(outbound["tag"])] {
			candidates = append(candidates, outbound)
		} else if !xrayServiceProtocols[protocol] && !dialers[stringValue(outbound["tag"])] {
			ignored++
		}
	}
	if len(candidates) == 0 {
		return nil, ignored + 1
	}

	routing, _ := config["routing"].(map[string]any)
	chosen := balancerMembers(routing, candidates)
	if len(chosen) == 0 {
		chosen = []map[string]any{mainOutbound(candidates)}
	}
	rules, domainStrategy := subscriptionRules(routing, byTag)

	members := make([]Profile, 0, len(chosen))
	for _, outbound := range chosen {
		name := firstNonEmpty(remarks, stringValue(outbound["tag"]))
		if len(chosen) > 1 {
			name = strings.TrimSpace(strings.Join(nonEmpty(remarks, stringValue(outbound["tag"])), " · "))
		}
		profile, err := profileFromOutbound(name, outbound, byTag)
		if err != nil {
			ignored++
			continue
		}
		profile.rules = rules
		profile.domainStrategy = domainStrategy
		members = append(members, profile)
	}
	switch len(members) {
	case 0:
		return nil, ignored
	case 1:
		members[0].Name = firstNonEmpty(remarks, members[0].Name)
		return members, ignored
	}
	group := newGroupProfile(firstNonEmpty(remarks, "Балансировщик"), members)
	group.rules = rules
	group.domainStrategy = domainStrategy
	return []Profile{group}, ignored
}

// newGroupProfile makes one selectable profile out of a balancer's servers.
func newGroupProfile(name string, members []Profile) Profile {
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	sum := sha256.Sum256([]byte("group:" + strings.Join(ids, ",")))
	return Profile{
		ID: hex.EncodeToString(sum[:12]), Name: name, Protocol: "balancer",
		SourceIndex: -1, Format: "json", Members: len(members), members: members,
	}
}

// flattenGroups replaces balancer profiles by their servers, which inherit
// the group's source. groupOf maps each such server to its group's ID.
func flattenGroups(profiles []Profile) (flat []Profile, groupOf map[string]string) {
	groupOf = map[string]string{}
	seen := map[string]bool{}
	for _, profile := range profiles {
		if len(profile.members) == 0 {
			if !seen[profile.ID] {
				seen[profile.ID] = true
				flat = append(flat, profile)
			}
			continue
		}
		for _, member := range profile.members {
			if seen[member.ID] {
				continue
			}
			seen[member.ID] = true
			member.SourceIndex = profile.SourceIndex
			groupOf[member.ID] = profile.ID
			flat = append(flat, member)
		}
	}
	return flat, groupOf
}

func nonEmpty(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

// mainOutbound picks the server a client config sends traffic to by default:
// the one tagged "proxy" by convention, otherwise the first server.
func mainOutbound(candidates []map[string]any) map[string]any {
	for _, outbound := range candidates {
		if stringValue(outbound["tag"]) == "proxy" {
			return outbound
		}
	}
	return candidates[0]
}

// balancerMembers returns every server referenced by a balancer, so a config
// that balances several servers is imported as several selectable servers.
func balancerMembers(routing map[string]any, candidates []map[string]any) []map[string]any {
	balancers, _ := routing["balancers"].([]any)
	var selectors []string
	for _, raw := range balancers {
		balancer, _ := raw.(map[string]any)
		list, _ := balancer["selector"].([]any)
		for _, selector := range list {
			if value := stringValue(selector); value != "" {
				selectors = append(selectors, value)
			}
		}
	}
	if len(selectors) == 0 {
		return nil
	}
	members := []map[string]any{}
	for _, outbound := range candidates {
		tag := stringValue(outbound["tag"])
		for _, selector := range selectors {
			if strings.HasPrefix(tag, selector) {
				members = append(members, outbound)
				break
			}
		}
	}
	return members
}

func dialerTag(outbound map[string]any) string {
	if stream, ok := outbound["streamSettings"].(map[string]any); ok {
		if sockopt, ok := stream["sockopt"].(map[string]any); ok {
			if tag := stringValue(sockopt["dialerProxy"]); tag != "" {
				return tag
			}
		}
	}
	if proxy, ok := outbound["proxySettings"].(map[string]any); ok {
		return stringValue(proxy["tag"])
	}
	return ""
}

// profileFromOutbound sanitizes a server outbound. When it chains through a
// freedom outbound (the usual way Xray JSON expresses TLS fragmentation), that
// hop is kept under a content-derived tag; chaining through another server is
// not supported.
func profileFromOutbound(name string, outbound map[string]any, byTag map[string]map[string]any) (Profile, error) {
	protocol := stringValue(outbound["protocol"])
	if !xrayProxyProtocols[protocol] {
		return Profile{}, errors.New("unsupported protocol")
	}
	settings, _ := outbound["settings"].(map[string]any)
	if settings == nil {
		return Profile{}, errors.New("missing settings")
	}
	clean := map[string]any{"protocol": protocol, "settings": settings, "tag": "proxy"}
	if mux := sanitizedMux(outbound["mux"]); mux != nil {
		clean["mux"] = mux
	}
	stream := map[string]any{}
	if source, ok := outbound["streamSettings"].(map[string]any); ok {
		for key, value := range source {
			if key != "sockopt" {
				stream[key] = value
			}
		}
	}
	sockopt := sanitizedSockopt(outbound["streamSettings"])

	var chain []map[string]any
	if tag := dialerTag(outbound); tag != "" {
		hop, ok := byTag[tag]
		if !ok || stringValue(hop["protocol"]) != "freedom" {
			return Profile{}, errors.New("proxy chaining is not supported")
		}
		dialer := sanitizedFreedom(hop)
		encoded, _ := json.Marshal(dialer)
		sum := sha256.Sum256(encoded)
		dialer["tag"] = "dialer-" + hex.EncodeToString(sum[:6])
		sockopt["dialerProxy"] = dialer["tag"]
		chain = []map[string]any{dialer}
	}
	if len(sockopt) > 0 {
		stream["sockopt"] = sockopt
	}
	if len(stream) > 0 {
		clean["streamSettings"] = stream
	}
	profile := newProfile(name, clean)
	profile.Format = "json"
	if protocol == "wireguard" {
		profile.Transport = "udp"
	}
	if profile.address == "" || profile.port < 1 || profile.port > 65535 {
		return Profile{}, errors.New("missing endpoint")
	}
	profile.chain = chain
	return profile, nil
}

func sanitizedMux(raw any) map[string]any {
	source, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	result := map[string]any{}
	for _, key := range []string{"enabled", "concurrency", "xudpConcurrency", "xudpProxyUDP443"} {
		if value, exists := source[key]; exists {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func sanitizedSockopt(streamSettings any) map[string]any {
	result := map[string]any{}
	stream, _ := streamSettings.(map[string]any)
	source, _ := stream["sockopt"].(map[string]any)
	for key, value := range source {
		if allowedSockopt[key] {
			result[key] = value
		}
	}
	return result
}

func sanitizedFreedom(outbound map[string]any) map[string]any {
	settings := map[string]any{}
	if source, ok := outbound["settings"].(map[string]any); ok {
		for _, key := range []string{"fragment", "noise", "noises", "domainStrategy"} {
			if value, exists := source[key]; exists {
				settings[key] = value
			}
		}
	}
	result := map[string]any{"protocol": "freedom", "settings": settings}
	stream := map[string]any{}
	if source, ok := outbound["streamSettings"].(map[string]any); ok {
		if mask, exists := source["finalmask"]; exists {
			stream["finalmask"] = mask
		}
	}
	if sockopt := sanitizedSockopt(outbound["streamSettings"]); len(sockopt) > 0 {
		stream["sockopt"] = sockopt
	}
	if len(stream) > 0 {
		result["streamSettings"] = stream
	}
	return result
}

// subscriptionRules converts the config's routing rules into rules whose
// target is one of "proxy", "direct" or "block". Rules bound to specific
// inbounds, users or processes, rules for DNS, catch-alls and references to
// custom ext: data files are dropped.
func subscriptionRules(routing map[string]any, byTag map[string]map[string]any) ([]map[string]any, string) {
	domainStrategy := ""
	switch value := stringValue(routing["domainStrategy"]); value {
	case "AsIs", "IPIfNonMatch", "IPOnDemand":
		domainStrategy = value
	}
	rawRules, _ := routing["rules"].([]any)
	rules := make([]map[string]any, 0, len(rawRules))
	for _, raw := range rawRules {
		if len(rules) >= maxSubscriptionRules {
			break
		}
		source, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		valid := true
		for key := range source {
			if !allowedRuleKeys[key] {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		target := ruleTarget(source, byTag)
		if target == "" {
			continue
		}
		rule := map[string]any{"type": "field", "outboundTag": target}
		matchers := 0
		for _, key := range []string{"domain", "ip"} {
			if values := cleanRuleValues(source[key]); len(values) > 0 {
				rule[key] = values
				matchers++
			} else if source[key] != nil {
				// Every value was dropped; keeping the rule would widen it.
				valid = false
			}
		}
		for _, key := range []string{"port", "network", "protocol"} {
			if value, exists := source[key]; exists {
				rule[key] = value
				if key != "network" {
					matchers++
				}
			}
		}
		if !valid || matchers == 0 {
			continue
		}
		rules = append(rules, rule)
	}
	return rules, domainStrategy
}

func ruleTarget(rule map[string]any, byTag map[string]map[string]any) string {
	if stringValue(rule["balancerTag"]) != "" {
		return "proxy"
	}
	outbound, ok := byTag[stringValue(rule["outboundTag"])]
	if !ok {
		return ""
	}
	switch protocol := stringValue(outbound["protocol"]); {
	case xrayProxyProtocols[protocol]:
		return "proxy"
	case protocol == "freedom":
		return "direct"
	case protocol == "blackhole":
		return "block"
	default:
		return ""
	}
}

func cleanRuleValues(raw any) []string {
	var list []any
	switch value := raw.(type) {
	case []any:
		list = value
	case string:
		list = []any{value}
	}
	result := make([]string, 0, len(list))
	for _, item := range list {
		value := stringValue(item)
		lower := strings.ToLower(value)
		if value == "" || len(value) > 255 || strings.ContainsAny(value, "\"'\\\n") ||
			strings.HasPrefix(lower, "ext:") || strings.HasPrefix(lower, "ext-") {
			continue
		}
		if prefix, tag, found := strings.Cut(lower, ":"); found && (prefix == "geosite" || prefix == "geoip") {
			if !validGeoDataTag(strings.TrimPrefix(tag, "!")) {
				continue
			}
		}
		result = append(result, value)
	}
	return result
}
