// Package bridge runs Syncthing inside the app's own process. It is bound to Java with gomobile,
// so the exported functions only use types that gomobile can translate.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thejerf/suture/v4"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/rand"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// The defaults of `syncthing serve`.
const (
	dbMaintenanceInterval = 8 * time.Hour
	dbDeleteRetention     = 10920 * time.Hour
)

// Where Syncthing's GUI keeps the sessions it has handed out: "sessions" in the "misc" namespace
// of the database's key-value store. Three places in Syncthing make it so: newTokenCookieManager
// in lib/api/tokenmanager.go keeps them under "sessions" in the store it is given, startup in
// lib/syncthing/syncthing.go hands the GUI a store from NewMiscDB, and NewMiscDB in
// internal/db/typed.go makes a store that puts "misc/" in front of its keys.
const sessionsKey = "misc/sessions"

var (
	mut           sync.Mutex
	attempted     bool
	app           *syncthing.App
	httpsCertFile atomic.Pointer[string]
)

// Start runs Syncthing and returns the URL of its GUI once that is being served. The GUI only
// lets in the given user, and every way in is new with each start: the password is the given one,
// the API key is replaced, and the sessions that earlier runs left in the database are ended.
// Syncthing keeps its configuration, keys and database in configDir, expands "~" in folder paths
// to homeDir, and puts temporary files in tmpDir. This device goes by deviceName unless the user
// has given it a name in the GUI.
//
// The URL is HTTPS: see the comment on the GUI address below and CertFile.
//
// It follows syncthingMain in Syncthing's cmd/syncthing/main.go, which sets up state that is
// global to the process, so a process can call it only once, successful or not.
func Start(configDir, homeDir, tmpDir, deviceName, guiUser, guiPassword string) (guiURL string, err error) {
	mut.Lock()
	defer mut.Unlock()
	if attempted {
		return "", errors.New("Syncthing was already started in this process")
	}
	attempted = true

	// Bound here rather than left to the configuration, so that the GUI is always on loopback,
	// over TLS, and on a port that is known to be free. Every app on the device can reach a
	// loopback listener, so TLS is what keeps another app that grabs the port from impersonating
	// the GUI to the WebView: Syncthing serves its self-signed certificate, whose key never leaves
	// this app's private storage, and the WebView pins it (see CertFile). The https:// scheme is
	// how STGUIADDRESS turns TLS on.
	guiAddr, err := freeGUIAddress()
	if err != nil {
		return "", fmt.Errorf("find a port for the GUI: %w", err)
	}
	// A Go library on Android starts out with an empty environment.
	for name, value := range map[string]string{
		"HOME":         homeDir,
		"TMPDIR":       tmpDir,
		"STGUIADDRESS": "https://" + guiAddr,
	} {
		if err := os.Setenv(name, value); err != nil {
			return "", err
		}
	}
	for dir, path := range map[locations.BaseDirEnum]string{
		locations.UserHomeBaseDir: homeDir,
		locations.ConfigBaseDir:   configDir,
		locations.DataBaseDir:     configDir,
	} {
		if err := locations.SetBaseDir(dir, path); err != nil {
			return "", err
		}
	}
	// Where Syncthing will put the GUI's HTTPS certificate, for CertFile to hand to the app to
	// pin. The path is known now that the locations are set; the file itself appears when the GUI
	// starts listening, which is before Start returns.
	guiCertFile := locations.Get(locations.HTTPSCertFile)
	httpsCertFile.Store(&guiCertFile)
	if err := syncthing.EnsureDir(configDir, 0o700); err != nil {
		return "", fmt.Errorf("create %s: %w", configDir, err)
	}

	cert, err := syncthing.LoadOrGenerateCertificate(
		locations.Get(locations.CertFile),
		locations.Get(locations.KeyFile),
	)
	if err != nil {
		return "", fmt.Errorf("load or generate the device certificate: %w", err)
	}

	// The supervisor of what has to run before the app itself: the event logger and the
	// configuration service.
	ctx, cancel := context.WithCancel(context.Background())
	defer func() {
		if err != nil {
			cancel()
		}
	}()
	earlyService := suture.New("early", svcutil.SpecWithDebugLogger())
	earlyService.ServeBackground(ctx)

	evLogger := events.NewLogger()
	earlyService.Add(evLogger)

	cfg, err := syncthing.LoadConfigAtStartup(locations.Get(locations.ConfigFile), cert, evLogger, false, false)
	if err != nil {
		return "", fmt.Errorf("load the configuration: %w", err)
	}
	earlyService.Add(cfg)
	config.RegisterInfoMetrics(cfg)

	// Syncthing names a new device after its host, which on Android is "localhost" on every
	// device. Any other name is one the user chose in the GUI.
	hostname, _ := os.Hostname()

	var passwordErr error
	if _, err := cfg.Modify(func(c *config.Configuration) {
		c.GUI.Enabled = true
		// STGUIADDRESS puts the GUI on HTTPS whatever this says, but this is what the GUI's
		// settings show, and they should not claim otherwise.
		c.GUI.RawUseTLS = true
		c.GUI.User = guiUser
		passwordErr = c.GUI.SetPassword(guiPassword)
		// The API key lets in whoever has it, without the password, and Syncthing keeps it in the
		// configuration from one run to the next. Nothing in the app uses it, so replace it.
		c.GUI.APIKey = rand.String(32)
		if _, i, ok := c.Device(cfg.MyID()); ok && c.Devices[i].Name == hostname {
			c.Devices[i].Name = deviceName
		}
	}); err != nil {
		return "", fmt.Errorf("adjust the configuration: %w", err)
	}
	if passwordErr != nil {
		return "", fmt.Errorf("protect the GUI: %w", passwordErr)
	}

	sdb, err := syncthing.OpenDatabase(locations.Get(locations.Database), dbDeleteRetention)
	if err != nil {
		return "", fmt.Errorf("open the database: %w", err)
	}

	// The GUI keeps its sessions in the database, where they outlive this process and stay good for
	// a week after their last use, whatever the password has become since. The app logs its WebView
	// in again at every start, so it has no use for the ones that are left, and nobody else should.
	// This has to come before the GUI starts, because that is when it loads them.
	if err := sdb.DeleteKV(sessionsKey); err != nil {
		sdb.Close()
		return "", fmt.Errorf("end the sessions of earlier runs: %w", err)
	}

	a, err := syncthing.New(cfg, sdb, evLogger, cert, syncthing.Options{
		NoUpgrade:             true,
		DBMaintenanceInterval: dbMaintenanceInterval,
	})
	if err != nil {
		sdb.Close()
		return "", err
	}
	// A failed Start stops the app, which closes the database.
	if err := a.Start(); err != nil {
		return "", fmt.Errorf("start Syncthing: %w", err)
	}

	app = a
	return "https://" + guiAddr + "/", nil
}

// CertFile returns the path of the GUI's HTTPS certificate, and "" until Start has worked it out.
// It does not take mut, which Start holds for as long as it runs, so it answers at once even then,
// and the path can come before the file: that is only sure to be there once Start has returned
// without an error. The GUI is served over TLS on loopback (see Start), and the app pins this
// certificate so that another app that seizes the port cannot impersonate the GUI: that app has no
// way to present this certificate, whose private key stays in this app's private storage.
func CertFile() string {
	if path := httpsCertFile.Load(); path != nil {
		return *path
	}
	return ""
}

// WaitForExit blocks until Syncthing has stopped, which its GUI can make it do, and reports
// whether it asks to be started again. It is not called Wait because the Java binding would
// clash with Object.wait.
func WaitForExit() (restart bool) {
	mut.Lock()
	a := app
	mut.Unlock()
	return a != nil && a.Wait() == svcutil.ExitRestart
}

// freeGUIAddress returns a loopback address that nothing listens on, with Syncthing's usual GUI
// port unless another Syncthing on the device already has it.
func freeGUIAddress() (string, error) {
	var lastErr error
	for _, addr := range []string{"127.0.0.1:8384", "127.0.0.1:0"} {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		addr = listener.Addr().String()
		listener.Close()
		return addr, nil
	}
	return "", lastErr
}
