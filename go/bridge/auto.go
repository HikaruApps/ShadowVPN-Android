package bridge

import (
	"context"
	"errors"
	"net"
	"strings"
)

const (
	autoProfileID     = "000000000000000000000000"
	autoNoRUProfileID = "000000000000000000000001"
)

const autoProbeInterval = "30s"

type proxyEndpoint struct {
	Address string
	Port    int
}

func autoProfile() Profile {
	return Profile{
		ID:        autoProfileID,
		Name:      "Авто",
		Protocol:  "auto",
		Transport: "tcp",
		Auto:      true,
	}
}

func autoNoRUProfile() Profile {
	return Profile{
		ID:        autoNoRUProfileID,
		Name:      "Авто без РФ",
		Protocol:  "auto",
		Transport: "tcp",
		Auto:      true,
	}
}

func isAutoProfileID(id string) bool {
	return id == autoProfileID || id == autoNoRUProfileID
}

func isRussianProfile(profile Profile) bool {
	name := strings.ToLower(strings.TrimSpace(profile.Name))
	if strings.Contains(name, "🇷🇺") {
		return true
	}
	for _, field := range strings.Fields(name) {
		field = strings.Trim(field, "[](){}|/\\:;,.—–-_🏴🏳️")
		switch field {
		case "ru", "rus", "russia", "россия", "рф":
			return true
		}
	}
	return false
}

func withoutRussianProfiles(profiles []Profile) []Profile {
	result := make([]Profile, 0, len(profiles))
	for _, profile := range profiles {
		if !isRussianProfile(profile) {
			result = append(result, profile)
		}
	}
	return result
}

func profilesForRenderer(profiles []Profile) []Profile {
	result := make([]Profile, 0, len(profiles)+2)
	result = append(result, autoProfile(), autoNoRUProfile())
	return append(result, profiles...)
}

func fastestProfile(profiles []Profile, results []PingResult) (int, PingResult, bool) {
	byID := make(map[string]PingResult, len(results))
	for _, result := range results {
		byID[result.ID] = result
	}
	bestIndex := -1
	best := PingResult{}
	for index, profile := range profiles {
		result, exists := byID[profile.ID]
		if !exists || !result.Available || result.LatencyMS < 1 {
			continue
		}
		if bestIndex == -1 || result.LatencyMS < best.LatencyMS {
			bestIndex = index
			best = result
		}
	}
	return bestIndex, best, bestIndex >= 0
}

func pingProfilesWithAuto(parent context.Context, profiles []Profile) []PingResult {
	return pingProfilesWithAutoMethod(parent, profiles, "tcp")
}

func pingProfilesWithAutoMethod(parent context.Context, profiles []Profile, method string) []PingResult {
	return pingProfilesWithAutoMethodOptions(parent, profiles, method, pingOptions{})
}

func pingProfilesWithAutoMethodOptions(parent context.Context, profiles []Profile, method string, options pingOptions) []PingResult {
	results := pingProfilesWithMethodOptions(parent, profiles, method, options)
	auto := PingResult{ID: autoProfileID}
	if _, best, ok := fastestProfile(profiles, results); ok {
		auto.Available = true
		auto.LatencyMS = best.LatencyMS
	}
	autoNoRU := PingResult{ID: autoNoRUProfileID}
	if _, best, ok := fastestProfile(withoutRussianProfiles(profiles), results); ok {
		autoNoRU.Available = true
		autoNoRU.LatencyMS = best.LatencyMS
	}
	return append([]PingResult{auto, autoNoRU}, results...)
}

// prepareAutoProfiles puts the fastest reachable profile first, then resolves
// every candidate before TUN takes over system routing. Xray can therefore
// observe all Auto candidates without creating a DNS bootstrap loop.
func prepareAutoProfiles(ctx context.Context, profiles []Profile, results []PingResult) ([]Profile, []proxyEndpoint, []string, error) {
	selectedIndex, _, ok := fastestProfile(profiles, results)
	if !ok {
		return nil, nil, nil, errors.New("Авто не нашёл доступных серверов. Запустите проверку задержки")
	}

	byID := make(map[string]PingResult, len(results))
	for _, result := range results {
		byID[result.ID] = result
	}
	order := make([]int, 0, len(profiles))
	order = append(order, selectedIndex)
	for index := range profiles {
		if index != selectedIndex {
			order = append(order, index)
		}
	}

	prepared := make([]Profile, 0, len(profiles))
	endpoints := make([]proxyEndpoint, 0, len(profiles))
	warnings := make([]string, 0)
	seenEndpoints := make(map[proxyEndpoint]struct{}, len(profiles))
	for position, index := range order {
		profile := profiles[index]
		resolvedAddress := strings.TrimSpace(byID[profile.ID].ResolvedAddress)
		var resolved Profile
		var err error
		if net.ParseIP(resolvedAddress) != nil {
			originalHost := strings.TrimSpace(profile.Address)
			if net.ParseIP(originalHost) != nil {
				originalHost = ""
			}
			resolved, err = profileWithEndpoint(profile, resolvedAddress, originalHost)
		} else {
			resolved, _, err = resolveProfileEndpoint(ctx, profile)
		}
		if err != nil {
			if position == 0 {
				return nil, nil, warnings, err
			}
			warnings = append(warnings, profile.Name)
			continue
		}
		prepared = append(prepared, resolved)
		endpoint := proxyEndpoint{Address: resolved.Address, Port: resolved.Port}
		if _, exists := seenEndpoints[endpoint]; !exists {
			seenEndpoints[endpoint] = struct{}{}
			endpoints = append(endpoints, endpoint)
		}
	}
	return prepared, endpoints, warnings, nil
}
