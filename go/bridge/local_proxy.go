package bridge

import (
	"errors"
	"fmt"
)

// localProxy is an optional SOCKS5/HTTP proxy in front of the same
// connection as the tunnel. Other apps can find any open local port, so it
// is off by default and always requires a login and password.
type localProxy struct {
	SocksPort int    `json:"socksPort"` // 0 disables SOCKS5
	HTTPPort  int    `json:"httpPort"`  // 0 disables HTTP
	Username  string `json:"username"`
	Password  string `json:"password"`
	// LAN listens on all interfaces, so devices on the phone's hotspot or
	// Wi-Fi can use it; otherwise only 127.0.0.1.
	LAN bool `json:"lan"`
}

const (
	socksInboundTag = "socks-in"
	httpInboundTag  = "http-in"
)

func (proxy localProxy) enabled() bool { return proxy.SocksPort != 0 || proxy.HTTPPort != 0 }

func (proxy localProxy) validate() error {
	if !proxy.enabled() {
		return nil
	}
	for _, port := range []int{proxy.SocksPort, proxy.HTTPPort} {
		if port != 0 && (port < 1024 || port > 65535) {
			return errors.New("Порт прокси должен быть от 1024 до 65535")
		}
	}
	if proxy.SocksPort != 0 && proxy.SocksPort == proxy.HTTPPort {
		return errors.New("Порты SOCKS5 и HTTP должны различаться")
	}
	if len(proxy.Username) < 4 || len(proxy.Password) < 8 {
		return errors.New("Для прокси нужны логин от 4 и пароль от 8 символов")
	}
	return nil
}

// inbounds returns the proxy listeners with the same sniffing as the TUN.
func (proxy localProxy) inbounds(sniffing map[string]any) []any {
	listen := "127.0.0.1"
	if proxy.LAN {
		listen = "0.0.0.0"
	}
	account := map[string]any{"user": proxy.Username, "pass": proxy.Password}
	var result []any
	if proxy.SocksPort != 0 {
		result = append(result, map[string]any{
			"tag": socksInboundTag, "protocol": "socks", "listen": listen, "port": proxy.SocksPort,
			// UDP ASSOCIATE answers with the listen address, which is only
			// reachable for clients on the phone itself.
			"settings": map[string]any{"auth": "password", "accounts": []any{account}, "udp": !proxy.LAN},
			"sniffing": sniffing,
		})
	}
	if proxy.HTTPPort != 0 {
		result = append(result, map[string]any{
			"tag": httpInboundTag, "protocol": "http", "listen": listen, "port": proxy.HTTPPort,
			"settings": map[string]any{"accounts": []any{account}},
			"sniffing": sniffing,
		})
	}
	return result
}

func (proxy localProxy) tags() []string {
	var tags []string
	if proxy.SocksPort != 0 {
		tags = append(tags, socksInboundTag)
	}
	if proxy.HTTPPort != 0 {
		tags = append(tags, httpInboundTag)
	}
	return tags
}

// applyLocalProxyRules makes every rule written for the TUN cover the proxy
// listeners too, and keeps proxy clients (possibly other devices) away from
// services listening on the phone's own loopback.
func applyLocalProxyRules(rules []any, proxy localProxy) []any {
	tags := proxy.tags()
	if len(tags) == 0 {
		return rules
	}
	for _, raw := range rules {
		rule, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		inbound := ruleValues(rule["inboundTag"])
		if len(inbound) == 1 && inbound[0] == "tun-in" {
			rule["inboundTag"] = append([]string{"tun-in"}, tags...)
		}
	}
	loopback := map[string]any{"type": "field", "inboundTag": tags, "ip": []string{"127.0.0.0/8", "::1/128"}, "outboundTag": "block"}
	return append([]any{loopback}, rules...)
}

func (proxy localProxy) String() string {
	return fmt.Sprintf("socks=%d http=%d lan=%t", proxy.SocksPort, proxy.HTTPPort, proxy.LAN)
}
