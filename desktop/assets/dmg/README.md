# DMG background

`background.html` is the source for `desktop/src-tauri/dmg-background.png`, the Finder window background of the macOS disk image.
The window is 660x400 points; the PNG is rendered at 2x and tagged 144 dpi so Retina Finder shows it sharp.
Icon positions live in `tauri.conf.json` under `bundle.macOS.dmg`.

Regenerate after editing:

```
cd desktop/assets/dmg
"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu --hide-scrollbars \
  --force-device-scale-factor=2 --window-size=660,400 \
  --screenshot=../../src-tauri/dmg-background.png "file://$PWD/background.html"
sips -s dpiWidth 144 -s dpiHeight 144 ../../src-tauri/dmg-background.png
```
