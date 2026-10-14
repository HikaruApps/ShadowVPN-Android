package net.shadownet.shadowvpn.android;

import android.Manifest;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.content.Context;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.os.Build;

import java.util.Set;

/** Tells the user what an automatic subscription update changed. */
final class UpdateNotifier {
    private static final String CHANNEL = "updates";
    private static final int ID = 2;

    private UpdateNotifier() { }

    static void notify(Context context, Set<String> before, Set<String> after, String error) {
        if (Build.VERSION.SDK_INT >= 33 && context.checkSelfPermission(
                Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            return;
        }
        NotificationManager manager = context.getSystemService(NotificationManager.class);
        if (manager == null) return;
        manager.createNotificationChannel(new NotificationChannel(
                CHANNEL, "Обновления подписки", NotificationManager.IMPORTANCE_LOW));
        PendingIntent open = PendingIntent.getActivity(context, 0,
                new Intent(context, MainActivity.class), PendingIntent.FLAG_IMMUTABLE);
        manager.notify(ID, new Notification.Builder(context, CHANNEL)
                .setContentTitle(error == null ? "Подписка обновлена" : "Подписка не обновилась")
                .setContentText(error == null ? summary(before, after) : error)
                .setSmallIcon(android.R.drawable.stat_notify_sync)
                .setContentIntent(open)
                .setAutoCancel(true)
                .build());
    }

    static String summary(Set<String> before, Set<String> after) {
        int added = 0;
        int removed = 0;
        for (String id : after) if (!before.contains(id)) added++;
        for (String id : before) if (!after.contains(id)) removed++;
        String total = servers(after.size());
        if (added == 0 && removed == 0) return total + " · без изменений";
        StringBuilder text = new StringBuilder(total).append(" ·");
        if (added > 0) text.append(" +").append(added);
        if (removed > 0) text.append(" −").append(removed);
        return text.toString();
    }

    static String servers(int count) {
        int tens = count % 100;
        int ones = count % 10;
        String word = tens >= 11 && tens <= 14 ? "серверов"
                : ones == 1 ? "сервер"
                : ones >= 2 && ones <= 4 ? "сервера" : "серверов";
        return count + " " + word;
    }
}
