package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
)

const profileCacheFile = "profiles.json"

type cachedProfile struct {
	Name           string           `json:"name"`
	Transport      string           `json:"transport"`
	Format         string           `json:"format,omitempty"`
	SourceIndex    int              `json:"sourceIndex"`
	Outbound       map[string]any   `json:"outbound"`
	Chain          []map[string]any `json:"chain,omitempty"`
	Rules          []map[string]any `json:"rules,omitempty"`
	DomainStrategy string           `json:"domainStrategy,omitempty"`
}

// writeFileAtomic replaces a file without leaving a truncated copy behind if
// the process dies mid-write.
func writeFileAtomic(path string, data []byte) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if _, err = temporary.Write(data); err == nil {
		err = temporary.Chmod(0o600)
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}

func saveProfileCache(directory string, profiles []Profile) error {
	if directory == "" {
		return nil
	}
	cached := make([]cachedProfile, 0, len(profiles))
	for _, profile := range profiles {
		cached = append(cached, cachedProfile{
			Name: profile.Name, Transport: profile.Transport, Format: profile.Format, SourceIndex: profile.SourceIndex,
			Outbound: profile.outbound, Chain: profile.chain,
			Rules: profile.rules, DomainStrategy: profile.domainStrategy,
		})
	}
	data, err := json.Marshal(cached)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(directory, profileCacheFile), data)
}

func loadProfileCache(directory string) ([]Profile, error) {
	if directory == "" {
		return nil, nil
	}
	data, err := os.ReadFile(filepath.Join(directory, profileCacheFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var cached []cachedProfile
	if err := json.Unmarshal(data, &cached); err != nil {
		return nil, errors.New("Кэш серверов повреждён")
	}
	profiles := make([]Profile, 0, len(cached))
	for _, item := range cached {
		if item.Outbound == nil {
			continue
		}
		profile := newProfile(item.Name, item.Outbound)
		if item.Transport != "" {
			profile.Transport = item.Transport
		}
		profile.SourceIndex = item.SourceIndex
		profile.Format = item.Format
		profile.chain = item.Chain
		profile.rules = item.Rules
		profile.domainStrategy = item.DomainStrategy
		profiles = append(profiles, profile)
	}
	return profiles, nil
}

const geoDataMaxAge = 7 * 24 * time.Hour

type geoDataEntry struct {
	URL     string    `json:"url"`
	Size    int64     `json:"size"`
	Updated time.Time `json:"updated"`
	Codes   []string  `json:"codes"`
}

// geoDataInfo describes prepared GeoData: its directory and the list codes
// each file contains (upper case, keyed by file name).
type geoDataInfo struct {
	Directory string
	Codes     map[string]map[string]bool
}

func (info geoDataInfo) has(file, code string) bool {
	codes, ok := info.Codes[file]
	return ok && codes[strings.ToUpper(code)]
}

// scanGeoData validates a geoip.dat/geosite.dat protobuf and lists its codes
// by walking the wire format, without materializing every CIDR and domain.
func scanGeoData(data []byte) ([]string, error) {
	codes := []string{}
	for len(data) > 0 {
		number, kind, length := protowire.ConsumeTag(data)
		if length < 0 {
			return nil, errors.New("файл не является GeoData protobuf")
		}
		data = data[length:]
		if number != 1 || kind != protowire.BytesType {
			length = protowire.ConsumeFieldValue(number, kind, data)
			if length < 0 {
				return nil, errors.New("файл не является GeoData protobuf")
			}
			data = data[length:]
			continue
		}
		entry, length := protowire.ConsumeBytes(data)
		if length < 0 {
			return nil, errors.New("файл не является GeoData protobuf")
		}
		data = data[length:]
		code, hasItems := "", false
		for len(entry) > 0 {
			field, fieldKind, n := protowire.ConsumeTag(entry)
			if n < 0 {
				return nil, errors.New("файл не является GeoData protobuf")
			}
			entry = entry[n:]
			if field == 1 && fieldKind == protowire.BytesType {
				value, m := protowire.ConsumeBytes(entry)
				if m < 0 {
					return nil, errors.New("файл не является GeoData protobuf")
				}
				code = string(value)
				entry = entry[m:]
				continue
			}
			hasItems = hasItems || field == 2
			n = protowire.ConsumeFieldValue(field, fieldKind, entry)
			if n < 0 {
				return nil, errors.New("файл не является GeoData protobuf")
			}
			entry = entry[n:]
		}
		if code = strings.TrimSpace(code); code != "" && hasItems {
			codes = append(codes, strings.ToUpper(code))
		}
	}
	if len(codes) == 0 {
		return nil, errors.New("файл не содержит списков")
	}
	return codes, nil
}

func downloadGeoData(ctx context.Context, client *http.Client, asset geoDataAsset, path string) ([]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: не удалось создать запрос", asset.Title)
	}
	request.Header.Set("User-Agent", appUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: источник недоступен", asset.Title)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: сервер вернул HTTP %d", asset.Title, response.StatusCode)
	}
	if response.ContentLength > maxGeoDataBytes {
		return nil, fmt.Errorf("%s: файл больше 64 МБ", asset.Title)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGeoDataBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%s: загрузка прервана", asset.Title)
	}
	if int64(len(data)) > maxGeoDataBytes {
		return nil, fmt.Errorf("%s: файл больше 64 МБ", asset.Title)
	}
	codes, err := scanGeoData(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", asset.Title, err)
	}
	if err := writeFileAtomic(path, data); err != nil {
		return nil, fmt.Errorf("%s: не удалось сохранить файл", asset.Title)
	}
	return codes, nil
}

// ensureGeoData downloads the GeoIP/GeoSite files that routing rules need
// into <data>/geodata. Files are parsed once, on download, and their codes
// recorded; connecting afterwards only compares sizes. A file older than a
// week is refreshed, keeping the old copy if the refresh fails.
func ensureGeoData(ctx context.Context, dataDir string, needIP, needSite bool, geoIPURL, geoSiteURL string) (geoDataInfo, error) {
	info := geoDataInfo{Codes: map[string]map[string]bool{}}
	if dataDir == "" {
		return info, errors.New("Каталог данных не задан")
	}
	info.Directory = filepath.Join(dataDir, "geodata")
	if err := os.MkdirAll(info.Directory, 0o700); err != nil {
		return info, errors.New("Не удалось создать каталог GeoData")
	}
	metaPath := filepath.Join(info.Directory, "meta.json")
	meta := map[string]geoDataEntry{}
	if data, err := os.ReadFile(metaPath); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	var assets []geoDataAsset
	if needIP {
		assets = append(assets, geoDataAsset{URL: geoIPURL, Filename: "geoip.dat", Title: "GeoIP"})
	}
	if needSite {
		assets = append(assets, geoDataAsset{URL: geoSiteURL, Filename: "geosite.dat", Title: "GeoSite"})
	}
	client := &http.Client{
		Timeout: 60 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("слишком много перенаправлений")
			}
			_, err := normalizeGeoDataURL(request.URL.String(), "GeoData")
			return err
		},
	}
	changed := false
	for _, asset := range assets {
		path := filepath.Join(info.Directory, asset.Filename)
		entry, known := meta[asset.Filename]
		stat, statErr := os.Stat(path)
		usable := known && statErr == nil && entry.URL == asset.URL && entry.Size == stat.Size() && len(entry.Codes) > 0
		if !usable || time.Since(entry.Updated) >= geoDataMaxAge {
			codes, err := downloadGeoData(ctx, client, asset, path)
			if err != nil && !usable {
				return info, err
			}
			if err == nil {
				stat, _ = os.Stat(path)
				entry = geoDataEntry{URL: asset.URL, Size: stat.Size(), Updated: time.Now(), Codes: codes}
				meta[asset.Filename] = entry
				changed = true
			}
		}
		codes := make(map[string]bool, len(entry.Codes))
		for _, code := range entry.Codes {
			codes[code] = true
		}
		info.Codes[asset.Filename] = codes
	}
	if changed {
		data, _ := json.Marshal(meta)
		_ = writeFileAtomic(metaPath, data)
	}
	return info, nil
}
