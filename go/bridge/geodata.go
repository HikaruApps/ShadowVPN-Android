package bridge

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	xgeodata "github.com/xtls/xray-core/common/geodata"
	"google.golang.org/protobuf/proto"
)

const maxGeoDataBytes int64 = 64 << 20

type geoDataAsset struct {
	URL      string
	Filename string
	Title    string
}

func normalizeGeoDataURL(raw, title string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", nil
	}
	if len(value) > 2048 || strings.ContainsAny(value, "\r\n\t ") {
		return "", fmt.Errorf("%s: ссылка слишком длинная или содержит пробелы", title)
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() || !strings.EqualFold(parsed.Scheme, "https") || parsed.Hostname() == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", fmt.Errorf("%s: нужна обычная HTTPS-ссылка без логина и фрагмента", title)
	}
	parsed.Scheme = "https"
	return parsed.String(), nil
}

func geoDataAssets(routing routingOptions) []geoDataAsset {
	assets := make([]geoDataAsset, 0, 2)
	if routing.NeedsGeoIP {
		assets = append(assets, geoDataAsset{URL: routing.GeoIPURL, Filename: "geoip.dat", Title: "GeoIP"})
	}
	if routing.NeedsGeoSite {
		assets = append(assets, geoDataAsset{URL: routing.GeoSiteURL, Filename: "geosite.dat", Title: "GeoSite"})
	}
	return assets
}

func validateGeoDataAsset(data []byte, filename string) error {
	if len(data) == 0 {
		return errors.New("файл пуст")
	}
	switch filename {
	case "geoip.dat":
		var list xgeodata.GeoIPList
		if err := proto.Unmarshal(data, &list); err != nil {
			return errors.New("файл не является GeoIP protobuf")
		}
		entries := 0
		for _, item := range list.GetEntry() {
			if strings.TrimSpace(item.GetCode()) != "" && len(item.GetCidr()) > 0 {
				entries++
			}
		}
		if entries == 0 {
			return errors.New("GeoIP не содержит списков CIDR")
		}
	case "geosite.dat":
		var list xgeodata.GeoSiteList
		if err := proto.Unmarshal(data, &list); err != nil {
			return errors.New("файл не является GeoSite protobuf")
		}
		entries := 0
		for _, item := range list.GetEntry() {
			if strings.TrimSpace(item.GetCode()) != "" && len(item.GetDomain()) > 0 {
				entries++
			}
		}
		if entries == 0 {
			return errors.New("GeoSite не содержит доменных списков")
		}
	default:
		return errors.New("неизвестный тип GeoData")
	}
	return nil
}

func validateGeoDataFile(path, filename string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return validateGeoDataAsset(data, filename)
}

func downloadGeoDataAsset(ctx context.Context, client *http.Client, asset geoDataAsset, directory string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return fmt.Errorf("%s: не удалось создать запрос", asset.Title)
	}
	request.Header.Set("User-Agent", appUserAgent)
	request.Header.Set("Accept", "application/octet-stream, application/protobuf;q=0.9, */*;q=0.1")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("%s: источник недоступен", asset.Title)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: сервер вернул HTTP %d", asset.Title, response.StatusCode)
	}
	if response.ContentLength > maxGeoDataBytes {
		return fmt.Errorf("%s: файл больше 64 МБ", asset.Title)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGeoDataBytes+1))
	if err != nil {
		return fmt.Errorf("%s: загрузка прервана", asset.Title)
	}
	if int64(len(data)) > maxGeoDataBytes {
		return fmt.Errorf("%s: файл больше 64 МБ", asset.Title)
	}
	if err := validateGeoDataAsset(data, asset.Filename); err != nil {
		return fmt.Errorf("%s: %w", asset.Title, err)
	}
	if err := os.WriteFile(filepath.Join(directory, asset.Filename), data, 0o600); err != nil {
		return fmt.Errorf("%s: не удалось сохранить файл", asset.Title)
	}
	return nil
}

func prepareGeoData(ctx context.Context, routing *routingOptions) error {
	assets := geoDataAssets(*routing)
	if len(assets) == 0 {
		return nil
	}
	cacheRoot, err := os.UserCacheDir()
	if err != nil {
		return errors.New("не удалось открыть системный каталог кэша")
	}
	sum := sha256.Sum256([]byte(routing.GeoIPURL + "\x00" + routing.GeoSiteURL))
	cacheKey := hex.EncodeToString(sum[:12])
	parent := filepath.Join(cacheRoot, "ShadowVPN", "geodata")
	assetDir := filepath.Join(parent, cacheKey)
	validCache := true
	for _, asset := range assets {
		if err := validateGeoDataFile(filepath.Join(assetDir, asset.Filename), asset.Filename); err != nil {
			validCache = false
			break
		}
	}
	if validCache {
		routing.AssetDir = assetDir
		return nil
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return errors.New("не удалось создать каталог GeoData")
	}
	staging, err := os.MkdirTemp(parent, cacheKey+"-tmp-")
	if err != nil {
		return errors.New("не удалось подготовить каталог GeoData")
	}
	defer os.RemoveAll(staging)
	client := &http.Client{
		Timeout: 40 * time.Second,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if len(via) >= 4 {
				return errors.New("слишком много перенаправлений")
			}
			_, err := normalizeGeoDataURL(request.URL.String(), "GeoData")
			return err
		},
	}
	for _, asset := range assets {
		if err := downloadGeoDataAsset(ctx, client, asset, staging); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(assetDir); err != nil {
		return errors.New("не удалось заменить устаревший кэш GeoData")
	}
	if err := os.Rename(staging, assetDir); err != nil {
		return errors.New("не удалось активировать GeoData")
	}
	routing.AssetDir = assetDir
	return nil
}
