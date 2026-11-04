package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagnoseExplainsEachStep(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	previous := realPingURLs
	realPingURLs = []string{target.URL + "/generate_204"}
	defer func() { realPingURLs = previous }()

	working := shadowsocksProfile(t, "000000000000000000000021", startShadowsocksServer(t, "correct-password"), "correct-password")
	wrongKey := shadowsocksProfile(t, "000000000000000000000022", startShadowsocksServer(t, "correct-password"), "other-password")
	unknown := working
	unknown.ID = "000000000000000000000023"
	unknown.address = "no-such-host.invalid"
	unknown.outbound = map[string]any{"protocol": "shadowsocks", "settings": map[string]any{"servers": []any{map[string]any{"address": "no-such-host.invalid", "port": 443, "method": "chacha20-ietf-poly1305", "password": "x"}}}}

	mobile.Lock()
	saved := mobile.profiles
	mobile.profiles = []Profile{working, wrongKey, unknown}
	mobile.Unlock()
	defer func() { mobile.Lock(); mobile.profiles = saved; mobile.Unlock() }()

	run := func(id string) diagnoseResult {
		t.Helper()
		raw, err := Diagnose(id)
		if err != nil {
			t.Fatal(err)
		}
		var result diagnoseResult
		_ = json.Unmarshal([]byte(raw), &result)
		return result
	}
	if r := run(working.ID); !r.DNS.OK || !r.Server.OK || !strings.Contains(r.Server.Detail, "мс") {
		t.Fatalf("working server: %+v", r)
	}
	if r := run(wrongKey.ID); !r.DNS.OK || r.Server.OK || r.Server.Detail == "" {
		t.Fatalf("wrong key must fail the server step with a reason: %+v", r)
	}
	if r := run(unknown.ID); r.DNS.OK || r.Server.OK || !strings.Contains(r.DNS.Detail, "IP сервера") {
		t.Fatalf("unknown host must fail the DNS step: %+v", r)
	}
	if _, err := Diagnose("000000000000000000000099"); err == nil {
		t.Fatal("a server that left the subscription must be reported")
	}
	t.Logf("wrong key: %q", run(wrongKey.ID).Server.Detail)
}
