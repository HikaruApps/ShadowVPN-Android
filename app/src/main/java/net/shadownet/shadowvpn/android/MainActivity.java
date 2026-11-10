package net.shadownet.shadowvpn.android;

import android.Manifest;
import android.app.Activity;
import android.content.ClipData;
import android.content.ClipDescription;
import android.content.ClipboardManager;
import android.content.Intent;
import android.content.SharedPreferences;
import android.content.pm.ApplicationInfo;
import android.content.pm.PackageManager;
import android.content.pm.ResolveInfo;
import android.graphics.Bitmap;
import android.graphics.Canvas;
import android.graphics.Color;
import android.graphics.Insets;
import android.graphics.drawable.Drawable;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
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
import android.view.ViewGroup;
import android.view.WindowInsets;
import android.webkit.JavascriptInterface;
import android.webkit.WebResourceRequest;
import android.webkit.WebSettings;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.FrameLayout;
import android.widget.Toast;
import android.util.Base64;

import net.shadownet.shadowvpn.core.bridge.Bridge;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.ByteArrayOutputStream;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
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
import java.util.concurrent.ExecutorCompletionService;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.Future;

public final class MainActivity extends Activity {
    private static final int VPN_REQUEST = 100;
    private static final String UI_URL = "file:///android_asset/shadowvpn/index.html";
    private static final int UI_BACKGROUND = Color.rgb(10, 10, 10);
    private static final int MANUAL_SOURCE = 100;

    private final ExecutorService worker = Executors.newSingleThreadExecutor();
    private final ExecutorService appWorker = Executors.newSingleThreadExecutor();
    // Server switches and live reloads run apart from imports and pings,
    // which can take tens of seconds.
    private final ExecutorService switchWorker = Executors.newSingleThreadExecutor();
    // A ping can take minutes, so it gets its own thread and never delays an
    // import or the public IP lookup.
    private final ExecutorService pingRunner = Executors.newSingleThreadExecutor();
    private final ExecutorService diagnosticsRunner = Executors.newSingleThreadExecutor();
    private String switchingTo = "";
    private final Handler ui = new Handler(Looper.getMainLooper());
    private final List<ServerItem> servers = new ArrayList<>();
    private final List<SubscriptionInfo> subscriptionInfo = new ArrayList<>();
    private final Map<String, PingData> pingResults = new HashMap<>();
    private final List<PingDelta> pendingPingDeltas = new ArrayList<>();
    private final Map<String, String> appIconCache = new ConcurrentHashMap<>();
    private final Set<String> pendingAppIcons = ConcurrentHashMap.newKeySet();
    private volatile String installedAppsCache = "";

    private SharedPreferences preferences;
    private WebView webView;
    private boolean webReady;
    private String lastRuntimeSnapshot = "";
    private String serversJsonCache = "";
    private String settingsJsonCache = "";
    private String appVersionCache = "";
    private boolean appVersionLoaded;
    private boolean importRunning;
    private boolean launchUpdateDone;
    /** Auto-connect on launch runs once per process, not on every return to the app. */
    private static boolean autoConnectChecked;
    private boolean pingRunning;
    private boolean pingFlushScheduled;
    private boolean wasConnected;
    private boolean serviceWarningShown;
    private int serviceWarningGeneration;
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
    // Raw rates for the connect button's glow; the strings above are for text.
    private long downloadRate;
    private long uploadRate;
    // Latencies restored from the last run: shown dimmed until checked again.
    private boolean pingResultsCached;
    // Re-check after the phone moved to another network (as Hiddify does):
    // latencies from Wi-Fi say nothing about mobile data.
    private ConnectivityManager.NetworkCallback networkWatcher;
    private Network watchedNetwork;
    private final Runnable networkRecheck = () -> {
        // Not while a connection is being set up: the check would compete
        // with it for the network.
        if (pingRunning || pingResults.isEmpty()
                || "Подключение…".equals(ShadowVpnService.status)) return;
        runPings();
    };

    private final Runnable statusUpdater = new Runnable() {
        @Override public void run() {
            updateRuntimeState();
            pushRuntimeSnapshot();
            ui.postDelayed(this, 500);
        }
    };

    @Override protected void onCreate(Bundle state) {
        super.onCreate(state);
        preferences = CoreConfig.preferences(this);
        CoreConfig.initialize(this);
        migrateSubscriptionSlots();
        restoreSubscriptionInfo();
        selectedId = preferences.getString("selected_id", "");
        doh = preferences.getString("doh", doh);
        try {
            deviceHwid = Bridge.deviceHWID();
        } catch (Exception ignored) { }
        getWindow().setStatusBarColor(UI_BACKGROUND);
        getWindow().setNavigationBarColor(UI_BACKGROUND);
        setupWebView();
        appWorker.execute(() -> installedAppsCache = installedAppsJson().toString());
        handleIntent(getIntent());
    }

    @Override protected void onNewIntent(Intent intent) {
        super.onNewIntent(intent);
        setIntent(intent);
        handleIntent(intent);
    }

    // ---------- Subscriptions from outside: link, clipboard, QR ----------
    // Nothing from outside is added silently: any web page can open a link,
    // so the user always confirms in the app first.

    private String pendingOffer = "";
    private String pendingScript = "";
    private static final int VPN_SHORTCUT_REQUEST = 103;

    private void runPendingScript() {
        if (pendingScript.isEmpty() || !webReady || webView == null) return;
        webView.evaluateJavascript(pendingScript, null);
        pendingScript = "";
    }

    private void handleIntent(Intent intent) {
        if (intent == null) return;
        String action = intent.getAction();
        if (VpnControl.ACTION_DISCONNECT.equals(action)) {
            if (VpnControl.isActive()) VpnControl.disconnect(this);
            return;
        }
        if (VpnControl.ACTION_CONNECT.equals(action)) {
            if (VpnControl.isActive()) return;
            if (preferences.getString("selected_id", "").isEmpty()) {
                showMessage("Сначала выберите сервер");
                return;
            }
            // Servers may not be loaded yet; the service reads the saved
            // server itself, so only the VPN permission is needed here.
            Intent permission = CoreConfig.proxyOnly(preferences) ? null : VpnService.prepare(this);
            if (permission != null) startActivityForResult(permission, VPN_SHORTCUT_REQUEST);
            else VpnControl.connect(this);
            return;
        }
        if (VpnControl.ACTION_SERVERS.equals(action)) {
            pendingScript = "window.ShadowVPN&&window.ShadowVPN.openServers()";
            runPendingScript();
            return;
        }
        if (Intent.ACTION_VIEW.equals(intent.getAction()) && intent.getData() != null) {
            String url = subscriptionFromLink(intent.getDataString());
            if (!url.isEmpty()) offerSubscription(url, "link");
            else showMessage("Ссылка не содержит HTTPS-подписку");
        }
    }

    /**
     * shadowvpn://import/https://host/path#Name, the same form as Hiddify's
     * hiddify://import/...; the URL may also be percent-encoded or passed as
     * shadowvpn://import?url=....
     */
    static String subscriptionFromLink(String link) {
        if (link == null) return "";
        Uri uri = Uri.parse(link);
        if (!"shadowvpn".equalsIgnoreCase(uri.getScheme())
                || !"import".equalsIgnoreCase(uri.getHost())) return "";
        String value = uri.getQueryParameter("url");
        if (value == null || value.isEmpty()) {
            int start = link.toLowerCase(Locale.ROOT).indexOf("import/");
            value = start < 0 ? "" : link.substring(start + "import/".length());
            int hash = value.indexOf('#');
            if (hash >= 0) value = value.substring(0, hash);
            if (value.regionMatches(true, 0, "https%3A", 0, 8)) value = Uri.decode(value);
        }
        return isHttpsLink(value) ? value.trim() : "";
    }

    static boolean isHttpsLink(String value) {
        if (value == null) return false;
        value = value.trim();
        if (value.isEmpty() || value.contains(" ") || value.contains("\n")) return false;
        Uri uri = Uri.parse(value);
        return "https".equalsIgnoreCase(uri.getScheme()) && uri.getHost() != null
                && !uri.getHost().isEmpty();
    }

    private void offerSubscription(String url, String source) {
        String wifi = preferences.getString("subscription_wifi", "");
        String lte = preferences.getString("subscription_lte", "");
        if (url.equals(wifi) || url.equals(lte)) {
            if (!"clipboard".equals(source)) showMessage("Эта подписка уже добавлена");
            return;
        }
        pendingOffer = "";
        try {
            JSONObject offer = new JSONObject().put("url", url).put("source", source);
            if (!webReady || webView == null) {
                pendingOffer = offer.toString();
                return;
            }
            webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.offerSubscription("
                    + offer + ")", null);
        } catch (Exception ignored) { }
    }

    private void deliverPendingOffer() {
        if (pendingOffer.isEmpty() || !webReady || webView == null) return;
        webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.offerSubscription("
                + pendingOffer + ")", null);
        pendingOffer = "";
    }

    /** The widget and the tile show the server's name without the app running. */
    private void rememberSelectedName() {
        ServerItem selected = findSelected();
        preferences.edit().putString("selected_name",
                selected == null ? "" : flagAndName(selected.name).name).apply();
        VpnControl.refreshSurfaces(this);
    }

    private void showMessage(String message) {
        uiMessage = message;
        pushSnapshot();
    }

    @Override public void onWindowFocusChanged(boolean hasFocus) {
        super.onWindowFocusChanged(hasFocus);
        // Android lets only the focused app read the clipboard.
        if (hasFocus) checkClipboard();
    }

    /**
     * Offers a subscription link the user just copied. The description is read
     * first: it does not count as reading the clipboard, so Android's "pasted"
     * notice appears only once per new clip, and only for text.
     */
    private void checkClipboard() {
        try {
            ClipboardManager manager = getSystemService(ClipboardManager.class);
            if (manager == null) return;
            ClipDescription description = manager.getPrimaryClipDescription();
            if (description == null || !description.hasMimeType(ClipDescription.MIMETYPE_TEXT_PLAIN)) return;
            long stamp = description.getTimestamp();
            if (stamp <= preferences.getLong("clipboard_checked_at", 0)) return;
            preferences.edit().putLong("clipboard_checked_at", stamp).apply();
            ClipData clip = manager.getPrimaryClip();
            if (clip == null || clip.getItemCount() == 0) return;
            CharSequence text = clip.getItemAt(0).getText();
            String value = text == null ? "" : text.toString().trim();
            String url = value.startsWith("shadowvpn://") ? subscriptionFromLink(value) : value;
            if (isHttpsLink(url)) offerSubscription(url, "clipboard");
        } catch (RuntimeException ignored) { }
    }

    private static final int QR_REQUEST = com.google.zxing.integration.android.IntentIntegrator.REQUEST_CODE;

    private void scanQr() {
        new com.google.zxing.integration.android.IntentIntegrator(this)
                .setDesiredBarcodeFormats(com.google.zxing.integration.android.IntentIntegrator.QR_CODE)
                .setPrompt("Наведите камеру на QR-код подписки")
                .setBeepEnabled(false)
                .setOrientationLocked(true)
                .initiateScan();
    }

    private void handleQrResult(int resultCode, Intent data) {
        com.google.zxing.integration.android.IntentResult result =
                com.google.zxing.integration.android.IntentIntegrator.parseActivityResult(resultCode, data);
        if (result == null || result.getContents() == null) return;
        String value = result.getContents().trim();
        String url = value.startsWith("shadowvpn://") ? subscriptionFromLink(value) : value;
        if (isHttpsLink(url)) offerSubscription(url, "qr");
        else showMessage("В QR-коде нет HTTPS-ссылки на подписку");
    }

    private void setupWebView() {
        webView = new WebView(this);
        webView.setBackgroundColor(UI_BACKGROUND);
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
                // A reloaded page lost the latencies; they are not part of the snapshot.
                requeuePingResults();
                loadCachedServers();
                deliverPendingOffer();
                runPendingScript();
            }
        });
        setContentView(createInsetRoot(webView));
        webView.loadUrl(UI_URL);
    }

    // Android 15 draws apps edge-to-edge; keep the web UI clear of the status bar,
    // navigation bar, display cutout and keyboard by padding its container.
    private FrameLayout createInsetRoot(WebView content) {
        FrameLayout root = new FrameLayout(this);
        root.setBackgroundColor(UI_BACKGROUND);
        root.addView(content, new FrameLayout.LayoutParams(
                ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT));
        if (Build.VERSION.SDK_INT >= 30) {
            getWindow().setDecorFitsSystemWindows(false);
            root.setOnApplyWindowInsetsListener((view, insets) -> {
                Insets bars = insets.getInsets(WindowInsets.Type.systemBars()
                        | WindowInsets.Type.displayCutout() | WindowInsets.Type.ime());
                view.setPadding(bars.left, bars.top, bars.right, bars.bottom);
                return WindowInsets.CONSUMED;
            });
        }
        return root;
    }

    @Override protected void onResume() {
        super.onResume();
        // Battery access and local addresses can change while the app is away.
        settingsJsonCache = "";
        ui.removeCallbacks(statusUpdater);
        ui.post(statusUpdater);
        ui.post(this::pushSnapshot);
        ui.postDelayed(this::maybeAutoUpdate, 1200);
        watchNetwork();
    }

    @Override protected void onPause() {
        ui.removeCallbacks(statusUpdater);
        unwatchNetwork();
        super.onPause();
    }

    private void watchNetwork() {
        ConnectivityManager manager = getSystemService(ConnectivityManager.class);
        if (manager == null || networkWatcher != null) return;
        networkWatcher = new ConnectivityManager.NetworkCallback() {
            @Override public void onAvailable(Network network) {
                // Our own VPN coming up is not a new network to measure from.
                try {
                    NetworkCapabilities capabilities = manager.getNetworkCapabilities(network);
                    if (capabilities != null
                            && capabilities.hasTransport(NetworkCapabilities.TRANSPORT_VPN)) return;
                } catch (RuntimeException ignored) { }
                ui.post(() -> {
                    Network previous = watchedNetwork;
                    watchedNetwork = network;
                    // The first callback reports the current network.
                    if (previous == null || previous.equals(network)) return;
                    ui.removeCallbacks(networkRecheck);
                    ui.postDelayed(networkRecheck, 2000);
                });
            }
        };
        try {
            manager.registerDefaultNetworkCallback(networkWatcher);
        } catch (RuntimeException error) {
            networkWatcher = null;
        }
    }

    private void unwatchNetwork() {
        ui.removeCallbacks(networkRecheck);
        if (networkWatcher == null) return;
        try {
            getSystemService(ConnectivityManager.class).unregisterNetworkCallback(networkWatcher);
        } catch (RuntimeException ignored) { }
        networkWatcher = null;
    }

    @Override protected void onDestroy() {
        ui.removeCallbacks(statusUpdater);
        webReady = false;
        worker.shutdownNow();
        appWorker.shutdownNow();
        switchWorker.shutdownNow();
        pingRunner.shutdownNow();
        diagnosticsRunner.shutdownNow();
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
        if (importRunning) return;
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

    /** Shows the servers saved by the last import at once; refreshes only if there are none. */
    private void loadCachedServers() {
        worker.execute(() -> {
            String cached = "";
            try { cached = Bridge.loadCachedProfiles(); } catch (Exception ignored) { }
            final String result = cached;
            ui.post(() -> {
                if (!result.isEmpty()) {
                    try { applyServers(parseServers(new JSONObject(result))); }
                    catch (Exception ignored) { }
                }
                restorePingCache();
                boolean refresh = hasSources() && (servers.isEmpty()
                        || !launchUpdateDone && preferences.getBoolean("update_on_open", false));
                launchUpdateDone = true;
                maybeAutoConnect();
                if (refresh) importSubscriptions(false);
                else {
                    fetchPublicIp(ShadowVpnService.connected);
                    if (preferences.getBoolean("ping_on_open", false)) runPings();
                }
            });
        });
    }

    private static Set<String> serverIds(List<ServerItem> list) {
        Set<String> ids = new HashSet<>();
        for (ServerItem server : list) if (!server.auto) ids.add(server.id);
        return ids;
    }

    /** Older versions pinged after every import when "ping on open" was on. */
    private boolean pingAfterUpdate() {
        return preferences.getBoolean("ping_after_update",
                preferences.getBoolean("ping_on_open", false));
    }

    private boolean hasSources() {
        return subscriptionUrls().length() > 0 || !manualConfigs().isEmpty();
    }

    private String manualConfigs() {
        return preferences.getString("manual_configs", "").trim();
    }

    private List<ServerItem> parseServers(JSONObject root) throws Exception {
        JSONArray profiles = root.getJSONArray("profiles");
        List<ServerItem> parsed = new ArrayList<>();
        for (int i = 0; i < profiles.length(); i++) {
            JSONObject profile = profiles.getJSONObject(i);
            parsed.add(new ServerItem(
                    profile.getString("id"),
                    profile.optString("name", "Server"),
                    profile.optString("protocol", "auto"),
                    profile.optString("transport", "tcp"),
                    profile.optBoolean("auto", false),
                    profile.optInt("sourceIndex", -1),
                    profile.optString("format", ""),
                    profile.optInt("members", 0)));
        }
        return parsed;
    }

    private void applyServers(List<ServerItem> parsed) {
        servers.clear();
        servers.addAll(parsed);
        serversJsonCache = "";
        boolean selectionChanged = false;
        if (findSelected() == null && !servers.isEmpty()) {
            selectedId = servers.get(0).id;
            preferences.edit().putString("selected_id", selectedId).apply();
            selectionChanged = true;
        }
        // Also after an update renamed the server, and for installs from
        // before the name was kept.
        rememberSelectedName();
        if (selectionChanged) notifySelectedServer(selectedId);
        pushSnapshot();
    }

    private void importSubscriptions(boolean userInitiated) {
        if (importRunning) return;
        if (!hasSources()) {
            uiMessage = "Добавьте ссылку на подписку";
            pushSnapshot();
            return;
        }
        JSONObject request = new JSONObject();
        try {
            request.put("urls", subscriptionUrls());
            request.put("manual", manualConfigs());
            request.put("sendHwid", preferences.getBoolean("send_hwid", true));
        } catch (Exception ignored) { }
        boolean notify = !userInitiated && preferences.getBoolean("notify_updates", false);
        Set<String> before = serverIds(servers);
        importRunning = true;
        if (userInitiated) uiMessage = "Обновляем подписку…";
        pushSnapshot();
        worker.execute(() -> {
            try {
                JSONObject root = new JSONObject(Bridge.importSources(request.toString()));
                List<ServerItem> parsed = parseServers(root);
                List<SubscriptionInfo> parsedSubscriptionInfo = parseSubscriptionInfo(
                        root.optJSONArray("sources"));
                String importedDoh = extractDoh(root);
                String sourceError = firstSourceError(root.optJSONArray("sources"));
                ui.post(() -> {
                    subscriptionInfo.clear();
                    subscriptionInfo.addAll(parsedSubscriptionInfo);
                    serversJsonCache = "";
                    preferences.edit().putString("subscription_info",
                            subscriptionInfoJson().toString()).apply();
                    if (!importedDoh.isEmpty()) {
                        doh = importedDoh;
                        settingsJsonCache = "";
                        preferences.edit().putString("doh", doh).apply();
                    }
                    importRunning = false;
                    uiMessage = parsed.isEmpty() ? "В подписке нет поддерживаемых серверов" : sourceError;
                    preferences.edit().putLong("last_sync", System.currentTimeMillis()).apply();
                    applyServers(parsed);
                    fetchPublicIp(ShadowVpnService.connected);
                    maybeAutoConnect();
                    if (pingAfterUpdate()) runPings();
                    if (notify) UpdateNotifier.notify(this, before, serverIds(parsed), null);
                });
            } catch (Exception error) {
                ui.post(() -> {
                    importRunning = false;
                    uiMessage = "Не удалось обновить · " + cleanError(error);
                    pushSnapshot();
                    if (notify) UpdateNotifier.notify(this, before, before, cleanError(error));
                });
            }
        });
    }

    /** A source that failed while others succeeded must not fail silently. */
    private String firstSourceError(JSONArray sources) {
        if (sources == null) return "";
        for (int i = 0; i < sources.length(); i++) {
            JSONObject source = sources.optJSONObject(i);
            String error = source == null ? "" : source.optString("error", "");
            if (!error.isEmpty()) {
                return subscriptionSlotName(source.optInt("index", i)) + " не обновлена · " + error;
            }
        }
        return "";
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
            String title = sourceIndex == MANUAL_SOURCE ? slotName
                    : providerTitle.isEmpty() ? slotName
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
        if (sourceIndex == MANUAL_SOURCE) return "Свои конфигурации";
        boolean hasWifi = !preferences.getString("subscription_wifi", "").trim().isEmpty();
        if (hasWifi) return sourceIndex == 0 ? "Wi-Fi" : "LTE";
        return "LTE";
    }

    private void restoreSubscriptionInfo() {
        try {
            subscriptionInfo.addAll(parseSubscriptionInfo(
                    new JSONArray(preferences.getString("subscription_info", "[]"))));
        } catch (Exception ignored) { }
        serversJsonCache = "";
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
        if (pingRunning) return;
        pingRunning = true;
        pingResults.clear();
        pingResultsCached = false;
        pendingPingDeltas.clear();
        if (webReady && webView != null) {
            webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.pingStarted()", null);
        }
        pushSnapshot();
        String method = CoreConfig.pingMethod(preferences);
        pingRunner.execute(() -> runPingChecks(method));
    }

    /** Runs the checks in a bounded pool and posts each completed host immediately. */
    private void runPingChecks(String method) {
        ExecutorService pool = null;
        try {
            JSONObject plan = new JSONObject(Bridge.pingPlan());
            List<PingHost> hosts = parsePingHosts(plan.optJSONArray("servers"));
            List<PingGroup> groups = parsePingGroups(plan.optJSONArray("groups"));
            String autoId = plan.optString("autoId", "");
            String autoNoRUId = plan.optString("autoNoRUId", "");
            if (hosts.isEmpty()) {
                ui.post(this::finishPings);
                return;
            }
            // Real checks wait on the network, not the CPU; the core still
            // serializes the instance creation itself.
            int concurrency = "real".equals(method) ? 6 : 16;
            pool = Executors.newFixedThreadPool(concurrency);
            ExecutorCompletionService<PingResult> completion =
                    new ExecutorCompletionService<>(pool);
            for (PingHost host : hosts) {
                completion.submit(() -> pingOne(host, method));
            }
            Map<String, PingData> known = new HashMap<>();
            Map<String, PingData> aggregateState = new HashMap<>();
            for (int i = 0; i < hosts.size(); i++) {
                Future<PingResult> future = completion.take();
                PingResult result = future.get();
                known.put(result.id, result.data);
                List<PingDelta> deltas = new ArrayList<>();
                deltas.add(new PingDelta(result.id, result.data));
                for (PingGroup group : groups) {
                    addAggregateDelta(aggregateState, deltas, group.id,
                            aggregateForGroup(group, known));
                }
                addAggregateDelta(aggregateState, deltas, autoId,
                        aggregateForHosts(hosts, known, false));
                addAggregateDelta(aggregateState, deltas, autoNoRUId,
                        aggregateForHosts(hosts, known, true));
                final List<PingDelta> update = deltas;
                ui.post(() -> applyPingUpdate(update));
            }
            pool.shutdown();
            ui.post(this::finishPings);
        } catch (Exception error) {
            if (pool != null) pool.shutdownNow();
            ui.post(this::failPings);
        }
    }

    private PingResult pingOne(PingHost host, String method) {
        PingData data;
        try {
            JSONObject result = new JSONObject(Bridge.pingProfile(host.id, method));
            data = new PingData(result.optLong("latencyMs", 0),
                    result.optBoolean("available", false), result.optBoolean("direct", false));
        } catch (Exception error) {
            // A missing or broken host is an unavailable result, not a failed run.
            data = new PingData(0, false);
        }
        return new PingResult(host.id, data);
    }

    private List<PingHost> parsePingHosts(JSONArray array) {
        List<PingHost> result = new ArrayList<>();
        if (array == null) return result;
        for (int i = 0; i < array.length(); i++) {
            JSONObject item = array.optJSONObject(i);
            if (item == null) continue;
            String id = item.optString("id", "");
            if (!id.isEmpty()) {
                result.add(new PingHost(id, item.optString("protocol", ""),
                        item.optBoolean("russian", false)));
            }
        }
        return result;
    }

    private List<PingGroup> parsePingGroups(JSONArray array) {
        List<PingGroup> result = new ArrayList<>();
        if (array == null) return result;
        for (int i = 0; i < array.length(); i++) {
            JSONObject item = array.optJSONObject(i);
            if (item == null) continue;
            String id = item.optString("id", "");
            if (id.isEmpty()) continue;
            List<String> members = new ArrayList<>();
            JSONArray values = item.optJSONArray("members");
            if (values != null) {
                for (int j = 0; j < values.length(); j++) {
                    String member = values.optString(j, "");
                    if (!member.isEmpty()) members.add(member);
                }
            }
            result.add(new PingGroup(id, members));
        }
        return result;
    }

    private PingData aggregateForGroup(PingGroup group, Map<String, PingData> known) {
        List<PingData> members = new ArrayList<>();
        for (String member : group.members) members.add(known.get(member));
        return fastestOf(members);
    }

    /**
     * The fastest server that passed the check; only when none did, the
     * fastest that at least answered directly, marked as such.
     */
    private static PingData fastestOf(List<PingData> results) {
        PingData verified = null;
        PingData direct = null;
        for (PingData data : results) {
            if (data == null || !data.available) continue;
            if (!data.direct) {
                if (verified == null || data.latency < verified.latency) verified = data;
            } else if (direct == null || data.latency < direct.latency) {
                direct = data;
            }
        }
        if (verified != null) return new PingData(verified.latency, true);
        if (direct != null) return new PingData(direct.latency, true, true);
        return new PingData(0, false);
    }

    private PingData aggregateForHosts(List<PingHost> hosts, Map<String, PingData> known,
                                       boolean withoutRussian) {
        List<PingData> members = new ArrayList<>();
        for (PingHost host : hosts) {
            if ("naive".equalsIgnoreCase(host.protocol)) continue;
            if (withoutRussian && host.russian) continue;
            members.add(known.get(host.id));
        }
        return fastestOf(members);
    }

    private void addAggregateDelta(Map<String, PingData> state, List<PingDelta> deltas,
                                   String id, PingData value) {
        if (id.isEmpty()) return;
        PingData previous = state.get(id);
        if (previous != null && previous.latency == value.latency
                && previous.available == value.available && previous.direct == value.direct) return;
        state.put(id, value);
        deltas.add(new PingDelta(id, value));
    }

    private void applyPingUpdate(List<PingDelta> deltas) {
        for (PingDelta delta : deltas) {
            pingResults.put(delta.id, delta.data);
            pendingPingDeltas.add(delta);
        }
        schedulePingFlush();
    }

    /** Coalesces fast results so the WebView receives at most one call per 80 ms. */
    private void schedulePingFlush() {
        if (pingFlushScheduled || pendingPingDeltas.isEmpty()) return;
        pingFlushScheduled = true;
        ui.postDelayed(this::flushPingResults, 80);
    }

    private void flushPingResults() {
        pingFlushScheduled = false;
        if (pendingPingDeltas.isEmpty()) return;
        if (!webReady || webView == null) return;
        StringBuilder script = new StringBuilder();
        for (PingDelta delta : pendingPingDeltas) {
            script.append("window.ShadowVPN&&window.ShadowVPN.pingResult({")
                    .append("\"id\":").append(JSONObject.quote(delta.id))
                    .append(",\"latency\":").append(delta.data.latency)
                    .append(",\"available\":").append(delta.data.available)
                    .append(",\"direct\":").append(delta.data.direct)
                    .append(",\"cached\":").append(delta.cached)
                    .append("});");
        }
        pendingPingDeltas.clear();
        webView.evaluateJavascript(script.toString(), null);
    }

    private void requeuePingResults() {
        for (Map.Entry<String, PingData> entry : pingResults.entrySet()) {
            pendingPingDeltas.add(new PingDelta(entry.getKey(), entry.getValue(), pingResultsCached));
        }
        schedulePingFlush();
    }

    // ---------- Connection check: network, DNS, server, internet via VPN ----------

    private void runDiagnostics() {
        String profileId = selectedId;
        diagnosticsRunner.execute(() -> {
            diagnosticStep("network", "running", "");
            String[] network = networkState();
            diagnosticStep("network", network[0], network[1]);
            if (!"ok".equals(network[0])) {
                diagnosticStep("whitelist", "skip", "Пропущено: нет сети");
                diagnosticStep("dns", "skip", "Пропущено: нет сети");
                diagnosticStep("server", "skip", "Пропущено: нет сети");
                diagnosticStep("vpn", "skip", "Пропущено: нет сети");
                return;
            }
            diagnosticStep("whitelist", "running", "");
            diagnosticStep("dns", "running", "");
            diagnosticStep("server", "running", "");
            // Both checks go over the phone's own network, so they run side
            // by side; the whitelist answer is ready within 8 seconds.
            Thread whitelist = new Thread(this::diagnoseWhitelist, "diagnostics-whitelist");
            whitelist.start();
            try {
                JSONObject result = new JSONObject(Bridge.diagnose(profileId));
                JSONObject dns = result.getJSONObject("dns");
                JSONObject server = result.getJSONObject("server");
                diagnosticStep("dns", dns.optBoolean("ok") ? "ok" : "fail", dns.optString("detail"));
                diagnosticStep("server", server.optBoolean("ok") ? "ok"
                        : dns.optBoolean("ok") ? "fail" : "skip", server.optString("detail"));
            } catch (Exception error) {
                diagnosticStep("dns", "fail", cleanError(error));
                diagnosticStep("server", "skip", "Пропущено");
            }
            try {
                whitelist.join();
            } catch (InterruptedException interrupted) {
                return;
            }
            if (!ShadowVpnService.connected) {
                diagnosticStep("vpn", "skip", "VPN не подключён: подключитесь и запустите проверку снова");
                return;
            }
            diagnosticStep("vpn", "running", "");
            try {
                diagnosticStep("vpn", "ok", "Работает · ваш IP " + maskIp(Bridge.publicIP()));
            } catch (Exception error) {
                diagnosticStep("vpn", "fail", "Не работает: " + maskIps(cleanError(error)));
            }
        });
    }

    /**
     * Mobile whitelists: VK, Yandex and MAX open while ordinary sites do not.
     * "fail" here means the network looks restricted, not that the check broke.
     */
    private void diagnoseWhitelist() {
        try {
            JSONObject result = new JSONObject(Bridge.checkWhitelist());
            if (result.optBoolean("whitelist")) {
                diagnosticStep("whitelist", "fail", "Похоже на белые списки: открываются VK, Яндекс и MAX, "
                        + "а обычные сайты нет. Работают только серверы, которые обходят белые списки");
            } else if (result.optBoolean("offline")) {
                diagnosticStep("whitelist", "skip", "Не удалось проверить: сайты не отвечают");
            } else {
                diagnosticStep("whitelist", "ok", "Не обнаружены · обычные сайты открываются");
            }
        } catch (Exception error) {
            diagnosticStep("whitelist", "skip", "Не удалось проверить");
        }
    }

    /** {state, detail} of the phone's own network, outside the VPN. */
    private String[] networkState() {
        ConnectivityManager manager = getSystemService(ConnectivityManager.class);
        try {
            Network network = manager == null ? null : manager.getActiveNetwork();
            NetworkCapabilities caps = network == null ? null : manager.getNetworkCapabilities(network);
            if (caps == null) return new String[]{"fail", "Телефон не подключён к сети"};
            String kind = caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) ? "Wi-Fi"
                    : caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) ? "Мобильная сеть"
                    : caps.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET) ? "Ethernet" : "Сеть";
            if (!caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)) {
                return new String[]{"fail", kind + " без доступа в интернет: Android не смог выйти в сеть"};
            }
            return new String[]{"ok", kind + " · интернет есть"};
        } catch (RuntimeException error) {
            return new String[]{"fail", "Не удалось проверить сеть"};
        }
    }

    private void diagnosticStep(String id, String state, String detail) {
        ui.post(() -> {
            if (!webReady || webView == null) return;
            try {
                JSONObject step = new JSONObject().put("id", id).put("state", state).put("detail", maskIps(detail));
                webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.diagnosticsStep("
                        + step + ")", null);
            } catch (Exception ignored) { }
        });
    }

    private void finishPings() {
        pingRunning = false;
        savePingCache();
        pushSnapshot();
    }

    private static final long PING_CACHE_MAX_AGE = 24L * 60 * 60 * 1000;

    /** Keeps the last latencies so the list is not blank on the next start. */
    private void savePingCache() {
        try {
            JSONObject results = new JSONObject();
            for (Map.Entry<String, PingData> entry : pingResults.entrySet()) {
                results.put(entry.getKey(), new JSONArray()
                        .put(entry.getValue().latency).put(entry.getValue().available)
                        .put(entry.getValue().direct));
            }
            JSONObject cache = new JSONObject()
                    .put("at", System.currentTimeMillis())
                    .put("method", CoreConfig.pingMethod(preferences))
                    .put("results", results);
            preferences.edit().putString("ping_cache", cache.toString()).apply();
        } catch (Exception ignored) { }
    }

    private void restorePingCache() {
        if (pingRunning || !pingResults.isEmpty()) return;
        try {
            JSONObject cache = new JSONObject(preferences.getString("ping_cache", "{}"));
            long age = System.currentTimeMillis() - cache.optLong("at", 0);
            // Old results, or ones from the other method, would only mislead.
            if (age < 0 || age > PING_CACHE_MAX_AGE
                    || !CoreConfig.pingMethod(preferences).equals(cache.optString("method"))) return;
            JSONObject results = cache.getJSONObject("results");
            for (java.util.Iterator<String> keys = results.keys(); keys.hasNext(); ) {
                String id = keys.next();
                JSONArray value = results.getJSONArray(id);
                pingResults.put(id, new PingData(value.optLong(0), value.optBoolean(1), value.optBoolean(2)));
            }
            pingResultsCached = !pingResults.isEmpty();
            requeuePingResults();
        } catch (Exception ignored) { }
    }

    /**
     * Checks one server on request, the way Hiddify does for a tap: a quick
     * failure gets a second attempt from the 3rd second on, so a transient
     * drop does not mark a working server dead. Either way it ends within
     * about 10 seconds plus the attempt itself.
     */
    private void pingServer(String id) {
        if (pingRunning || id == null || id.isEmpty()) return;
        String method = CoreConfig.pingMethod(preferences);
        pingRunner.execute(() -> {
            long started = SystemClock.elapsedRealtime();
            PingResult result = pingOne(new PingHost(id, "", false), method);
            long elapsed = SystemClock.elapsedRealtime() - started;
            if (!result.data.available && elapsed < 5000) {
                try {
                    Thread.sleep(Math.max(0, 3000 - elapsed));
                } catch (InterruptedException interrupted) {
                    return;
                }
                result = pingOne(new PingHost(id, "", false), method);
            }
            PingResult answer = result;
            ui.post(() -> {
                pingResults.put(answer.id, answer.data);
                List<PingDelta> update = new ArrayList<>();
                update.add(new PingDelta(answer.id, answer.data));
                applyPingUpdate(update);
            });
        });
    }

    private void failPings() {
        pingRunning = false;
        uiMessage = "Не удалось проверить задержку";
        pushSnapshot();
    }

    /** Connects once at launch; with an empty cache it waits for the first import. */
    private void maybeAutoConnect() {
        if (autoConnectChecked) return;
        if (!preferences.getBoolean("auto_connect_open", false)) {
            autoConnectChecked = true;
            return;
        }
        if (findSelected() == null) return;
        autoConnectChecked = true;
        if (!ShadowVpnService.connected && !ShadowVpnService.blocked
                && !"Подключение…".equals(ShadowVpnService.status)) {
            toggleVpn();
        }
    }

    private void toggleVpn() {
        if (ShadowVpnService.connected || ShadowVpnService.blocked
                || "Подключение…".equals(ShadowVpnService.status)) {
            startService(new Intent(this, ShadowVpnService.class).setAction(ShadowVpnService.STOP));
            return;
        }
        if (findSelected() == null) {
            uiMessage = "Сначала выберите сервер";
            pushSnapshot();
            return;
        }
        // Proxy only opens no VPN interface and needs no VPN permission.
        Intent permission = CoreConfig.proxyOnly(preferences) ? null : VpnService.prepare(this);
        if (permission != null) startActivityForResult(permission, VPN_REQUEST);
        else startSelectedServer();
    }

    @Override protected void onActivityResult(int request, int result, Intent data) {
        super.onActivityResult(request, result, data);
        if (request == VPN_REQUEST && result == RESULT_OK) startSelectedServer();
        if (request == QR_REQUEST) handleQrResult(result, data);
        if (request == VPN_SHORTCUT_REQUEST && result == RESULT_OK) VpnControl.connect(this);
    }

    private void startSelectedServer() {
        ServerItem selected = findSelected();
        if (selected == null) return;
        // Moving from proxy only to VPN may be the first time a VPN is needed.
        if (!CoreConfig.proxyOnly(preferences)) {
            Intent permission = VpnService.prepare(this);
            if (permission != null) {
                startActivityForResult(permission, VPN_REQUEST);
                return;
            }
        }
        if ("include".equals(preferences.getString("app_routing_mode", "all"))) {
            try {
                if (new JSONArray(preferences.getString("app_routing_packages", "[]")).length() == 0) {
                    uiMessage = "Выберите хотя бы одно приложение для VPN";
                    pushSnapshot();
                    return;
                }
            } catch (Exception ignored) { }
        }
        try {
            CoreConfig.configure(this, preferences);
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
        startForegroundService(new Intent(this, ShadowVpnService.class)
                .setAction(ShadowVpnService.START));
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
            serviceWarningShown = false;
            serviceWarningGeneration++;
            connectedAt = 0;
            downloadSpeed = uploadSpeed = "0 Б/с";
            downloadRate = uploadRate = 0;
            downloadTotal = uploadTotal = "0 Б";
            if (wasConnected) {
                publicIp = "";
                ipRequestMode = -1;
                fetchPublicIp(false);
            }
        } else {
            updateTraffic();
            showServiceWarning();
        }
        wasConnected = connected;
    }

    /** Shows a non-fatal core warning once without changing the connected state. */
    private void showServiceWarning() {
        if (serviceWarningShown) return;
        String warning = ShadowVpnService.warning;
        if (warning == null || warning.trim().isEmpty()) return;
        serviceWarningShown = true;
        final int generation = serviceWarningGeneration;
        uiMessage = warning;
        pushSnapshot();
        ui.postDelayed(() -> {
            if (generation == serviceWarningGeneration && warning.equals(uiMessage)) {
                uiMessage = "";
                pushSnapshot();
            }
        }, 100);
    }

    private void fetchPublicIp(boolean throughVpn) {
        int mode = throughVpn ? 1 : 0;
        if (ipRequestMode == mode) return;
        ipRequestMode = mode;
        worker.execute(() -> {
            String ip = "";
            if (throughVpn) {
                try { ip = Bridge.publicIP(); } catch (Exception ignored) { }
            } else {
                ip = directPublicIp();
            }
            final String result = ip;
            if (result.isEmpty()) return;
            ui.post(() -> {
                if (ipRequestMode == mode) {
                    publicIp = result;
                    pushSnapshot();
                }
            });
        });
    }

    private String directPublicIp() {
        for (String endpoint : new String[]{"https://api4.ipify.org", "https://ipv4.icanhazip.com"}) {
            try {
                HttpURLConnection connection = (HttpURLConnection) new URL(endpoint).openConnection();
                connection.setConnectTimeout(6000);
                connection.setReadTimeout(6000);
                connection.setRequestProperty("User-Agent", "ShadowVPN-Android");
                try (BufferedReader reader = new BufferedReader(
                        new InputStreamReader(connection.getInputStream()))) {
                    String value = reader.readLine();
                    if (value != null && !value.trim().isEmpty() && value.trim().length() <= 64) {
                        return value.trim();
                    }
                } finally {
                    connection.disconnect();
                }
            } catch (Exception ignored) { }
        }
        return "";
    }

    private void updateTraffic() {
        long now = SystemClock.elapsedRealtime();
        long rx = safeTraffic(TrafficStats.getUidRxBytes(getApplicationInfo().uid));
        long tx = safeTraffic(TrafficStats.getUidTxBytes(getApplicationInfo().uid));
        long elapsed = Math.max(1, now - trafficLastAt);
        downloadRate = Math.max(0, rx - trafficLastRx) * 1000 / elapsed;
        uploadRate = Math.max(0, tx - trafficLastTx) * 1000 / elapsed;
        downloadSpeed = formatBytes(downloadRate) + "/с";
        uploadSpeed = formatBytes(uploadRate) + "/с";
        downloadTotal = formatBytes(Math.max(0, rx - trafficStartedRx));
        uploadTotal = formatBytes(Math.max(0, tx - trafficStartedTx));
        trafficLastRx = rx;
        trafficLastTx = tx;
        trafficLastAt = now;
    }

    private void pushSnapshot() {
        if (!webReady || webView == null) return;
        lastRuntimeSnapshot = "";
        String snapshot = buildSnapshot();
        webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.render(" + snapshot + ")", null);
        schedulePingFlush();
    }

    private void pushRuntimeSnapshot() {
        if (!webReady || webView == null) return;
        String snapshot = runtimeSnapshot().toString();
        // The status timer ticks twice a second; skip frames that change nothing.
        if (snapshot.equals(lastRuntimeSnapshot)) return;
        lastRuntimeSnapshot = snapshot;
        webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.render(" + snapshot + ")", null);
    }

    /** Builds the snapshot from cached JSON fragments instead of one large object. */
    private String buildSnapshot() {
        String runtime = runtimeSnapshot().toString();
        StringBuilder root = new StringBuilder(runtime.length() + 512);
        // The runtime part is a JSON object; its closing brace is replaced by
        // the fields below. An empty runtime object would leave a stray comma.
        boolean empty = runtime.length() <= 2;
        root.append(runtime, 0, Math.max(0, runtime.length() - 1));
        if (!empty) root.append(',');
        root.append("\"needsSubscription\":").append(!hasSources());
        root.append(",\"importRunning\":").append(importRunning);
        root.append(",\"pingRunning\":").append(pingRunning);
        root.append(",\"servers\":").append(serverListJson());
        ServerItem selected = findSelected();
        try {
            root.append(",\"selected\":")
                    .append(selected == null ? "null" : serverJson(selected).toString());
        } catch (Exception ignored) {
            root.append(",\"selected\":null");
        }
        root.append(",\"selectedId\":").append(JSONObject.quote(selectedId));
        root.append(",\"subscriptions\":").append(subscriptionSlotsJson().toString());
        root.append(",\"settings\":").append(cachedSettingsJson());
        root.append(",\"appVersion\":").append(JSONObject.quote(appVersion()));
        root.append("}");
        return root.toString();
    }

    private String serverListJson() {
        if (!serversJsonCache.isEmpty()) return serversJsonCache;
        JSONArray list = new JSONArray();
        for (ServerItem server : servers) {
            try { list.put(serverJson(server)); } catch (Exception ignored) { }
        }
        serversJsonCache = list.toString();
        return serversJsonCache;
    }

    private String cachedSettingsJson() {
        if (settingsJsonCache.isEmpty()) {
            try { settingsJsonCache = settingsJson().toString(); }
            catch (Exception ignored) { settingsJsonCache = "{}"; }
        }
        return settingsJsonCache;
    }

    private String appVersion() {
        if (!appVersionLoaded) {
            appVersionLoaded = true;
            try {
                String version = getPackageManager().getPackageInfo(getPackageName(), 0).versionName;
                appVersionCache = version == null ? "" : version;
            } catch (Exception ignored) {
                appVersionCache = "";
            }
        }
        return appVersionCache;
    }

    private JSONObject runtimeSnapshot() {
        JSONObject root = new JSONObject();
        try {
            boolean connected = ShadowVpnService.connected;
            boolean connecting = !connected && "Подключение…".equals(ShadowVpnService.status);
            // A Kill Switch retry also says "connecting"; the block matters more.
            String state = connected ? "connected"
                    : ShadowVpnService.blocked ? "blocked"
                    : connecting ? "connecting"
                    : ShadowVpnService.status.startsWith("Ошибка") ? "error" : "disconnected";
            root.put("state", state);
            root.put("switchingTo", switchingTo);
            root.put("serviceStatus", maskIps(ShadowVpnService.status));
            root.put("message", maskIps(uiMessage));
            root.put("publicIp", maskIp(publicIp));
            root.put("session", connected ? formatDuration(
                    Math.max(0, SystemClock.elapsedRealtime() - connectedAt) / 1000) : "00:00:00");
            JSONObject traffic = new JSONObject();
            traffic.put("downloadSpeed", downloadSpeed);
            traffic.put("downloadTotal", downloadTotal);
            traffic.put("uploadSpeed", uploadSpeed);
            traffic.put("uploadTotal", uploadTotal);
            traffic.put("downloadRate", downloadRate);
            traffic.put("uploadRate", uploadRate);
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
        item.put("format", server.format);
        item.put("members", server.members);
        item.put("auto", server.auto);
        item.put("source", server.sourceIndex == MANUAL_SOURCE ? "manual"
                : server.sourceIndex < 0 ? "" : subscriptionSlotName(server.sourceIndex));
        SubscriptionInfo source = server.sourceIndex == MANUAL_SOURCE
                ? null : findSubscriptionInfo(server.sourceIndex);
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
        return item;
    }

    private JSONObject settingsJson() throws Exception {
        JSONObject value = new JSONObject();
        value.put("autoUpdate", preferences.getString("auto_update", "1440"));
        value.put("pingOnOpen", preferences.getBoolean("ping_on_open", false));
        value.put("updateOnOpen", preferences.getBoolean("update_on_open", false));
        value.put("pingAfterUpdate", pingAfterUpdate());
        value.put("notifyUpdates", preferences.getBoolean("notify_updates", false));
        value.put("sendHwid", preferences.getBoolean("send_hwid", true));
        value.put("autoConnectOpen", preferences.getBoolean("auto_connect_open", false));
        value.put("autoConnectBoot", preferences.getBoolean("auto_connect_boot", false));
        value.put("killSwitch", preferences.getBoolean("kill_switch", false));
        value.put("proxyEnabled", CoreConfig.proxyEnabled(preferences));
        value.put("proxyHttp", preferences.getBoolean("proxy_http", false));
        value.put("proxySocksPort", String.valueOf(CoreConfig.port(preferences, "proxy_socks_port", 10808)));
        value.put("proxyHttpPort", String.valueOf(CoreConfig.port(preferences, "proxy_http_port", 10809)));
        value.put("proxyLan", preferences.getBoolean("proxy_lan", false));
        value.put("proxyOnly", preferences.getBoolean("proxy_only", false));
        value.put("proxyUser", preferences.getString("proxy_user", ""));
        value.put("proxyPass", preferences.getString("proxy_pass", ""));
        value.put("proxyAddresses", localAddresses());
        value.put("dnsProvider", preferences.getString("dns_provider",
                doh.isEmpty() ? "cloudflare" : "subscription-doh"));
        value.put("routingMode", preferences.getString("routing_mode", "full"));
        value.put("routingRules", preferences.getString("routing_rules", ""));
        value.put("pingMethod", CoreConfig.pingMethod(preferences));
        value.put("fragmentation", preferences.getBoolean("fragmentation", false));
        value.put("mux", preferences.getBoolean("mux", false));
        value.put("ipType", preferences.getString("ip_type", "auto"));
        value.put("routeOnly", preferences.getBoolean("route_only", false));
        value.put("blockUdp", preferences.getBoolean("block_udp", false));
        value.put("memoryLimit", preferences.getString("memory_limit", "0"));
        value.put("tunMtu", preferences.getString("tun_mtu", "1400"));
        value.put("ipv6Enabled", preferences.getBoolean("ipv6_enabled", false));
        value.put("lanDirect", preferences.getBoolean("lan_direct", true));
        value.put("customDns", preferences.getString("custom_dns", ""));
        value.put("subscriptionRules", preferences.getBoolean("subscription_rules", true));
        value.put("geoipUrl", preferences.getString("geoip_url", ""));
        value.put("geositeUrl", preferences.getString("geosite_url", ""));
        value.put("appRoutingMode", preferences.getString("app_routing_mode", "all"));
        value.put("appRoutingPackages", new JSONArray(
                preferences.getString("app_routing_packages", "[]")));
        value.put("doh", doh);
        value.put("autoProfiles", new JSONArray(preferences.getString("auto_profiles", "[]")));
        value.put("hwid", deviceHwid);
        value.put("batteryUnrestricted", isBatteryUnrestricted());
        return value;
    }

    // Settings Android applies to the TUN itself need a new VpnService
    // interface; the rest only changes Xray and is applied without a drop.
    private static final String[] TUN_KEYS = {
            "tun_mtu", "ipv6_enabled", "lan_direct", "app_routing_mode", "app_routing_packages"};
    private static final String[] CORE_KEYS = {
            "dns_provider", "custom_dns", "fragmentation", "routing_mode", "routing_rules",
            "subscription_rules", "geoip_url", "geosite_url", "auto_profiles",
            "mux", "ip_type", "route_only", "block_udp",
            "proxy_enabled", "proxy_http", "proxy_socks_port", "proxy_http_port", "proxy_lan",
            "proxy_user", "proxy_pass"};

    private String signature(String[] keys) {
        Map<String, ?> all = preferences.getAll();
        StringBuilder signature = new StringBuilder();
        for (String key : keys) signature.append(key).append('=').append(all.get(key)).append('\n');
        return signature.toString();
    }

    /**
     * Moves the running tunnel to another server without closing it. The core
     * connects and tests the new server first; if that fails, traffic stays
     * on the current one and the selection is not changed.
     */
    private void switchServer(ServerItem target) {
        if (!switchingTo.isEmpty()) {
            uiMessage = "Подождите, сервер уже переключается";
            pushSnapshot();
            return;
        }
        switchingTo = target.id;
        uiMessage = "";
        pushSnapshot();
        ShadowVpnService.addLog("Переключение на " + target.name);
        switchWorker.execute(() -> {
            String failure = null;
            try {
                CoreConfig.configure(this, preferences);
                Bridge.switchProfile(target.id);
            } catch (Exception error) {
                failure = cleanError(error);
            }
            final String result = failure;
            ui.post(() -> {
                switchingTo = "";
                if (result == null) {
                    selectedId = target.id;
                    preferences.edit().putString("selected_id", target.id).apply();
                    rememberSelectedName();
                    uiMessage = "Сервер переключён";
                    ShadowVpnService.addLog("Сервер переключён без разрыва VPN");
                    notifySelectedServer(target.id);
                    refreshVpnIp();
                } else {
                    uiMessage = result;
                    ShadowVpnService.addLog("Переключение не удалось: " + result);
                    // The core gave up the tunnel; bring the current server back.
                    if (result.startsWith("Туннель потерян")) startSelectedServer();
                }
                pushSnapshot();
            });
        });
    }

    /** Applies Xray-only settings to the running tunnel without reconnecting. */
    private void reloadCore() {
        switchWorker.execute(() -> {
            String failure = null;
            try {
                Bridge.reload();
            } catch (Exception error) {
                failure = cleanError(error);
            }
            final String result = failure;
            ui.post(() -> {
                uiMessage = result == null ? "Настройки применены без переподключения"
                        : "Настройки сохранены, но не применены: " + result;
                if (result == null) refreshVpnIp();
                else if (result.startsWith("Туннель потерян")) startSelectedServer();
                pushSnapshot();
            });
        });
    }

    private void refreshVpnIp() {
        publicIp = "";
        ipRequestMode = -1;
        fetchPublicIp(true);
    }

    private void saveSettings(String json) {
        Map<String, ?> previous = preferences.getAll();
        String tunBefore = signature(TUN_KEYS);
        String coreBefore = signature(CORE_KEYS);
        boolean proxyOnlyBefore = CoreConfig.proxyOnly(preferences);
        try {
            JSONObject value = new JSONObject(json);
            SharedPreferences.Editor editor = preferences.edit();
            editor.putString("auto_update", allowed(value.optString("autoUpdate"),
                    new String[]{"15", "60", "360", "1440", "off"}, "1440"));
            editor.putBoolean("ping_on_open", value.optBoolean("pingOnOpen", false));
            editor.putBoolean("update_on_open", value.optBoolean("updateOnOpen", false));
            editor.putBoolean("ping_after_update", value.optBoolean("pingAfterUpdate", pingAfterUpdate()));
            editor.putBoolean("notify_updates", value.optBoolean("notifyUpdates", false));
            editor.putBoolean("send_hwid", value.optBoolean("sendHwid", true));
            editor.putBoolean("auto_connect_open", value.optBoolean("autoConnectOpen", false));
            editor.putBoolean("auto_connect_boot", value.optBoolean("autoConnectBoot", false));
            editor.putBoolean("kill_switch", value.optBoolean("killSwitch", false));
            boolean proxyEnabled = value.optBoolean("proxyEnabled", false);
            editor.putBoolean("proxy_enabled", proxyEnabled);
            editor.putBoolean("proxy_http", value.optBoolean("proxyHttp", false));
            editor.putString("proxy_socks_port", cleanPort(value.optString("proxySocksPort"), 10808));
            editor.putString("proxy_http_port", cleanPort(value.optString("proxyHttpPort"), 10809));
            editor.putBoolean("proxy_lan", value.optBoolean("proxyLan", false));
            editor.putBoolean("proxy_only", value.optBoolean("proxyOnly", false));
            if (proxyEnabled && preferences.getString("proxy_pass", "").isEmpty()) {
                CoreConfig.newProxyCredentials(editor);
            }
            editor.putString("dns_provider", allowed(value.optString("dnsProvider"),
                    new String[]{"subscription-doh", "cloudflare", "google", "quad9", "custom"}, "cloudflare"));
            editor.putString("custom_dns", value.optString("customDns", "").trim());
            editor.putString("routing_mode", allowed(value.optString("routingMode"),
                    new String[]{"full", "bypass", "proxy_only"}, "full"));
            editor.putString("ping_mode", allowed(value.optString("pingMethod"),
                    new String[]{"real", "tcp"}, "real"));
            editor.putBoolean("fragmentation", value.optBoolean("fragmentation", false));
            editor.putBoolean("mux", value.optBoolean("mux", false));
            String ipType = allowed(value.optString("ipType"),
                    new String[]{"auto", "ipv4", "ipv6"}, "auto");
            // Turning IPv6 off must not leave the core asking for IPv6 only.
            if ("ipv6".equals(ipType) && !value.optBoolean("ipv6Enabled", false)) ipType = "auto";
            editor.putString("ip_type", ipType);
            editor.putBoolean("route_only", value.optBoolean("routeOnly", false));
            editor.putBoolean("block_udp", value.optBoolean("blockUdp", false));
            editor.putString("memory_limit", allowed(value.optString("memoryLimit"),
                    new String[]{"0", "40", "60", "80", "100", "150"}, "0"));
            editor.putString("tun_mtu", allowed(value.optString("tunMtu"),
                    new String[]{"1280", "1360", "1400", "1500"}, "1400"));
            editor.putBoolean("ipv6_enabled", value.optBoolean("ipv6Enabled", false));
            editor.putBoolean("lan_direct", value.optBoolean("lanDirect", true));
            editor.putBoolean("subscription_rules", value.optBoolean("subscriptionRules", true));
            editor.putString("geoip_url", value.optString("geoipUrl", "").trim());
            editor.putString("geosite_url", value.optString("geositeUrl", "").trim());
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
            editor.commit();
            // The core is the single source of truth for what is valid.
            CoreConfig.configure(this, preferences);
        } catch (Exception error) {
            restorePreferences(previous);
            settingsJsonCache = "";
            ShadowVpnService.addLog("Настройки не сохранены: " + cleanError(error));
            uiMessage = "Не сохранено · " + cleanError(error);
            pushSnapshot();
            return;
        }
        settingsJsonCache = "";
        boolean connected = ShadowVpnService.connected;
        // Proxy only and VPN are different kinds of connection; switching needs a reconnect.
        boolean reconnect = connected && (!tunBefore.equals(signature(TUN_KEYS))
                || proxyOnlyBefore != CoreConfig.proxyOnly(preferences));
        boolean reload = connected && !reconnect && !coreBefore.equals(signature(CORE_KEYS));
        uiMessage = reconnect ? "Настройки сохранены, переподключаемся"
                : reload ? "Применяем настройки…" : "Настройки сохранены";
        pushSnapshot();
        if (preferences.getBoolean("notify_updates", false) && Build.VERSION.SDK_INT >= 33
                && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS)
                        != PackageManager.PERMISSION_GRANTED) {
            requestPermissions(new String[]{Manifest.permission.POST_NOTIFICATIONS}, 102);
        }
        if (reconnect) startSelectedServer();
        else if (reload) reloadCore();
    }

    @SuppressWarnings("unchecked")
    private static String cleanPort(String value, int fallback) {
        try {
            int port = Integer.parseInt(value.trim());
            if (port >= 1024 && port <= 65535) return String.valueOf(port);
        } catch (Exception ignored) { }
        return String.valueOf(fallback);
    }

    /** The phone's IPv4 addresses on Wi-Fi, hotspot and USB/Ethernet tethering. */
    private static JSONArray localAddresses() {
        JSONArray result = new JSONArray();
        try {
            for (java.net.NetworkInterface face : java.util.Collections.list(
                    java.net.NetworkInterface.getNetworkInterfaces())) {
                if (!face.isUp() || face.isLoopback() || face.getName().startsWith("tun")) continue;
                if (face.getName().startsWith("rmnet") || face.getName().startsWith("ccmni")) continue;
                for (java.net.InetAddress address : java.util.Collections.list(face.getInetAddresses())) {
                    if (address instanceof java.net.Inet4Address && address.isSiteLocalAddress()) {
                        result.put(address.getHostAddress());
                    }
                }
            }
        } catch (Exception ignored) { }
        return result;
    }

    private void resetProxyCredentials() {
        SharedPreferences.Editor editor = preferences.edit();
        CoreConfig.newProxyCredentials(editor);
        editor.commit();
        settingsJsonCache = "";
        try {
            CoreConfig.configure(this, preferences);
            if (ShadowVpnService.connected) reloadCore();
            uiMessage = "Новые логин и пароль прокси";
        } catch (Exception error) {
            uiMessage = cleanError(error);
        }
        pushSnapshot();
    }

    private void restorePreferences(Map<String, ?> values) {
        settingsJsonCache = "";
        SharedPreferences.Editor editor = preferences.edit().clear();
        for (Map.Entry<String, ?> entry : values.entrySet()) {
            Object value = entry.getValue();
            if (value instanceof String) editor.putString(entry.getKey(), (String) value);
            else if (value instanceof Boolean) editor.putBoolean(entry.getKey(), (Boolean) value);
            else if (value instanceof Long) editor.putLong(entry.getKey(), (Long) value);
            else if (value instanceof Integer) editor.putInt(entry.getKey(), (Integer) value);
            else if (value instanceof Float) editor.putFloat(entry.getKey(), (Float) value);
            else if (value instanceof Set) editor.putStringSet(entry.getKey(), (Set<String>) value);
        }
        editor.commit();
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

    private void notifySelectedServer(String id) {
        if (!webReady || webView == null) return;
        webView.evaluateJavascript("window.ShadowVPN&&window.ShadowVPN.selectedServer("
                + JSONObject.quote(id) + ")", null);
    }

    private final class AndroidBridge {
        @JavascriptInterface public void ready() { ui.post(MainActivity.this::pushSnapshot); }
        @JavascriptInterface public void toggleVpn() { ui.post(MainActivity.this::toggleVpn); }

        @JavascriptInterface public void selectServer(String id) {
            ui.post(() -> {
                for (ServerItem server : servers) {
                    if (server.id.equals(id)) {
                        if (ShadowVpnService.connected && !id.equals(selectedId)) {
                            switchServer(server);
                            return;
                        }
                        selectedId = id;
                        preferences.edit().putString("selected_id", id).apply();
                        rememberSelectedName();
                        uiMessage = "";
                        notifySelectedServer(id);
                        pushSnapshot();
                        return;
                    }
                }
            });
        }

        @JavascriptInterface public void refreshSubscriptions() {
            ui.post(() -> importSubscriptions(true));
        }

        @JavascriptInterface public void scanQr() { ui.post(MainActivity.this::scanQr); }
        @JavascriptInterface public void runDiagnostics() { ui.post(MainActivity.this::runDiagnostics); }
        @JavascriptInterface public void pingServers() { ui.post(MainActivity.this::runPings); }
        // Qualified, or the call would resolve to this bridge method itself.
        @JavascriptInterface public void pingServer(String id) {
            ui.post(() -> MainActivity.this.pingServer(id));
        }

        @JavascriptInterface public void saveSubscriptions(String json) {
            ui.post(() -> {
                try {
                    JSONObject input = new JSONObject(json);
                    String rawWifi = input.optString("wifi", "").trim();
                    String rawLte = input.optString("lte", "").trim();
                    String manual = input.optString("manual", "").trim();
                    if (manual.length() > 1024 * 1024) {
                        throw new IllegalArgumentException("Свои конфигурации больше 1 МБ");
                    }
                    String wifi = cleanSubscriptionUrl(rawWifi);
                    String lte = cleanSubscriptionUrl(rawLte);
                    if ((!rawWifi.isEmpty() && wifi.isEmpty()) || (!rawLte.isEmpty() && lte.isEmpty())) {
                        throw new IllegalArgumentException("Нужна корректная HTTPS-ссылка");
                    }
                    if (!wifi.isEmpty() && wifi.equals(lte)) {
                        throw new IllegalArgumentException("Для Wi-Fi и LTE нужны разные ссылки");
                    }
                    saveSubscriptionSlots(wifi, lte);
                    preferences.edit().putString("manual_configs", manual).apply();
                    if (!hasSources()) {
                        servers.clear();
                        subscriptionInfo.clear();
                        serversJsonCache = "";
                        selectedId = "";
                        preferences.edit().remove("selected_id")
                                .remove("subscription_info").apply();
                        uiMessage = "";
                        notifySelectedServer("");
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
            // Qualified: a bare call resolves to this bridge method and would
            // re-post itself forever without ever saving.
            ui.post(() -> MainActivity.this.saveSettings(json));
        }

        @JavascriptInterface public String getLogs() {
            String core = "";
            try { core = Bridge.logs(); } catch (Exception ignored) { }
            String app = ShadowVpnService.getLogs();
            return maskIps(core.trim().isEmpty() ? app : app + "\n— Xray —\n" + core);
        }

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

        // Opens the app's system page (Battery → Unrestricted is one tap away)
        // instead of asking for REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, which
        // Play Protect treats as a warning sign.
        @JavascriptInterface public void resetProxyCredentials() {
            ui.post(MainActivity.this::resetProxyCredentials);
        }

        @JavascriptInterface public void openBatterySettings() {
            ui.post(() -> {
                try {
                    startActivity(new Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS,
                            Uri.parse("package:" + getPackageName())));
                } catch (Exception firstError) {
                    try {
                        startActivity(new Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS));
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
            result.put("manual", preferences.getString("manual_configs", ""));
        } catch (Exception ignored) { }
        return result;
    }

    private void saveSubscriptionSlots(String wifi, String lte) {
        JSONArray urls = new JSONArray();
        if (!wifi.isEmpty()) urls.put(wifi);
        if (!lte.isEmpty() && !lte.equals(wifi)) urls.put(lte);
        serversJsonCache = "";
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

    /** Hides the host part of an address: the last two octets, or IPv6 after two groups. */
    static String maskIp(String ip) {
        if (ip == null || ip.isEmpty()) return "";
        String value = ip.trim();
        if (value.indexOf(':') < 0) return maskIps(value);
        String[] groups = value.split(":", 3);
        return groups.length < 3 ? "*" : groups[0] + ":" + groups[1] + ":*:*";
    }

    private static final java.util.regex.Pattern IPV4 = java.util.regex.Pattern.compile(
            "\\b(\\d{1,3})\\.(\\d{1,3})\\.\\d{1,3}\\.\\d{1,3}\\b");

    /** Masks public IPv4 addresses inside text; local ones identify no one. */
    static String maskIps(String text) {
        if (text == null || text.isEmpty()) return text == null ? "" : text;
        java.util.regex.Matcher matcher = IPV4.matcher(text);
        StringBuffer result = new StringBuffer();
        while (matcher.find()) {
            int a = Integer.parseInt(matcher.group(1));
            int b = Integer.parseInt(matcher.group(2));
            boolean local = a == 127 || a == 10 || a == 0 || (a == 192 && b == 168)
                    || (a == 172 && b >= 16 && b <= 31) || (a == 169 && b == 254);
            matcher.appendReplacement(result, local ? matcher.group()
                    : matcher.group(1) + "." + matcher.group(2) + ".*.*");
        }
        matcher.appendTail(result);
        return result.toString();
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
        final String format;
        final int members;

        ServerItem(String id, String name, String protocol, String transport, boolean auto,
                   int sourceIndex, String format, int members) {
            this.id = id;
            this.name = name;
            this.protocol = protocol;
            this.transport = transport;
            this.auto = auto;
            this.sourceIndex = sourceIndex;
            this.format = format;
            this.members = members;
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

    private static final class PingHost {
        final String id;
        final String protocol;
        final boolean russian;

        PingHost(String id, String protocol, boolean russian) {
            this.id = id;
            this.protocol = protocol;
            this.russian = russian;
        }
    }

    private static final class PingGroup {
        final String id;
        final List<String> members;

        PingGroup(String id, List<String> members) {
            this.id = id;
            this.members = members;
        }
    }

    private static final class PingResult {
        final String id;
        final PingData data;

        PingResult(String id, PingData data) {
            this.id = id;
            this.data = data;
        }
    }

    private static final class PingDelta {
        final String id;
        final PingData data;
        /** Restored from the last run rather than measured now. */
        final boolean cached;

        PingDelta(String id, PingData data) {
            this(id, data, false);
        }

        PingDelta(String id, PingData data, boolean cached) {
            this.id = id;
            this.data = data;
            this.cached = cached;
        }
    }

    private static final class PingData {
        final long latency;
        final boolean available;
        /** Only the server's port answered; the check through it did not pass. */
        final boolean direct;

        PingData(long latency, boolean available) {
            this(latency, available, false);
        }

        PingData(long latency, boolean available, boolean direct) {
            this.latency = available ? Math.max(0, latency) : 0;
            this.available = available;
            this.direct = available && direct;
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
