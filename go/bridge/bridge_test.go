package bridge

import (
	"encoding/json"
	"net/http"
	"testing"

	utls "github.com/refraction-networking/utls"
	xraytls "github.com/xtls/xray-core/transport/internet/tls"
)

func TestSubscriptionUserInfoMetadata(t *testing.T) {
	header := http.Header{}
	header.Set("Subscription-Userinfo", "upload=1048576; download=2097152; total=10737418240; expire=1893456000")
	metadata := subscriptionMetadataFromHeaders(header)
	if metadata.Upload != 1048576 || metadata.Download != 2097152 ||
		metadata.Total != 10737418240 || metadata.Expire != 1893456000 {
		t.Fatalf("subscription traffic metadata was parsed incorrectly: %#v", metadata)
	}
}

func TestPatchedUTLSUsesChrome152(t *testing.T) {
	if utls.HelloChrome_Auto.Version != "152" {
		t.Fatalf("chrome auto fingerprint is stale: %s", utls.HelloChrome_Auto.Version)
	}
	for _, name := range []string{"chrome", "edge"} {
		fingerprint := xraytls.GetFingerprint(name)
		if fingerprint == nil || fingerprint.Version != "152" {
			t.Fatalf("%s does not resolve to patched Chrome 152: %#v", name, fingerprint)
		}
	}
	spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
	if err != nil {
		t.Fatal(err)
	}
	foundTrustAnchors := false
	for _, extension := range spec.Extensions {
		if generic, ok := extension.(*utls.GenericExtension); ok && generic.Id == 0xca34 && len(generic.Data) > 2 {
			foundTrustAnchors = true
		}
	}
	if !foundTrustAnchors {
		t.Fatal("Chrome 152 trust_anchors extension is missing")
	}
}

func TestProfileDefaultsToPatchedChromeFingerprint(t *testing.T) {
	outbound := map[string]any{
		"protocol": "vless",
		"streamSettings": map[string]any{
			"security":        "reality",
			"realitySettings": map[string]any{"serverName": "example.com"},
		},
	}
	profile := newProfile("test", outbound)
	settings := profile.outbound["streamSettings"].(map[string]any)["realitySettings"].(map[string]any)
	if settings["fingerprint"] != "chrome" {
		t.Fatalf("unexpected default fingerprint: %#v", settings["fingerprint"])
	}
}

func TestAndroidRuntimeConfigRemovesPlatformOwnedTunSettings(t *testing.T) {
	raw, err := json.Marshal(runtimeConfig(
		[]any{map[string]any{"tag": "proxy", "protocol": "freedom"}},
		dnsPresets["cloudflare"],
		"auto",
	))
	if err != nil {
		t.Fatal(err)
	}

	encoded, err := androidRuntimeConfig(raw, 1400)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	inbounds := document["inbounds"].([]any)
	settings := inbounds[0].(map[string]any)["settings"].(map[string]any)
	for _, key := range []string{"autoSystemRoutingTable", "autoOutboundsInterface", "gateway", "dns"} {
		if _, exists := settings[key]; exists {
			t.Fatalf("Android runtime config still contains %q", key)
		}
	}
	if settings["mtu"] != float64(1400) {
		t.Fatalf("unexpected TUN MTU: %#v", settings["mtu"])
	}
}

func TestProfileJSONDoesNotExposeConnectionDetails(t *testing.T) {
	profile := newProfile("secret server", map[string]any{
		"protocol": "trojan",
		"settings": map[string]any{"servers": []any{map[string]any{
			"address": "vpn.example.com", "port": 443, "password": "secret",
		}}},
	})
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var display map[string]any
	if err := json.Unmarshal(encoded, &display); err != nil {
		t.Fatal(err)
	}
	if _, exists := display["address"]; exists {
		t.Fatal("profile JSON exposes server address")
	}
	if _, exists := display["outbound"]; exists {
		t.Fatal("profile JSON exposes outbound credentials")
	}
	if display["name"] != "secret server" || display["protocol"] != "trojan" {
		t.Fatalf("display metadata was lost: %#v", display)
	}
}

func TestProfilesForRendererKeepsAutoFirst(t *testing.T) {
	profiles := profilesForRenderer([]Profile{{ID: "server", Name: "Server"}})
	if len(profiles) != 3 || profiles[0].ID != autoProfileID || profiles[1].ID != autoNoRUProfileID {
		t.Fatalf("unexpected renderer profiles: %#v", profiles)
	}
}

func TestConfigureValidatesMobileOptions(t *testing.T) {
	if err := Configure(`{"dnsId":"cloudflare","fragmentation":true,"routingMode":"full","pingMethod":"get","tunMtu":1400}`); err != nil {
		t.Fatal(err)
	}
	if !mobile.options.Fragmentation || mobile.options.PingMethod != "get" || mobile.options.RoutingMode != "full" {
		t.Fatalf("mobile options were not saved: %#v", mobile.options)
	}
	if err := Configure(`{"dnsId":"cloudflare","routingMode":"full","pingMethod":"invalid"}`); err == nil {
		t.Fatal("invalid ping method was accepted")
	}
	if err := Configure(`{"dnsId":"cloudflare","routingMode":"full","pingMethod":"tcp","tunMtu":900}`); err == nil {
		t.Fatal("invalid TUN MTU was accepted")
	}
}

func TestPingProfilesWithoutImportReturnsEmptyJSON(t *testing.T) {
	mobile.Lock()
	previous := mobile.profiles
	mobile.profiles = nil
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.profiles = previous; mobile.Unlock() }()
	result, err := PingProfiles("tcp")
	if err != nil || result != "[]" {
		t.Fatalf("unexpected empty ping result: %q, %v", result, err)
	}
}
