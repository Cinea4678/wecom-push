package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultWindowsAddr is the default loopback TCP address on Windows.
const DefaultWindowsAddr = "tcp://127.0.0.1:17891"

// StateDir returns the directory used for daemon runtime files (socket, pid, log).
func StateDir() (string, error) {
	if xdg := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); xdg != "" {
		return filepath.Join(xdg, "wecom-push"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "wecom-push"), nil
}

// DefaultSocketAddr returns the default daemon socket address.
func DefaultSocketAddr() (string, error) {
	if env := strings.TrimSpace(os.Getenv("WECOM_PUSH_SOCKET")); env != "" {
		return env, nil
	}
	if runtime.GOOS == "windows" {
		return DefaultWindowsAddr, nil
	}
	if xdgRun := strings.TrimSpace(os.Getenv("XDG_RUNTIME_DIR")); xdgRun != "" {
		return filepath.Join(xdgRun, "wecom-push.sock"), nil
	}
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.sock"), nil
}

// ResolveSocketAddr returns explicitAddr if non-empty, otherwise DefaultSocketAddr.
func ResolveSocketAddr(explicitAddr string) (string, error) {
	if strings.TrimSpace(explicitAddr) != "" {
		return strings.TrimSpace(explicitAddr), nil
	}
	return DefaultSocketAddr()
}

// DefaultLogPath returns the default background daemon log path.
func DefaultLogPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.log"), nil
}

// DefaultPIDPath returns the default background daemon PID file path.
func DefaultPIDPath() (string, error) {
	dir, err := StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.pid"), nil
}

// ParseListenAddr parses "tcp://host:port", "unix:///path", or a raw socket file path.
func ParseListenAddr(addr string) (network, target string) {
	addr = strings.TrimSpace(addr)
	if strings.HasPrefix(addr, "tcp://") {
		return "tcp", strings.TrimPrefix(addr, "tcp://")
	}
	if strings.HasPrefix(addr, "unix://") {
		return "unix", strings.TrimPrefix(addr, "unix://")
	}
	return "unix", addr
}
