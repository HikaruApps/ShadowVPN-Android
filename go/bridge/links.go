package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// parseShareLink parses one subscription line. vmess:// and ss:// are handled
// here; every other scheme goes through the desktop parser.
func parseShareLink(line string) (Profile, error) {
	scheme, _, found := strings.Cut(line, "://")
	if !found {
		return Profile{}, errors.New("Некорректная ссылка сервера")
	}
	switch strings.ToLower(scheme) {
	case "vmess":
		return parseVMessLink(line)
	case "ss":
		return parseShadowsocksLink(line)
	}
	if !supportedURIScheme(scheme) {
		return Profile{}, fmt.Errorf("URI-протокол %s не поддерживается", scheme)
	}
	return parseURI(line)
}

// parseVMessLink reads the v2rayN format: vmess://base64(JSON).
func parseVMessLink(line string) (Profile, error) {
	decoded, err := decodeBase64(line[len("vmess://"):])
	if err != nil {
		return Profile{}, errors.New("Некорректная ссылка VMess")
	}
	var link map[string]any
	if json.Unmarshal(decoded, &link) != nil {
		return Profile{}, errors.New("Некорректная ссылка VMess")
	}
	text := func(key string) string {
		switch value := link[key].(type) {
		case string:
			return strings.TrimSpace(value)
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
		return ""
	}
	address, id := text("add"), text("id")
	port, err := strconv.Atoi(text("port"))
	if address == "" || id == "" || err != nil || port < 1 || port > 65535 {
		return Profile{}, errors.New("Ссылка VMess не содержит адрес, порт или ID")
	}
	network := strings.ToLower(firstNonEmpty(text("net"), "tcp"))
	security := "none"
	if strings.EqualFold(text("tls"), "tls") {
		security = "tls"
	}
	query := url.Values{}
	query.Set("type", network)
	query.Set("security", security)
	query.Set("sni", firstNonEmpty(text("sni"), text("host")))
	query.Set("fp", text("fp"))
	query.Set("alpn", text("alpn"))
	query.Set("host", text("host"))
	query.Set("path", text("path"))
	switch network {
	case "grpc":
		query.Set("serviceName", text("path"))
		query.Set("mode", text("type"))
		query.Set("authority", text("host"))
	case "xhttp":
		query.Set("mode", text("type"))
	case "tcp", "raw":
		query.Set("headerType", text("type"))
	}
	if security == "none" {
		query.Del("sni")
	}
	stream, err := streamFromQuery(query)
	if err != nil {
		return Profile{}, err
	}
	out := map[string]any{
		"tag": "proxy", "protocol": "vmess", "streamSettings": stream,
		"settings": map[string]any{"vnext": []any{map[string]any{
			"address": address, "port": port,
			"users": []any{map[string]any{"id": id, "security": firstNonEmpty(text("scy"), "auto")}},
		}}},
	}
	return newProfile(text("ps"), out), nil
}

// parseShadowsocksLink reads SIP002 links (ss://userinfo@host:port#name, with
// base64 or plain method:password) and the legacy fully base64-encoded form.
func parseShadowsocksLink(line string) (Profile, error) {
	body, fragment, _ := strings.Cut(line[len("ss://"):], "#")
	name, _ := url.PathUnescape(fragment)
	body, rawQuery, _ := strings.Cut(body, "?")
	if query, _ := url.ParseQuery(rawQuery); query.Get("plugin") != "" {
		return Profile{}, errors.New("Плагины Shadowsocks не поддерживаются")
	}
	if !strings.Contains(body, "@") {
		decoded, err := decodeBase64(body)
		if err != nil {
			return Profile{}, errors.New("Некорректная ссылка Shadowsocks")
		}
		body = string(decoded)
	}
	at := strings.LastIndex(body, "@")
	if at < 0 {
		return Profile{}, errors.New("Некорректная ссылка Shadowsocks")
	}
	userInfo, hostPort := body[:at], body[at+1:]
	if unescaped, err := url.PathUnescape(userInfo); err == nil {
		userInfo = unescaped
	}
	if !strings.Contains(userInfo, ":") {
		decoded, err := decodeBase64(userInfo)
		if err != nil {
			return Profile{}, errors.New("Некорректная ссылка Shadowsocks")
		}
		userInfo = string(decoded)
	}
	method, password, found := strings.Cut(userInfo, ":")
	host, portText, err := net.SplitHostPort(strings.TrimSuffix(hostPort, "/"))
	port, portErr := strconv.Atoi(portText)
	if !found || method == "" || password == "" || err != nil || host == "" || portErr != nil || port < 1 || port > 65535 {
		return Profile{}, errors.New("Ссылка Shadowsocks не содержит метод, пароль или адрес")
	}
	out := map[string]any{
		"tag": "proxy", "protocol": "shadowsocks",
		"settings": map[string]any{"servers": []any{map[string]any{
			"address": host, "port": port, "method": strings.ToLower(method), "password": password,
		}}},
	}
	return newProfile(name, out), nil
}
