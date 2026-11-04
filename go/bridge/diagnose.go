package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"
)

// diagnoseStep is one line of the connection check the app shows.
type diagnoseStep struct {
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

type diagnoseResult struct {
	DNS    diagnoseStep `json:"dns"`
	Server diagnoseStep `json:"server"`
}

// Diagnose checks the selected server the way a connection would reach it:
// is its name resolvable, and does a real request pass through it. Auto and
// balancers check their best few candidates. The result is JSON; a failed
// step is a normal result with a human reason, not an error.
func Diagnose(profileID string) (string, error) {
	mobile.Lock()
	profiles := append([]Profile(nil), mobile.profiles...)
	options := mobile.options
	mobile.Unlock()

	candidates, err := diagnoseCandidates(profiles, options, profileID)
	if err != nil {
		return "", err
	}
	var result diagnoseResult
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	// DNS: the server's own name, resolved like Prepare resolves it.
	resolvedNames := 0
	var dnsErr error
	for _, profile := range candidates {
		resolveCtx, done := context.WithTimeout(ctx, 6*time.Second)
		_, info, err := resolveProfileEndpoint(resolveCtx, profile)
		done()
		if err != nil {
			dnsErr = err
			continue
		}
		resolvedNames++
		if result.DNS.Detail == "" {
			if net.ParseIP(info.OriginalAddress) != nil {
				result.DNS.Detail = "Найден · задан IP-адресом " + maskIP(info.SelectedAddress)
			} else {
				result.DNS.Detail = "Найден · " + info.OriginalAddress + " → " + maskIP(info.SelectedAddress)
			}
		}
	}
	result.DNS.OK = resolvedNames > 0
	if !result.DNS.OK {
		result.DNS.Detail = "Не найден: не удалось узнать IP сервера. Проверьте DNS провайдера или смените сеть"
		if dnsErr != nil {
			result.DNS.Detail += " (" + maskIPs(shortError(dnsErr)) + ")"
		}
		result.Server.Detail = "Пропущено: сервер не найден"
		return marshalDiagnose(result)
	}

	// Server: a real request through it, as the server list checks it.
	type answer struct {
		name    string
		latency int64
		err     error
	}
	answers := make(chan answer, len(candidates))
	for _, profile := range candidates {
		go func(profile Profile) {
			latency, err := realPingProfile(ctx, profile, options.Fragmentation)
			answers <- answer{profile.Name, latency, err}
		}(profile)
	}
	var working []answer
	var firstErr error
	for range candidates {
		item := <-answers
		if item.err == nil {
			working = append(working, item)
		} else if firstErr == nil {
			firstErr = item.err
		}
	}
	if len(working) > 0 {
		sort.Slice(working, func(a, b int) bool { return working[a].latency < working[b].latency })
		result.Server.OK = true
		// The sheet already names the server; with several (Auto, a
		// balancer) say which one answered best.
		if len(candidates) > 1 {
			result.Server.Detail = fmt.Sprintf("Доступен · быстрее всех «%s», %d мс · работают %d из %d",
				working[0].name, working[0].latency, len(working), len(candidates))
		} else {
			result.Server.Detail = fmt.Sprintf("Доступен · отвечает за %d мс", working[0].latency)
		}
	} else {
		result.Server.Detail = "Недоступен: " + maskIPs(explainProbeError(firstErr))
	}
	return marshalDiagnose(result)
}

func diagnoseCandidates(profiles []Profile, options mobileOptions, profileID string) ([]Profile, error) {
	if profileID == "" {
		return nil, errors.New("Сервер не выбран")
	}
	var candidates []Profile
	if isAutoProfileID(profileID) {
		candidates = autoCandidates(profiles, options, profileID)
	} else {
		for _, profile := range profiles {
			if profile.ID != profileID {
				continue
			}
			if len(profile.members) > 0 {
				candidates = profilesWithoutProtocol(profile.members, "naive")
			} else {
				candidates = []Profile{profile}
			}
		}
		if candidates == nil {
			flat, _ := flattenGroups(profiles)
			for _, profile := range flat {
				if profile.ID == profileID {
					candidates = []Profile{profile}
				}
			}
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("Сервер отсутствует в подписке. Обновите её")
	}
	if len(candidates) > 4 {
		candidates = candidates[:4]
	}
	return candidates, nil
}

// explainProbeError turns a probe failure into something a user can act on.
func explainProbeError(err error) string {
	if err == nil {
		return "нет ответа"
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "deadline") || strings.Contains(text, "timeout"):
		return "нет ответа за 8 секунд. Сервер заблокирован в этой сети, перегружен или не принимает ваш ключ"
	case strings.Contains(text, "refused"):
		return "сервер отклонил подключение. Возможно, он выключен"
	case strings.Contains(text, "eof") || strings.Contains(text, "reset") || strings.Contains(text, "closed pipe"):
		return "сервер обрывает соединение. Обычно это неверный ключ или истёкшая подписка"
	case strings.Contains(text, "certificate") || strings.Contains(text, "tls"):
		return "ошибка TLS. Проверьте настройки сервера или обновите подписку"
	}
	return shortError(err)
}

func shortError(err error) string {
	text := err.Error()
	if len(text) > 120 {
		text = text[:120] + "…"
	}
	return text
}

func marshalDiagnose(result diagnoseResult) (string, error) {
	encoded, err := json.Marshal(result)
	return string(encoded), err
}

var ipv4Pattern = regexp.MustCompile(`\b(\d{1,3}\.\d{1,3})\.\d{1,3}\.\d{1,3}\b`)

// maskIP hides the host part of an address shown to the user: the last two
// octets of IPv4, everything after the first two groups of IPv6.
func maskIP(address string) string {
	ip := net.ParseIP(address)
	if ip == nil {
		return maskIPs(address)
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.*.*", v4[0], v4[1])
	}
	groups := strings.SplitN(ip.String(), ":", 3)
	if len(groups) < 3 {
		return "*"
	}
	return groups[0] + ":" + groups[1] + ":*:*"
}

// maskIPs masks the IPv4 addresses inside a message, e.g. a dial error.
func maskIPs(text string) string {
	return ipv4Pattern.ReplaceAllString(text, "$1.*.*")
}
