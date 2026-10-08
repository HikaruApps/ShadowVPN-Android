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
        Network network = manager.getActiveNetwork();
        NetworkCapabilities capabilities = network == null ? null : manager.getNetworkCapabilities(network);
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

    static JSONArray lines(String value) {
        JSONArray result = new JSONArray();
        for (String line : value.split("\\r?\\n")) {
            if (!line.trim().isEmpty()) result.put(line.trim());
        }
        return result;
    }
}
