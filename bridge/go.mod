module github.com/clyu/sync-droid/bridge

go 1.26.2

require (
	github.com/syncthing/syncthing v0.0.0
	golang.org/x/mobile v0.0.0-20260908204917-8b95e45f8d3e
)

// The git submodule, which is what pins the Syncthing version.
replace github.com/syncthing/syncthing => ../syncthing

tool (
	golang.org/x/mobile/cmd/gobind
	golang.org/x/mobile/cmd/gomobile
)
