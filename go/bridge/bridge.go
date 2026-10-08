// Package bridge is built together with generated copies of the desktop core's
// subscription, DNS, bootstrap and routing sources. See build-bridge.sh.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	xraynet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/core"
	_ "github.com/xtls/xray-core/main/distro/all"
)

// manualSourceIndex marks servers the user pasted by hand rather than
// imported from a subscription URL.
const manualSourceIndex = 100

const (
	defaultGeoIPURL = "https://raw.githubusercontent.com/runetfreedom/russia-v2ray-rules-dat/release/geoip.dat"
	// v2fly's list (~2 MB) has every common category; runetfreedom's geosite
	// is over 70 MB, too heavy for low-end phones.
	defaultGeoSiteURL = "https://github.com/v2fly/domain-list-community/releases/latest/download/dlc.dat"
)

var mobile struct {
	sync.Mutex
	profiles  []Profile
	prepared  []byte
	instance  *core.Instance
	naive     *naiveRuntime
	seed      string
	importing bool
	options   mobileOptions
	dataDir   string
}

type mobileOptions struct {
	DNSID             string   `json:"dnsId"`
	CustomDNS         []string `json:"customDns"`
	DoHURL            string   `json:"dohUrl"`
	Fragmentation     bool     `json:"fragmentation"`
	TunMTU            int      `json:"tunMtu"`
	IPv6              bool     `json:"ipv6"`
	RoutingMode       string   `json:"routingMode"`
	RoutingRules      []string `json:"routingRules"`
	SubscriptionRules bool     `json:"subscriptionRules"`
	GeoIPURL          string   `json:"geoipUrl"`
	GeoSiteURL        string   `json:"geositeUrl"`
	PingMethod        string   `json:"pingMethod"`
	AutoProfileIDs    []string `json:"autoProfileIds"`
	// PreferredSource restricts Auto to servers of one subscription (the one
	// for the current network type). -1 disables the restriction.
	PreferredSource int `json:"preferredSource"`
}

func (options mobileOptions) geoURLs() (string, string) {
	return firstNonEmpty(options.GeoIPURL, defaultGeoIPURL), firstNonEmpty(options.GeoSiteURL, defaultGeoSiteURL)
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

// SetDataDir sets the app-private directory for the profile cache, GeoData
// and the Xray log. It must be called before any other method.
func SetDataDir(directory string) error {
	if directory == "" {
		return errors.New("Каталог данных не задан")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("Не удалось создать каталог данных")
	}
	mobile.Lock()
	defer mobile.Unlock()
	mobile.dataDir = directory
	return nil
}

type importResult struct {
	Profiles []Profile                  `json:"profiles"`
	Skipped  int                        `json:"skipped"`
	Title    string                     `json:"title"`
	DNSDoH   string                     `json:"dnsDoh"`
	Sources  []subscriptionSourceStatus `json:"sources,omitempty"`
}

// LoadCachedProfiles restores servers saved by the last successful import, so
// the app and always-on VPN work before (or without) a network refresh.
// It returns an empty string when there is no cache.
func LoadCachedProfiles() (string, error) {
	mobile.Lock()
	directory := mobile.dataDir
	mobile.Unlock()
	profiles, err := loadProfileCache(directory)
	if err != nil || len(profiles) == 0 {
		return "", err
	}
	mobile.Lock()
	if len(mobile.profiles) == 0 {
		mobile.profiles = profiles
	}
	mobile.Unlock()
	result, err := json.Marshal(importResult{Profiles: profilesForRenderer(profiles)})
	return string(result), err
}

// Configure validates and stores options used by the next Prepare call. It
// may be called while connected; the running tunnel keeps its own config.
func Configure(raw string) error {
	options := mobileOptions{PreferredSource: -1}
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
	var err error
	if options.GeoIPURL, err = normalizeGeoDataURL(options.GeoIPURL, "GeoIP"); err != nil {
		return err
	}
	if options.GeoSiteURL, err = normalizeGeoDataURL(options.GeoSiteURL, "GeoSite"); err != nil {
		return err
	}
	geoIP, geoSite := options.geoURLs()
	if _, err := newRoutingOptions(options.RoutingMode, options.RoutingRules, geoIP, geoSite); err != nil {
		return err
	}
	mobile.Lock()
	defer mobile.Unlock()
	mobile.options = options
	return nil
}

// PingProfiles checks all imported servers and returns display-safe JSON.
// The app's own sockets bypass the tunnel, so this also works while connected.
func PingProfiles(method string) (string, error) {
	mobile.Lock()
	if mobile.importing {
		mobile.Unlock()
		return "", errors.New("Проверка недоступна во время обновления подписки")
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

// ImportSubscription downloads a single HTTPS subscription.
func ImportSubscription(address string) (string, error) {
	encoded, _ := json.Marshal([]string{address})
	return ImportSubscriptions(string(encoded))
}

// ImportSubscriptions downloads and merges several subscription sources.
func ImportSubscriptions(addressesJSON string) (string, error) {
	var addresses []string
	if err := json.Unmarshal([]byte(addressesJSON), &addresses); err != nil {
		return "", errors.New("Список подписок пуст")
	}
	encoded, _ := json.Marshal(map[string]any{"urls": addresses})
	return ImportSources(string(encoded))
}

type importSources struct {
	URLs   []string `json:"urls"`
	Manual string   `json:"manual"`
}

// ImportSources downloads subscription URLs and parses manually added
// configs (Xray JSON or share links), merging them into one server list.
// Subscriptions are fetched through the tunnel when it is up.
func ImportSources(sourcesJSON string) (string, error) {
	var sources importSources
	if err := json.Unmarshal([]byte(sourcesJSON), &sources); err != nil {
		return "", errors.New("Некорректный список источников")
	}
	sources.Manual = strings.TrimSpace(sources.Manual)
	if len(sources.URLs) == 0 && sources.Manual == "" {
		return "", errors.New("Список подписок пуст")
	}
	if len(sources.URLs) > 16 {
		return "", errors.New("Можно добавить не больше 16 подписок")
	}
	if len(sources.Manual) > 4*1024*1024 {
		return "", errors.New("Свои конфигурации превышают 4 МБ")
	}
	mobile.Lock()
	if mobile.importing {
		mobile.Unlock()
		return "", errors.New("Подписка уже обновляется")
	}
	mobile.importing = true
	directory := mobile.dataDir
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.importing = false; mobile.Unlock() }()

	var sets [][]Profile
	var statuses []subscriptionSourceStatus
	skipped := 0
	var firstErr error
	if len(sources.URLs) > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		profiles, urlStatuses, urlSkipped, err := fetchSubscriptions(ctx, sources.URLs)
		cancel()
		statuses = append(statuses, urlStatuses...)
		if err != nil {
			firstErr = err
		} else {
			sets = append(sets, profiles)
			skipped += urlSkipped
		}
	}
	if sources.Manual != "" {
		manualSkipped := 0
		profiles, err := parseSubscriptionWithStats([]byte(sources.Manual), &manualSkipped)
		status := subscriptionSourceStatus{Index: manualSourceIndex, Profiles: len(profiles), Skipped: manualSkipped,
			Metadata: subscriptionMetadata{Title: "Свои конфигурации"}}
		if err != nil {
			status.Error = err.Error()
			if firstErr == nil {
				firstErr = err
			}
		} else {
			for index := range profiles {
				profiles[index].SourceIndex = manualSourceIndex
			}
			sets = append(sets, profiles)
			skipped += manualSkipped
		}
		statuses = append(statuses, status)
	}
	if len(sets) == 0 {
		return "", firstErr
	}
	profiles := mergeSubscriptionProfiles(sets...)
	mobile.Lock()
	mobile.profiles = profiles
	mobile.Unlock()
	_ = saveProfileCache(directory, profiles)
	result, err := json.Marshal(importResult{Profiles: profilesForRenderer(profiles), Skipped: skipped, Sources: statuses})
	return string(result), err
}

// subscriptionTransport sends subscription requests through the running
// tunnel first (the app's own sockets bypass the VPN), falling back to the
// physical network.
func subscriptionTransport() http.RoundTripper {
	mobile.Lock()
	instance := mobile.instance
	mobile.Unlock()
	if instance == nil {
		return nil
	}
	direct := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if conn, err := dialThroughInstance(ctx, instance, address); err == nil {
				return conn, nil
			}
			return direct.DialContext(ctx, network, address)
		},
		TLSHandshakeTimeout: 10 * time.Second,
	}
}

func dialThroughInstance(ctx context.Context, instance *core.Instance, address string) (net.Conn, error) {
	destination, err := xraynet.ParseDestination("tcp:" + address)
	if err != nil {
		return nil, err
	}
	return core.Dial(ctx, instance, destination)
}

// Prepare resolves the server and builds a config BEFORE VpnService establishes
// the default route. This avoids a DNS bootstrap loop.
func Prepare(profileID string) error {
	mobile.Lock()
	defer mobile.Unlock()
	if mobile.instance != nil {
		return errors.New("VPN уже подключён")
	}
	if mobile.naive != nil {
		_ = mobile.naive.Close()
		mobile.naive = nil
	}
	mobile.prepared = nil
	options := mobile.options
	dnsID := options.DNSID
	if dnsID == "" {
		dnsID = "cloudflare"
	}
	_, dns, err := selectedDNSWithDoH(dnsID, options.CustomDNS, options.DoHURL)
	if err != nil {
		return err
	}
	geoIP, geoSite := options.geoURLs()
	routing, err := newRoutingOptions(options.RoutingMode, options.RoutingRules, geoIP, geoSite)
	if err != nil {
		return err
	}
	runtime := androidRuntimeOptions{
		MTU:               options.TunMTU,
		IPv6:              options.IPv6,
		DNS:               dns,
		RoutingMode:       routing.Mode,
		SubscriptionRules: options.SubscriptionRules,
		LogPath:           xrayLogPath(mobile.dataDir),
		GeoData: func(needIP, needSite bool) (geoDataInfo, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			return ensureGeoData(ctx, mobile.dataDir, needIP, needSite, geoIP, geoSite)
		},
	}

	if isAutoProfileID(profileID) {
		candidates := autoCandidates(mobile.profiles, options, profileID)
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
		runtime.Profiles = prepared
		runtime.Auto = true
		mobile.prepared, err = androidRuntimeConfig(config, runtime)
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
	var naive *naiveRuntime
	if resolved.Protocol == "naive" {
		naive, err = newNaiveRuntime(resolved)
		if err != nil {
			return err
		}
	}
	// A profile that already chains through a fragmenting dialer must not be
	// fragmented twice.
	fragmentation := options.Fragmentation && len(resolved.chain) == 0
	config, err := makeConfigWithRoutingAndDoH(resolved, dnsID, options.CustomDNS, options.DoHURL, fragmentation, "", routing)
	if err == nil {
		runtime.Profiles = []Profile{resolved}
		mobile.prepared, err = androidRuntimeConfig(config, runtime)
	}
	if err != nil {
		if naive != nil {
			_ = naive.Close()
		}
		return err
	}
	mobile.naive = naive
	return nil
}

// autoCandidates applies the user's Auto selection and the current network's
// subscription. Stale selections (servers that left the subscription) and an
// empty network-specific list fall back to the wider set instead of failing.
func autoCandidates(profiles []Profile, options mobileOptions, profileID string) []Profile {
	candidates := profilesWithoutProtocol(profiles, "naive")
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
		if len(filtered) > 0 {
			candidates = filtered
		}
	}
	if options.PreferredSource >= 0 {
		filtered := make([]Profile, 0, len(candidates))
		for _, profile := range candidates {
			if profile.SourceIndex == options.PreferredSource {
				filtered = append(filtered, profile)
			}
		}
		if len(filtered) > 0 {
			candidates = filtered
		}
	}
	if profileID == autoNoRUProfileID {
		candidates = withoutRussianProfiles(candidates)
	}
	return candidates
}

func xrayLogPath(directory string) string {
	if directory == "" {
		return ""
	}
	return filepath.Join(directory, "xray.log")
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
	if logPath := xrayLogPath(mobile.dataDir); logPath != "" {
		_ = os.WriteFile(logPath, nil, 0o600)
	}
	if err := os.Setenv("XRAY_TUN_FD", strconv.Itoa(tunFD)); err != nil {
		return err
	}
	defer os.Unsetenv("XRAY_TUN_FD")
	if mobile.naive != nil {
		if err := mobile.naive.Start(); err != nil {
			mobile.naive = nil
			mobile.prepared = nil
			return err
		}
	}
	config, err := core.LoadConfig("json", bytes.NewReader(mobile.prepared))
	if err != nil {
		if mobile.naive != nil {
			_ = mobile.naive.Close()
			mobile.naive = nil
		}
		return err
	}
	instance, err := core.New(config)
	if err != nil {
		if mobile.naive != nil {
			_ = mobile.naive.Close()
			mobile.naive = nil
		}
		return err
	}
	if mobile.naive != nil {
		if err = mobile.naive.Install(instance, "proxy"); err != nil {
			_ = instance.Close()
			_ = mobile.naive.Close()
			mobile.naive = nil
			return err
		}
	}
	if err = instance.Start(); err != nil {
		_ = instance.Close()
		if mobile.naive != nil {
			_ = mobile.naive.Close()
			mobile.naive = nil
		}
		return err
	}
	mobile.instance = instance
	return nil
}

func Stop() error {
	mobile.Lock()
	defer mobile.Unlock()
	mobile.prepared = nil
	var closeErr error
	if mobile.instance != nil {
		instance := mobile.instance
		mobile.instance = nil
		closeErr = instance.Close()
	}
	if mobile.naive != nil {
		naive := mobile.naive
		mobile.naive = nil
		if err := naive.Close(); closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

// PublicIP returns the exit IP of the running tunnel. Requests go through the
// Xray instance directly; nothing listens on a local port.
func PublicIP() (string, error) {
	mobile.Lock()
	instance := mobile.instance
	mobile.Unlock()
	if instance == nil {
		return "", errors.New("VPN не подключён")
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dialThroughInstance(ctx, instance, address)
		},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 8 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	var lastErr error
	for _, endpoint := range []string{"https://api4.ipify.org", "https://ipv4.icanhazip.com"} {
		response, err := client.Get(endpoint)
		if err != nil {
			lastErr = err
			continue
		}
		body, err := io.ReadAll(io.LimitReader(response.Body, 128))
		response.Body.Close()
		ip := strings.TrimSpace(string(body))
		if err == nil && response.StatusCode == http.StatusOK && net.ParseIP(ip) != nil {
			return ip, nil
		}
		lastErr = errors.New("некорректный ответ")
	}
	return "", lastErr
}

// Logs returns the tail of the Xray warning/error log of the current session.
func Logs() string {
	mobile.Lock()
	path := xrayLogPath(mobile.dataDir)
	mobile.Unlock()
	if path == "" {
		return ""
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	const limit = 32 * 1024
	if info, err := file.Stat(); err == nil && info.Size() > limit {
		_, _ = file.Seek(info.Size()-limit, io.SeekStart)
	}
	data, _ := io.ReadAll(io.LimitReader(file, limit))
	text := string(data)
	if len(data) == limit {
		if newline := strings.IndexByte(text, '\n'); newline >= 0 {
			text = text[newline+1:]
		}
	}
	return strings.ToValidUTF8(text, "")
}
