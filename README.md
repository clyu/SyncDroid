# SyncDroid

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

The GUI listens on loopback only, over HTTPS. Because any other app on the device can reach
loopback, the WebView pins Syncthing's self-signed GUI certificate, so an app that grabs the port
cannot impersonate the GUI (its private key never leaves the app's private storage). The app also
gives the GUI a fixed user and a new random password every time it starts and logs its WebView in
with them. Syncthing would otherwise remember a login for a week and keep its API key for good, so
every start also ends the logins that earlier runs left behind and replaces the API key: a login or
key that got out stops working once the app is started again. The GUI authentication fields in
Syncthing's settings, and the API key, are therefore managed by the app, and changing them has no
lasting effect.

## Updating Syncthing

```sh
git -C syncthing fetch --tags
git -C syncthing checkout v2.1.7
git add syncthing
```

Then check that `go` in `bridge/go.mod` is not older than in `syncthing/go.mod`, that the Go version
in the workflow matches the one Syncthing's own `build-syncthing.yaml` uses, that
`bridge/bridge.go` still follows `syncthingMain` in `syncthing/cmd/syncthing/main.go`, and that
`sessionsKey` in `bridge/bridge.go` is still where the GUI keeps its logins: the `"sessions"` token
store that `newTokenCookieManager` in `syncthing/lib/api/tokenmanager.go` opens in the database
that `startup` in `syncthing/lib/syncthing/syncthing.go` hands the GUI, which is the miscellaneous
database. If it moved, the app would keep working but the logins of earlier runs would stay valid.
