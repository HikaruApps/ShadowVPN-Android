package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The app checks the servers one by one, so a latency shows up as soon as its
// host answers instead of waiting for the whole list. These helpers give the
// Android side the same picture the core has: which servers to check, how
// balancer groups combine them and which servers Auto leaves out.

type pingPlanServer struct {
	ID       string `json:"id"`
	Protocol string `json:"protocol"`
	Russian  bool   `json:"russian"`
}

type pingPlanGroup struct {
	ID      string   `json:"id"`
	Members []string `json:"members"`
}

type pingPlanData struct {
	Servers    []pingPlanServer `json:"servers"`
	Groups     []pingPlanGroup  `json:"groups"`
	AutoID     string           `json:"autoId"`
	AutoNoRUID string           `json:"autoNoRUId"`
}

// PingPlan returns the servers to check and how the results combine. It is
// the same set PingProfiles uses, split so the app can apply every result as
// soon as it arrives.
func PingPlan() (string, error) {
	mobile.Lock()
	if mobile.importing {
		mobile.Unlock()
		return "", errors.New("Проверка недоступна во время обновления подписки")
	}
	profiles := append([]Profile(nil), mobile.profiles...)
	mobile.Unlock()

	flat, _ := flattenGroups(profiles)
	plan := pingPlanData{
		Servers:    make([]pingPlanServer, 0, len(flat)),
		Groups:     make([]pingPlanGroup, 0),
		AutoID:     autoProfileID,
		AutoNoRUID: autoNoRUProfileID,
	}
	for _, profile := range flat {
		plan.Servers = append(plan.Servers, pingPlanServer{
			ID: profile.ID, Protocol: profile.Protocol, Russian: isRussianProfile(profile),
		})
	}
	for _, profile := range profiles {
		if len(profile.members) == 0 {
			continue
		}
		group := pingPlanGroup{ID: profile.ID, Members: make([]string, 0, len(profile.members))}
		for _, member := range profile.members {
			group.Members = append(group.Members, member.ID)
		}
		plan.Groups = append(plan.Groups, group)
	}
	result, err := json.Marshal(plan)
	return string(result), err
}

// PingProfile checks one server and returns its result as JSON. A failed
// check is a normal result, not an error: only a wrong request is.
func PingProfile(profileID, method string) (string, error) {
	mobile.Lock()
	if mobile.importing {
		mobile.Unlock()
		return "", errors.New("Проверка недоступна во время обновления подписки")
	}
	profiles := append([]Profile(nil), mobile.profiles...)
	fragmentation := mobile.options.Fragmentation
	mobile.Unlock()

	flat, _ := flattenGroups(profiles)
	target := Profile{}
	found := false
	for _, profile := range flat {
		if profile.ID == profileID {
			target = profile
			found = true
			break
		}
	}
	if !found {
		return "", errors.New("Сервер не найден в подписке")
	}

	switch method {
	case "", "tcp", "real", "head", "get", "auto":
	default:
		return "", errors.New("Неизвестный метод проверки задержки")
	}
	switch method {
	case "", "tcp":
		method = "tcp"
	case "auto":
		// A UDP protocol has no TCP port worth knocking on.
		method = "tcp"
		if target.Protocol == "hysteria" {
			method = "real"
		}
	default:
		// The old HTTP methods opened a listener on localhost and created
		// Xray instances outside the dialer guard; the real check covers
		// what they were for.
		method = "real"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	var latency int64
	var err error
	direct := false
	switch method {
	case "real":
		latency, err = realPingProfile(ctx, target, fragmentation)
		// The check through the server failed, but the server may still
		// answer over the phone's network: under mobile whitelists that is
		// what tells the servers that get through from the rest. Shown apart
		// from a passed check. UDP protocols have no TCP port to knock on.
		if err != nil && !udpProtocols[target.Protocol] {
			if tcp, tcpErr := directTCPLatency(ctx, target); tcpErr == nil {
				latency, err, direct = tcp, nil, true
			}
		}
	default:
		latency, _, err = tcpPingEndpoint(ctx, target.address, target.port)
	}

	result, marshalErr := json.Marshal(streamedPingResult{
		ID: target.ID, LatencyMS: latency, Available: err == nil, Direct: direct,
	})
	if marshalErr != nil {
		return "", marshalErr
	}
	return string(result), nil
}

// streamedPingResult is PingResult plus Direct: the server's port answered
// over the phone's network, but the check through the server did not pass.
type streamedPingResult struct {
	ID        string `json:"id"`
	LatencyMS int64  `json:"latencyMs,omitempty"`
	Available bool   `json:"available"`
	Direct    bool   `json:"direct,omitempty"`
}

var udpProtocols = map[string]bool{"hysteria": true, "hysteria2": true, "tuic": true, "wireguard": true}
