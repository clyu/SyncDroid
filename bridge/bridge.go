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
	"time"

	"github.com/thejerf/suture/v4"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/svcutil"
	"github.com/syncthing/syncthing/lib/syncthing"
)

// The defaults of `syncthing serve`.
const (
	dbMaintenanceInterval = 8 * time.Hour
	dbDeleteRetention     = 10920 * time.Hour
)

var (
	mut       sync.Mutex
	attempted bool
	app       *syncthing.App
)

// Start runs Syncthing and returns the URL of its GUI once that is being served. The GUI only
// lets in the given user. Syncthing keeps its configuration, keys and database in configDir,
// expands "~" in folder paths to homeDir, and puts temporary files in tmpDir.
//
// It follows syncthingMain in Syncthing's cmd/syncthing/main.go, which sets up state that is
// global to the process, so a process can call it only once, successful or not.
func Start(configDir, homeDir, tmpDir, guiUser, guiPassword string) (guiURL string, err error) {
	mut.Lock()
	defer mut.Unlock()
	if attempted {
		return "", errors.New("Syncthing was already started in this process")
	}
	attempted = true

	// Bound here rather than left to the configuration, so that the GUI is always on loopback,
	// without TLS, and on a port that is known to be free.
	guiAddr, err := freeGUIAddress()
	if err != nil {
		return "", fmt.Errorf("find a port for the GUI: %w", err)
	}
	// A Go library on Android starts out with an empty environment.
	for name, value := range map[string]string{
		"HOME":         homeDir,
		"TMPDIR":       tmpDir,
		"STGUIADDRESS": guiAddr,
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

	var passwordErr error
	if _, err := cfg.Modify(func(c *config.Configuration) {
		c.GUI.Enabled = true
		c.GUI.User = guiUser
		passwordErr = c.GUI.SetPassword(guiPassword)
	}); err != nil {
		return "", fmt.Errorf("protect the GUI: %w", err)
	}
	if passwordErr != nil {
		return "", fmt.Errorf("protect the GUI: %w", passwordErr)
	}

	sdb, err := syncthing.OpenDatabase(locations.Get(locations.Database), dbDeleteRetention)
	if err != nil {
		return "", fmt.Errorf("open the database: %w", err)
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
	return "http://" + guiAddr + "/", nil
}

// WaitForExit blocks until Syncthing has stopped, which its GUI can make it do, and returns its
// exit status: 3 when it asks to be restarted. It is not called Wait because the Java binding
// would clash with Object.wait.
func WaitForExit() int {
	mut.Lock()
	a := app
	mut.Unlock()
	if a == nil {
		return svcutil.ExitError.AsInt()
	}
	return a.Wait().AsInt()
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
