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
	profiles []Profile
	prepared []byte
	instance *core.Instance
	seed string
	importing bool
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

type importResult struct {
	Profiles []Profile `json:"profiles"`
	Skipped int `json:"skipped"`
	Title string `json:"title"`
	DNSDoH string `json:"dnsDoh"`
}

// ImportSubscription downloads an HTTPS subscription using the desktop parser.
// It returns only display metadata, never credentials or raw outbounds.
func ImportSubscription(address string) (string, error) {
	mobile.Lock()
	if mobile.instance != nil || mobile.importing {
		mobile.Unlock()
		return "", errors.New("Отключите VPN и дождитесь завершения текущей загрузки")
	}
	mobile.importing = true
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.importing = false; mobile.Unlock() }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	profiles, skipped, meta, err := fetchSubscription(ctx, address)
	if err != nil { return "", err }
	mobile.Lock()
	mobile.profiles = profiles
	mobile.prepared = nil
	mobile.Unlock()
	result, err := json.Marshal(importResult{Profiles: profilesForRenderer(profiles), Skipped: skipped, Title: meta.Title, DNSDoH: meta.DNSDoH})
	return string(result), err
}

// Prepare resolves the server and builds a config BEFORE VpnService establishes
// the default route. This avoids a DNS bootstrap loop.
func Prepare(profileID string, dohURL string) error {
	mobile.Lock()
	defer mobile.Unlock()
	if mobile.instance != nil || mobile.importing { return errors.New("VPN занят обновлением или уже подключён") }
	var chosen *Profile
	for i := range mobile.profiles {
		if mobile.profiles[i].ID == profileID { chosen = &mobile.profiles[i]; break }
	}
	if chosen == nil { return errors.New("Сервер отсутствует в подписке. Обновите её") }
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	resolved, _, err := resolveProfileEndpoint(ctx, *chosen)
	if err != nil { return errors.New("Не удалось определить IP сервера") }
	dnsID := "cloudflare"
	if dohURL != "" { dnsID = "subscription-doh" }
	config, err := makeConfigWithRoutingAndDoH(resolved, dnsID, nil, dohURL, false, "", routingOptions{})
	if err != nil { return err }
	var document map[string]any
	if err = json.Unmarshal(config, &document); err != nil { return err }
	inbounds := document["inbounds"].([]any)
	settings := inbounds[0].(map[string]any)["settings"].(map[string]any)
	// Android owns routes, addresses and DNS. Xray only consumes the fd.
	delete(settings, "autoSystemRoutingTable")
	delete(settings, "autoOutboundsInterface")
	delete(settings, "gateway")
	delete(settings, "dns")
	mobile.prepared, err = json.Marshal(document)
	return err
}

// Start runs the pinned Xray core against the fd owned by Android VpnService.
// The Android app excludes its own UID from the VPN before calling this method,
// so outbound Xray sockets and bootstrap DNS cannot loop into the tunnel.
func Start(tunFD int) error {
	mobile.Lock()
	defer mobile.Unlock()
	if tunFD < 0 || mobile.prepared == nil || mobile.instance != nil { return errors.New("VPN не подготовлен") }
	if err := os.Setenv("XRAY_TUN_FD", strconv.Itoa(tunFD)); err != nil { return err }
	defer os.Unsetenv("XRAY_TUN_FD")
	config, err := core.LoadConfig("json", bytes.NewReader(mobile.prepared))
	if err != nil { return err }
	instance, err := core.New(config)
	if err != nil { return err }
	if err = instance.Start(); err != nil { _ = instance.Close(); return err }
	mobile.instance = instance
	return nil
}

func Stop() error {
	mobile.Lock()
	defer mobile.Unlock()
	mobile.prepared = nil
	if mobile.instance == nil { return nil }
	instance := mobile.instance
	mobile.instance = nil
	return instance.Close()
}
