package net.shadownet.shadowvpn.android;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.net.VpnService;
import android.os.ParcelFileDescriptor;

import net.shadownet.shadowvpn.core.bridge.Bridge;

import org.json.JSONArray;

import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.ArrayDeque;
import java.util.Deque;

public final class ShadowVpnService extends VpnService {
    public static final String START = "net.shadownet.shadowvpn.START";
    public static final String STOP = "net.shadownet.shadowvpn.STOP";
    public static volatile String status = "Отключено";
    public static volatile boolean connected = false;
    private static final Deque<String> logs = new ArrayDeque<>();

    private final ExecutorService executor = Executors.newSingleThreadExecutor();
    private ParcelFileDescriptor tunnel;

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
        if (!START.equals(action)) return START_NOT_STICKY;
        String profileId = intent.getStringExtra("profileId");
        if (profileId == null) return START_NOT_STICKY;
        String doh = intent.getStringExtra("dnsDoh");
        final String selectedDoh = doh == null ? "" : doh;
        final int mtu = Math.max(1280, Math.min(1500, intent.getIntExtra("mtu", 1400)));
        final boolean ipv6Enabled = intent.getBooleanExtra("ipv6Enabled", false);
        final String appRoutingMode = intent.getStringExtra("appRoutingMode") == null
                ? "all" : intent.getStringExtra("appRoutingMode");
        final String appPackages = intent.getStringExtra("appPackages") == null
                ? "[]" : intent.getStringExtra("appPackages");
        createForegroundNotification();
        executor.execute(() -> connect(profileId, selectedDoh, mtu, ipv6Enabled,
                appRoutingMode, appPackages));
        return START_NOT_STICKY;
    }

    private void connect(String profileId, String doh, int mtu, boolean ipv6Enabled,
                         String appRoutingMode, String appPackages) {
        closeTunnel(false);
        status = "Подключение…";
        addLog("Подготовка профиля " + profileId);
        try {
            Bridge.prepare(profileId, doh);
            Builder builder = new Builder()
                    .setSession("ShadowVPN")
                    .setMtu(mtu)
                    .addAddress("172.31.255.2", 30)
                    .addRoute("0.0.0.0", 0)
                    .addDnsServer("1.1.1.1")
                    .setBlocking(true);
            if (ipv6Enabled) {
                builder.addAddress("fd31:ffff::2", 126)
                        .addRoute("::", 0)
                        .addDnsServer("2606:4700:4700::1111");
            }
            applyApplicationRouting(builder, appRoutingMode, appPackages);
            addLog("TUN: MTU " + mtu + ", IPv6 " + (ipv6Enabled ? "включён" : "выключен")
                    + ", приложения: " + appRoutingMode);
            tunnel = builder.establish();
            if (tunnel == null) {
                throw new IllegalStateException("Android не создал VPN-интерфейс");
            }
            Bridge.start(tunnel.getFd());
            connected = true;
            status = "Подключено";
            addLog("TUN-интерфейс и Xray запущены");
            updateNotification("VPN подключён");
        } catch (Exception error) {
            status = "Ошибка: " + (error.getMessage() == null
                    ? "не удалось подключиться" : error.getMessage());
            addLog(status);
            closeTunnel(false);
            stopForeground(STOP_FOREGROUND_REMOVE);
            stopSelf();
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
            addLog("Список выбранных приложений пуст; используется режим «Все»");
        }

        // Процесс Xray работает внутри ShadowVPN и всегда должен выходить в физическую сеть.
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
        try { Bridge.stop(); } catch (Exception ignored) { }
        try { if (tunnel != null) tunnel.close(); } catch (Exception ignored) { }
        tunnel = null;
        connected = false;
        if (updateStatus) status = "Отключено";
        if (updateStatus) addLog("VPN отключён");
    }

    private static synchronized void addLog(String value) {
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
