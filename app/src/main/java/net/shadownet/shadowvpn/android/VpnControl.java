package net.shadownet.shadowvpn.android;

import android.appwidget.AppWidgetManager;
import android.content.ComponentName;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.net.VpnService;
import android.service.quicksettings.TileService;

/**
 * Connecting and disconnecting from outside the app (Quick Settings tile,
 * launcher shortcuts, home screen widget), and keeping those surfaces in
 * step with the connection.
 */
final class VpnControl {
    static final String ACTION_CONNECT = "net.shadownet.shadowvpn.action.CONNECT";
    static final String ACTION_DISCONNECT = "net.shadownet.shadowvpn.action.DISCONNECT";
    static final String ACTION_SERVERS = "net.shadownet.shadowvpn.action.SERVERS";

    private VpnControl() { }

    static boolean isActive() {
        return ShadowVpnService.connected || ShadowVpnService.blocked
                || "Подключение…".equals(ShadowVpnService.status);
    }

    /**
     * True when connecting needs nothing from the user: a server is chosen
     * and Android already granted the VPN permission (proxy-only mode needs
     * none).
     */
    static boolean canConnectNow(Context context) {
        SharedPreferences preferences = CoreConfig.preferences(context);
        if (preferences.getString("selected_id", "").isEmpty()) return false;
        return CoreConfig.proxyOnly(preferences) || VpnService.prepare(context) == null;
    }

    /** Starts the service; it reads the saved settings and server itself. */
    static void connect(Context context) {
        ShadowVpnService.status = "Подключение…";
        context.startForegroundService(new Intent(context, ShadowVpnService.class)
                .setAction(ShadowVpnService.START));
        refreshSurfaces(context);
    }

    static void disconnect(Context context) {
        context.startService(new Intent(context, ShadowVpnService.class)
                .setAction(ShadowVpnService.STOP));
    }

    /** Opens the app to connect when Android or the user still has to agree. */
    static Intent connectInApp(Context context) {
        return new Intent(context, MainActivity.class).setAction(ACTION_CONNECT)
                .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK);
    }

    static String statusLine() {
        if (ShadowVpnService.connected) return "Подключено";
        if (ShadowVpnService.blocked) return "Интернет заблокирован";
        String status = ShadowVpnService.status;
        if (status.startsWith("Ошибка")) return "Ошибка подключения";
        return "Подключение…".equals(status) ? status : "Не подключено";
    }

    static String serverName(Context context) {
        return CoreConfig.preferences(context).getString("selected_name", "");
    }

    /** Tile and widget show the live state; called whenever it changes. */
    static void refreshSurfaces(Context context) {
        try {
            TileService.requestListeningState(context, new ComponentName(context, ShadowTileService.class));
        } catch (RuntimeException ignored) { }
        try {
            AppWidgetManager manager = AppWidgetManager.getInstance(context);
            int[] ids = manager.getAppWidgetIds(new ComponentName(context, VpnWidget.class));
            if (ids.length > 0) VpnWidget.update(context, manager, ids);
        } catch (RuntimeException ignored) { }
    }
}
