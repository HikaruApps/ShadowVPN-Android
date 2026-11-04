package net.shadownet.shadowvpn.android;

import android.app.PendingIntent;
import android.appwidget.AppWidgetManager;
import android.appwidget.AppWidgetProvider;
import android.content.Context;
import android.content.Intent;
import android.widget.RemoteViews;

/** Home screen widget: status, server and a connect / disconnect button. */
public final class VpnWidget extends AppWidgetProvider {
    @Override public void onUpdate(Context context, AppWidgetManager manager, int[] ids) {
        update(context, manager, ids);
    }

    static void update(Context context, AppWidgetManager manager, int[] ids) {
        boolean active = VpnControl.isActive();
        RemoteViews views = new RemoteViews(context.getPackageName(), R.layout.widget_vpn);
        views.setTextViewText(R.id.widget_status, VpnControl.statusLine());
        String server = VpnControl.serverName(context);
        views.setTextViewText(R.id.widget_server, server.isEmpty() ? "Сервер не выбран" : server);
        views.setTextViewText(R.id.widget_toggle, active ? "Отключить" : "Подключить");
        views.setInt(R.id.widget_toggle, "setBackgroundResource",
                active ? R.drawable.widget_button_active : R.drawable.widget_button);
        views.setTextColor(R.id.widget_toggle, active ? 0xFFF5F5F5 : 0xFF0A0A0A);
        views.setInt(R.id.widget_dot, "setBackgroundResource",
                ShadowVpnService.connected ? R.drawable.widget_dot_on : R.drawable.widget_dot_off);

        // The button acts directly where it can: a tap on a widget may start
        // a foreground service. Otherwise it opens the app to finish.
        PendingIntent toggle;
        if (active) {
            toggle = PendingIntent.getService(context, 1, new Intent(context, ShadowVpnService.class)
                    .setAction(ShadowVpnService.STOP), PendingIntent.FLAG_IMMUTABLE);
        } else if (VpnControl.canConnectNow(context)) {
            toggle = PendingIntent.getForegroundService(context, 2, new Intent(context, ShadowVpnService.class)
                    .setAction(ShadowVpnService.START), PendingIntent.FLAG_IMMUTABLE);
        } else {
            toggle = PendingIntent.getActivity(context, 3, VpnControl.connectInApp(context),
                    PendingIntent.FLAG_IMMUTABLE);
        }
        views.setOnClickPendingIntent(R.id.widget_toggle, toggle);
        views.setOnClickPendingIntent(R.id.widget_root, PendingIntent.getActivity(context, 4,
                new Intent(context, MainActivity.class), PendingIntent.FLAG_IMMUTABLE));
        manager.updateAppWidget(ids, views);
    }
}
