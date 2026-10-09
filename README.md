# Sync Droid

A minimal Android app for [Syncthing](https://syncthing.net): Syncthing runs inside the app's
process, and the app's whole interface is a WebView on Syncthing's own web GUI.

There is no background sync. Syncthing runs while the app is open; once the app is in the
background Android may freeze or kill it, and swiping it away ends it.

## How it is put together

- `syncthing/` is a git submodule pinned to the Syncthing release that gets built.
- `bridge/` is a small Go package that starts Syncthing the way `syncthing serve` does.
  [gomobile](https://pkg.go.dev/golang.org/x/mobile/cmd/gomobile) turns it into an Android library.
- `app/` is the Android app: it asks for access to all files on first launch, starts the bridge,
  and points a WebView at the GUI.

The GUI listens on loopback only. Because any other app on the device can reach loopback, the app
gives the GUI a new random user and password every time it starts and logs its WebView in with
them. The GUI authentication fields in Syncthing's settings are therefore managed by the app, and
changing them has no lasting effect.

## Building

Everything is built by GitHub Actions (`.github/workflows/android.yml`), for arm64 only. Every push
uploads the APK as a workflow artifact. Pushes to `master` also publish it as the `Latest Build`
pre-release, provided these repository secrets are set so that the APK is signed with a fixed key:

| Secret | Value |
|---|---|
| `SIGNING_KEYSTORE_BASE64` | The keystore file, base64 encoded |
| `SIGNING_KEYSTORE_PASSWORD` | The password of the keystore and of the key |
| `SIGNING_KEY_ALIAS` | The alias of the key |

## Updating Syncthing

```sh
git -C syncthing fetch --tags
git -C syncthing checkout v2.1.7
git add syncthing
```

Then check that `go` in `bridge/go.mod` is not older than in `syncthing/go.mod`, that the Go version
in the workflow matches the one Syncthing's own `build-syncthing.yaml` uses, and that
`bridge/bridge.go` still follows `syncthingMain` in `syncthing/cmd/syncthing/main.go`.
