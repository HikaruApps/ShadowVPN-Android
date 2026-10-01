package net.shadownet.shadowvpn.android;

import android.Manifest;
import android.annotation.SuppressLint;
import android.app.Activity;
import android.content.ClipData;
import android.content.ClipboardManager;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageManager;
import android.content.pm.ResolveInfo;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.drawable.Drawable;
import android.net.TrafficStats;
import android.net.Uri;
import android.net.VpnService;
import android.os.Build;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.os.PowerManager;
import android.os.SystemClock;
import android.provider.Settings;
import android.webkit.JavascriptInterface;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Toast;
import android.util.Base64;

import net.shadownet.shadowvpn.core.bridge.Bridge;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.ByteArrayOutputStream;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.InetSocketAddress;
import java.net.Proxy;
import java.net.URL;
import java.util.ArrayList;
import java.util.Comparator;
import java.util.HashSet;
import java.util.HashMap;
import java.util.List;
import java.util.Locale;
import java.util.Map;
import java.util.Set;
import java.util.concurrent.ConcurrentHashMap;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public final class MainActivity extends Activity {
    private static final int VPN_REQUEST = 100;
    private static final String UI_URL = "file:///android_asset/shadowvpn/index.html";

    private final ExecutorService worker = Executors.newSingleThreadExecutor();
    private final ExecutorService appWorker = Executors.newSingleThreadExecutor();
    private final Handler ui = new Handler(Looper.getMainLooper());
    private final List<ServerItem> servers = new ArrayList<>();
    private final List<SubscriptionInfo> subscriptionInfo = new ArrayList<>();
    private final Map<String, PingData> pingResults = new HashMap<>();
    private final Map<String, String> appIconCache = new ConcurrentHashMap<>();
    private final Set<String> pendingAppIcons = ConcurrentHashMap.newKeySet();
    private volatile String installedAppsCache = "";

    private SharedPreferences preferences;
    private WebView webView;
    private boolean webReady;
    private boolean importRunning;
    private boolean pingRunning;
    private boolean wasConnected;
    private String selectedId = "";
    private String doh = "https://1.1.1.1/dns-query";
    private String deviceHwid = "Недоступно";
    private String publicIp = "";
    private String uiMessage = "";
    private int ipRequestMode = -1;
    private long connectedAt;
    private long trafficStartedRx;
    private long trafficStartedTx;
    private long trafficLastRx;
    private long trafficLastTx;
    private long trafficLastAt;
    private String downloadSpeed = "0 Б/с";
    private String uploadSpeed = "0 Б/с";
    private String downloadTotal = "0 Б";
    private String uploadTotal = "0 Б";

    private final Runnable statusUpdater = new Runnable() {
        @Override public void run() {
            updateRuntimeState();
            pushRuntimeSnapshot();
            ui.postDelayed(this, 500);
        }
    };

    @Override protected void onCreate(Bundle state) {
        super.onCreate(state);
        preferences = getSharedPreferences("shadowvpn", MODE_PRIVATE);
        migrateSubscriptionSlots();
        restoreSubscriptionInfo();
        selectedId = preferences.getString("selected_id", "");
        doh = preferences.getString("doh", doh);
        try {
            Bridge.setDeviceSeed(Settings.Secure.getString(
                    getContentResolver(), Settings.Secure.ANDROID_ID));
            deviceHwid = Bridge.deviceHWID();
        } catch (Exception ignored) { }
        getWindow().setStatusBarColor(Color.BLACK);
        getWindow().setNavigationBarColor(Color.BLACK);
        setupWebView();
        appWorker.execute(() -> installedAppsCache = installedAppsJson().toString());
    }

    private void setupWebView() {
        webView = new WebView(this);
        webView.setBackgroundColor(Color.rgb(8, 8, 8));
        WebSettings settings = webView.getSettings();
        settings.setJavaScriptEnabled(true);
        settings.setDomStorageEnabled(false);
        settings.setAllowContentAccess(false);
        settings.setAllowFileAccess(true);
        settings.setAllowFileAccessFromFileURLs(false);
        settings.setAllowUniversalAccessFromFileURLs(false);
        settings.setMediaPlaybackRequiresUserGesture(true);
        settings.setBuiltInZoomControls(false);
        settings.setDisplayZoomControls(false);
        settings.setTextZoom(100);
        webView.addJavascriptInterface(new AndroidBridge(), "ShadowVpnAndroid");
        webView.setWebViewClient(new WebViewClient() {
            @Override public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                Uri uri = request.getUrl();
                return !("file".equals(uri.getScheme())
                        && "/android_asset/shadowvpn/index.html".equals(uri.getPath()));
            }

            @Override public void onPageFinished(WebView view, String url) {
                if (!UI_URL.equals(url)) return;
                webReady = true;
                pushSnapshot();
                if (subscriptionUrls().length() > 0) importSubscriptions(false);
                else fetchPublicIp(false);
            }
        });
        setContentView(webView);
        webView.loadUrl(UI_URL);
    }

    @Override protected void onResume() {
        super.onResume();
        ui.removeCallbacks(statusUpdater);
        ui.post(statusUpdater);
        ui.post(this::pushSnapshot);
        ui.postDelayed(this::maybeAutoUpdate, 1200);
    }

    @Override protected void onPause() {
        ui.removeCallbacks(statusUpdater);
        super.onPause();
    }

    @Override protected void onDestroy() {
        ui.removeCallbacks(statusUpdater);
        webReady = false;
        worker.shutdownNow();
        appWorker.shutdownNow();
        if (webView != null) {
            webView.removeJavascriptInterface("ShadowVpnAndroid");
            webView.destroy();
            webView = null;
        }
        super.onDestroy();
    }

    @Override public void onBackPressed() {
        if (webView == null || !webReady) {
            super.onBackPressed();
            return;
        }
        webView.evaluateJavascript("window.ShadowVPN?window.ShadowVPN.closeTopLayer():false",
                value -> {
                    if (!"true".equals(value)) MainActivity.super.onBackPressed();
                });
    }

    private void maybeAutoUpdate() {
        if (ShadowVpnService.connected || importRunning) return;
        String interval = preferences.getString("auto_update", "1440");
        if ("off".equals(interval)) return;
        long minutes;
        try { minutes = Long.parseLong(interval); }
        catch (Exception ignored) { minutes = 1440; }
        long last = preferences.getLong("last_sync", 0);
        if (last > 0 && System.currentTimeMillis() - last >= minutes * 60_000L) {
            importSubscriptions(false);
        }
    }

    private void importSubscriptions(boolean userInitiated) {
        if (importRunning) return;
        JSONArray sources = subscriptionUrls();
        if (sources.length() == 0) {
            uiMessage = "Добавьте ссылку на подписку";
            pushSnapshot();
            return;
        }
        importRunning = true;
        if (userInitiated) uiMessage = "Обновляем подписку…";
        pushSnapshot();
        worker.execute(() -> {
            try {
                Bridge.setDeviceSeed(Settings.Secure.getString(
                        getContentResolver(), Settings.Secure.ANDROID_ID));
                JSONObject root = new JSONObject(Bridge.importSubscriptions(sources.toString()));
                JSONArray profiles = root.getJSONArray("profiles");
                List<SubscriptionInfo> parsedSubscriptionInfo = parseSubscriptionInfo(
                        root.optJSONArray("sources"));
                List<ServerItem> parsed = new ArrayList<>();
                for (int i = 0; i < profiles.length(); i++) {
                    JSONObject profile = profiles.getJSONObject(i);
                    parsed.add(new ServerItem(
                            profile.getString("id"),
                            profile.optString("name", "Server"),
                            profile.optString("protocol", "auto"),
                            profile.optString("transport", "tcp"),
                            profile.optBoolean("auto", false),
                            profile.optInt("sourceIndex", -1)));
                }
                String importedDoh = extractDoh(root);
                ui.post(() -> {
                    servers.clear();
                    servers.addAll(parsed);
                    subscriptionInfo.clear();
                    subscriptionInfo.addAll(parsedSubscriptionInfo);
                    preferences.edit().putString("subscription_info",
                            subscriptionInfoJson().toString()).apply();
                    if (!importedDoh.isEmpty()) {
                        doh = importedDoh;
                        preferences.edit().putString("doh", doh).apply();
                    }
                    if (findSelected() == null && !servers.isEmpty()) {
                        selectedId = servers.get(0).id;
                        preferences.edit().putString("selected_id", selectedId).apply();
                    }
                    importRunning = false;
                    uiMessage = parsed.isEmpty() ? "В подписке нет поддерживаемых серверов" : "";
                    preferences.edit().putLong("last_sync", System.currentTimeMillis()).apply();
                    pushSnapshot();
                    fetchPublicIp(false);
                    if (preferences.getBoolean("ping_on_open", false)) runPings();
                });
            } catch (Exception error) {
                ui.post(() -> {
                    importRunning = false;
                    uiMessage = "Не удалось обновить · " + cleanError(error);
                    pushSnapshot();
                });
            }
        });
    }

    private String extractDoh(JSONObject root) {
        String imported = root.optString("dnsDoh", "");
        JSONArray sources = root.optJSONArray("sources");
        if (!imported.isEmpty() || sources == null) return imported;
        for (int i = 0; i < sources.length(); i++) {
            JSONObject source = sources.optJSONObject(i);
            JSONObject metadata = source == null ? null : source.optJSONObject("metadata");
            if (metadata != null && !metadata.optString("dnsDoh", "").isEmpty()) {
                return metadata.optString("dnsDoh", "");
            }
        }
        return "";
    }

    private List<SubscriptionInfo> parseSubscriptionInfo(JSONArray sources) {
        List<SubscriptionInfo> result = new ArrayList<>();
        if (sources == null) return result;
        for (int i = 0; i < sources.length(); i++) {
            JSONObject source = sources.optJSONObject(i);
            JSONObject metadata = source == null ? null : source.optJSONObject("metadata");
            if (source == null || metadata == null) continue;
            int sourceIndex = source.optInt("index", i);
            String slotName = subscriptionSlotName(sourceIndex);
            String providerTitle = metadata.optString("title", "").trim();
            String title = providerTitle.isEmpty() ? slotName
                    : providerTitle.startsWith("Wi-Fi") || providerTitle.startsWith("LTE")
                    ? providerTitle : slotName + " · " + providerTitle;
            result.add(new SubscriptionInfo(
                    sourceIndex,
                    title,
                    Math.max(0, metadata.optLong("upload", 0)),
                    Math.max(0, metadata.optLong("download", 0)),
                    Math.max(0, metadata.optLong("total", 0)),
                    Math.max(0, metadata.optLong("expire", 0))));
        }
        return result;
    }

    private String subscriptionSlotName(int sourceIndex) {
        boolean hasWifi = !preferences.getString("subscription_wifi", "").trim().isEmpty();
        if (hasWifi) return sourceIndex == 0 ? "Wi-Fi" : "LTE";
        return "LTE";
    }

    private void restoreSubscriptionInfo() {
        try {
            subscriptionInfo.addAll(parseSubscriptionInfo(
                    new JSONArray(preferences.getString("subscription_info", "[]"))));
        } catch (Exception ignored) { }
    }

    private JSONArray subscriptionInfoJson() {
        JSONArray result = new JSONArray();
        for (SubscriptionInfo info : subscriptionInfo) {
            JSONObject source = new JSONObject();
            JSONObject metadata = new JSONObject();
            try {
                source.put("index", info.index);
                metadata.put("title", info.title);
                metadata.put("upload", info.upload);
                metadata.put("download", info.download);
                metadata.put("total", info.total);
                metadata.put("expire", info.expire);
                source.put("metadata", metadata);
                result.put(source);
            } catch (Exception ignored) { }
        }
        return result;
    }

    private void runPings() {
        if (pingRunning || servers.isEmpty() || ShadowVpnService.connected) return;
        pingRunning = true;
        pingResults.clear();
        pushSnapshot();
        String method = preferences.getString("ping_method", "tcp");
        worker.execute(() -> {
            try {
                JSONArray results = new JSONArray(Bridge.pingProfiles(method));
                Map<String, PingData> loaded = new HashMap<>();
                for (int i = 0; i < results.length(); i++) {
                    JSONObject item = results.getJSONObject(i);
                    loaded.put(item.getString("id"), new PingData(
                            item.optLong("latencyMs", 0), item.optBoolean("available", false)));
                }
                ui.post(() -> {
                    pingResults.putAll(loaded);
                    pingRunning = false;
                    pushSnapshot();
                });
            } catch (Exception ignored) {
                ui.post(() -> {
                    pingRunning = false;
                    uiMessage = "Не удалось проверить задержку";
                    pushSnapshot();
                });
            }
        });
    }

    private void toggleVpn() {
        if (ShadowVpnService.connected || "Подключение…".equals(ShadowVpnService.status)) {
            startService(new Intent(this, ShadowVpnService.class).setAction(ShadowVpnService.STOP));
            return;
        }
        if (findSelected() == null) {
            uiMessage = "Сначала выберите сервер";
            pushSnapshot();
            return;
        }
        Intent permission = VpnService.prepare(this);
        if (permission != null) startActivityForResult(permission, VPN_REQUEST);
        else startSelectedServer();
    }

    @Override protected void onActivityResult(int request, int result, Intent data) {
        super.onActivityResult(request, result, data);
        if (request == VPN_REQUEST && result == RESULT_OK) startSelectedServer();
    }

    private void startSelectedServer() {
        ServerItem selected = findSelected();
        if (selected == null) return;
        String appRoutingMode = preferences.getString("app_routing_mode", "all");
        String appPackages = preferences.getString("app_routing_packages", "[]");
        if ("include".equals(appRoutingMode)) {
            try {
                if (new JSONArray(appPackages).length() == 0) {
                    uiMessage = "Выберите хотя бы одно приложение для VPN";
                    pushSnapshot();
                    return;
                }
            } catch (Exception error) {
                appRoutingMode = "all";
                appPackages = "[]";
            }
        }
        try {
            JSONObject options = new JSONObject();
            String dnsId = preferences.getString("dns_provider",
                    doh.isEmpty() ? "cloudflare" : "subscription-doh");
            options.put("dnsId", dnsId);
            options.put("dohUrl", "subscription-doh".equals(dnsId) ? doh : "");
            options.put("fragmentation", preferences.getBoolean("fragmentation", false));
            options.put("tunMtu", parseMtu(preferences.getString("tun_mtu", "1400")));
            options.put("routingMode", preferences.getString("routing_mode", "full"));
            options.put("pingMethod", preferences.getString("ping_method", "tcp"));
            JSONArray rules = new JSONArray();
            for (String line : preferences.getString("routing_rules", "").split("\\r?\\n")) {
                if (!line.trim().isEmpty()) rules.put(line.trim());
            }
            options.put("routingRules", rules);
            try {
                options.put("autoProfileIds", new JSONArray(
                        preferences.getString("auto_profiles", "[]")));
            } catch (Exception ignored) {
                options.put("autoProfileIds", new JSONArray());
            }
            Bridge.configure(options.toString());
        } catch (Exception error) {
            uiMessage = cleanError(error);
            pushSnapshot();
            return;
        }
        if (Build.VERSION.SDK_INT >= 33 &&
                checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                        != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 101);
        }
        Intent intent = new Intent(this, ShadowVpnService.class)
                .setAction(ShadowVpnService.START)
                .putExtra("profileId", selected.id)
                .putExtra("dnsDoh", "subscription-doh".equals(
                        preferences.getString("dns_provider", "subscription-doh")) ? doh : "")
                .putExtra("mtu", parseMtu(preferences.getString("tun_mtu", "1400")))
                .putExtra("ipv6Enabled", preferences.getBoolean("ipv6_enabled", false))
                .putExtra("appRoutingMode", appRoutingMode)
                .putExtra("appPackages", appPackages);
        startForegroundService(intent);
        ShadowVpnService.status = "Подключение…";
        uiMessage = "";
        pushSnapshot();
    }

    private void updateRuntimeState() {
        boolean connected = ShadowVpnService.connected;
        if (connected && !wasConnected) {
            connectedAt = SystemClock.elapsedRealtime();
            trafficStartedRx = trafficLastRx = safeTraffic(
                    TrafficStats.getUidRxBytes(getApplicationInfo().uid));
            trafficStartedTx = trafficLastTx = safeTraffic(
                    TrafficStats.getUidTxBytes(getApplicationInfo().uid));
            trafficLastAt = connectedAt;
            publicIp = "";
            ipRequestMode = -1;
            fetchPublicIp(true);
        }
        if (!connected) {
            connectedAt = 0;
            downloadSpeed = uploadSpeed = "0 Б/с";
            downloadTotal = uploadTotal = "0 Б";
            if (wasConnected) {
                publicIp = "";
                ipRequestMode = -1;
                fetchPublicIp(false);
            }
        } else {
            updateTraffic();
        }
        wasConnected = connected;
    }

    private void fetchPublicIp(boolean throughVpn) {
        int mode = throughVpn ? 1 : 0;
        if (ipRequestMode == mode) return;
        ipRequestMode = mode;
        worker.execute(() -> {
            String[] endpoints = {"https://api4.ipify.org", "https://ipv4.icanhazip.com"};
            for (String endpoint : endpoints) {
                try {
                    Proxy proxy = throughVpn
                            ? new Proxy(Proxy.Type.HTTP, new InetSocketAddress("127.0.0.1", 18443))
                            : Proxy.NO_PROXY;
                    HttpURLConnection connection = (HttpURLConnection) new URL(endpoint).openConnection(proxy);
                    connection.setConnectTimeout(6000);
                    connection.setReadTimeout(6000);
                    connection.setRequestProperty("User-Agent", "ShadowVPN-Android/0.15");
                    String value;
                    try (BufferedReader reader = new BufferedReader(
                            new InputStreamReader(connection.getInputStream()))) {
                        value = reader.readLine();
                    } finally {
                        connection.disconnect();
                    }
                    if (value != null && value.trim().length() <= 64) {
                        String ip = value.trim();
                        ui.post(() -> {
                            if (ipRequestMode == mode) {
                                publicIp = ip;
                                pushSnapshot();
                            }
                        });
                        return;
                    }
                } catch (Exception ignored) { }
            }
        });
    }

    private void updateTraffic() {
        long now = SystemClock.elapsedRealtime();
        long rx = safeTraffic(TrafficStats.getUidRxBytes(getApplicationInfo().uid));
        long tx = safeTraffic(TrafficStats.getUidTxBytes(getApplicationInfo().uid));
        long elapsed = Math.max(1, now - trafficLastAt);
        downloadSpeed = formatBytes(Math.max(0, rx - trafficLastRx) * 1000 / elapsed) + "/с";
        uploadSpeed = formatBytes(Math.max(0, tx - trafficLastTx) * 1000 / elapsed) + "/с";
        downloadTotal = formatBytes(Math.max(0, rx - trafficStartedRx));
        uploadTotal = formatBytes(Math.max(0, tx - trafficStartedTx));
        trafficLastRx = rx;
        trafficLastTx = tx;
        trafficLastAt = now;
    }

    private void pushSnapshot() {
        if (!webReady || webView == null) return;
        JSONObject snapshot = buildSnapshot();
        webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.render(" + snapshot + ")", null);
    }

    private void pushRuntimeSnapshot() {
        if (!webReady || webView == null) return;
        JSONObject snapshot = runtimeSnapshot();
        webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.render(" + snapshot + ")", null);
    }

    private JSONObject buildSnapshot() {
        JSONObject root = runtimeSnapshot();
        try {
            root.put("needsSubscription", subscriptionUrls().length() == 0);
            root.put("importRunning", importRunning);
            root.put("pingRunning", pingRunning);
            JSONArray list = new JSONArray();
            for (ServerItem server : servers) list.put(serverJson(server));
            root.put("servers", list);
            ServerItem selected = findSelected();
            root.put("selected", selected == null ? JSONObject.NULL : serverJson(selected));
            root.put("subscriptions", subscriptionSlotsJson());
            root.put("settings", settingsJson());
            root.put("appVersion", getPackageManager()
                    .getPackageInfo(getPackageName(), 0).versionName);
        } catch (Exception ignored) { }
        return root;
    }

    private JSONObject runtimeSnapshot() {
        JSONObject root = new JSONObject();
        try {
            boolean connected = ShadowVpnService.connected;
            boolean connecting = !connected && "Подключение…".equals(ShadowVpnService.status);
            String state = connected ? "connected"
                    : connecting ? "connecting"
                    : ShadowVpnService.status.startsWith("Ошибка") ? "error" : "disconnected";
            root.put("state", state);
            root.put("serviceStatus", ShadowVpnService.status);
            root.put("message", uiMessage);
            root.put("publicIp", connected ? maskIp(publicIp) : publicIp);
            root.put("session", connected ? formatDuration(
                    Math.max(0, SystemClock.elapsedRealtime() - connectedAt) / 1000) : "00:00:00");
            JSONObject traffic = new JSONObject();
            traffic.put("downloadSpeed", downloadSpeed);
            traffic.put("downloadTotal", downloadTotal);
            traffic.put("uploadSpeed", uploadSpeed);
            traffic.put("uploadTotal", uploadTotal);
            root.put("traffic", traffic);
        } catch (Exception ignored) { }
        return root;
    }

    private JSONObject serverJson(ServerItem server) throws Exception {
        FlagName display = flagAndName(server.name);
        JSONObject item = new JSONObject();
        item.put("id", server.id);
        item.put("name", display.name);
        item.put("countryCode", display.countryCode.toLowerCase(Locale.ROOT));
        item.put("protocol", server.protocol.toUpperCase(Locale.ROOT));
        item.put("transport", server.transport.toUpperCase(Locale.ROOT));
        item.put("auto", server.auto);
        item.put("selected", server.id.equals(selectedId));
        SubscriptionInfo source = findSubscriptionInfo(server.sourceIndex);
        if (source != null) {
            JSONObject subscription = new JSONObject();
            subscription.put("title", source.title);
            long used = source.upload > Long.MAX_VALUE - source.download
                    ? Long.MAX_VALUE : source.upload + source.download;
            subscription.put("used", used);
            subscription.put("total", source.total);
            subscription.put("expire", source.expire);
            item.put("subscription", subscription);
        }
        PingData ping = pingResults.get(server.id);
        if (ping != null) {
            item.put("latency", ping.latency);
            item.put("available", ping.available);
        }
        return item;
    }

    private JSONObject settingsJson() throws Exception {
        JSONObject value = new JSONObject();
        value.put("autoUpdate", preferences.getString("auto_update", "1440"));
        value.put("pingOnOpen", preferences.getBoolean("ping_on_open", false));
        value.put("dnsProvider", preferences.getString("dns_provider",
                doh.isEmpty() ? "cloudflare" : "subscription-doh"));
        value.put("routingMode", preferences.getString("routing_mode", "full"));
        value.put("routingRules", preferences.getString("routing_rules", ""));
        value.put("pingMethod", preferences.getString("ping_method", "tcp"));
        value.put("fragmentation", preferences.getBoolean("fragmentation", false));
        value.put("tunMtu", preferences.getString("tun_mtu", "1400"));
        value.put("ipv6Enabled", preferences.getBoolean("ipv6_enabled", false));
        value.put("appRoutingMode", preferences.getString("app_routing_mode", "all"));
        value.put("appRoutingPackages", new JSONArray(
                preferences.getString("app_routing_packages", "[]")));
        value.put("doh", doh);
        value.put("autoProfiles", new JSONArray(preferences.getString("auto_profiles", "[]")));
        value.put("hwid", deviceHwid);
        value.put("batteryUnrestricted", isBatteryUnrestricted());
        return value;
    }

    private SubscriptionInfo findSubscriptionInfo(int sourceIndex) {
        for (SubscriptionInfo info : subscriptionInfo) {
            if (info.index == sourceIndex) return info;
        }
        return null;
    }

    private boolean isBatteryUnrestricted() {
        PowerManager manager = getSystemService(PowerManager.class);
        return manager != null && manager.isIgnoringBatteryOptimizations(getPackageName());
    }

    private final class AndroidBridge {
        @JavascriptInterface public void ready() { ui.post(MainActivity.this::pushSnapshot); }
        @JavascriptInterface public void toggleVpn() { ui.post(MainActivity.this::toggleVpn); }

        @JavascriptInterface public void selectServer(String id) {
            ui.post(() -> {
                for (ServerItem server : servers) {
                    if (server.id.equals(id)) {
                        selectedId = id;
                        preferences.edit().putString("selected_id", id).apply();
                        uiMessage = "";
                        pushSnapshot();
                        return;
                    }
                }
            });
        }

        @JavascriptInterface public void refreshSubscriptions() {
            ui.post(() -> importSubscriptions(true));
        }

        @JavascriptInterface public void pingServers() { ui.post(MainActivity.this::runPings); }

        @JavascriptInterface public void saveSubscriptions(String json) {
            ui.post(() -> {
                try {
                    JSONObject input = new JSONObject(json);
                    String rawWifi = input.optString("wifi", "").trim();
                    String rawLte = input.optString("lte", "").trim();
                    String wifi = cleanSubscriptionUrl(rawWifi);
                    String lte = cleanSubscriptionUrl(rawLte);
                    if ((!rawWifi.isEmpty() && wifi.isEmpty()) || (!rawLte.isEmpty() && lte.isEmpty())) {
                        throw new IllegalArgumentException("Нужна корректная HTTPS-ссылка");
                    }
                    if (!wifi.isEmpty() && wifi.equals(lte)) {
                        throw new IllegalArgumentException("Для Wi-Fi и LTE нужны разные ссылки");
                    }
                    saveSubscriptionSlots(wifi, lte);
                    if (wifi.isEmpty() && lte.isEmpty()) {
                        servers.clear();
                        subscriptionInfo.clear();
                        selectedId = "";
                        preferences.edit().remove("selected_id")
                                .remove("subscription_info").apply();
                        uiMessage = "";
                        pushSnapshot();
                    } else {
                        importSubscriptions(true);
                    }
                } catch (Exception error) {
                    uiMessage = error.getMessage() == null
                            ? "Некорректные подписки" : error.getMessage();
                    pushSnapshot();
                }
            });
        }

        @JavascriptInterface public void saveSettings(String json) {
            ui.post(() -> {
                try {
                    JSONObject value = new JSONObject(json);
                    SharedPreferences.Editor editor = preferences.edit();
                    editor.putString("auto_update", allowed(value.optString("autoUpdate"),
                            new String[]{"15", "60", "360", "1440", "off"}, "1440"));
                    editor.putBoolean("ping_on_open", value.optBoolean("pingOnOpen", false));
                    editor.putString("dns_provider", allowed(value.optString("dnsProvider"),
                            new String[]{"subscription-doh", "cloudflare", "google", "quad9"}, "cloudflare"));
                    editor.putString("routing_mode", allowed(value.optString("routingMode"),
                            new String[]{"full", "bypass", "proxy_only"}, "full"));
                    editor.putString("ping_method", allowed(value.optString("pingMethod"),
                            new String[]{"tcp", "head", "get"}, "tcp"));
                    editor.putBoolean("fragmentation", value.optBoolean("fragmentation", false));
                    editor.putString("tun_mtu", allowed(value.optString("tunMtu"),
                            new String[]{"1280", "1360", "1400", "1500"}, "1400"));
                    editor.putBoolean("ipv6_enabled", value.optBoolean("ipv6Enabled", false));
                    editor.putString("app_routing_mode", allowed(value.optString("appRoutingMode"),
                            new String[]{"all", "exclude", "include"}, "all"));
                    JSONArray appPackages = value.optJSONArray("appRoutingPackages");
                    if (appPackages != null) {
                        JSONArray cleanPackages = new JSONArray();
                        Set<String> seen = new HashSet<>();
                        for (int i = 0; i < appPackages.length() && cleanPackages.length() < 256; i++) {
                            String packageName = appPackages.optString(i, "").trim();
                            if (packageName.matches("[A-Za-z0-9_]+(?:\\.[A-Za-z0-9_]+)+")
                                    && !getPackageName().equals(packageName)
                                    && seen.add(packageName)) {
                                cleanPackages.put(packageName);
                            }
                        }
                        editor.putString("app_routing_packages", cleanPackages.toString());
                    }
                    editor.putString("routing_rules", value.optString("routingRules", "").trim());
                    JSONArray auto = value.optJSONArray("autoProfiles");
                    if (auto != null) editor.putString("auto_profiles", auto.toString());
                    editor.apply();
                    uiMessage = "Настройки сохранены";
                    pushSnapshot();
                } catch (Exception error) {
                    uiMessage = "Не удалось сохранить настройки";
                    pushSnapshot();
                }
            });
        }

        @JavascriptInterface public String getLogs() { return ShadowVpnService.getLogs(); }

        @JavascriptInterface public void copyText(String value) {
            ui.post(() -> {
                ClipboardManager clipboard = (ClipboardManager) getSystemService(CLIPBOARD_SERVICE);
                clipboard.setPrimaryClip(ClipData.newPlainText("ShadowVPN", value));
                Toast.makeText(MainActivity.this, "Скопировано", Toast.LENGTH_SHORT).show();
            });
        }

        @JavascriptInterface public void openVpnSettings() {
            ui.post(() -> startActivity(new Intent(Settings.ACTION_VPN_SETTINGS)));
        }

        @JavascriptInterface public String getInstalledApps() {
            String cached = installedAppsCache;
            return cached.isEmpty() ? "{\"apps\":[],\"loading\":true}" : cached;
        }

        @JavascriptInterface public void requestInstalledApps() {
            appWorker.execute(() -> {
                String payload = installedAppsCache;
                if (payload.isEmpty()) {
                    payload = installedAppsJson().toString();
                    installedAppsCache = payload;
                }
                final String result = payload;
                ui.post(() -> {
                    if (!webReady || webView == null) return;
                    webView.evaluateJavascript(
                            "window.ShadowVPN&&window.ShadowVPN.installedApps(" + JSONObject.quote(result) + ")",
                            null);
                });
            });
        }

        @JavascriptInterface public void requestAppIcon(String packageName) {
            if (packageName == null
                    || !packageName.matches("[A-Za-z0-9_]+(?:\\.[A-Za-z0-9_]+)+")
                    || !pendingAppIcons.add(packageName)) return;
            appWorker.execute(() -> {
                String source = appIconCache.get(packageName);
                if (source == null) {
                    try {
                        ApplicationInfo application = getPackageManager().getApplicationInfo(packageName, 0);
                        source = drawableDataUrl(application.loadIcon(getPackageManager()));
                    } catch (Exception ignored) {
                        source = "";
                    }
                    appIconCache.put(packageName, source);
                }
                pendingAppIcons.remove(packageName);
                final String icon = source;
                ui.post(() -> {
                    if (!webReady || webView == null) return;
                    webView.evaluateJavascript(
                            "window.ShadowVPN&&window.ShadowVPN.appIcon(" + JSONObject.quote(packageName)
                                    + "," + JSONObject.quote(icon) + ")", null);
                });
            });
        }

        @SuppressLint("BatteryLife")
        @JavascriptInterface public void openBatterySettings() {
            ui.post(() -> {
                try {
                    startActivity(new Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                            Uri.parse("package:" + getPackageName())));
                } catch (Exception firstError) {
                    try {
                        startActivity(new Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                                Uri.parse("package:" + getPackageName())));
                    } catch (Exception ignored) { }
                }
            });
        }
    }

    private JSONObject installedAppsJson() {
        JSONObject root = new JSONObject();
        JSONArray apps = new JSONArray();
        try {
            Intent launcher = new Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER);
            List<ResolveInfo> resolved = getPackageManager().queryIntentActivities(launcher, 0);
            Map<String, ApplicationInfo> unique = new HashMap<>();
            for (ResolveInfo info : resolved) {
                if (info.activityInfo == null || info.activityInfo.applicationInfo == null) continue;
                ApplicationInfo application = info.activityInfo.applicationInfo;
                if (!getPackageName().equals(application.packageName)) {
                    unique.put(application.packageName, application);
                }
            }
            List<JSONObject> sorted = new ArrayList<>();
            for (ApplicationInfo application : unique.values()) {
                sorted.add(applicationJson(application));
            }
            sorted.sort(Comparator.comparing(
                    app -> app.optString("name", "").toLowerCase(Locale.ROOT)));
            for (JSONObject application : sorted) apps.put(application);
            root.put("apps", apps);
        } catch (Exception error) {
            try { root.put("error", cleanError(error)); } catch (Exception ignored) { }
        }
        return root;
    }

    private JSONObject applicationJson(ApplicationInfo application) throws Exception {
        JSONObject item = new JSONObject();
        item.put("packageName", application.packageName);
        item.put("name", application.loadLabel(getPackageManager()).toString());
        return item;
    }

    private String drawableDataUrl(Drawable drawable) {
        try {
            int size = 72;
            Bitmap bitmap = Bitmap.createBitmap(size, size, Bitmap.Config.ARGB_8888);
            Canvas canvas = new Canvas(bitmap);
            drawable.setBounds(0, 0, size, size);
            drawable.draw(canvas);
            ByteArrayOutputStream output = new ByteArrayOutputStream();
            bitmap.compress(Bitmap.CompressFormat.PNG, 90, output);
            bitmap.recycle();
            return "data:image/png;base64," + Base64.encodeToString(
                    output.toByteArray(), Base64.NO_WRAP);
        } catch (Exception ignored) {
            return "";
        }
    }

    private void migrateSubscriptionSlots() {
        if (preferences.contains("subscription_wifi") || preferences.contains("subscription_lte")) {
            return;
        }
        JSONArray legacy = new JSONArray();
        try { legacy = new JSONArray(preferences.getString("subscriptions", "[]")); }
        catch (Exception ignored) { }
        if (legacy.length() == 0) {
            String single = preferences.getString("subscription_url", "").trim();
            if (!single.isEmpty()) legacy.put(single);
        }
        saveSubscriptionSlots(legacy.optString(0, ""), legacy.optString(1, ""));
    }

    private JSONArray subscriptionUrls() {
        JSONArray urls = new JSONArray();
        String wifi = preferences.getString("subscription_wifi", "").trim();
        String lte = preferences.getString("subscription_lte", "").trim();
        if (!wifi.isEmpty()) urls.put(wifi);
        if (!lte.isEmpty() && !lte.equals(wifi)) urls.put(lte);
        return urls;
    }

    private JSONObject subscriptionSlotsJson() {
        JSONObject result = new JSONObject();
        try {
            result.put("wifi", preferences.getString("subscription_wifi", ""));
            result.put("lte", preferences.getString("subscription_lte", ""));
        } catch (Exception ignored) { }
        return result;
    }

    private void saveSubscriptionSlots(String wifi, String lte) {
        JSONArray urls = new JSONArray();
        if (!wifi.isEmpty()) urls.put(wifi);
        if (!lte.isEmpty() && !lte.equals(wifi)) urls.put(lte);
        preferences.edit()
                .putString("subscription_wifi", wifi)
                .putString("subscription_lte", lte)
                .putString("subscriptions", urls.toString())
                .putString("subscription_url", wifi.isEmpty() ? lte : wifi)
                .apply();
    }

    private String cleanSubscriptionUrl(String value) {
        value = value == null ? "" : value.trim();
        if (value.isEmpty()) return "";
        try {
            Uri uri = Uri.parse(value);
            return "https".equalsIgnoreCase(uri.getScheme()) && uri.getHost() != null
                    ? value : "";
        } catch (Exception ignored) {
            return "";
        }
    }

    private ServerItem findSelected() {
        for (ServerItem server : servers) if (server.id.equals(selectedId)) return server;
        return null;
    }

    private FlagName flagAndName(String source) {
        if (source == null) return new FlagName("Сервер", "");
        int[] points = source.codePoints().toArray();
        for (int i = 0; i + 1 < points.length; i++) {
            if (points[i] >= 0x1f1e6 && points[i] <= 0x1f1ff &&
                    points[i + 1] >= 0x1f1e6 && points[i + 1] <= 0x1f1ff) {
                String flag = new String(points, i, 2);
                String code = "" + (char) ('A' + points[i] - 0x1f1e6)
                        + (char) ('A' + points[i + 1] - 0x1f1e6);
                String name = source.replace(flag, "").replaceAll("\\s{2,}", " ").trim();
                return new FlagName(name.isEmpty() ? "Сервер" : name, code);
            }
        }
        return new FlagName(source.trim().isEmpty() ? "Сервер" : source.trim(), "");
    }

    private static String allowed(String value, String[] values, String fallback) {
        for (String candidate : values) if (candidate.equals(value)) return candidate;
        return fallback;
    }

    private static int parseMtu(String value) {
        try {
            int mtu = Integer.parseInt(value);
            return Math.max(1280, Math.min(1500, mtu));
        } catch (Exception ignored) {
            return 1400;
        }
    }

    private long safeTraffic(long value) {
        return value == TrafficStats.UNSUPPORTED ? 0 : value;
    }

    private String formatBytes(long value) {
        if (value < 1024) return value + " Б";
        if (value < 1024L * 1024L) return String.format(Locale.ROOT, "%.1f КБ", value / 1024d);
        if (value < 1024L * 1024L * 1024L) {
            return String.format(Locale.ROOT, "%.1f МБ", value / (1024d * 1024d));
        }
        return String.format(Locale.ROOT, "%.1f ГБ", value / (1024d * 1024d * 1024d));
    }

    private String formatDuration(long seconds) {
        return String.format(Locale.ROOT, "%02d:%02d:%02d",
                seconds / 3600, (seconds % 3600) / 60, seconds % 60);
    }

    private String maskIp(String ip) {
        if (ip == null || ip.isEmpty()) return "";
        int lastDot = ip.lastIndexOf('.');
        if (lastDot > 0) return ip.substring(0, lastDot + 1) + "***";
        int colon = ip.indexOf(':');
        return colon > 0 ? ip.substring(0, colon) + ":****:****" : ip;
    }

    private String cleanError(Exception error) {
        String message = error.getMessage();
        if (message == null || message.trim().isEmpty()) return "неизвестная ошибка";
        return message.length() > 110 ? message.substring(0, 110) + "…" : message;
    }

    private static final class ServerItem {
        final String id;
        final String name;
        final String protocol;
        final String transport;
        final boolean auto;
        final int sourceIndex;

        ServerItem(String id, String name, String protocol, String transport, boolean auto,
                   int sourceIndex) {
            this.id = id;
            this.name = name;
            this.protocol = protocol;
            this.transport = transport;
            this.auto = auto;
            this.sourceIndex = sourceIndex;
        }
    }

    private static final class SubscriptionInfo {
        final int index;
        final String title;
        final long upload;
        final long download;
        final long total;
        final long expire;

        SubscriptionInfo(int index, String title, long upload, long download,
                         long total, long expire) {
            this.index = index;
            this.title = title;
            this.upload = upload;
            this.download = download;
            this.total = total;
            this.expire = expire;
        }
    }

    private static final class PingData {
        final long latency;
        final boolean available;

        PingData(long latency, boolean available) {
            this.latency = latency;
            this.available = available;
        }
    }

    private static final class FlagName {
        final String name;
        final String countryCode;

        FlagName(String name, String countryCode) {
            this.name = name;
            this.countryCode = countryCode;
        }
    }
}
