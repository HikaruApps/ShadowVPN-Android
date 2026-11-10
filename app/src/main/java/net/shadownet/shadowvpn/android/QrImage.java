package net.shadownet.shadowvpn.android;

import android.content.Context;
import android.graphics.Bitmap;
import android.graphics.BitmapFactory;
import android.net.Uri;

import com.google.zxing.BarcodeFormat;
import com.google.zxing.BinaryBitmap;
import com.google.zxing.DecodeHintType;
import com.google.zxing.LuminanceSource;
import com.google.zxing.MultiFormatReader;
import com.google.zxing.RGBLuminanceSource;
import com.google.zxing.Result;
import com.google.zxing.common.GlobalHistogramBinarizer;
import com.google.zxing.common.HybridBinarizer;

import java.io.InputStream;
import java.util.Collections;
import java.util.EnumMap;
import java.util.Map;

/** Reads a QR code from a picture, e.g. a screenshot of a subscription page. */
final class QrImage {
    private QrImage() { }

    /** The QR code's text, or null when the picture holds none. */
    static String decode(Context context, Uri uri) {
        // Large pictures first at a size that keeps a small code readable,
        // then smaller, which helps with noisy photos of a screen.
        for (int maxSide : new int[]{2048, 1024}) {
            Bitmap bitmap = load(context, uri, maxSide);
            if (bitmap == null) return null;
            try {
                String text = decode(bitmap);
                if (text != null) return text;
            } finally {
                bitmap.recycle();
            }
        }
        return null;
    }

    private static String decode(Bitmap bitmap) {
        int width = bitmap.getWidth();
        int height = bitmap.getHeight();
        int[] pixels = new int[width * height];
        bitmap.getPixels(pixels, 0, width, 0, 0, width, height);
        LuminanceSource source = new RGBLuminanceSource(width, height, pixels);
        Map<DecodeHintType, Object> hints = new EnumMap<>(DecodeHintType.class);
        hints.put(DecodeHintType.POSSIBLE_FORMATS, Collections.singletonList(BarcodeFormat.QR_CODE));
        hints.put(DecodeHintType.TRY_HARDER, Boolean.TRUE);
        MultiFormatReader reader = new MultiFormatReader();
        reader.setHints(hints);
        // The inverted source reads light codes on a dark background, as
        // dark-theme pages show them (zxing core 3.4 has no ALSO_INVERTED).
        LuminanceSource inverted = source.invert();
        for (BinaryBitmap image : new BinaryBitmap[]{
                new BinaryBitmap(new HybridBinarizer(source)),
                new BinaryBitmap(new GlobalHistogramBinarizer(source)),
                new BinaryBitmap(new HybridBinarizer(inverted))}) {
            try {
                Result result = reader.decodeWithState(image);
                if (result != null && result.getText() != null) return result.getText();
            } catch (Exception notFound) {
                // Try the next binarizer.
            } finally {
                reader.reset();
            }
        }
        return null;
    }

    /** Decodes the picture scaled down so its longer side is at most maxSide. */
    private static Bitmap load(Context context, Uri uri, int maxSide) {
        try {
            BitmapFactory.Options bounds = new BitmapFactory.Options();
            bounds.inJustDecodeBounds = true;
            try (InputStream in = context.getContentResolver().openInputStream(uri)) {
                if (in == null) return null;
                BitmapFactory.decodeStream(in, null, bounds);
            }
            if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null;
            int sample = 1;
            while (Math.max(bounds.outWidth, bounds.outHeight) / sample > maxSide) sample *= 2;
            BitmapFactory.Options options = new BitmapFactory.Options();
            options.inSampleSize = sample;
            options.inPreferredConfig = Bitmap.Config.ARGB_8888;
            try (InputStream in = context.getContentResolver().openInputStream(uri)) {
                return in == null ? null : BitmapFactory.decodeStream(in, null, options);
            }
        } catch (Exception | OutOfMemoryError error) {
            return null;
        }
    }
}
