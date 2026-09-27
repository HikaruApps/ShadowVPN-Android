package net.shadownet.shadowvpn.android;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Intent;
import android.net.VpnService;
import android.os.ParcelFileDescriptor;

import net.shadownet.shadowvpn.core.bridge.Bridge;

import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public final class ShadowVpnService extends VpnService {
    public static final String START = "net.shadownet.shadowvpn.START";
    public static final String STOP = "net.shadownet.shadowvpn.STOP";
    public static volatile String status = "Отключено";
    public static volatile boolean connected = false;

    private final ExecutorService executor = Executors.newSingleThreadExecutor();
    private ParcelFileDescriptor tunnel;

    @Override public int onStartCommand(Intent intent, int flags, int startId) {
        String action = intent == null ? null : intent.getAction();
        if (STOP.equals(action)) {
            executor.execute(() -> { closeTunnel(true); stopSelf(); });
            return START_NOT_STICKY;
        }
        if (!START.equals(action)) return START_NOT_STICKY;
        String profileId = intent.getStringExtra("profileId");
        if (profileId == null) return START_NOT_STICKY;
        String dnsDoh = intent.getStringExtra("dnsDoh");
        if (dnsDoh == null) dnsDoh = "";
        final String selectedDoh = dnsDoh;
        createForegroundNotification();
        executor.execute(() -> {
            closeTunnel(true);
            status = "Подключение…";
            try {
                Bridge.prepare(profileId, selectedDoh);
                Builder builder = new Builder().setSession("ShadowVPN").setMtu(1500)
                    .addAddress("172.31.255.2", 30).addRoute("0.0.0.0", 0)
                    .addAddress("fd31:ffff::2", 126).addRoute("::", 0)
                    .addDnsServer("1.1.1.1").addDnsServer("2606:4700:4700::1111")
                    .addDisallowedApplication(getPackageName()).setBlocking(true);
                tunnel = builder.establish();
                if (tunnel == null) throw new IllegalStateException("Android не создал VPN-интерфейс");
                Bridge.start(tunnel.getFd());
                connected = true;
                status = "Подключено";
            } catch (Exception error) {
                status = "Ошибка: " + (error.getMessage() == null ? "не удалось подключиться" : error.getMessage());
                closeTunnel(false);
                stopSelf();
            }
        });
        return START_NOT_STICKY;
    }

    private void createForegroundNotification() {
        NotificationManager manager = getSystemService(NotificationManager.class);
        manager.createNotificationChannel(new NotificationChannel(
            "vpn", "VPN-соединение", NotificationManager.IMPORTANCE_LOW));
        PendingIntent open = PendingIntent.getActivity(this, 0,
            new Intent(this, MainActivity.class), PendingIntent.FLAG_IMMUTABLE);
        Notification notification = new Notification.Builder(this, "vpn")
            .setContentTitle("ShadowVPN")
            .setContentText("VPN запущен")
            .setSmallIcon(android.R.drawable.stat_sys_upload_done)
            .setContentIntent(open).setOngoing(true).build();
        startForeground(1, notification);
    }

    private void closeTunnel(boolean updateStatus) {
        try { Bridge.stop(); } catch (Exception ignored) { }
        try { if (tunnel != null) tunnel.close(); } catch (Exception ignored) { }
        tunnel = null;
        connected = false;
        if (updateStatus) status = "Отключено";
    }

    @Override public void onRevoke() {
        executor.execute(() -> { closeTunnel(true); stopSelf(); });
        super.onRevoke();
    }

    @Override public void onDestroy() {
        // Serialise fd close with the Xray start/stop operations.
        executor.execute(() -> closeTunnel(true));
        executor.shutdown();
        super.onDestroy();
    }
}
