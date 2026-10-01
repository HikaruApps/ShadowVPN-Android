package bridge

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xtls/xray-core/core"
)

type PingResult struct {
	ID              string `json:"id"`
	LatencyMS       int64  `json:"latencyMs,omitempty"`
	Available       bool   `json:"available"`
	ResolvedAddress string `json:"-"`
}

type pingOptions struct {
	OutboundInterface string
	AllowEndpoint     func(proxyEndpoint) error
}

func endpointFromOutbound(out map[string]any) (string, int) {
	settings, _ := out["settings"].(map[string]any)
	protocol, _ := out["protocol"].(string)
	if protocol == "hysteria" || protocol == "naive" {
		address, _ := settings["address"].(string)
		return address, integerValue(settings["port"])
	}
	key := "servers"
	if protocol == "vless" || protocol == "vmess" {
		key = "vnext"
	}
	raw, _ := settings[key].([]any)
	if len(raw) == 0 {
		return "", 0
	}
	server, _ := raw[0].(map[string]any)
	address, _ := server["address"].(string)
	return address, integerValue(server["port"])
}

func integerValue(value any) int {
	switch value := value.(type) {
	case int:
		return value
	case float64:
		return int(value)
	case json.Number:
		result, _ := strconv.Atoi(value.String())
		return result
	case string:
		result, _ := strconv.Atoi(value)
		return result
	default:
		return 0
	}
}

func tcpPing(ctx context.Context, address string, port int) (int64, error) {
	latency, _, err := tcpPingEndpoint(ctx, address, port)
	return latency, err
}

func tcpPingEndpoint(ctx context.Context, address string, port int) (int64, string, error) {
	return tcpPingEndpointWithOptions(ctx, address, port, pingOptions{})
}

func tcpPingEndpointWithOptions(ctx context.Context, address string, port int, options pingOptions) (int64, string, error) {
	if address == "" || port < 1 || port > 65535 {
		return 0, "", fmt.Errorf("missing endpoint")
	}
	if options.AllowEndpoint != nil {
		if err := options.AllowEndpoint(proxyEndpoint{Address: address, Port: port}); err != nil {
			return 0, "", err
		}
	}
	dialer, err := interfaceDialer(options.OutboundInterface, 2500*time.Millisecond)
	if err != nil {
		return 0, "", err
	}
	started := time.Now()
	connection, err := dialer.DialContext(
		ctx,
		"tcp",
		net.JoinHostPort(address, strconv.Itoa(port)),
	)
	if err != nil {
		return 0, "", err
	}
	resolvedAddress := ""
	if remote, ok := connection.RemoteAddr().(*net.TCPAddr); ok && remote.IP != nil {
		resolvedAddress = remote.IP.String()
	}
	_ = connection.Close()
	latency := time.Since(started).Milliseconds()
	if latency < 1 {
		latency = 1
	}
	return latency, resolvedAddress, nil
}

func pingProfiles(parent context.Context, profiles []Profile) []PingResult {
	return pingProfilesWithMethod(parent, profiles, "tcp")
}

func httpPingProfile(ctx context.Context, profile Profile, method string) (int64, string, error) {
	return httpPingProfileWithOptions(ctx, profile, method, pingOptions{})
}

func httpPingProfileWithOptions(ctx context.Context, profile Profile, method string, options pingOptions) (int64, string, error) {
	resolved, bootstrap, err := resolveProfileEndpoint(ctx, profile)
	if err != nil {
		return 0, "", err
	}
	if options.AllowEndpoint != nil {
		if err = options.AllowEndpoint(proxyEndpoint{Address: resolved.address, Port: resolved.port}); err != nil {
			return 0, "", err
		}
	}
	var naive *naiveRuntime
	if resolved.Protocol == "naive" {
		naive, err = newNaiveRuntime(resolved)
		if err != nil {
			return 0, "", err
		}
		defer naive.Close()
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, "", err
	}
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()
	tokenBytes := make([]byte, 16)
	if _, err = rand.Read(tokenBytes); err != nil {
		return 0, "", err
	}
	probePassword := hex.EncodeToString(tokenBytes)
	outbound, err := configuredOutbound(resolved, "probe", false)
	if err != nil {
		return 0, "", err
	}
	if options.OutboundInterface != "" {
		stream := nestedMap(outbound, "streamSettings")
		sockopt := nestedMap(stream, "sockopt")
		sockopt["interface"] = options.OutboundInterface
	}
	config, err := json.Marshal(map[string]any{
		"log": map[string]any{"loglevel": "none"},
		"inbounds": []any{map[string]any{
			"tag": "probe-in", "listen": "127.0.0.1", "port": port, "protocol": "http",
			"settings": map[string]any{
				"timeout":  8,
				"accounts": []any{map[string]any{"user": "shadowvpn", "pass": probePassword}},
			},
		}},
		"outbounds": []any{outbound},
	})
	if err != nil {
		return 0, "", err
	}
	parsed, err := core.LoadConfig("json", bytes.NewReader(config))
	if err != nil {
		return 0, "", err
	}
	instance, err := core.New(parsed)
	if err != nil {
		return 0, "", err
	}
	defer instance.Close()
	if naive != nil {
		if err = naive.Start(); err != nil {
			return 0, "", err
		}
		if err = naive.Install(instance, "probe"); err != nil {
			return 0, "", err
		}
	}
	if err = instance.Start(); err != nil {
		return 0, "", err
	}
	proxyURL := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort("127.0.0.1", strconv.Itoa(port)),
		User:   url.UserPassword("shadowvpn", probePassword),
	}
	transport := &http.Transport{
		Proxy: http.ProxyURL(proxyURL), DisableKeepAlives: true,
		DialContext:         (&net.Dialer{Timeout: 4 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 7 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 9 * time.Second}
	request, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), "https://www.gstatic.com/generate_204", nil)
	if err != nil {
		return 0, "", err
	}
	request.Header.Set("User-Agent", "shadowvpn-probe/1")
	started := time.Now()
	response, err := client.Do(request)
	if err != nil {
		return 0, "", err
	}
	defer response.Body.Close()
	if method == "get" {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	}
	if response.StatusCode < 200 || response.StatusCode >= 500 {
		return 0, "", fmt.Errorf("probe returned HTTP %d", response.StatusCode)
	}
	latency := time.Since(started).Milliseconds()
	if latency < 1 {
		latency = 1
	}
	return latency, bootstrap.SelectedAddress, nil
}

func pingProfilesWithMethod(parent context.Context, profiles []Profile, method string) []PingResult {
	return pingProfilesWithMethodOptions(parent, profiles, method, pingOptions{})
}

func pingProfilesWithMethodOptions(parent context.Context, profiles []Profile, method string, options pingOptions) []PingResult {
	ctx, cancel := context.WithTimeout(parent, 45*time.Second)
	defer cancel()
	results := make([]PingResult, len(profiles))
	jobs := make(chan int)
	workers := 32
	if method != "tcp" {
		workers = 6
	}
	if len(profiles) < workers {
		workers = len(profiles)
	}
	var group sync.WaitGroup
	for range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				profile := profiles[index]
				probeMethod := method
				// A Naive profile is backed by Cronet only after the user selects it.
				// Before that, latency checks verify reachability of its endpoint.
				if profile.Protocol == "naive" {
					probeMethod = "tcp"
				}
				if probeMethod == "auto" {
					if profile.Protocol == "hysteria" {
						probeMethod = "head"
					} else {
						probeMethod = "tcp"
					}
				}
				var latency int64
				var resolvedAddress string
				var err error
				if probeMethod == "head" || probeMethod == "get" {
					latency, resolvedAddress, err = httpPingProfileWithOptions(ctx, profile, probeMethod, options)
				} else {
					if options.OutboundInterface != "" {
						var resolved Profile
						resolved, _, err = resolveProfileEndpoint(ctx, profile)
						if err == nil {
							latency, resolvedAddress, err = tcpPingEndpointWithOptions(ctx, resolved.address, resolved.port, options)
						}
					} else {
						latency, resolvedAddress, err = tcpPingEndpointWithOptions(ctx, profile.address, profile.port, options)
					}
				}
				results[index] = PingResult{ID: profile.ID, LatencyMS: latency, Available: err == nil, ResolvedAddress: resolvedAddress}
			}
		}()
	}
	for index := range profiles {
		select {
		case jobs <- index:
		case <-ctx.Done():
			results[index] = PingResult{ID: profiles[index].ID}
		}
	}
	close(jobs)
	group.Wait()
	return results
}
