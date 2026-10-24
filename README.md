# Portable Browsers (auto-built, auto-updating)

Windows x64 portable builds of **Zen Browser** and **Vivaldi**, built from the
official installers (Zen: github.com/zen-browser/desktop, as linked by zen-browser.app;
Vivaldi: downloads.vivaldi.com). GitHub Actions checks every hour and publishes a
release when upstream has a new version.

- `ZenPortable-windows-x64.zip` -> run `zen-portable.exe`
- `VivaldiPortable-windows-x64.zip` -> run `vivaldi-portable.exe`

## Layout
    ZenPortable/
      zen-portable.exe   launcher + self-updater
      App/               browser (replaced on update)
      Data/              profile, cache, temp, update.log (never touched by updates)

## Auto-update (pendrive friendly)
On launch the launcher (1) applies any update downloaded earlier by swapping only `App/`,
(2) starts the browser, (3) checks this repo's Releases (max once per 3 h), downloads the
new zip, verifies its SHA-256 and stages it in `Data/update/`. It applies on the next launch,
so startup is never slowed. If the browser is still running, the swap is skipped safely.
- Log: `Data/update.log`
- Disable: create an empty file `Data/no-update`
- The repo must be **public** for the updater to read releases.

## Setup
1. Push these files to a GitHub repo.
2. Settings -> Actions -> General -> Workflow permissions -> **Read and write**.
3. Actions tab -> run both workflows once ("Run workflow").
