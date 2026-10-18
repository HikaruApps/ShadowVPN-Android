package bridge

import (
	"sync"

	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/transport/internet"
)

// Xray keeps one process-wide dialer state: the outbound manager that
// resolves dialerProxy chains and the DNS client for sockopt domain
// strategies. Every core.New points it at the new instance. Probe
// instances must therefore not leave it pointing at themselves while the
// VPN runs, and probes that use chains must not overlap.
var (
	// xrayDialerMu: readers create short-lived instances that need no
	// chain; a writer holds the global state for a whole chain probe.
	xrayDialerMu sync.RWMutex
	xrayNewMu    sync.Mutex
)

func newXrayInstance(config *core.Config) (*core.Instance, error) {
	xrayNewMu.Lock()
	defer xrayNewMu.Unlock()
	return core.New(config)
}

// activateDialer points the process-wide dialer state at instance.
func activateDialer(instance *core.Instance) {
	client, _ := instance.GetFeature(dns.ClientType()).(dns.Client)
	manager, _ := instance.GetFeature(outbound.ManagerType()).(outbound.Manager)
	xrayNewMu.Lock()
	defer xrayNewMu.Unlock()
	internet.InitSystemDialer(client, manager)
}

// restoreActiveDialer gives the dialer state back to the running VPN, if any.
func restoreActiveDialer() {
	mobile.Lock()
	instance := mobile.instance
	mobile.Unlock()
	if instance != nil {
		activateDialer(instance)
	}
}
