package bridge

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

// Server chaining (like Karing's proxy chain): the phone connects to a
// server in Russia, and that server carries the connection on to the chosen
// one. Xray expresses it with streamSettings.sockopt.dialerProxy: the chosen
// server's outbound dials through the relay's outbound. The app turns it on
// only on Wi-Fi (it sends Relay only there), so mobile data stays direct.

// relayOptions is present only when chaining is on for the current network.
type relayOptions struct {
	// ProfileID is the relay server; empty picks the fastest Russian one.
	ProfileID string `json:"profileId"`
}

const relayTag = "relay"

// relayPingTimeout bounds the TCP check that picks the fastest relay; it runs
// before every connection with the relay on.
var relayPingTimeout = 3 * time.Second

// activeRelay is the name of the relay the running connection goes through,
// read by the app without taking the session lock.
var activeRelay atomic.Value

func setActiveRelay(name string) { activeRelay.Store(name) }

// ActiveRelay returns the name of the server the running connection is
// chained through, or an empty string.
func ActiveRelay() string {
	name, _ := activeRelay.Load().(string)
	return name
}

// relayCandidates are the servers that can carry a chain: Russian ones, except
// Naive, which runs in Cronet outside Xray's dialer.
func relayCandidates(profiles []Profile) []Profile {
	flat, _ := flattenGroups(profiles)
	result := make([]Profile, 0)
	for _, profile := range flat {
		if isRussianProfile(profile) && profile.Protocol != "naive" {
			result = append(result, profile)
		}
	}
	return result
}

// chainable says whether a server can be reached through the relay, and why
// not. UDP protocols (Hysteria, TUIC, WireGuard) and Naive cannot be carried
// by a dialer chain; a Russian server needs no relay in Russia.
func chainable(profile Profile) (bool, string) {
	switch {
	case isRussianProfile(profile):
		return false, "выбран российский сервер"
	case profile.Protocol == "naive":
		return false, "Naive не поддерживает цепочку"
	case udpProtocols[profile.Protocol] || profile.Transport == "udp":
		return false, "сервер работает по UDP"
	}
	return true, ""
}

// pickRelay returns the relay to chain through, resolved to an IP before the
// tunnel comes up, or ok=false with the reason when there is none.
func pickRelay(ctx context.Context, profiles []Profile, options *relayOptions) (Profile, bool, string) {
	candidates := relayCandidates(profiles)
	if options.ProfileID != "" {
		for _, profile := range candidates {
			if profile.ID == options.ProfileID {
				candidates = []Profile{profile}
				break
			}
		}
		if len(candidates) != 1 || candidates[0].ID != options.ProfileID {
			// The chosen relay left the subscription: fall back to the fastest.
			candidates = relayCandidates(profiles)
		}
	}
	if len(candidates) == 0 {
		return Profile{}, false, "в подписке нет российских серверов"
	}
	relay, err := fastestReachable(ctx, candidates)
	if err != nil {
		return Profile{}, false, "российские серверы не отвечают"
	}
	resolved, _, err := resolveProfileEndpoint(ctx, relay)
	if err != nil {
		return Profile{}, false, "не удалось определить IP российского сервера"
	}
	return resolved, true, ""
}

// fastestReachable knocks on every candidate's port at once and returns the
// one that answered first.
func fastestReachable(parent context.Context, candidates []Profile) (Profile, error) {
	if len(candidates) == 1 {
		return candidates[0], nil
	}
	ctx, cancel := context.WithTimeout(parent, relayPingTimeout)
	defer cancel()
	var (
		mu      sync.Mutex
		best    Profile
		latency int64 = -1
		wait    sync.WaitGroup
	)
	for _, profile := range candidates {
		wait.Add(1)
		go func(profile Profile) {
			defer wait.Done()
			ms, err := directTCPLatency(ctx, profile)
			if err != nil {
				return
			}
			mu.Lock()
			if latency < 0 || ms < latency {
				best, latency = profile, ms
			}
			mu.Unlock()
		}(profile)
	}
	wait.Wait()
	if latency < 0 {
		return Profile{}, errors.New("no relay answered")
	}
	return best, nil
}

// chainThrough makes profile dial through relay. The profile's own dialer
// hop (fragmentation from Xray JSON) is dropped: what DPI sees now is the
// connection to the relay, so the relay keeps its own hop and fragmentation.
func chainThrough(profile, relay Profile, fragmentation bool) (Profile, error) {
	direct, _, err := withoutDialerChain(profile)
	if err != nil {
		return Profile{}, err
	}
	outbound, err := cloneObject(direct.outbound)
	if err != nil {
		return Profile{}, err
	}
	stream, _ := outbound["streamSettings"].(map[string]any)
	if stream == nil {
		stream = map[string]any{}
		outbound["streamSettings"] = stream
	}
	sockopt, _ := stream["sockopt"].(map[string]any)
	if sockopt == nil {
		sockopt = map[string]any{}
		stream["sockopt"] = sockopt
	}
	sockopt["dialerProxy"] = relayTag
	relayOutbound, err := configuredOutbound(relay, relayTag, fragmentation && len(relay.chain) == 0)
	if err != nil {
		return Profile{}, err
	}
	chain := []map[string]any{relayOutbound}
	for _, hop := range relay.chain {
		clone, err := cloneObject(hop)
		if err != nil {
			return Profile{}, err
		}
		chain = append(chain, clone)
	}
	direct.outbound = outbound
	direct.chain = chain
	return direct, nil
}
