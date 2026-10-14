package bridge

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/xtls/xray-core/core"
)

// sessionInputs is what building a runtime config reads from the shared
// state. It is copied under the lock so that slow work (DNS, pings, GeoData)
// runs without holding it.
type sessionInputs struct {
	options  mobileOptions
	profiles []Profile
	dataDir  string
}

func currentSessionInputs() sessionInputs {
	return sessionInputs{
		options:  mobile.options,
		profiles: append([]Profile(nil), mobile.profiles...),
		dataDir:  mobile.dataDir,
	}
}

// buildSession resolves the chosen profile and returns its Android runtime
// config, plus the Cronet runtime when the profile is Naive.
func buildSession(profileID string, in sessionInputs) ([]byte, *naiveRuntime, error) {
	options := in.options
	dnsID := options.DNSID
	if dnsID == "" {
		dnsID = "cloudflare"
	}
	_, dns, err := selectedDNSWithDoH(dnsID, options.CustomDNS, options.DoHURL)
	if err != nil {
		return nil, nil, err
	}
	geoIP, geoSite := options.geoURLs()
	routing, err := newRoutingOptions(options.RoutingMode, options.RoutingRules, geoIP, geoSite)
	if err != nil {
		return nil, nil, err
	}
	runtime := androidRuntimeOptions{
		MTU:               options.TunMTU,
		IPv6:              options.IPv6,
		IPType:            options.IPType,
		Mux:               options.Mux,
		RouteOnly:         options.RouteOnly,
		BlockUDP:          options.BlockUDP,
		LocalProxy:        options.LocalProxy,
		DNS:               dns,
		RoutingMode:       routing.Mode,
		SubscriptionRules: options.SubscriptionRules,
		LogPath:           xrayLogPath(in.dataDir),
		GeoData: func(needIP, needSite bool) (geoDataInfo, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			return ensureGeoData(ctx, in.dataDir, needIP, needSite, geoIP, geoSite)
		},
	}

	// Auto and balancer configs from Xray JSON both run Xray's leastPing
	// balancer over a set of servers, fastest first.
	balance := func(candidates []Profile, empty string) ([]byte, *naiveRuntime, error) {
		if len(candidates) == 0 {
			return nil, nil, errors.New(empty)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		results := pingProfiles(ctx, candidates)
		prepared, _, _, err := prepareAutoProfiles(ctx, candidates, results)
		if err != nil {
			return nil, nil, err
		}
		config, err := makeAutoConfigWithRoutingAndDoH(prepared, dnsID, options.CustomDNS, options.DoHURL, options.Fragmentation, "", routing)
		if err != nil {
			return nil, nil, err
		}
		runtime.Profiles = prepared
		runtime.Auto = true
		config, err = androidRuntimeConfig(config, runtime)
		return config, nil, err
	}
	if isAutoProfileID(profileID) {
		return balance(autoCandidates(in.profiles, options, profileID), "Для Auto нет подходящих серверов")
	}

	var chosen *Profile
	for i := range in.profiles {
		if in.profiles[i].ID == profileID {
			chosen = &in.profiles[i]
			break
		}
	}
	if chosen == nil {
		return nil, nil, errors.New("Сервер отсутствует в подписке. Обновите её")
	}
	if len(chosen.members) > 0 {
		return balance(profilesWithoutProtocol(chosen.members, "naive"), "В балансировщике нет подходящих серверов")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resolved, _, err := resolveProfileEndpoint(ctx, *chosen)
	if err != nil {
		return nil, nil, errors.New("Не удалось определить IP сервера")
	}
	var naive *naiveRuntime
	if resolved.Protocol == "naive" {
		if naive, err = newNaiveRuntime(resolved); err != nil {
			return nil, nil, err
		}
	}
	// A profile that already chains through a fragmenting dialer must not be
	// fragmented twice.
	fragmentation := options.Fragmentation && len(resolved.chain) == 0
	config, err := makeConfigWithRoutingAndDoH(resolved, dnsID, options.CustomDNS, options.DoHURL, fragmentation, "", routing)
	if err == nil {
		runtime.Profiles = []Profile{resolved}
		config, err = androidRuntimeConfig(config, runtime)
	}
	if err != nil {
		if naive != nil {
			_ = naive.Close()
		}
		return nil, nil, err
	}
	return config, naive, nil
}

// detachedInbounds are the listeners of a config that start only once the
// instance takes over: the TUN, and local proxy ports the previous instance
// may still hold.
type detachedInbounds struct {
	tun   *core.InboundHandlerConfig
	proxy []*core.InboundHandlerConfig
}

// startDetached starts an Xray instance from a runtime config without its
// inbounds, which are returned separately. Outbounds, routing and DNS are
// live and can be tested before the instance takes over the tunnel.
func startDetached(config []byte, naive *naiveRuntime) (*core.Instance, detachedInbounds, error) {
	parsed, err := core.LoadConfig("json", bytes.NewReader(config))
	if err != nil {
		return nil, detachedInbounds{}, err
	}
	if len(parsed.Inbound) == 0 || parsed.Inbound[0].Tag != "tun-in" {
		return nil, detachedInbounds{}, errors.New("Конфигурация Xray должна начинаться с TUN")
	}
	inbounds := detachedInbounds{tun: parsed.Inbound[0], proxy: parsed.Inbound[1:]}
	parsed.Inbound = nil
	if naive != nil {
		if err := naive.Start(); err != nil {
			return nil, detachedInbounds{}, err
		}
	}
	instance, err := core.New(parsed)
	if err != nil {
		return nil, detachedInbounds{}, err
	}
	if naive != nil {
		if err := naive.Install(instance, "proxy"); err != nil {
			_ = instance.Close()
			return nil, detachedInbounds{}, err
		}
	}
	if err := instance.Start(); err != nil {
		_ = instance.Close()
		return nil, detachedInbounds{}, err
	}
	return instance, inbounds, nil
}

// tunEnvMu serializes use of XRAY_TUN_FD, which the TUN inbound reads while
// it starts.
var tunEnvMu sync.Mutex

// attachTun makes a running instance read and write the Android TUN fd. The
// fd itself is owned by VpnService and stays open across instances.
func attachTun(instance *core.Instance, tun *core.InboundHandlerConfig, fd int) error {
	tunEnvMu.Lock()
	defer tunEnvMu.Unlock()
	if err := os.Setenv("XRAY_TUN_FD", strconv.Itoa(fd)); err != nil {
		return err
	}
	defer os.Unsetenv("XRAY_TUN_FD")
	return core.AddInboundHandler(instance, tun)
}

// attachInbounds hands the tunnel (unless running as a proxy only, tunFD < 0)
// and the local proxy ports to a running instance.
func attachInbounds(instance *core.Instance, inbounds detachedInbounds, tunFD int) error {
	if tunFD >= 0 {
		if err := attachTunFunc(instance, inbounds.tun, tunFD); err != nil {
			return err
		}
	}
	for _, inbound := range inbounds.proxy {
		if err := core.AddInboundHandler(instance, inbound); err != nil {
			return fmt.Errorf("Не удалось открыть порт прокси: %v", err)
		}
	}
	return nil
}

// probeURL answers 204 quickly from everywhere the proxies are likely to be.
var probeURL = "https://www.gstatic.com/generate_204"

// Replaced in tests, which cannot open a real TUN device.
var (
	buildSessionFunc = buildSession
	attachTunFunc    = attachTun
)

// probeInstance makes a real HTTPS request through the instance's default
// outbound, proving the server accepts connections and forwards traffic.
func probeInstance(instance *core.Instance, timeout time.Duration) error {
	client, done := instanceHTTPClient(instance, timeout)
	defer done()
	request, err := http.NewRequest(http.MethodGet, probeURL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "shadowvpn-probe/1")
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
	response.Body.Close()
	if response.StatusCode >= 500 {
		return fmt.Errorf("HTTP %d", response.StatusCode)
	}
	return nil
}

func instanceHTTPClient(instance *core.Instance, timeout time.Duration) (*http.Client, func()) {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return dialThroughInstance(ctx, instance, address)
		},
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: timeout,
	}
	return &http.Client{Transport: transport, Timeout: timeout}, transport.CloseIdleConnections
}

// SwitchProfile moves a running tunnel to another server without closing
// the TUN: the new server is connected and tested first, then the tunnel is
// handed over and the old connection closed. If the new server does not
// answer, the current one stays and an error is returned.
func SwitchProfile(profileID string) error {
	return switchSession(profileID, false)
}

// Reload rebuilds the current connection the same way, for example after
// the phone moved to another network and the old sockets went stale.
func Reload() error {
	mobile.Lock()
	profileID := mobile.profileID
	mobile.Unlock()
	if profileID == "" {
		return errors.New("VPN не подключён")
	}
	return switchSession(profileID, true)
}

func switchSession(profileID string, reload bool) error {
	mobile.Lock()
	if mobile.instance == nil {
		mobile.Unlock()
		return errors.New("VPN не подключён")
	}
	if mobile.switching {
		mobile.Unlock()
		return errors.New("Переключение уже выполняется")
	}
	mobile.switching = true
	inputs := currentSessionInputs()
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.switching = false; mobile.Unlock() }()

	config, naive, err := buildSessionFunc(profileID, inputs)
	if err != nil {
		return err
	}
	instance, inbounds, err := startDetached(config, naive)
	if err != nil {
		closeNaive(naive)
		return err
	}
	if err := probeInstance(instance, 10*time.Second); err != nil {
		_ = instance.Close()
		closeNaive(naive)
		if reload {
			return fmt.Errorf("Сервер не отвечает: %v", err)
		}
		return fmt.Errorf("Новый сервер не отвечает, соединение осталось на текущем: %v", err)
	}

	mobile.Lock()
	defer mobile.Unlock()
	if mobile.instance == nil {
		// Disconnected while the new server was being tested.
		_ = instance.Close()
		closeNaive(naive)
		return errors.New("VPN отключён")
	}
	old, oldNaive := mobile.instance, mobile.naive
	// The old stack must stop reading the fd before the new one starts.
	_ = old.Close()
	if err := attachInbounds(instance, inbounds, mobile.tunFD); err != nil {
		_ = instance.Close()
		closeNaive(naive)
		closeNaive(oldNaive)
		mobile.instance, mobile.naive = nil, nil
		stopWatchdogLocked()
		return fmt.Errorf("Туннель потерян при переключении: %v", err)
	}
	closeNaive(oldNaive)
	mobile.instance, mobile.naive, mobile.profileID = instance, naive, profileID
	return nil
}

func closeNaive(naive *naiveRuntime) {
	if naive != nil {
		_ = naive.Close()
	}
}

// Watchdog: a connection can go silently dead (a NAT mapping expires, the
// server drops an idle QUIC session, the old network disappears without a
// clean signal). Every interval a small request goes through the tunnel;
// after consecutive failures the connection is rebuilt on the same server.
const (
	watchdogInterval = 60 * time.Second
	watchdogFailures = 2
)

// watchdogProbe checks that traffic still flows through an instance.
var watchdogProbe = func(instance *core.Instance) error { return probeInstance(instance, 10*time.Second) }

func startWatchdogLocked() {
	stopWatchdogLocked()
	ctx, cancel := context.WithCancel(context.Background())
	mobile.watchdogCancel = cancel
	go runWatchdog(ctx, watchdogInterval, watchdogProbe)
}

func stopWatchdogLocked() {
	if mobile.watchdogCancel != nil {
		mobile.watchdogCancel()
		mobile.watchdogCancel = nil
	}
}

func runWatchdog(ctx context.Context, interval time.Duration, probe func(*core.Instance) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		mobile.Lock()
		instance, busy := mobile.instance, mobile.switching
		mobile.Unlock()
		if instance == nil {
			return
		}
		if busy {
			continue
		}
		if err := probe(instance); err == nil {
			failures = 0
			continue
		}
		failures++
		if failures < watchdogFailures {
			continue
		}
		failures = 0
		if ctx.Err() != nil {
			return
		}
		_ = Reload()
	}
}
