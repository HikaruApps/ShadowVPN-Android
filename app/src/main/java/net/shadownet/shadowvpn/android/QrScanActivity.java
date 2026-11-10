package net.shadownet.shadowvpn.android;

import android.os.Bundle;

import com.journeyapps.barcodescanner.CaptureActivity;
import com.journeyapps.barcodescanner.DecoratedBarcodeView;
import com.journeyapps.barcodescanner.camera.CameraSettings;

/**
 * zxing's scanner with the camera tuned for QR codes on screens and paper.
 * By default it refocuses in steps every couple of seconds, which on some
 * phones (Samsung among them) leaves a close QR code blurred for good;
 * continuous focus keeps it sharp. The settings are applied after the
 * library has read them from the intent and before the camera opens.
 */
public final class QrScanActivity extends CaptureActivity {
    private DecoratedBarcodeView scanner;

    @Override protected DecoratedBarcodeView initializeContent() {
        scanner = super.initializeContent();
        return scanner;
    }

    @Override protected void onCreate(Bundle state) {
        super.onCreate(state);
        if (scanner == null) return;
        CameraSettings settings = scanner.getBarcodeView().getCameraSettings();
        settings.setAutoFocusEnabled(true);
        settings.setContinuousFocusEnabled(true);
        settings.setFocusMode(CameraSettings.FocusMode.CONTINUOUS);
        // The camera's barcode scene mode and metering where it has them;
        // cameras without them ignore both.
        settings.setBarcodeSceneModeEnabled(true);
        settings.setMeteringEnabled(true);
    }
}
