package net.shadownet.shadowvpn.android;

import android.app.PendingIntent;
import android.graphics.drawable.Icon;
import android.os.Build;
import android.service.quicksettings.Tile;
import android.service.quicksettings.TileService;

/** Quick Settings tile: one tap connects or disconnects. */
public final class ShadowTileService extends TileService {
    @Override public void onStartListening() {
        render();
    }

    @Override public void onClick() {
        if (VpnControl.isActive()) {
            VpnControl.disconnect(this);
        } else if (VpnControl.canConnectNow(this)) {
            try {
                VpnControl.connect(this);
            } catch (RuntimeException notAllowed) {
                // Some Android versions do not let a tile start a foreground
                // service; the app finishes the job instead.
                openApp();
            }
        } else {
            openApp();
        }
        render();
    }

    private void openApp() {
        if (Build.VERSION.SDK_INT >= 34) {
            startActivityAndCollapse(PendingIntent.getActivity(this, 0,
                    VpnControl.connectInApp(this), PendingIntent.FLAG_IMMUTABLE));
        } else {
            startActivityAndCollapse(VpnControl.connectInApp(this));
        }
    }

    private void render() {
        Tile tile = getQsTile();
        if (tile == null) return;
        boolean active = VpnControl.isActive();
        tile.setState(active ? Tile.STATE_ACTIVE : Tile.STATE_INACTIVE);
        tile.setIcon(Icon.createWithResource(this, R.drawable.ic_stat_shadowvpn));
        tile.setLabel("ShadowVPN");
        String server = VpnControl.serverName(this);
        tile.setSubtitle(active && !server.isEmpty() ? server : VpnControl.statusLine());
        tile.updateTile();
    }
}
