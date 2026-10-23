# Android app signing — canonical flow (project + fork plan)

## What already exists (canonical; reuse as-is)

The previous APK-signing flow was found intact at
`/home/user/src/repos/utils/android/android/android-signing-keys` (a script, run from the
root of any Gradle project that has an `app/` module). Companion Makefile targets live in
the small apps at `/home/user/src/repos/utils/android/kotlin/` (e.g. `CalendarWriter`).

How it works:

- `android-signing-keys <pass-namespace> allow-keystore|force-keystore
  allow-password|force-password`
  - reads `applicationId` from `app/build.gradle`
  - stores the keystore at `~/.password-store/<pass-namespace>/android/<applicationId>/
    release.keystore` and a generated password at `.../storepass.gpg` (i.e. **the keys are
    already managed by `pass`**, which is a git repo — that is the backup story)
  - symlinks `./app/release.keystore` → the pass keystore and writes
    `~/.config/android/<applicationId>/storepass`
  - runs `keytool -genkeypair` with PKCS12, alias `release`, RSA 2048, SHA256withRSA,
    validity 36500 days, storepass == keypass (Gradle limitation)
  - refuses to run if the password store has uncommitted changes
- Makefile pattern (per app): `make keys-<namespace>`, `keys-check` (`keytool -list`),
  `clean-keystore-<ns>` / `clean-password-<ns>`, `install-debug` / `install-release`
  (`adb uninstall <appId> && adb install -g app-release.apk`), `build`
- Gradle side: `signingConfigs.release { storeFile file("release.keystore"); storePassword
  System.getenv("ANDROID_STOREPASS"); keyAlias "release"; keyPassword
  System.getenv("ANDROID_STOREPASS"); storeType "pkcs12" }`; the Makefile exports
  `ANDROID_STOREPASS` from `~/.config/android/<appId>/storepass`
- `.gitignore` excludes `*.keystore`, `*.jks`, `/signing/`

Keys that already exist in pass (namespace `<user>`):
`android/com.example.termuxintents`, `android/com.example.calendarwriter`,
`android/com.example.lastlocation`.

## Nothing is at risk — but do this

1. **Back up the password store now** (commit + push `~/.password-store`; confirm a remote
   exists). Its git history is the safety net for existing keystores.
2. **Offline export**: `pass show <user>/android/<appId>/release.keystore > release.jks` +
   the `storepass` entry, stored somewhere offline (password manager backup/encrypted USB).
3. The Fossify fork needs a **new** keystore (new app id / new app), generated with the
   same script at fork time — there is nothing to migrate from the old SMS setup
   (`eu.droogers.smsmatrix` is a third-party app; no key of ours involved).
4. Do not regenerate the keystore of an app users already installed with a previous key —
   signature mismatch blocks updates (`INSTALL_FAILED_UPDATE_INCOMPATIBLE`).

## Fork decisions to make

1. **applicationId**: drop-in `org.fossify.messages` (replaces the F-Droid build; must
   `adb uninstall` first; app-local data like settings/recycle bin is lost, telephony
   provider history and threads are NOT affected) vs side-by-side
   `org.fossify.messages.bridge` (both installed at once; our fork must still be set as the
   **default SMS app** to receive SMS/MMS). Recommendation: side-by-side trial, then
   decide whether to switch the daily driver; keep the applicationId stable once chosen.
2. **CI**: signed release builds in CI need the keystore exported to a secret (base64) or
   `pass` available on the runner; verify with `apksigner verify --print-certs`; keep
   `versionCode` monotonic. Decide runner (Forgejo Actions / GitHub Actions) and channel
   (private F-Droid repo, Obtainium, direct APK).
3. **Which phones**: the release APK only needs the app id + minSdk; the same artifact
   installs on GrapheneOS and LineageOS.
