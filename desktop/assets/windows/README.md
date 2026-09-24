# Windows installer images

The HTML files here are the sources for the bitmaps in `desktop/src-tauri/windows/`, referenced from `tauri.conf.json` under `bundle.windows`:

| File | Used by | Size |
|---|---|---|
| `nsis-header.html` | setup.exe, header of every page | 150x57 |
| `nsis-sidebar.html` | setup.exe, welcome and finish pages | 164x314 |
| `wix-banner.html` | .msi, top banner | 493x58 |
| `wix-dialog.html` | .msi, welcome and finish dialogs | 493x312 |

Regenerate after editing (the PNGs are intermediates and are not committed):

```
cd desktop/assets/windows
for spec in "nsis-header 150 57" "nsis-sidebar 164 314" "wix-banner 493 58" "wix-dialog 493 312"; do
  set -- $spec
  "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" --headless=new --disable-gpu --hide-scrollbars \
    --force-device-scale-factor=1 --window-size=$2,$3 --screenshot="$1.png" "file://$PWD/$1.html"
  sips -s format bmp "$1.png" --out "../../src-tauri/windows/$1.bmp" && rm "$1.png"
done
```

Run it under bash; zsh does not word-split `$spec`.
