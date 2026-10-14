package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"runtime"
	"strings"
)

const appVersion = "0.24.1"
const appUserAgent = "shadowvpn/" + appVersion

type deviceInfo struct {
	HWID      string `json:"hwid"`
	UserAgent string `json:"userAgent"`
	OS        string `json:"os"`
}

func hashedHWID(seed string) string {
	sum := sha256.Sum256([]byte("net.shadownet.shadowvpn:hwid:v1:" + strings.TrimSpace(seed)))
	return hex.EncodeToString(sum[:])
}

func currentDeviceInfo() deviceInfo {
	if hideHWID.Load() {
		return deviceInfo{UserAgent: appUserAgent, OS: runtime.GOOS}
	}
	return deviceInfo{HWID: deviceHWID(), UserAgent: appUserAgent, OS: runtime.GOOS}
}

func setClientIdentityHeaders(request *http.Request, info deviceInfo) {
	request.Header.Set("User-Agent", info.UserAgent)
	if info.HWID != "" {
		request.Header.Set("X-Hwid", info.HWID)
	}
	request.Header.Set("X-Device-Os", info.OS)
}
