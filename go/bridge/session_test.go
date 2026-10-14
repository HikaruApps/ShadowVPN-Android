package bridge

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xtls/xray-core/core"
)

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func listening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// A SOCKS listener stands in for the TUN inbound: tests cannot open a TUN
// device, and a listener shows exactly when an instance owns the "tunnel".
func sessionConfig(port int, outbound string) []byte {
	return []byte(fmt.Sprintf(`{"log":{"loglevel":"none"},
	  "inbounds":[{"tag":"tun-in","protocol":"socks","listen":"127.0.0.1","port":%d,"settings":{}}],
	  "outbounds":[{"tag":"proxy","protocol":%q}]}`, port, outbound))
}

func TestSwitchKeepsTunnelAndFallsBackOnDeadServer(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	ports := map[string]int{"good": freePort(t), "good2": freePort(t), "dead": freePort(t)}
	outbounds := map[string]string{"good": "freedom", "good2": "freedom", "dead": "blackhole"}
	previousBuild, previousAttach, previousProbe := buildSessionFunc, attachTunFunc, probeURL
	buildSessionFunc = func(id string, _ sessionInputs) ([]byte, *naiveRuntime, error) {
		return sessionConfig(ports[id], outbounds[id]), nil, nil
	}
	attachTunFunc = func(instance *core.Instance, tun *core.InboundHandlerConfig, _ int) error {
		return core.AddInboundHandler(instance, tun)
	}
	probeURL = target.URL + "/generate_204"
	defer func() { buildSessionFunc, attachTunFunc, probeURL = previousBuild, previousAttach, previousProbe }()
	defer Stop()

	if err := SwitchProfile("good"); err == nil {
		t.Fatal("switching without a running tunnel must fail")
	}
	if err := Prepare("good"); err != nil {
		t.Fatal(err)
	}
	if err := Start(3); err != nil {
		t.Fatal(err)
	}
	if !listening(ports["good"]) {
		t.Fatal("first server does not own the tunnel")
	}
	mobile.Lock()
	first := mobile.instance
	mobile.Unlock()

	// A server that accepts nothing must leave the current one in place.
	err := SwitchProfile("dead")
	if err == nil || !strings.Contains(err.Error(), "осталось на текущем") {
		t.Fatalf("dead server must be rejected, got %v", err)
	}
	mobile.Lock()
	kept, keptID := mobile.instance, mobile.profileID
	mobile.Unlock()
	if kept != first || keptID != "good" || !listening(ports["good"]) || listening(ports["dead"]) {
		t.Fatal("the working connection was lost on a failed switch")
	}

	// A working server takes over the tunnel and the old one is closed.
	if err := SwitchProfile("good2"); err != nil {
		t.Fatal(err)
	}
	mobile.Lock()
	switched, switchedID := mobile.instance, mobile.profileID
	mobile.Unlock()
	if switched == first || switchedID != "good2" {
		t.Fatal("switch did not replace the instance")
	}
	if !listening(ports["good2"]) || listening(ports["good"]) {
		t.Fatal("tunnel must move to the new server and leave the old one")
	}

	// The watchdog rebuilds the connection after consecutive failed probes.
	stalled := func(*core.Instance) error { return fmt.Errorf("stalled") }
	ctx, cancel := context.WithCancel(context.Background())
	go runWatchdog(ctx, 20*time.Millisecond, stalled)
	deadline := time.Now().Add(5 * time.Second)
	for {
		mobile.Lock()
		current := mobile.instance
		mobile.Unlock()
		if current != switched {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("watchdog did not rebuild a stalled connection")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if !listening(ports["good2"]) {
		t.Fatal("rebuilt connection must stay on the same server")
	}

	if err := Stop(); err != nil {
		t.Fatal(err)
	}
	if listening(ports["good2"]) {
		t.Fatal("Stop must release the tunnel")
	}
}
