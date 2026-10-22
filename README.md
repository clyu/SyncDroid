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
key that got out stops working once the app is started again. Syncthing's settings for the GUI,
apart from its theme, are therefore managed by the app, and changing them has no lasting effect:
besides setting the user, the password and the API key, every start puts the others back to
Syncthing's defaults. That way nothing changed in an earlier run decides how, or whether, the GUI
asks for the password, such as its authentication mode, or whom it answers, such as its host check,
or whether the WebView stays logged in, such as the path of its session cookie.

An app that holds the GUI's port can still keep the GUI out of reach. It gets its chance when
Syncthing binds the port again after a change to the GUI's own settings, such as the theme, and
Syncthing does not get the port back for as long as the other app keeps it. The app therefore checks
every few seconds whether another app holds the GUI's port, and when one has for about fifteen
seconds it restarts, which puts the GUI on another port. It does not restart when the GUI merely
stops answering: any app can make it do that without holding the port, by opening connections to it
that send nothing, and a restart would not get the GUI away from that app.

What any app on the device can still do is open a connection to the GUI's port. Without the password
it gets the GUI's page and files, a health check and the language list, and with them, in the
headers, Syncthing's version and this device's ID; everything else is turned away. Closing even that
would take a proxy in front of the GUI that asks for a client certificate, which this app does not
have.

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
Also look through `GUIConfiguration` in `syncthing/lib/config/guiconfiguration.go` for settings that
are new and, like the theme, only a matter of taste: `Start` keeps nothing of the GUI's settings
but the theme from one run to the next, so the user would lose any such setting at every start
unless `Start` kept it too.
