// Package bridge runs Syncthing inside the app's own process. It is bound to Java with gomobile,
// so the exported functions only use types that gomobile can translate.
package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/thejerf/suture/v4"

	"github.com/syncthing/syncthing/lib/config"
	"github.com/syncthing/syncthing/lib/events"
	"github.com/syncthing/syncthing/lib/locations"
	"github.com/syncthing/syncthing/lib/rand"
	"github.com/syncthing/syncthing/lib/structutil"
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

// How watchGUI keeps an eye on the GUI's port: how often it looks, and how many looks in a row that
// find the port in another process's hands make it give up on this process.
const (
	guiCheckInterval = 5 * time.Second
	guiLostAfter     = 3
)

var (
	mut           sync.Mutex
	attempted     bool
	app           *syncthing.App
	httpsCertFile atomic.Pointer[string]
)

// Start runs Syncthing and returns the URL of its GUI once that is being served. The GUI only
// lets in the given user, and every way in is new with each start: the password is the given one,
// the API key is replaced, and the sessions that earlier runs left in the database are ended. Of
// the GUI's other settings only the theme is kept, and the rest start over from Syncthing's
// defaults, so that nothing changed in an earlier run decides how, or whether, the GUI asks for
// the password, or whether a login holds. Syncthing keeps its configuration, keys and database in
// configDir, expands "~" in folder paths to homeDir, and puts temporary files in tmpDir. This
// device goes by deviceName unless the user has given it a name in the GUI.
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
		"STGUIADDRESS": "https://" + guiAddr.String(),
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
		// Whoever has a session can change any of the GUI's settings, and they would stay changed
		// from one run to the next. Some decide how, or whether, the GUI asks for the password and
		// whom it answers, and the password is only worth something with them as they are by
		// default. Others decide whether the WebView stays logged in: its session cookie has to go
		// along with the requests to /rest/, and its session must not run out between them. So
		// the GUI starts over from Syncthing's defaults, keeping only the theme, which is the
		// user's to choose. That also covers the settings that later versions of Syncthing add.
		var gui config.GUIConfiguration
		structutil.SetDefaults(&gui)
		gui.Theme = c.GUI.Theme
		c.GUI = gui
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
	go watchGUI(a, guiAddr)
	return "https://" + guiAddr.String() + "/", nil
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

// WaitForExit blocks until Syncthing has stopped, which its GUI can make it do and which it does
// itself when it loses the GUI (see watchGUI), and reports whether it asks to be started again. It
// is not called Wait because the Java binding would clash with Object.wait.
func WaitForExit() (restart bool) {
	mut.Lock()
	a := app
	mut.Unlock()
	return a != nil && a.Wait() == svcutil.ExitRestart
}

// watchGUI stops Syncthing, asking to be started again, when another process holds the GUI's port.
// Syncthing lets go of the port and binds it again whenever the GUI's own settings change, and an
// app that takes the port in between can keep it for as long as it likes, with the GUI out of reach
// all that time: Syncthing neither gets the port back nor gives up. Starting over does, since a
// start finds the port taken and has the GUI listen on another one (see freeGUIAddress).
//
// It goes by who holds the port, not by whether the GUI answers. Any app can keep the GUI from
// answering without taking the port, by opening connections to it that send nothing, since
// Syncthing waits up to a second for each new connection to speak before it takes the next one;
// starting over would not get the GUI away from that app and would only keep cutting the syncing
// short. Nor does a port that nobody holds count against the GUI: Syncthing is then either on its
// way out, where a stop from here would turn a shutdown into a restart, or binding the port again,
// which can take it longer than fifteen seconds after the GUI's settings have changed several
// times in quick succession, since its supervisor counts each change as a failure and pauses after
// too many. Fewer than guiLostAfter looks in a row that find the port taken are let go, which
// covers Syncthing binding the port in the middle of a look (see guiPortTaken).
func watchGUI(a *syncthing.App, addr netip.AddrPort) {
	stopped := make(chan struct{})
	go func() {
		a.Wait()
		close(stopped)
	}()

	ticker := time.NewTicker(guiCheckInterval)
	defer ticker.Stop()
	misses := 0
	for {
		select {
		case <-stopped:
			return
		case <-ticker.C:
		}
		if !guiPortTaken(addr) {
			misses = 0
			continue
		}
		if misses++; misses >= guiLostAfter {
			slog.Warn("Another process holds the GUI's port; stopping Syncthing to be started again", slog.String("address", addr.String()))
			a.Stop(svcutil.ExitRestart)
			return
		}
	}
}

// guiPortTaken reports whether a process other than this one holds the port of addr, an IPv4
// address as freeGUIAddress hands out. It first looks among this process's own sockets for one that
// listens on addr, which would be Syncthing's, and only when there is none tries to bind addr
// itself. Go sets SO_REUSEADDR on every socket it listens on, Syncthing's included, and with it a
// bind fails only over a socket that listens on the address or one that was bound there without it.
// This bind sets it too, so it fails when Syncthing's would, and since it does not listen, it never
// stands in the way of Syncthing binding the port at the same moment. Syncthing can still bind the
// port between the two steps, and then passes for another process.
func guiPortTaken(addr netip.AddrPort) bool {
	if listening, err := listensOn(addr); err != nil || listening {
		// Syncthing has the port, or there is no telling whether it has.
		return false
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return false
	}
	err = syscall.Bind(fd, &syscall.SockaddrInet4{Port: int(addr.Port()), Addr: addr.Addr().As4()})
	return errors.Is(err, syscall.EADDRINUSE)
}

// listensOn reports whether one of this process's own sockets listens on addr. Android does not let
// apps read the device's sockets out of /proc/net, which would say whose a socket is, so this goes
// through the file descriptors in /proc/self/fd instead. One that is closed while it looks, and
// maybe opened again as something else, does no harm, since this only asks each one what it is.
func listensOn(addr netip.AddrPort) (bool, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// Only a socket has an address. The connections that the GUI has accepted have the GUI's
		// too, but do not listen.
		sa, err := syscall.Getsockname(fd)
		if err != nil {
			continue
		}
		in4, ok := sa.(*syscall.SockaddrInet4)
		if !ok || netip.AddrPortFrom(netip.AddrFrom4(in4.Addr), uint16(in4.Port)) != addr {
			continue
		}
		listening, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ACCEPTCONN)
		if err == nil && listening != 0 {
			return true, nil
		}
	}
	return false, nil
}

// freeGUIAddress returns a loopback address that nothing listens on, with Syncthing's usual GUI
// port unless another Syncthing on the device already has it.
func freeGUIAddress() (netip.AddrPort, error) {
	var lastErr error
	for _, addr := range []string{"127.0.0.1:8384", "127.0.0.1:0"} {
		listener, err := net.Listen("tcp", addr)
		if err != nil {
			lastErr = err
			continue
		}
		found, err := netip.ParseAddrPort(listener.Addr().String())
		listener.Close()
		return found, err
	}
	return netip.AddrPort{}, lastErr
}
