package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type Profile struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Protocol  string         `json:"protocol"`
	Transport string         `json:"transport"`
	Auto      bool           `json:"auto,omitempty"`
	Address   string         `json:"-"`
	Port      int            `json:"-"`
	Outbound  map[string]any `json:"-"`
}

func newProfile(name string, out map[string]any) Profile {
	b, _ := json.Marshal(out)
	sum := sha256.Sum256(b)
	proto, _ := out["protocol"].(string)
	transport := "tcp"
	if stream, ok := out["streamSettings"].(map[string]any); ok {
		if v, ok := stream["network"].(string); ok {
			transport = v
		}
	}
	if name == "" {
		name = strings.ToUpper(proto)
	}
	address, port := endpointFromOutbound(out)
	return Profile{
		ID:        hex.EncodeToString(sum[:12]),
		Name:      name,
		Protocol:  proto,
		Transport: transport,
		Address:   address,
		Port:      port,
		Outbound:  out,
	}
}
func decodeBase64(s string) ([]byte, error) {
	s = strings.Join(strings.Fields(s), "")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, e := enc.DecodeString(s); e == nil {
			return b, nil
		}
	}
	return nil, errors.New("Некорректный Base64")
}

type subscriptionMetadata struct {
	Title      string `json:"title,omitempty"`
	SupportURL string `json:"supportUrl,omitempty"`
	DNSDoH     string `json:"dnsDoh,omitempty"`
}

func cleanHeaderText(value string, maxLength int) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	value = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, value)
	if len(value) > maxLength {
		value = value[:maxLength]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return strings.TrimSpace(value)
}

func decodeProfileTitle(value string) string {
	value = cleanHeaderText(value, 1024)
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return cleanHeaderText(value, 120)
	}
	prefix, payload := strings.ToLower(strings.TrimSpace(parts[0])), strings.TrimSpace(parts[1])
	switch prefix {
	case "base64":
		if decoded, err := decodeBase64(payload); err == nil && utf8.Valid(decoded) {
			return cleanHeaderText(string(decoded), 120)
		}
	case "rwencodebase64":
		if decoded, err := decodeBase64(payload); err == nil && utf8.Valid(decoded) {
			return cleanHeaderText(string(decoded), 120)
		}
		return cleanHeaderText(payload, 120)
	case "urlencoded", "urlencode":
		if decoded, err := url.QueryUnescape(payload); err == nil {
			return cleanHeaderText(decoded, 120)
		}
	}
	return cleanHeaderText(value, 120)
}

func normalizedHTTPSURL(value string, defaultPath string, allowQuery bool) string {
	value = strings.TrimSpace(strings.ToValidUTF8(value, ""))
	if len(value) > 2048 {
		return ""
	}
	value = cleanHeaderText(value, 2048)
	if value == "" {
		return ""
	}
	if !strings.Contains(value, "://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return ""
	}
	if defaultPath != "" && (parsed.Path == "" || parsed.Path == "/") {
		parsed.Path = defaultPath
	}
	if !allowQuery && parsed.RawQuery != "" {
		return ""
	}
	return parsed.String()
}

func subscriptionMetadataFromHeaders(header http.Header) subscriptionMetadata {
	return subscriptionMetadata{
		Title:      decodeProfileTitle(header.Get("profile-title")),
		SupportURL: normalizedHTTPSURL(header.Get("support-url"), "", true),
		DNSDoH:     normalizedHTTPSURL(header.Get("dns-doh"), "/dns-query", false),
	}
}

func fetchSubscription(ctx context.Context, address string) ([]Profile, int, subscriptionMetadata, error) {
	u, e := url.Parse(address)
	if e != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
		return nil, 0, subscriptionMetadata{}, errors.New("Нужна HTTPS-ссылка на подписку")
	}
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 || r.URL.Scheme != "https" || r.URL.User != nil {
			return errors.New("Недопустимое перенаправление")
		}
		return nil
	}}
	req, e := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if e != nil {
		return nil, 0, subscriptionMetadata{}, errors.New("Некорректная ссылка")
	}
	setClientIdentityHeaders(req, currentDeviceInfo())
	req.Header.Set("Accept", "application/json, text/plain;q=0.9")
	resp, e := client.Do(req)
	if e != nil {
		return nil, 0, subscriptionMetadata{}, errors.New("Не удалось загрузить подписку. Проверьте интернет и ссылку")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, subscriptionMetadata{}, fmt.Errorf("Сервер подписки вернул HTTP %d", resp.StatusCode)
	}
	metadata := subscriptionMetadataFromHeaders(resp.Header)
	b, e := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024+1))
	if e != nil {
		return nil, 0, metadata, errors.New("Ошибка чтения подписки")
	}
	if len(b) > 4*1024*1024 {
		return nil, 0, metadata, errors.New("Подписка превышает 4 МБ")
	}
	skipped := 0
	profiles, err := parseSubscriptionWithStats(b, &skipped)
	return profiles, skipped, metadata, err
}

type subscriptionSourceStatus struct {
	Index    int                  `json:"index"`
	Profiles int                  `json:"profiles"`
	Skipped  int                  `json:"skipped"`
	Metadata subscriptionMetadata `json:"metadata"`
	Error    string               `json:"error,omitempty"`
}

type subscriptionFetchResult struct {
	index    int
	profiles []Profile
	skipped  int
	metadata subscriptionMetadata
	err      error
}

func mergeSubscriptionProfiles(profileSets ...[]Profile) []Profile {
	seen := make(map[string]struct{})
	var merged []Profile
	for _, profiles := range profileSets {
		for _, profile := range profiles {
			if _, exists := seen[profile.ID]; exists {
				continue
			}
			seen[profile.ID] = struct{}{}
			merged = append(merged, profile)
		}
	}
	return merged
}

func fetchSubscriptions(ctx context.Context, addresses []string) ([]Profile, []subscriptionSourceStatus, int, error) {
	results := make([]subscriptionFetchResult, len(addresses))
	var wait sync.WaitGroup
	for index, address := range addresses {
		wait.Add(1)
		go func() {
			defer wait.Done()
			profiles, skipped, metadata, err := fetchSubscription(ctx, address)
			results[index] = subscriptionFetchResult{index: index, profiles: profiles, skipped: skipped, metadata: metadata, err: err}
		}()
	}
	wait.Wait()

	sets := make([][]Profile, 0, len(results))
	statuses := make([]subscriptionSourceStatus, 0, len(results))
	totalSkipped := 0
	successful := 0
	firstError := ""
	for _, result := range results {
		status := subscriptionSourceStatus{Index: result.index, Profiles: len(result.profiles), Skipped: result.skipped, Metadata: result.metadata}
		if result.err != nil {
			status.Error = result.err.Error()
			if firstError == "" {
				firstError = status.Error
			}
		} else {
			successful++
			sets = append(sets, result.profiles)
			totalSkipped += result.skipped
		}
		statuses = append(statuses, status)
	}
	if successful == 0 {
		if firstError == "" {
			firstError = "Список подписок пуст"
		}
		return nil, statuses, 0, errors.New(firstError)
	}
	return mergeSubscriptionProfiles(sets...), statuses, totalSkipped, nil
}
func parseSubscription(b []byte) ([]Profile, error) {
	return parseSubscriptionWithStats(b, nil)
}

func parseSubscriptionWithStats(b []byte, skippedUnsupported *int) ([]Profile, error) {
	s := strings.TrimSpace(strings.TrimPrefix(string(b), "\ufeff"))
	profiles := []Profile{}
	if strings.HasPrefix(s, "[") || strings.HasPrefix(s, "{") {
		var items []map[string]any
		if strings.HasPrefix(s, "[") {
			if json.Unmarshal([]byte(s), &items) != nil {
				return nil, errors.New("Некорректный JSON")
			}
		} else {
			var item map[string]any
			if json.Unmarshal([]byte(s), &item) != nil {
				return nil, errors.New("Некорректный JSON")
			}
			items = append(items, item)
		}
		for _, item := range items {
			name, _ := item["remarks"].(string)
			outs, _ := item["outbounds"].([]any)
			if _, ok := item["protocol"]; ok {
				outs = []any{item}
			}
			for _, raw := range outs {
				out, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				proto, _ := out["protocol"].(string)
				switch proto {
				case "vless", "vmess", "trojan", "shadowsocks", "hysteria":
				default:
					continue
				}
				// Import connection settings only. Never accept subscription-supplied listeners,
				// routing, local source addresses, proxy chaining, or socket/interface overrides.
				clean := map[string]any{"protocol": proto, "settings": out["settings"], "tag": "proxy"}
				if st, ok := out["streamSettings"].(map[string]any); ok {
					copy := map[string]any{}
					for k, v := range st {
						if k != "sockopt" && k != "finalmask" {
							copy[k] = v
						}
					}
					clean["streamSettings"] = copy
				}
				if name == "" {
					name, _ = out["tag"].(string)
				}
				profiles = append(profiles, newProfile(name, clean))
				break
			}
		}
	} else {
		if !strings.Contains(s, "://") {
			decoded, e := decodeBase64(s)
			if e != nil {
				return nil, errors.New("Формат подписки не распознан")
			}
			s = string(decoded)
		}
		for _, line := range strings.Fields(s) {
			if strings.HasPrefix(line, "#") {
				continue
			}
			if parsed, parseErr := url.Parse(line); parseErr == nil && parsed.Scheme != "vless" && parsed.Scheme != "trojan" && parsed.Scheme != "hysteria2" && parsed.Scheme != "hy2" {
				if skippedUnsupported != nil {
					(*skippedUnsupported)++
				}
				continue
			}
			p, e := parseURI(line)
			if e != nil {
				return nil, e
			}
			profiles = append(profiles, p)
		}
	}
	visible := profiles[:0]
	for _, profile := range profiles {
		ip := net.ParseIP(strings.TrimSpace(profile.Address))
		if ip != nil && ip.IsUnspecified() {
			continue
		}
		visible = append(visible, profile)
	}
	profiles = visible
	if len(profiles) == 0 {
		return nil, errors.New("Нет поддерживаемых серверов. Нужна Xray JSON или подписка VLESS/Trojan/Hysteria2")
	}
	if len(profiles) > 500 {
		return nil, errors.New("В подписке больше 500 серверов")
	}
	seen := map[string]bool{}
	unique := []Profile{}
	for _, p := range profiles {
		if !seen[p.ID] {
			unique = append(unique, p)
			seen[p.ID] = true
		}
	}
	return unique, nil
}
func parseURI(raw string) (Profile, error) {
	u, e := url.Parse(raw)
	if e != nil || u.User == nil || u.Hostname() == "" {
		return Profile{}, errors.New("Некорректная ссылка сервера")
	}
	if u.Scheme != "vless" && u.Scheme != "trojan" && u.Scheme != "hysteria2" && u.Scheme != "hy2" {
		return Profile{}, fmt.Errorf("URI-протокол %s пока не поддерживается; используйте Xray JSON", u.Scheme)
	}
	port, e := strconv.Atoi(u.Port())
	if e != nil || port < 1 || port > 65535 {
		return Profile{}, errors.New("Некорректный порт сервера")
	}
	q := u.Query()
	if u.Scheme == "hysteria2" || u.Scheme == "hy2" {
		if q.Get("obfs") != "" || q.Get("obfs-password") != "" {
			return Profile{}, errors.New("Hysteria2 obfs пока не поддерживается")
		}
		password := u.User.Username()
		if suffix, exists := u.User.Password(); exists {
			password += ":" + suffix
		}
		if password == "" {
			return Profile{}, errors.New("Hysteria2 требует пароль")
		}
		serverName := q.Get("sni")
		if serverName == "" {
			serverName = u.Hostname()
		}
		alpn := []string{"h3"}
		if q.Get("alpn") != "" {
			alpn = strings.Split(q.Get("alpn"), ",")
		}
		certificatePin := strings.TrimSpace(q.Get("pinSHA256"))
		if certificatePin == "" {
			certificatePin = strings.TrimSpace(q.Get("pcs"))
		}
		insecure := q.Get("insecure") == "1" || strings.EqualFold(q.Get("insecure"), "true")
		if insecure && certificatePin == "" {
			return Profile{}, errors.New("Hysteria2 insecure=1 больше не поддерживается Xray; добавьте pinSHA256 сертификата")
		}
		tlsSettings := map[string]any{"serverName": serverName, "alpn": alpn}
		if certificatePin != "" {
			tlsSettings["pinnedPeerCertSha256"] = certificatePin
		}
		if verifyName := strings.TrimSpace(q.Get("vcn")); verifyName != "" {
			tlsSettings["verifyPeerCertByName"] = verifyName
		}
		stream := map[string]any{
			"network":          "hysteria",
			"security":         "tls",
			"tlsSettings":      tlsSettings,
			"hysteriaSettings": map[string]any{"version": 2, "auth": password},
		}
		out := map[string]any{
			"tag":            "proxy",
			"protocol":       "hysteria",
			"settings":       map[string]any{"version": 2, "address": u.Hostname(), "port": port},
			"streamSettings": stream,
		}
		return newProfile(u.Fragment, out), nil
	}
	security := q.Get("security")
	if security == "" {
		if u.Scheme == "trojan" {
			security = "tls"
		} else {
			security = "none"
		}
	}
	stream := map[string]any{}
	network := q.Get("type")
	if network == "" {
		network = "tcp"
	}
	stream["network"] = network
	stream["security"] = security
	switch security {
	case "tls":
		tls := map[string]any{"serverName": q.Get("sni"), "fingerprint": q.Get("fp")}
		if q.Get("alpn") != "" {
			tls["alpn"] = strings.Split(q.Get("alpn"), ",")
		}
		stream["tlsSettings"] = tls
	case "reality":
		stream["realitySettings"] = map[string]any{"serverName": q.Get("sni"), "fingerprint": q.Get("fp"), "publicKey": q.Get("pbk"), "shortId": q.Get("sid"), "spiderX": q.Get("spx")}
	case "none":
	default:
		return Profile{}, errors.New("Неизвестный тип защиты соединения")
	}
	switch network {
	case "tcp", "raw":
		if h := q.Get("headerType"); h != "" && h != "none" {
			return Profile{}, errors.New("TCP headerType требует Xray JSON")
		}
	case "ws":
		stream["wsSettings"] = map[string]any{"path": q.Get("path"), "headers": map[string]string{"Host": q.Get("host")}}
	case "grpc":
		stream["grpcSettings"] = map[string]any{"serviceName": q.Get("serviceName"), "authority": q.Get("authority"), "multiMode": q.Get("mode") == "multi"}
	case "xhttp":
		x := map[string]any{"path": q.Get("path"), "host": q.Get("host")}
		if q.Get("mode") != "" {
			x["mode"] = q.Get("mode")
		}
		if q.Get("extra") != "" {
			var extra map[string]any
			if json.Unmarshal([]byte(q.Get("extra")), &extra) != nil {
				return Profile{}, errors.New("Некорректный XHTTP extra")
			}
			x["extra"] = extra
		}
		stream["xhttpSettings"] = x
	case "httpupgrade":
		stream["httpupgradeSettings"] = map[string]any{"path": q.Get("path"), "host": q.Get("host")}
	default:
		return Profile{}, fmt.Errorf("Транспорт %s требует Xray JSON", network)
	}
	out := map[string]any{"tag": "proxy", "protocol": u.Scheme, "streamSettings": stream}
	if u.Scheme == "vless" {
		encryption := q.Get("encryption")
		if encryption == "" {
			encryption = "none"
		}
		out["settings"] = map[string]any{"vnext": []any{map[string]any{"address": u.Hostname(), "port": port, "users": []any{map[string]any{"id": u.User.Username(), "encryption": encryption, "flow": q.Get("flow")}}}}}
	} else {
		out["settings"] = map[string]any{"servers": []any{map[string]any{"address": u.Hostname(), "port": port, "password": u.User.Username()}}}
	}
	return newProfile(u.Fragment, out), nil
}
func makeConfig(p Profile) ([]byte, error) {
	return makeConfigWithDNS(p, "cloudflare", nil)
}

func makeConfigWithDNS(p Profile, dnsID string, customServers []string) ([]byte, error) {
	return makeConfigWithOptions(p, dnsID, customServers, false, "auto")
}

func makeConfigWithOptions(p Profile, dnsID string, customServers []string, fragmentation bool, outboundInterface string) ([]byte, error) {
	return makeConfigWithRoutingOptions(p, dnsID, customServers, fragmentation, outboundInterface, routingOptions{})
}

type routingOptions struct {
	Mode          string
	DirectDomains []string
	IPRules       []string
	GeoIPURL      string
	GeoSiteURL    string
	AssetDir      string
	NeedsGeoIP    bool
	NeedsGeoSite  bool
}

func validGeoDataTag(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("-_.@", character) {
			continue
		}
		return false
	}
	return true
}

func newRoutingOptions(mode string, directDomains []string, geoIPURL, geoSiteURL string) (routingOptions, error) {
	if mode == "" {
		mode = "full"
	}
	if mode != "full" && mode != "bypass" && mode != "proxy_only" {
		return routingOptions{}, errors.New("Неизвестный режим маршрутизации")
	}
	if mode == "full" {
		// Keep the saved rules and sources in the UI, but do not activate them.
		return routingOptions{Mode: mode}, nil
	}
	normalized := make([]string, 0, len(directDomains))
	ipRules := make([]string, 0, len(directDomains))
	seen := make(map[string]struct{}, len(directDomains))
	needsGeoIP := false
	needsGeoSite := false
	for _, raw := range directDomains {
		for _, item := range strings.Fields(strings.ReplaceAll(raw, ",", "\n")) {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			if len(item) > 255 || strings.ContainsAny(item, "\\\"'") {
				return routingOptions{}, errors.New("Правило маршрутизации содержит недопустимый домен")
			}
			lowerItem := strings.ToLower(item)
			if strings.HasPrefix(lowerItem, "geoip:") {
				value := strings.TrimSpace(item[len("geoip:"):])
				if !validGeoDataTag(value) {
					return routingOptions{}, errors.New("Правило geoip: содержит недопустимое имя списка")
				}
				normalizedItem := "geoip:" + strings.ToLower(value)
				if _, exists := seen[normalizedItem]; !exists {
					seen[normalizedItem] = struct{}{}
					ipRules = append(ipRules, normalizedItem)
					needsGeoIP = true
				}
				if len(normalized)+len(ipRules) > 512 {
					return routingOptions{}, errors.New("Можно добавить не больше 512 правил маршрутизации")
				}
				continue
			}
			prefix := "domain:"
			value := item
			for _, candidate := range []string{"domain:", "full:", "keyword:", "regexp:", "geosite:"} {
				if strings.HasPrefix(strings.ToLower(value), candidate) {
					prefix = candidate
					value = value[len(candidate):]
					break
				}
			}
			value = strings.TrimSpace(value)
			if value == "" || strings.ContainsAny(value, " \t\r\n") {
				return routingOptions{}, errors.New("Правило маршрутизации содержит пустой или недопустимый домен")
			}
			if prefix == "geosite:" {
				if !validGeoDataTag(value) {
					return routingOptions{}, errors.New("Правило geosite: содержит недопустимое имя списка")
				}
				value = strings.ToLower(value)
				needsGeoSite = true
			} else if prefix == "domain:" {
				value = strings.TrimSuffix(strings.ToLower(value), ".")
				if strings.Contains(value, "/") || strings.Contains(value, ":") || !strings.Contains(value, ".") {
					return routingOptions{}, errors.New("Укажите домен вроде example.com или используйте full:/keyword:")
				}
			}
			normalizedItem := prefix + value
			if _, exists := seen[normalizedItem]; exists {
				continue
			}
			seen[normalizedItem] = struct{}{}
			normalized = append(normalized, normalizedItem)
			if len(normalized)+len(ipRules) > 512 {
				return routingOptions{}, errors.New("Можно добавить не больше 512 правил маршрутизации")
			}
		}
	}
	if len(normalized)+len(ipRules) == 0 {
		return routingOptions{}, errors.New("Для выбранного режима укажите хотя бы одно правило")
	}
	geoIPURL, err := normalizeGeoDataURL(geoIPURL, "GeoIP")
	if err != nil {
		return routingOptions{}, err
	}
	geoSiteURL, err = normalizeGeoDataURL(geoSiteURL, "GeoSite")
	if err != nil {
		return routingOptions{}, err
	}
	if needsGeoIP && geoIPURL == "" {
		return routingOptions{}, errors.New("Для правил geoip: укажите HTTPS-ссылку на geoip.dat")
	}
	if needsGeoSite && geoSiteURL == "" {
		return routingOptions{}, errors.New("Для правил geosite: укажите HTTPS-ссылку на geosite.dat")
	}
	return routingOptions{
		Mode:          mode,
		DirectDomains: normalized,
		IPRules:       ipRules,
		GeoIPURL:      geoIPURL,
		GeoSiteURL:    geoSiteURL,
		NeedsGeoIP:    needsGeoIP,
		NeedsGeoSite:  needsGeoSite,
	}, nil
}

func routingSelectionRules(routing routingOptions, targetKey, target string) []any {
	rules := make([]any, 0, 2)
	if len(routing.DirectDomains) > 0 {
		rules = append(rules, map[string]any{
			"type": "field", "inboundTag": []string{"tun-in"},
			"domain": routing.DirectDomains, targetKey: target,
		})
	}
	if len(routing.IPRules) > 0 {
		rules = append(rules, map[string]any{
			"type": "field", "inboundTag": []string{"tun-in"},
			"ip": routing.IPRules, targetKey: target,
		})
	}
	return rules
}

func attachGeoDataConfig(config map[string]any, routing routingOptions) {
	if routing.AssetDir == "" {
		return
	}
	assets := make([]any, 0, 2)
	if routing.NeedsGeoIP {
		assets = append(assets, map[string]any{"url": routing.GeoIPURL, "file": "geoip.dat"})
	}
	if routing.NeedsGeoSite {
		assets = append(assets, map[string]any{"url": routing.GeoSiteURL, "file": "geosite.dat"})
	}
	config["env"] = map[string]any{"XRAY_LOCATION_ASSET": routing.AssetDir}
	config["geodata"] = map[string]any{"cron": "0 4 * * *", "assets": assets}
}

func makeConfigWithRoutingOptions(p Profile, dnsID string, customServers []string, fragmentation bool, outboundInterface string, routing routingOptions) ([]byte, error) {
	return makeConfigWithRoutingAndDoH(p, dnsID, customServers, "", fragmentation, outboundInterface, routing)
}

func makeConfigWithRoutingAndDoH(p Profile, dnsID string, customServers []string, dohURL string, fragmentation bool, outboundInterface string, routing routingOptions) ([]byte, error) {
	_, dns, err := selectedDNSWithDoH(dnsID, customServers, dohURL)
	if err != nil {
		return nil, err
	}
	outbound, err := configuredOutbound(p, "proxy", fragmentation)
	if err != nil {
		return nil, err
	}
	return marshalRuntimeConfigWithRouting([]any{outbound}, dns, outboundInterface, routing)
}

func makeAutoConfigWithOptions(profiles []Profile, dnsID string, customServers []string, fragmentation bool, outboundInterface string) ([]byte, error) {
	return makeAutoConfigWithRoutingOptions(profiles, dnsID, customServers, fragmentation, outboundInterface, routingOptions{})
}

func makeAutoConfigWithRoutingOptions(profiles []Profile, dnsID string, customServers []string, fragmentation bool, outboundInterface string, routing routingOptions) ([]byte, error) {
	return makeAutoConfigWithRoutingAndDoH(profiles, dnsID, customServers, "", fragmentation, outboundInterface, routing)
}

func makeAutoConfigWithRoutingAndDoH(profiles []Profile, dnsID string, customServers []string, dohURL string, fragmentation bool, outboundInterface string, routing routingOptions) ([]byte, error) {
	if len(profiles) == 0 {
		return nil, errors.New("для Auto не переданы серверы")
	}
	_, dns, err := selectedDNSWithDoH(dnsID, customServers, dohURL)
	if err != nil {
		return nil, err
	}
	outbounds := make([]any, 0, len(profiles))
	for _, profile := range profiles {
		outbound, err := configuredOutbound(profile, "auto-"+profile.ID, fragmentation)
		if err != nil {
			return nil, err
		}
		outbounds = append(outbounds, outbound)
	}
	config := runtimeConfigWithRouting(outbounds, dns, outboundInterface, routing)
	fallbackTag := "auto-" + profiles[0].ID
	config["observatory"] = map[string]any{
		"subjectSelector":   []string{"auto-"},
		"probeUrl":          "https://www.gstatic.com/generate_204",
		"probeInterval":     autoProbeInterval,
		"enableConcurrency": true,
	}
	rules := dnsRoutingRules(dns)
	if routing.Mode == "bypass" {
		rules = append(rules, routingSelectionRules(routing, "outboundTag", "direct")...)
	} else if routing.Mode == "proxy_only" {
		rules = append(rules, routingSelectionRules(routing, "balancerTag", "shadow-auto")...)
		rules = append(rules, map[string]any{"type": "field", "inboundTag": []string{"tun-in"}, "outboundTag": "direct"})
	}
	if routing.Mode != "proxy_only" {
		rules = append(rules, map[string]any{"type": "field", "inboundTag": []string{"tun-in"}, "balancerTag": "shadow-auto"})
	}
	config["routing"] = map[string]any{
		"domainStrategy": "IPIfNonMatch",
		"rules":          rules,
		"balancers": []any{map[string]any{
			"tag":         "shadow-auto",
			"selector":    []string{"auto-"},
			"fallbackTag": fallbackTag,
			"strategy":    map[string]any{"type": "leastping"},
		}},
	}
	attachGeoDataConfig(config, routing)
	return json.Marshal(config)
}

func configuredOutbound(p Profile, tag string, fragmentation bool) (map[string]any, error) {
	outbound, err := cloneObject(p.Outbound)
	if err != nil {
		return nil, err
	}
	outbound["tag"] = tag
	if fragmentation && p.Protocol != "hysteria" {
		stream, _ := outbound["streamSettings"].(map[string]any)
		if stream == nil {
			stream = map[string]any{}
			outbound["streamSettings"] = stream
		}
		stream["finalmask"] = map[string]any{
			"tcp": []any{map[string]any{
				"type": "fragment",
				"settings": map[string]any{
					"packets":  "tlshello",
					"length":   "10-35",
					"delay":    "0-1",
					"maxSplit": "3-5",
				},
			}},
		}
	}
	return outbound, nil
}

func runtimeConfig(outbounds []any, dns dnsPreset, outboundInterface string) map[string]any {
	return runtimeConfigWithRouting(outbounds, dns, outboundInterface, routingOptions{})
}

func runtimeConfigWithRouting(outbounds []any, dns dnsPreset, outboundInterface string, routing routingOptions) map[string]any {
	if outboundInterface == "" {
		outboundInterface = "auto"
	}
	// Keep a direct outbound available for domain split-routing. It is harmless
	// when the default full-VPN mode is selected because no rule points to it.
	runtimeOutbounds := make([]any, 0, len(outbounds)+2)
	runtimeOutbounds = append(runtimeOutbounds, outbounds...)
	runtimeOutbounds = append(runtimeOutbounds, map[string]any{"tag": "direct", "protocol": "freedom", "settings": map[string]any{}})
	if dns.DoHURL != "" {
		runtimeOutbounds = append(runtimeOutbounds, map[string]any{"tag": "dns-out", "protocol": "dns", "settings": map[string]any{}})
	}
	config := map[string]any{
		"log":       map[string]any{"loglevel": "debug"},
		"inbounds":  []any{map[string]any{"tag": "tun-in", "protocol": "tun", "sniffing": map[string]any{"enabled": true, "destOverride": []string{"http", "tls", "quic"}}, "settings": map[string]any{"name": "ShadowVPN", "desc": "ShadowVPN", "mtu": 1500, "gateway": []string{"172.31.255.1/30", "fd31:ffff::1/126"}, "dns": dns.Servers, "autoSystemRoutingTable": []string{"0.0.0.0/0", "::/0"}, "autoOutboundsInterface": outboundInterface}}},
		"outbounds": runtimeOutbounds,
		"policy": map[string]any{"system": map[string]any{
			"statsInboundUplink":   true,
			"statsInboundDownlink": true,
		}},
		"stats": map[string]any{},
	}
	if dns.DoHURL != "" {
		config["dns"] = map[string]any{"servers": []string{dns.DoHURL}, "queryStrategy": "UseIP"}
	}
	rules := dnsRoutingRules(dns)
	if routing.Mode != "full" && (len(routing.DirectDomains) > 0 || len(routing.IPRules) > 0) {
		if routing.Mode == "proxy_only" {
			rules = append(rules, routingSelectionRules(routing, "outboundTag", "proxy")...)
			rules = append(rules, map[string]any{"type": "field", "inboundTag": []string{"tun-in"}, "outboundTag": "direct"})
		} else {
			rules = append(rules, routingSelectionRules(routing, "outboundTag", "direct")...)
		}
	}
	if len(rules) > 0 {
		config["routing"] = map[string]any{"domainStrategy": "IPIfNonMatch", "rules": rules}
	}
	attachGeoDataConfig(config, routing)
	return config
}

func dnsRoutingRules(dns dnsPreset) []any {
	if dns.DoHURL == "" {
		return []any{}
	}
	return []any{map[string]any{
		"type":        "field",
		"inboundTag":  []string{"tun-in"},
		"network":     "tcp,udp",
		"port":        "53",
		"outboundTag": "dns-out",
	}}
}

func marshalRuntimeConfig(outbounds []any, dns dnsPreset, outboundInterface string) ([]byte, error) {
	return marshalRuntimeConfigWithRouting(outbounds, dns, outboundInterface, routingOptions{})
}

func marshalRuntimeConfigWithRouting(outbounds []any, dns dnsPreset, outboundInterface string, routing routingOptions) ([]byte, error) {
	return json.Marshal(runtimeConfigWithRouting(outbounds, dns, outboundInterface, routing))
}

func cloneObject(value map[string]any) (map[string]any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	return result, nil
}
