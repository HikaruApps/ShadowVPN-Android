package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

// startShadowsocksServer runs a real Xray Shadowsocks server on loopback
// that forwards to the internet (here: loopback test servers).
func startShadowsocksServer(t *testing.T, password string) int {
	t.Helper()
	port := freePort(t)
	config := fmt.Sprintf(`{"log":{"loglevel":"none"},
	  "inbounds":[{"listen":"127.0.0.1","port":%d,"protocol":"shadowsocks","settings":{"method":"chacha20-ietf-poly1305","password":%q,"network":"tcp"}}],
	  "outbounds":[{"protocol":"freedom","settings":{"finalRules":[{"action":"allow","ip":["127.0.0.0/8"]}]}}]}`, port, password)
	parsed, err := core.LoadConfig("json", bytes.NewReader([]byte(config)))
	if err != nil {
		t.Fatal(err)
	}
	instance, err := core.New(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if err := instance.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { instance.Close() })
	return port
}

func shadowsocksProfile(t *testing.T, id string, port int, password string) Profile {
	t.Helper()
	userInfo := base64.RawURLEncoding.EncodeToString([]byte("chacha20-ietf-poly1305:" + password))
	profile, err := parseShareLink(fmt.Sprintf("ss://%s@127.0.0.1:%d#%s", userInfo, port, id))
	if err != nil {
		t.Fatal(err)
	}
	profile.ID = id
	return profile
}

func TestRealPingTellsWorkingFromMerelyReachable(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	previous := realPingURLs
	// The first endpoint is down: one blocked endpoint must not fail a server.
	realPingURLs = []string{dead.URL + "/generate_204", target.URL + "/generate_204"}
	defer func() { realPingURLs = previous }()

	working := shadowsocksProfile(t, "working", startShadowsocksServer(t, "correct-password"), "correct-password")
	wrongKey := shadowsocksProfile(t, "wrong-key", startShadowsocksServer(t, "correct-password"), "other-password")

	// A port that accepts connections but is not a working proxy: the case
	// where a server "pings" yet nothing loads.
	decoy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer decoy.Close()
	go func() {
		for {
			conn, err := decoy.Accept()
			if err != nil {
				return
			}
			go func() { time.Sleep(200 * time.Millisecond); conn.Close() }()
		}
	}()
	decoyProfile := shadowsocksProfile(t, "decoy", decoy.Addr().(*net.TCPAddr).Port, "correct-password")

	ctx := context.Background()
	if tcp := pingProfiles(ctx, []Profile{decoyProfile}); !tcp[0].Available {
		t.Fatal("the decoy must pass a TCP ping, that is the point of the test")
	}

	results := realPingProfiles(ctx, []Profile{working, wrongKey, decoyProfile}, false)
	if !results[0].Available || results[0].LatencyMS < 1 {
		t.Fatalf("working server must pass: %+v", results[0])
	}
	if results[1].Available {
		t.Fatalf("a server with a wrong key must fail: %+v", results[1])
	}
	if results[2].Available {
		t.Fatalf("a port that only accepts TCP must fail: %+v", results[2])
	}

	// Auto picks from TCP-fast servers only those that pass the real check.
	verified := verifiedPingProfiles(ctx, []Profile{decoyProfile, wrongKey, working}, false)
	if verified[0].Available || verified[1].Available || !verified[2].Available {
		t.Fatalf("only the working server may be chosen: %+v", verified)
	}
	if index, _, ok := fastestProfile([]Profile{decoyProfile, wrongKey, working}, verified); !ok || index != 2 {
		t.Fatalf("Auto must choose the working server, got %d %v", index, ok)
	}

	withAuto := withAutoPingResults([]Profile{decoyProfile, working}, realPingProfiles(ctx, []Profile{decoyProfile, working}, false))
	if withAuto[0].ID != autoProfileID || !withAuto[0].Available {
		t.Fatalf("Auto must be available through the working server: %+v", withAuto[0])
	}
}
