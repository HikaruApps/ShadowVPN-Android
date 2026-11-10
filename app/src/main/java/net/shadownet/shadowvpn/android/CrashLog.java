package net.shadownet.shadowvpn.android;

import android.content.Context;

import java.io.File;
import java.io.FileOutputStream;
import java.io.PrintWriter;
import java.io.StringWriter;
import java.nio.charset.StandardCharsets;
import java.nio.file.Files;
import java.util.Date;
import java.util.Locale;

/**
 * Keeps the stack trace of the last crash, so a tester can send it from the
 * Logs screen after reopening the app instead of needing adb. Nothing leaves
 * the phone unless the user copies it.
 */
final class CrashLog {
    private static final String FILE = "last_crash.txt";
    private static final int MAX_CHARS = 16 * 1024;

    private CrashLog() { }

    static synchronized void install(Context context) {
        if (Thread.getDefaultUncaughtExceptionHandler() instanceof Recorder) return;
        File file = new File(context.getApplicationContext().getFilesDir(), FILE);
        String version = "?";
        try {
            version = context.getPackageManager().getPackageInfo(context.getPackageName(), 0).versionName;
        } catch (Exception ignored) { }
        Thread.setDefaultUncaughtExceptionHandler(
                new Recorder(file, version, Thread.getDefaultUncaughtExceptionHandler()));
    }

    /** The last crash, or an empty string. */
    static String read(Context context) {
        File file = new File(context.getFilesDir(), FILE);
        try {
            return file.exists() ? new String(Files.readAllBytes(file.toPath()), StandardCharsets.UTF_8) : "";
        } catch (Exception error) {
            return "";
        }
    }

    private static final class Recorder implements Thread.UncaughtExceptionHandler {
        private final File file;
        private final String version;
        private final Thread.UncaughtExceptionHandler next;

        Recorder(File file, String version, Thread.UncaughtExceptionHandler next) {
            this.file = file;
            this.version = version;
            this.next = next;
        }

        @Override public void uncaughtException(Thread thread, Throwable error) {
            try {
                StringWriter trace = new StringWriter();
                error.printStackTrace(new PrintWriter(trace));
                String text = String.format(Locale.ROOT, "%tF %<tT · %s · версия %s%n%s",
                        new Date(), thread.getName(), version, trace);
                if (text.length() > MAX_CHARS) text = text.substring(0, MAX_CHARS);
                try (FileOutputStream out = new FileOutputStream(file)) {
                    out.write(text.getBytes(StandardCharsets.UTF_8));
                }
            } catch (Throwable ignored) {
                // Recording must never hide the crash itself.
            }
            if (next != null) next.uncaughtException(thread, error);
        }
    }
}
