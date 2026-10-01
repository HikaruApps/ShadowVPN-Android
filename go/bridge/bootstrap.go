package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
)

var lookupEndpointIPs = func(ctx context.Context, host string) ([]net.IPAddr, error) {
	return net.DefaultResolver.LookupIPAddr(ctx, host)
}

type endpointBootstrap struct {
	OriginalAddress string
	SelectedAddress string
	Candidates      []string
}

// resolveProfileEndpoint resolves the proxy server before TUN changes the
// system DNS and routes. The runtime config then dials the selected IP while
// retaining the original hostname everywhere it is part of TLS or transport
// semantics. This prevents the bootstrap loop where resolving the proxy itself
// requires an already working proxy connection.
func resolveProfileEndpoint(ctx context.Context, profile Profile) (Profile, endpointBootstrap, error) {
	host := strings.TrimSpace(profile.address)
	if host == "" || profile.port < 1 || profile.port > 65535 {
		return Profile{}, endpointBootstrap{}, errors.New("профиль не содержит корректный адрес сервера")
	}

	info := endpointBootstrap{OriginalAddress: host}
	if parsed := net.ParseIP(host); parsed != nil {
		info.SelectedAddress = parsed.String()
		info.Candidates = []string{info.SelectedAddress}
		resolved, err := profileWithEndpoint(profile, info.SelectedAddress, "")
		return resolved, info, err
	}

	addresses, err := lookupEndpointIPs(ctx, host)
	if err != nil {
		return Profile{}, info, fmt.Errorf("не удалось разрешить домен VPN-сервера: %w", err)
	}
	info.Candidates = uniqueEndpointIPs(addresses)
	if len(info.Candidates) == 0 {
		return Profile{}, info, errors.New("DNS не вернул IP-адрес VPN-сервера")
	}

	// IPv4 is preferred on Windows because many client networks advertise IPv6
	// without providing a usable upstream route. uniqueEndpointIPs orders IPv4
	// candidates first and keeps IPv6 as a fallback.
	info.SelectedAddress = info.Candidates[0]
	resolved, err := profileWithEndpoint(profile, info.SelectedAddress, host)
	if err != nil {
		return Profile{}, info, err
	}
	return resolved, info, nil
}

func uniqueEndpointIPs(addresses []net.IPAddr) []string {
	seen := make(map[string]struct{}, len(addresses))
	ipv4 := make([]string, 0, len(addresses))
	ipv6 := make([]string, 0, len(addresses))
	for _, address := range addresses {
		ip := address.IP
		if ip == nil {
			continue
		}
		value := ip.String()
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		if ip.To4() != nil {
			ipv4 = append(ipv4, value)
		} else {
			ipv6 = append(ipv6, value)
		}
	}
	return append(ipv4, ipv6...)
}

func profileWithEndpoint(profile Profile, address, originalHost string) (Profile, error) {
	encoded, err := json.Marshal(profile.outbound)
	if err != nil {
		return Profile{}, fmt.Errorf("не удалось скопировать конфигурацию сервера: %w", err)
	}
	var outbound map[string]any
	if err := json.Unmarshal(encoded, &outbound); err != nil {
		return Profile{}, fmt.Errorf("не удалось скопировать конфигурацию сервера: %w", err)
	}
	if !replaceOutboundEndpoint(outbound, address) {
		return Profile{}, errors.New("не удалось заменить адрес сервера в конфигурации")
	}
	if originalHost != "" {
		preserveTransportHostname(outbound, originalHost)
	}
	profile.address = address
	profile.outbound = outbound
	return profile, nil
}

func replaceOutboundEndpoint(outbound map[string]any, address string) bool {
	settings, _ := outbound["settings"].(map[string]any)
	protocol, _ := outbound["protocol"].(string)
	if protocol == "hysteria" || protocol == "naive" {
		if settings == nil {
			return false
		}
		settings["address"] = address
		return true
	}
	key := "servers"
	if protocol == "vless" || protocol == "vmess" {
		key = "vnext"
	}
	servers, _ := settings[key].([]any)
	if len(servers) == 0 {
		return false
	}
	server, _ := servers[0].(map[string]any)
	if server == nil {
		return false
	}
	server["address"] = address
	return true
}

func preserveTransportHostname(outbound map[string]any, hostname string) {
	stream, _ := outbound["streamSettings"].(map[string]any)
	if stream == nil {
		return
	}
	security, _ := stream["security"].(string)
	switch security {
	case "tls":
		setStringIfEmpty(stream, "tlsSettings", "serverName", hostname)
	case "reality":
		setStringIfEmpty(stream, "realitySettings", "serverName", hostname)
	}

	network, _ := stream["network"].(string)
	switch network {
	case "ws":
		settings := nestedMap(stream, "wsSettings")
		headers := nestedMap(settings, "headers")
		if stringValue(headers["Host"]) == "" && stringValue(headers["host"]) == "" {
			headers["Host"] = hostname
		}
	case "xhttp":
		setStringIfEmpty(stream, "xhttpSettings", "host", hostname)
	case "httpupgrade":
		setStringIfEmpty(stream, "httpupgradeSettings", "host", hostname)
	case "grpc":
		setStringIfEmpty(stream, "grpcSettings", "authority", hostname)
	}
}

func setStringIfEmpty(parent map[string]any, settingsKey, valueKey, value string) {
	settings := nestedMap(parent, settingsKey)
	if stringValue(settings[valueKey]) == "" {
		settings[valueKey] = value
	}
}

func nestedMap(parent map[string]any, key string) map[string]any {
	if existing, ok := parent[key].(map[string]any); ok {
		return existing
	}
	created := map[string]any{}
	parent[key] = created
	return created
}

func stringValue(value any) string {
	result, _ := value.(string)
	return strings.TrimSpace(result)
}
