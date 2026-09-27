package bridge

import (
	"errors"
	"net/netip"
	"net/url"
	"strings"
)

type dnsPreset struct {
	Name    string
	Servers []string
	DoHURL  string
}

var dnsPresets = map[string]dnsPreset{
	"cloudflare": {Name: "Cloudflare", Servers: []string{"1.1.1.1", "1.0.0.1", "2606:4700:4700::1111", "2606:4700:4700::1001"}},
	"google":     {Name: "Google", Servers: []string{"8.8.8.8", "8.8.4.4", "2001:4860:4860::8888", "2001:4860:4860::8844"}},
	"quad9":      {Name: "Quad9", Servers: []string{"9.9.9.9", "149.112.112.112", "2620:fe::fe", "2620:fe::9"}},
}

func selectedDNS(value string, customServers []string) (string, dnsPreset, error) {
	return selectedDNSWithDoH(value, customServers, "")
}

func selectedDNSWithDoH(value string, customServers []string, dohURL string) (string, dnsPreset, error) {
	id := strings.ToLower(strings.TrimSpace(value))
	if id == "" {
		id = "cloudflare"
	}
	if id == "custom" {
		servers, err := validatedCustomDNS(customServers)
		if err != nil {
			return "", dnsPreset{}, err
		}
		return id, dnsPreset{Name: "Пользовательский DNS", Servers: servers}, nil
	}
	if id == "subscription-doh" {
		normalized, err := validatedDoHURL(dohURL)
		if err != nil {
			return "", dnsPreset{}, err
		}
		return id, dnsPreset{Name: "DoH подписки", Servers: []string{"1.1.1.1"}, DoHURL: normalized}, nil
	}
	preset, ok := dnsPresets[id]
	if !ok {
		return "", dnsPreset{}, errors.New("Выбран неизвестный DNS-сервер")
	}
	return id, preset, nil
}

func validatedDoHURL(value string) (string, error) {
	if len(value) == 0 || len(value) > 2048 || strings.TrimSpace(value) != value {
		return "", errors.New("Подписка не предоставила корректный DoH-адрес")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" || parsed.RawQuery != "" {
		return "", errors.New("DoH должен использовать корректный HTTPS-адрес")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/dns-query"
	}
	return parsed.String(), nil
}

func validatedCustomDNS(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > 4 {
		return nil, errors.New("Укажите от одного до четырёх DNS IP-адресов")
	}
	servers := make([]string, 0, len(values))
	seen := make(map[netip.Addr]bool, len(values))
	for _, value := range values {
		address, err := netip.ParseAddr(strings.TrimSpace(value))
		if err != nil {
			return nil, errors.New("Пользовательский DNS должен содержать только IPv4 или IPv6-адреса")
		}
		address = address.Unmap()
		isBroadcast := address.Is4() && address.As4() == [4]byte{255, 255, 255, 255}
		if address.IsUnspecified() || address.IsMulticast() || isBroadcast {
			return nil, errors.New("Этот DNS IP-адрес нельзя использовать")
		}
		if !seen[address] {
			servers = append(servers, address.String())
			seen[address] = true
		}
	}
	if len(servers) == 0 {
		return nil, errors.New("Укажите хотя бы один DNS IP-адрес")
	}
	return servers, nil
}
