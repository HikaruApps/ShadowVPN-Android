package net.shadownet.shadowvpn.android;

import android.content.Context;
import android.content.SharedPreferences;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.provider.Settings;

import net.shadownet.shadowvpn.core.bridge.Bridge;

import org.json.JSONArray;
import org.json.JSONObject;

/**
 * Builds the Go core's options from preferences. The activity and the VPN
 * service (including an always-on start after boot) share it, so a connection
 * always uses exactly what the settings screen saved.
 */
final class CoreConfig {
    static final String PREFERENCES = "shadowvpn";
    static final String DEFAULT_DOH = "https://1.1.1.1/dns-query";

    private static boolean initialized;

    private CoreConfig() { }

    static SharedPreferences preferences(Context context) {
        return context.getSharedPreferences(PREFERENCES, Context.MODE_PRIVATE);
    }

    /** Points the core at app-private storage once per process. */
    static synchronized void initialize(Context context) {
        if (initialized) return;
        try {
            Bridge.setDataDir(context.getFilesDir().getAbsolutePath() + "/core");
            Bridge.setDeviceSeed(Settings.Secure.getString(
                    context.getContentResolver(), Settings.Secure.ANDROID_ID));
        } catch (Exception ignored) { }
        initialized = true;
    }

    static JSONObject options(Context context, SharedPreferences preferences) throws Exception {
        String doh = preferences.getString("doh", DEFAULT_DOH);
        String dnsId = preferences.getString("dns_provider",
                doh.isEmpty() ? "cloudflare" : "subscription-doh");
        JSONObject options = new JSONObject();
        options.put("dnsId", dnsId);
        options.put("dohUrl", "subscription-doh".equals(dnsId) ? doh : "");
        options.put("customDns", lines(preferences.getString("custom_dns", "")));
        options.put("fragmentation", preferences.getBoolean("fragmentation", false));
        options.put("tunMtu", mtu(preferences));
        options.put("ipv6", preferences.getBoolean("ipv6_enabled", false));
        options.put("routingMode", preferences.getString("routing_mode", "full"));
        options.put("routingRules", lines(preferences.getString("routing_rules", "")));
        options.put("subscriptionRules", preferences.getBoolean("subscription_rules", true));
        options.put("geoipUrl", preferences.getString("geoip_url", ""));
        options.put("geositeUrl", preferences.getString("geosite_url", ""));
        options.put("pingMethod", preferences.getString("ping_method", "tcp"));
        options.put("mux", preferences.getBoolean("mux", false));
        options.put("ipType", preferences.getString("ip_type", "auto"));
        options.put("routeOnly", preferences.getBoolean("route_only", false));
        options.put("blockUdp", preferences.getBoolean("block_udp", false));
        options.put("memoryLimitMb", memoryLimit(preferences));
        options.put("localProxy", localProxy(preferences));
        try {
            options.put("autoProfileIds", new JSONArray(preferences.getString("auto_profiles", "[]")));
        } catch (Exception ignored) {
            options.put("autoProfileIds", new JSONArray());
        }
        options.put("preferredSource", preferredSource(context, preferences));
        return options;
    }

    /** Applies the saved options to the core; throws with a user-facing message if invalid. */
    static void configure(Context context, SharedPreferences preferences) throws Exception {
        Bridge.configure(options(context, preferences).toString());
    }

    /**
     * With separate Wi-Fi and mobile subscriptions, Auto picks servers from the
     * one that matches the current network. Sources are imported in the order
     * Wi-Fi, LTE; -1 means "no preference".
     */
    static int preferredSource(Context context, SharedPreferences preferences) {
        boolean hasWifi = !preferences.getString("subscription_wifi", "").trim().isEmpty();
        boolean hasLte = !preferences.getString("subscription_lte", "").trim().isEmpty();
        if (!hasWifi || !hasLte) return -1;
        int transport = currentTransport(context);
        if (transport == NetworkCapabilities.TRANSPORT_WIFI) return 0;
        if (transport == NetworkCapabilities.TRANSPORT_CELLULAR) return 1;
        return -1;
    }

    /** Transport of the physical default network (the app itself bypasses the VPN). */
    static int currentTransport(Context context) {
        ConnectivityManager manager = context.getSystemService(ConnectivityManager.class);
        if (manager == null) return -1;
        NetworkCapabilities capabilities;
        try {
            Network network = manager.getActiveNetwork();
            capabilities = network == null ? null : manager.getNetworkCapabilities(network);
        } catch (RuntimeException error) {
            // Not knowing the network only disables the Wi-Fi/LTE preference;
            // it must never stop the VPN from connecting.
            return -1;
        }
        if (capabilities == null) return -1;
        if (capabilities.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)
                || capabilities.hasTransport(NetworkCapabilities.TRANSPORT_ETHERNET)) {
            return NetworkCapabilities.TRANSPORT_WIFI;
        }
        if (capabilities.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR)) {
            return NetworkCapabilities.TRANSPORT_CELLULAR;
        }
        return -1;
    }

    static int mtu(SharedPreferences preferences) {
        try {
            int mtu = Integer.parseInt(preferences.getString("tun_mtu", "1400"));
            return Math.max(1280, Math.min(1500, mtu));
        } catch (Exception ignored) {
            return 1400;
        }
    }

    static boolean proxyEnabled(SharedPreferences preferences) {
        return preferences.getBoolean("proxy_enabled", false);
    }

    /** Proxy only: the core runs the local proxy without a TUN, so no VPN appears in Android. */
    static boolean proxyOnly(SharedPreferences preferences) {
        return proxyEnabled(preferences) && preferences.getBoolean("proxy_only", false);
    }

    static int port(SharedPreferences preferences, String key, int fallback) {
        try {
            int port = Integer.parseInt(preferences.getString(key, String.valueOf(fallback)));
            return port >= 1024 && port <= 65535 ? port : fallback;
        } catch (Exception ignored) {
            return fallback;
        }
    }

    private static JSONObject localProxy(SharedPreferences preferences) throws Exception {
        JSONObject proxy = new JSONObject();
        boolean enabled = proxyEnabled(preferences);
        proxy.put("socksPort", enabled ? port(preferences, "proxy_socks_port", 10808) : 0);
        proxy.put("httpPort", enabled && preferences.getBoolean("proxy_http", false)
                ? port(preferences, "proxy_http_port", 10809) : 0);
        proxy.put("username", preferences.getString("proxy_user", ""));
        proxy.put("password", preferences.getString("proxy_pass", ""));
        proxy.put("lan", preferences.getBoolean("proxy_lan", false));
        return proxy;
    }

    /** Random credentials: the proxy must never be usable without them. */
    static void newProxyCredentials(SharedPreferences.Editor editor) {
        java.security.SecureRandom random = new java.security.SecureRandom();
        String alphabet = "abcdefghijkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789";
        StringBuilder user = new StringBuilder("svpn");
        for (int i = 0; i < 4; i++) user.append(alphabet.charAt(random.nextInt(alphabet.length())));
        StringBuilder password = new StringBuilder();
        for (int i = 0; i < 16; i++) password.append(alphabet.charAt(random.nextInt(alphabet.length())));
        editor.putString("proxy_user", user.toString()).putString("proxy_pass", password.toString());
    }

    static int memoryLimit(SharedPreferences preferences) {
        try {
            return Integer.parseInt(preferences.getString("memory_limit", "0"));
        } catch (Exception ignored) {
            return 0;
        }
    }

    static JSONArray lines(String value) {
        JSONArray result = new JSONArray();
        for (String line : value.split("\\r?\\n")) {
            if (!line.trim().isEmpty()) result.put(line.trim());
        }
        return result;
    }
}
