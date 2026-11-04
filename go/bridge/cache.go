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
	"sync"
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
	Members        []cachedProfile  `json:"members,omitempty"`
}

func toCached(profile Profile) cachedProfile {
	cached := cachedProfile{
		Name: profile.Name, Transport: profile.Transport, Format: profile.Format, SourceIndex: profile.SourceIndex,
		Outbound: profile.outbound, Chain: profile.chain,
		Rules: profile.rules, DomainStrategy: profile.domainStrategy,
	}
	for _, member := range profile.members {
		cached.Members = append(cached.Members, toCached(member))
	}
	return cached
}

func fromCached(item cachedProfile) (Profile, bool) {
	if len(item.Members) > 0 {
		members := make([]Profile, 0, len(item.Members))
		for _, cachedMember := range item.Members {
			if member, ok := fromCached(cachedMember); ok {
				members = append(members, member)
			}
		}
		if len(members) == 0 {
			return Profile{}, false
		}
		group := newGroupProfile(item.Name, members)
		group.SourceIndex = item.SourceIndex
		group.rules = item.Rules
		group.domainStrategy = item.DomainStrategy
		return group, true
	}
	if item.Outbound == nil {
		return Profile{}, false
	}
	profile := newProfile(item.Name, item.Outbound)
	if item.Transport != "" {
		profile.Transport = item.Transport
	}
	profile.Format = item.Format
	profile.SourceIndex = item.SourceIndex
	profile.chain = item.Chain
	profile.rules = item.Rules
	profile.domainStrategy = item.DomainStrategy
	return profile, true
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
		cached = append(cached, toCached(profile))
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
		if profile, ok := fromCached(item); ok {
			profiles = append(profiles, profile)
		}
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

// GeoData downloads run on their own, not on the caller's context: a list
// that takes longer than a connection is willing to wait still lands on disk
// for the next one. One download per file runs at a time, and a failed one
// is not retried for a while, so a slow or blocked source does not hold up
// every connection.
const (
	geoDownloadTimeout = 5 * time.Minute
	geoRetryAfter      = 5 * time.Minute
)

type geoDownload struct {
	done  chan struct{}
	entry geoDataEntry
	err   error
}

var geoDownloads = struct {
	sync.Mutex
	active map[string]*geoDownload
	failed map[string]time.Time
	meta   sync.Mutex // guards meta.json read-modify-write
}{active: map[string]*geoDownload{}, failed: map[string]time.Time{}}

var geoDataClient = &http.Client{
	// No overall timeout: an 18 MB list on a slow line may take minutes, and
	// geoDownloadTimeout bounds it instead.
	CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 4 {
			return errors.New("слишком много перенаправлений")
		}
		_, err := normalizeGeoDataURL(request.URL.String(), "GeoData")
		return err
	},
}

// startGeoDownload downloads asset into directory in the background, or
// joins the download of that file already running. It returns nil when the
// last attempt failed less than geoRetryAfter ago.
func startGeoDownload(directory string, asset geoDataAsset) *geoDownload {
	path := filepath.Join(directory, asset.Filename)
	geoDownloads.Lock()
	defer geoDownloads.Unlock()
	if download := geoDownloads.active[path]; download != nil {
		return download
	}
	if failed, ok := geoDownloads.failed[path]; ok && time.Since(failed) < geoRetryAfter {
		return nil
	}
	download := &geoDownload{done: make(chan struct{})}
	geoDownloads.active[path] = download
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), geoDownloadTimeout)
		codes, err := downloadGeoData(ctx, geoDataClient, asset, path)
		cancel()
		if err == nil {
			if stat, statErr := os.Stat(path); statErr == nil {
				download.entry = geoDataEntry{URL: asset.URL, Size: stat.Size(), Updated: time.Now(), Codes: codes}
				recordGeoData(directory, asset.Filename, download.entry)
			} else {
				err = statErr
			}
		}
		download.err = err
		geoDownloads.Lock()
		delete(geoDownloads.active, path)
		if err != nil {
			geoDownloads.failed[path] = time.Now()
		} else {
			delete(geoDownloads.failed, path)
		}
		geoDownloads.Unlock()
		close(download.done)
	}()
	return download
}

func readGeoMeta(directory string) map[string]geoDataEntry {
	meta := map[string]geoDataEntry{}
	if data, err := os.ReadFile(filepath.Join(directory, "meta.json")); err == nil {
		_ = json.Unmarshal(data, &meta)
	}
	return meta
}

func recordGeoData(directory, filename string, entry geoDataEntry) {
	geoDownloads.meta.Lock()
	defer geoDownloads.meta.Unlock()
	meta := readGeoMeta(directory)
	meta[filename] = entry
	data, _ := json.Marshal(meta)
	_ = writeFileAtomic(filepath.Join(directory, "meta.json"), data)
}

// ensureGeoData prepares the GeoIP/GeoSite files that routing rules need in
// <data>/geodata. Files are parsed once, on download, and their codes
// recorded; connecting afterwards only compares sizes. A file older than a
// week is refreshed in the background while the current copy is used. A
// missing file is waited for only as long as ctx allows; the download goes
// on afterwards for the next connection.
func ensureGeoData(ctx context.Context, dataDir string, needIP, needSite bool, geoIPURL, geoSiteURL string) (geoDataInfo, error) {
	info := geoDataInfo{Codes: map[string]map[string]bool{}}
	if dataDir == "" {
		return info, errors.New("Каталог данных не задан")
	}
	info.Directory = filepath.Join(dataDir, "geodata")
	if err := os.MkdirAll(info.Directory, 0o700); err != nil {
		return info, errors.New("Не удалось создать каталог GeoData")
	}
	geoDownloads.meta.Lock()
	meta := readGeoMeta(info.Directory)
	geoDownloads.meta.Unlock()
	var assets []geoDataAsset
	if needIP {
		assets = append(assets, geoDataAsset{URL: geoIPURL, Filename: "geoip.dat", Title: "GeoIP"})
	}
	if needSite {
		assets = append(assets, geoDataAsset{URL: geoSiteURL, Filename: "geosite.dat", Title: "GeoSite"})
	}
	// Start every missing download first, so both files load in parallel.
	pending := map[string]*geoDownload{}
	for _, asset := range assets {
		entry, known := meta[asset.Filename]
		stat, statErr := os.Stat(filepath.Join(info.Directory, asset.Filename))
		usable := known && statErr == nil && entry.URL == asset.URL && entry.Size == stat.Size() && len(entry.Codes) > 0
		switch {
		case !usable:
			download := startGeoDownload(info.Directory, asset)
			if download == nil {
				return info, fmt.Errorf("%s: источник недоступен, повторим позже", asset.Title)
			}
			pending[asset.Filename] = download
		case time.Since(entry.Updated) >= geoDataMaxAge:
			_ = startGeoDownload(info.Directory, asset)
		}
	}
	for _, asset := range assets {
		entry := meta[asset.Filename]
		if download := pending[asset.Filename]; download != nil {
			select {
			case <-download.done:
			case <-ctx.Done():
				return info, fmt.Errorf("%s: загрузка идёт слишком долго, продолжится в фоне", asset.Title)
			}
			if download.err != nil {
				return info, download.err
			}
			entry = download.entry
		}
		codes := make(map[string]bool, len(entry.Codes))
		for _, code := range entry.Codes {
			codes[code] = true
		}
		info.Codes[asset.Filename] = codes
	}
	return info, nil
}
