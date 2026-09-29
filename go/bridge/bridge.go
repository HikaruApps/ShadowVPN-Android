// Package bridge is built together with generated copies of the desktop core's
// subscription, DNS, bootstrap and routing sources. See build-bridge.sh.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/xtls/xray-core/core"
	_ "github.com/xtls/xray-core/main/distro/all"
)

var mobile struct {
	sync.Mutex
	profiles  []Profile
	prepared  []byte
	instance  *core.Instance
	seed      string
	importing bool
	options   mobileOptions
}

type mobileOptions struct {
	DNSID          string   `json:"dnsId"`
	CustomDNS      []string `json:"customDns"`
	DoHURL         string   `json:"dohUrl"`
	Fragmentation  bool     `json:"fragmentation"`
	TunMTU         int      `json:"tunMtu"`
	RoutingMode    string   `json:"routingMode"`
	RoutingRules   []string `json:"routingRules"`
	PingMethod     string   `json:"pingMethod"`
	AutoProfileIDs []string `json:"autoProfileIds"`
}

// SetDeviceSeed must be called with Settings.Secure.ANDROID_ID before importing.
// The raw identifier stays in the app; only the namespaced hash is sent.
func SetDeviceSeed(seed string) {
	mobile.Lock()
	defer mobile.Unlock()
	mobile.seed = seed
}

func deviceHWID() string {
	mobile.Lock()
	defer mobile.Unlock()
	return hashedHWID("android:" + mobile.seed)
}

// DeviceHWID returns the same privacy-preserving hash sent to subscription servers.
func DeviceHWID() string { return deviceHWID() }

type importResult struct {
	Profiles []Profile                  `json:"profiles"`
	Skipped  int                        `json:"skipped"`
	Title    string                     `json:"title"`
	DNSDoH   string                     `json:"dnsDoh"`
	Sources  []subscriptionSourceStatus `json:"sources,omitempty"`
}

// Configure validates and stores options used by the next Prepare call.
func Configure(raw string) error {
	var options mobileOptions
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &options); err != nil {
			return errors.New("Некорректные настройки VPN")
		}
	}
	if options.DNSID == "" {
		options.DNSID = "cloudflare"
	}
	if options.PingMethod == "" {
		options.PingMethod = "tcp"
	}
	if options.TunMTU == 0 {
		options.TunMTU = 1400
	}
	if options.TunMTU < 1280 || options.TunMTU > 1500 {
		return errors.New("MTU туннеля должен быть от 1280 до 1500")
	}
	if options.PingMethod != "tcp" && options.PingMethod != "head" && options.PingMethod != "get" {
		return errors.New("Неизвестный метод проверки задержки")
	}
	if _, _, err := selectedDNSWithDoH(options.DNSID, options.CustomDNS, options.DoHURL); err != nil {
		return err
	}
	if _, err := newRoutingOptions(options.RoutingMode, options.RoutingRules, "", ""); err != nil {
		return err
	}
	mobile.Lock()
	defer mobile.Unlock()
	if mobile.instance != nil {
		return errors.New("Сначала отключите VPN")
	}
	mobile.options = options
	return nil
}

// PingProfiles checks all imported servers and returns display-safe JSON.
func PingProfiles(method string) (string, error) {
	mobile.Lock()
	if mobile.instance != nil || mobile.importing {
		mobile.Unlock()
		return "", errors.New("Проверка недоступна во время подключения или обновления")
	}
	profiles := append([]Profile(nil), mobile.profiles...)
	mobile.Unlock()
	if len(profiles) == 0 {
		return "[]", nil
	}
	if method == "" {
		method = "tcp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	result, err := json.Marshal(pingProfilesWithAutoMethod(ctx, profiles, method))
	return string(result), err
}

// ImportSubscription downloads an HTTPS subscription using the desktop parser.
// It returns only display metadata, never credentials or raw outbounds.
func ImportSubscription(address string) (string, error) {
	mobile.Lock()
	if mobile.instance != nil || mobile.prepared != nil || mobile.importing {
		mobile.Unlock()
		return "", errors.New("Отключите VPN и дождитесь завершения текущей загрузки")
	}
	mobile.importing = true
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.importing = false; mobile.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profiles, skipped, meta, err := fetchSubscription(ctx, address)
	if err != nil {
		return "", err
	}
	mobile.Lock()
	mobile.profiles = profiles
	mobile.prepared = nil
	mobile.Unlock()
	result, err := json.Marshal(importResult{Profiles: profilesForRenderer(profiles), Skipped: skipped, Title: meta.Title, DNSDoH: meta.DNSDoH})
	return string(result), err
}

// ImportSubscriptions downloads and merges several subscription sources.
func ImportSubscriptions(addressesJSON string) (string, error) {
	var addresses []string
	if err := json.Unmarshal([]byte(addressesJSON), &addresses); err != nil || len(addresses) == 0 {
		return "", errors.New("Список подписок пуст")
	}
	if len(addresses) > 16 {
		return "", errors.New("Можно добавить не больше 16 подписок")
	}
	mobile.Lock()
	if mobile.instance != nil || mobile.prepared != nil || mobile.importing {
		mobile.Unlock()
		return "", errors.New("Отключите VPN перед обновлением подписок")
	}
	mobile.importing = true
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.importing = false; mobile.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	profiles, sources, skipped, err := fetchSubscriptions(ctx, addresses)
	if err != nil {
		return "", err
	}
	mobile.Lock()
	mobile.profiles = profiles
	mobile.prepared = nil
	mobile.Unlock()
	result, err := json.Marshal(importResult{Profiles: profilesForRenderer(profiles), Skipped: skipped, Sources: sources})
	return string(result), err
}

// Prepare resolves the server and builds a config BEFORE VpnService establishes
// the default route. This avoids a DNS bootstrap loop.
func Prepare(profileID string, dohURL string) error {
	mobile.Lock()
	defer mobile.Unlock()
	if mobile.instance != nil || mobile.importing {
		return errors.New("VPN занят обновлением или уже подключён")
	}
	mobile.prepared = nil
	options := mobile.options
	dnsID := options.DNSID
	if dnsID == "" {
		dnsID = "cloudflare"
	}
	if dohURL != "" {
		options.DoHURL = dohURL
		dnsID = "subscription-doh"
	}
	routing, err := newRoutingOptions(options.RoutingMode, options.RoutingRules, "", "")
	if err != nil {
		return err
	}

	if isAutoProfileID(profileID) {
		candidates := mobile.profiles
		if len(options.AutoProfileIDs) > 0 {
			allowed := make(map[string]struct{}, len(options.AutoProfileIDs))
			for _, id := range options.AutoProfileIDs {
				allowed[id] = struct{}{}
			}
			filtered := make([]Profile, 0, len(candidates))
			for _, profile := range candidates {
				if _, ok := allowed[profile.ID]; ok {
					filtered = append(filtered, profile)
				}
			}
			candidates = filtered
		}
		if profileID == autoNoRUProfileID {
			candidates = withoutRussianProfiles(candidates)
		}
		if len(candidates) == 0 {
			return errors.New("Для Auto нет подходящих серверов")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		results := pingProfiles(ctx, candidates)
		prepared, _, _, err := prepareAutoProfiles(ctx, candidates, results)
		if err != nil {
			return err
		}
		config, err := makeAutoConfigWithRoutingAndDoH(prepared, dnsID, options.CustomDNS, options.DoHURL, options.Fragmentation, "", routing)
		if err != nil {
			return err
		}
		mobile.prepared, err = androidRuntimeConfig(config, options.TunMTU)
		return err
	}

	var chosen *Profile
	for i := range mobile.profiles {
		if mobile.profiles[i].ID == profileID {
			chosen = &mobile.profiles[i]
			break
		}
	}
	if chosen == nil {
		return errors.New("Сервер отсутствует в подписке. Обновите её")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resolved, _, err := resolveProfileEndpoint(ctx, *chosen)
	if err != nil {
		return errors.New("Не удалось определить IP сервера")
	}
	config, err := makeConfigWithRoutingAndDoH(resolved, dnsID, options.CustomDNS, options.DoHURL, options.Fragmentation, "", routing)
	if err != nil {
		return err
	}
	mobile.prepared, err = androidRuntimeConfig(config, options.TunMTU)
	return err
}

func androidRuntimeConfig(config []byte, mtu int) ([]byte, error) {
	var document map[string]any
	if err := json.Unmarshal(config, &document); err != nil {
		return nil, err
	}
	inbounds, ok := document["inbounds"].([]any)
	if !ok || len(inbounds) == 0 {
		return nil, errors.New("Конфигурация Xray не содержит TUN")
	}
	inbound, ok := inbounds[0].(map[string]any)
	if !ok {
		return nil, errors.New("Некорректная конфигурация TUN")
	}
	settings, ok := inbound["settings"].(map[string]any)
	if !ok {
		return nil, errors.New("Конфигурация TUN не содержит настроек")
	}
	// Android owns routes, addresses and DNS. Xray only consumes the fd.
	delete(settings, "autoSystemRoutingTable")
	delete(settings, "autoOutboundsInterface")
	delete(settings, "gateway")
	delete(settings, "dns")
	settings["mtu"] = mtu
	return json.Marshal(document)
}

// Start runs the pinned Xray core against the fd owned by Android VpnService.
// The Android app excludes its own UID from the VPN before calling this method,
// so outbound Xray sockets and bootstrap DNS cannot loop into the tunnel.
func Start(tunFD int) error {
	mobile.Lock()
	defer mobile.Unlock()
	if tunFD < 0 || mobile.prepared == nil || mobile.instance != nil {
		return errors.New("VPN не подготовлен")
	}
	if err := os.Setenv("XRAY_TUN_FD", strconv.Itoa(tunFD)); err != nil {
		return err
	}
	defer os.Unsetenv("XRAY_TUN_FD")
	config, err := core.LoadConfig("json", bytes.NewReader(mobile.prepared))
	if err != nil {
		return err
	}
	instance, err := core.New(config)
	if err != nil {
		return err
	}
	if err = instance.Start(); err != nil {
		_ = instance.Close()
		return err
	}
	mobile.instance = instance
	return nil
}

func Stop() error {
	mobile.Lock()
	defer mobile.Unlock()
	mobile.prepared = nil
	if mobile.instance == nil {
		return nil
	}
	instance := mobile.instance
	mobile.instance = nil
	return instance.Close()
}
