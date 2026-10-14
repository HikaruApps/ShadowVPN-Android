package net.shadownet.shadowvpn.android;

import android.content.BroadcastReceiver;
import android.content.Context;
import android.content.Intent;
import android.content.SharedPreferences;
import android.net.VpnService;

/** Connects after the phone boots when the user turned that on. */
public final class BootReceiver extends BroadcastReceiver {
    @Override public void onReceive(Context context, Intent intent) {
        if (!Intent.ACTION_BOOT_COMPLETED.equals(intent.getAction())) return;
        SharedPreferences preferences = CoreConfig.preferences(context);
        if (!preferences.getBoolean("auto_connect_boot", false)) return;
        if (preferences.getString("selected_id", "").isEmpty()) return;
        // Without a granted VPN consent there is nobody to show the dialog to;
        // the same is true when another app holds the always-on slot.
        if (VpnService.prepare(context) != null) return;
        if (ShadowVpnService.connected) return;
        context.startForegroundService(new Intent(context, ShadowVpnService.class)
                .setAction(ShadowVpnService.START));
    }
}
