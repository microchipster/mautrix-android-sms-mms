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

## Persistence, backup and recovery (current state)

Keys for the fork live in `pass` under namespace `<user>`:

- keystore: `~/.password-store/<user>/android/org.fossify.messages.bridge/release.keystore`
  (PKCS12, alias `release`, storepass == keypass)
- password: `pass` entry `<user>/android/org.fossify.messages.bridge/storepass`, cached for
  automation at `~/.config/android/org.fossify.messages.bridge/storepass` (chmod 600)
- certificate to recognise a good key: `CN=org.fossify.messages.bridge, OU=Android,
  O=<user>`, SHA-256 `678d78dd1d9953c6363739c136bafa59683c66760b63dc33f88416889df2b34b`
  (check with `apksigner verify --print-certs <apk>`)

`~/.password-store` is a git repository with remote
`ssh://git@git.example.com/<user>/auth.git`; the fork's keystore and password are committed
and pushed (`git -C ~/.password-store log -- <user>/android/org.fossify.messages.bridge`).
That is the primary off-machine copy — **keep it pushed** after any key change.

Things that have bitten us and are now guarded:

- `scripts/build-release.sh` (app repo) **refuses to generate a new key** unless
  `--generate-key` is passed. A silently generated key would produce an APK that cannot
  upgrade the installed one (`INSTALL_FAILED_UPDATE_INCOMPATIBLE` unless users uninstall
  first, losing app settings but not telephony history).
- Reading `pass` entries needs an unlocked GPG agent (the signing key is
  passphrase-protected; non-interactive reads fail with "public key decryption failed").
  Writing new entries only needs the public key. If reads are unavailable, restore the
  cached password file instead (contents equal to the `storepass` entry).
- Do not regenerate the keystore of any app users already installed with a previous key.

Restore on a fresh machine:

1. `git clone` the pass repo to `~/.password-store` (or `git -C ~/.password-store pull`).
2. Unlock the GPG key, `pass show <user>/android/org.fossify.messages.bridge/storepass`
   (or restore the cached `~/.config/android/<appId>/storepass` from backup).
3. Symlink the keystore: `ln -sfn ~/.password-store/<user>/android/<appId>/release.keystore app/release.keystore`.
4. `scripts/build-release.sh` builds and `apksigner verify --print-certs` confirms the
   certificate fingerprint above.

Offline copy (recommended, covers loss of the git remote and of the GPG key):
a GPG-encrypted archive of the keystore + password + this certificate fingerprint, stored
off-machine (already prepared at `~/backups/android-signing/`).

Also existing in `pass` (older apps, same flow): `android/com.example.termuxintents`,
`android/com.example.calendarwriter`, `android/com.example.lastlocation`.

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
