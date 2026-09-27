package net.shadownet.shadowvpn.android;

import android.app.Activity;
import android.content.Intent;
import android.content.SharedPreferences;
import android.net.VpnService;
import android.os.Bundle;
import android.os.Handler;
import android.os.Looper;
import android.provider.Settings;
import android.view.ViewGroup;
import android.widget.Button;
import android.widget.EditText;
import android.widget.LinearLayout;
import android.widget.ScrollView;
import android.widget.TextView;

import net.shadownet.shadowvpn.core.bridge.Bridge;

import org.json.JSONArray;
import org.json.JSONObject;

import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;

public final class MainActivity extends Activity {
    private final ExecutorService worker = Executors.newSingleThreadExecutor();
    private final Handler handler = new Handler(Looper.getMainLooper());
    private EditText url;
    private TextView status;
    private TextView message;
    private LinearLayout servers;
    private String selectedId = "";
    private String doh = "";
    private SharedPreferences preferences;

    private final Runnable refresh = new Runnable() {
        @Override public void run() {
            status.setText(ShadowVpnService.status);
            handler.postDelayed(this, 1000);
        }
    };

    @Override public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        preferences = getSharedPreferences("subscription", MODE_PRIVATE);
        String androidId = Settings.Secure.getString(getContentResolver(), Settings.Secure.ANDROID_ID);
        Bridge.setDeviceSeed(androidId == null ? "" : androidId);

        LinearLayout column = new LinearLayout(this);
        column.setOrientation(LinearLayout.VERTICAL);
        column.setPadding(28, 48, 28, 28);
        TextView title = new TextView(this);
        title.setText("ShadowVPN · Android");
        title.setTextSize(24);
        column.addView(title);

        url = new EditText(this);
        url.setHint("HTTPS-ссылка на подписку");
        url.setSingleLine(true);
        url.setText(preferences.getString("url", ""));
        column.addView(url);

        Button importButton = new Button(this);
        importButton.setText("Загрузить серверы");
        importButton.setOnClickListener(view -> importSubscription());
        column.addView(importButton);

        status = new TextView(this);
        status.setTextSize(16);
        status.setPadding(0, 24, 0, 24);
        column.addView(status);
        message = new TextView(this);
        message.setTextSize(14);
        message.setPadding(0, 0, 0, 16);
        column.addView(message);

        Button connectButton = new Button(this);
        connectButton.setText("Подключить выбранный сервер");
        connectButton.setOnClickListener(view -> requestVpnPermission());
        column.addView(connectButton);
        Button disconnectButton = new Button(this);
        disconnectButton.setText("Отключить");
        disconnectButton.setOnClickListener(view -> startService(
            new Intent(this, ShadowVpnService.class).setAction(ShadowVpnService.STOP)));
        column.addView(disconnectButton);

        servers = new LinearLayout(this);
        servers.setOrientation(LinearLayout.VERTICAL);
        column.addView(servers);
        ScrollView scroll = new ScrollView(this);
        scroll.addView(column, new ViewGroup.LayoutParams(-1, -2));
        setContentView(scroll);
        if (url.length() > 0) importSubscription();
    }

    private void importSubscription() {
        if (ShadowVpnService.connected) {
            message.setText("Отключите VPN перед обновлением");
            return;
        }
        String address = url.getText().toString().trim();
        if (!address.startsWith("https://")) {
            message.setText("Нужна HTTPS-ссылка");
            return;
        }
        message.setText("Загружаю подписку…");
        worker.execute(() -> {
            try {
                JSONObject result = new JSONObject(Bridge.importSubscription(address));
                JSONArray profiles = result.getJSONArray("profiles");
                List<String[]> items = new ArrayList<>();
                for (int i = 0; i < profiles.length(); i++) {
                    JSONObject profile = profiles.getJSONObject(i);
                    items.add(new String[]{profile.getString("id"), profile.getString("name")});
                }
                runOnUiThread(() -> {
                    preferences.edit().putString("url", address).apply();
                    doh = result.optString("dnsDoh");
                    selectedId = items.isEmpty() ? "" : items.get(0)[0];
                    servers.removeAllViews();
                    for (String[] item : items) {
                        Button button = new Button(this);
                        button.setText(item[1]);
                        button.setOnClickListener(view -> {
                            selectedId = item[0];
                            message.setText("Выбрано: " + item[1]);
                        });
                        servers.addView(button);
                    }
                    message.setText("Серверов: " + items.size() + "; пропущено: " + result.optInt("skipped"));
                });
            } catch (Exception error) {
                runOnUiThread(() -> message.setText(
                    error.getMessage() == null ? "Ошибка загрузки" : error.getMessage()));
            }
        });
    }

    private void requestVpnPermission() {
        if (selectedId.isEmpty()) {
            message.setText("Сначала выберите сервер");
            return;
        }
        Intent consent = VpnService.prepare(this);
        if (consent == null) startConnection();
        else startActivityForResult(consent, 42);
    }

    @Override protected void onActivityResult(int requestCode, int resultCode, Intent data) {
        super.onActivityResult(requestCode, resultCode, data);
        if (requestCode == 42 && resultCode == RESULT_OK) startConnection();
    }

    private void startConnection() {
        Intent intent = new Intent(this, ShadowVpnService.class)
            .setAction(ShadowVpnService.START)
            .putExtra("profileId", selectedId)
            .putExtra("dnsDoh", doh);
        startForegroundService(intent);
    }

    @Override protected void onResume() { super.onResume(); handler.post(refresh); }
    @Override protected void onPause() { handler.removeCallbacks(refresh); super.onPause(); }
    @Override protected void onDestroy() { worker.shutdownNow(); super.onDestroy(); }
}
