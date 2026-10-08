package net.shadownet.shadowvpn.android;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.content.SharedPreferences;
import android.net.ConnectivityManager;
import android.net.Network;
import android.net.NetworkCapabilities;
import android.net.VpnService;
import android.os.ParcelFileDescriptor;

import net.shadownet.shadowvpn.core.bridge.Bridge;

import org.json.JSONArray;

import java.math.BigInteger;
import java.net.InetAddress;
import java.util.ArrayDeque;
import java.util.Deque;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public final class ShadowVpnService extends VpnService {
    public static final String START = "net.shadownet.shadowvpn.START";
    public static final String STOP = "net.shadownet.shadowvpn.STOP";
    public static volatile String status = "Отключено";
    public static volatile boolean connected = false;
    private static final Deque<String> logs = new ArrayDeque<>();
    private static final String AUTO_PREFIX = "00000000000000000000000";

    // Local networks stay outside the tunnel so printers, routers and casting work.
    private static final String[] LAN_V4 = {
            "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "224.0.0.0/3"};
    private static final String[] LAN_V6 = {"fc00::/7", "fe80::/10", "ff00::/8"};

    private final ExecutorService executor = Executors.newSingleThreadExecutor();
    private ParcelFileDescriptor tunnel;
    private ConnectivityManager.NetworkCallback networkCallback;
    private volatile int lastTransport = -2;

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? null : intent.getAction();
        if (STOP.equals(action)) {
            executor.execute(() -> {
                closeTunnel(true);
                stopForeground(STOP_FOREGROUND_REMOVE);
                stopSelf();
            });
            return START_NOT_STICKY;
        }
        // START comes from the app; anything else is the system starting an
        // always-on VPN, which must connect with the saved settings.
        createForegroundNotification();
        status = "Подключение…";
        executor.execute(this::connect);
        return START_NOT_STICKY;
    }

    private void connect() {
        closeTunnel(false);
        status = "Подключение…";
        SharedPreferences preferences = CoreConfig.preferences(this);
        String profileId = preferences.getString("selected_id", "");
        try {
            CoreConfig.initialize(this);
            // An always-on start after boot has no servers in memory yet.
            Bridge.loadCachedProfiles();
            if (profileId.isEmpty()) throw new IllegalStateException("Сервер не выбран");
            CoreConfig.configure(this, preferences);
            addLog("Подготовка " + (profileId.startsWith(AUTO_PREFIX) ? "Auto" : "профиля " + profileId));
            Bridge.prepare(profileId);

            int mtu = CoreConfig.mtu(preferences);
            boolean ipv6 = preferences.getBoolean("ipv6_enabled", false);
            boolean lanDirect = preferences.getBoolean("lan_direct", true);
            String appMode = preferences.getString("app_routing_mode", "all");
            String appPackages = preferences.getString("app_routing_packages", "[]");
            Builder builder = new Builder()
                    .setSession("ShadowVPN")
                    .setMtu(mtu)
                    .setMetered(false)
                    .setBlocking(true)
                    .addAddress("172.31.255.2", 30)
                    .addDnsServer("1.1.1.1");
            addRoutes(builder, "0.0.0.0/0", lanDirect ? LAN_V4 : new String[0]);
            if (ipv6) {
                // Without an IPv6 address Android blocks IPv6 for VPN apps, so
                // nothing leaks when IPv6 is off.
                builder.addAddress("fd31:ffff::2", 126).addDnsServer("2606:4700:4700::1111");
                addRoutes(builder, "::/0", lanDirect ? LAN_V6 : new String[0]);
            }
            applyApplicationRouting(builder, appMode, appPackages);
            addLog("TUN: MTU " + mtu + ", IPv6 " + (ipv6 ? "вкл" : "выкл")
                    + ", локальная сеть " + (lanDirect ? "напрямую" : "через VPN")
                    + ", приложения: " + appMode);
            tunnel = builder.establish();
            if (tunnel == null) {
                throw new IllegalStateException("Android не создал VPN-интерфейс");
            }
            Bridge.start(tunnel.getFd());
            connected = true;
            status = "Подключено";
            addLog("Xray запущен");
            updateNotification("VPN подключён");
            watchNetwork(profileId.startsWith(AUTO_PREFIX) && CoreConfig.preferredSource(this, preferences) >= 0);
        } catch (Exception error) {
            status = "Ошибка: " + (error.getMessage() == null
                    ? "не удалось подключиться" : error.getMessage());
            addLog(status);
            closeTunnel(false);
            stopForeground(STOP_FOREGROUND_REMOVE);
            stopSelf();
        }
    }

    /**
     * Auto with separate Wi-Fi and mobile subscriptions must switch server
     * sets when the phone changes networks.
     */
    private void watchNetwork(boolean enabled) {
        unwatchNetwork();
        if (!enabled) return;
        ConnectivityManager manager = getSystemService(ConnectivityManager.class);
        lastTransport = CoreConfig.currentTransport(this);
        networkCallback = new ConnectivityManager.NetworkCallback() {
            @Override public void onCapabilitiesChanged(Network network, NetworkCapabilities capabilities) {
                int transport = CoreConfig.currentTransport(ShadowVpnService.this);
                if (transport < 0 || transport == lastTransport) return;
                lastTransport = transport;
                addLog("Сеть изменилась, переподключение Auto");
                executor.execute(ShadowVpnService.this::connect);
            }
        };
        try {
            manager.registerDefaultNetworkCallback(networkCallback);
        } catch (Exception error) {
            networkCallback = null;
        }
    }

    private void unwatchNetwork() {
        if (networkCallback == null) return;
        try {
            getSystemService(ConnectivityManager.class).unregisterNetworkCallback(networkCallback);
        } catch (Exception ignored) { }
        networkCallback = null;
    }

    /** Adds routes covering {@code universe} minus the {@code excluded} prefixes. */
    static void addRoutes(Builder builder, String universe, String[] excluded) throws Exception {
        Prefix root = Prefix.parse(universe);
        Prefix[] holes = new Prefix[excluded.length];
        for (int i = 0; i < excluded.length; i++) holes[i] = Prefix.parse(excluded[i]);
        addComplement(builder, root, holes);
    }

    private static void addComplement(Builder builder, Prefix prefix, Prefix[] holes) throws Exception {
        boolean overlaps = false;
        for (Prefix hole : holes) {
            if (hole.contains(prefix)) return;
            if (prefix.contains(hole)) overlaps = true;
        }
        if (!overlaps) {
            builder.addRoute(prefix.address(), prefix.length);
            return;
        }
        addComplement(builder, prefix.half(false), holes);
        addComplement(builder, prefix.half(true), holes);
    }

    private static final class Prefix {
        final BigInteger start;
        final int length;
        final int bits;

        Prefix(BigInteger start, int length, int bits) {
            this.start = start;
            this.length = length;
            this.bits = bits;
        }

        static Prefix parse(String cidr) throws Exception {
            String[] parts = cidr.split("/");
            byte[] raw = InetAddress.getByName(parts[0]).getAddress();
            return new Prefix(new BigInteger(1, raw), Integer.parseInt(parts[1]), raw.length * 8);
        }

        boolean contains(Prefix other) {
            if (other.bits != bits || other.length < length) return false;
            int shift = bits - length;
            return other.start.shiftRight(shift).equals(start.shiftRight(shift));
        }

        Prefix half(boolean upper) {
            BigInteger next = upper ? start.setBit(bits - length - 1) : start;
            return new Prefix(next, length + 1, bits);
        }

        InetAddress address() throws Exception {
            byte[] value = start.toByteArray();
            byte[] raw = new byte[bits / 8];
            int copy = Math.min(value.length, raw.length);
            System.arraycopy(value, value.length - copy, raw, raw.length - copy, copy);
            return InetAddress.getByAddress(raw);
        }
    }

    private void applyApplicationRouting(Builder builder, String mode, String packagesJson)
            throws Exception {
        JSONArray packages;
        try {
            packages = new JSONArray(packagesJson);
        } catch (Exception ignored) {
            packages = new JSONArray();
        }
        if ("include".equals(mode) && packages.length() > 0) {
            int allowedCount = 0;
            for (int i = 0; i < packages.length(); i++) {
                String packageName = packages.optString(i, "");
                if (!packageName.isEmpty() && !getPackageName().equals(packageName)) {
                    if (addAllowedPackage(builder, packageName)) allowedCount++;
                }
            }
            if (allowedCount > 0) return;
            addLog("Выбранные приложения не установлены; используется режим «Все»");
        }

        // Xray runs inside this app and must always reach the physical network.
        builder.addDisallowedApplication(getPackageName());
        if ("exclude".equals(mode)) {
            for (int i = 0; i < packages.length(); i++) {
                String packageName = packages.optString(i, "");
                if (!packageName.isEmpty() && !getPackageName().equals(packageName)) {
                    addDisallowedPackage(builder, packageName);
                }
            }
        }
    }

    private boolean addAllowedPackage(Builder builder, String packageName) {
        try {
            builder.addAllowedApplication(packageName);
            return true;
        } catch (android.content.pm.PackageManager.NameNotFoundException error) {
            addLog("Приложение не найдено: " + packageName);
            return false;
        }
    }

    private void addDisallowedPackage(Builder builder, String packageName) {
        try {
            builder.addDisallowedApplication(packageName);
        } catch (android.content.pm.PackageManager.NameNotFoundException error) {
            addLog("Приложение не найдено: " + packageName);
        }
    }

    private void createForegroundNotification() {
        NotificationManager manager = getSystemService(NotificationManager.class);
        manager.createNotificationChannel(new NotificationChannel(
                "vpn", "VPN-соединение", NotificationManager.IMPORTANCE_LOW));
        startForeground(1, notification("Подключение…"));
    }

    private void updateNotification(String text) {
        getSystemService(NotificationManager.class).notify(1, notification(text));
    }

    private Notification notification(String text) {
        PendingIntent open = PendingIntent.getActivity(this, 0,
                new Intent(this, MainActivity.class), PendingIntent.FLAG_IMMUTABLE);
        return new Notification.Builder(this, "vpn")
                .setContentTitle("ShadowVPN")
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_sys_upload_done)
                .setContentIntent(open)
                .setOngoing(true)
                .build();
    }

    private void closeTunnel(boolean updateStatus) {
        if (updateStatus) unwatchNetwork();
        try { Bridge.stop(); } catch (Exception ignored) { }
        try { if (tunnel != null) tunnel.close(); } catch (Exception ignored) { }
        tunnel = null;
        connected = false;
        if (updateStatus) status = "Отключено";
        if (updateStatus) addLog("VPN отключён");
    }

    static synchronized void addLog(String value) {
        if (logs.size() >= 120) logs.removeFirst();
        logs.addLast(String.format(java.util.Locale.ROOT, "%tT  %s", new java.util.Date(), value));
    }

    public static synchronized String getLogs() {
        StringBuilder result = new StringBuilder();
        for (String line : logs) result.append(line).append('\n');
        return result.toString();
    }

    @Override public void onRevoke() {
        executor.execute(() -> {
            closeTunnel(true);
            stopSelf();
        });
        super.onRevoke();
    }

    @Override public void onDestroy() {
        executor.execute(() -> closeTunnel(true));
        executor.shutdown();
        super.onDestroy();
    }
}
