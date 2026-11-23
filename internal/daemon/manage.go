package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// StartBackground spawns `wecom-push daemon run` as a detached background process and waits for it to listen.
func StartBackground(ctx context.Context, configPath, socketAddr, logPath string, waitConnect bool) (*StatusResponse, error) {
	client := NewClient(socketAddr)
	if st, err := client.Status(ctx); err == nil {
		return st, fmt.Errorf("daemon is already running (pid=%d, state=%s)", st.PID, st.State)
	}

	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolve executable path: %w", err)
	}

	if logPath == "" {
		logPath, err = DefaultLogPath()
		if err != nil {
			return nil, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, fmt.Errorf("create log directory: %w", err)
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open daemon log %s: %w", logPath, err)
	}
	defer logFile.Close()

	args := []string{"daemon", "run", "--socket", socketAddr}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	cmd := exec.Command(exe, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	detachCmd(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start background daemon: %w", err)
	}

	// Best-effort PID file write.
	if pidPath, err := DefaultPIDPath(); err == nil {
		_ = os.MkdirAll(filepath.Dir(pidPath), 0o700)
		_ = os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(10 * time.Second)
	var lastStatus *StatusResponse
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
		st, err := client.Status(probeCtx)
		cancel()
		if err == nil {
			lastStatus = st
			if !waitConnect || st.State == "connected" || st.LastError != "" {
				return st, nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	if lastStatus != nil {
		return lastStatus, nil
	}
	return nil, fmt.Errorf("daemon did not become ready within 10s (check logs at %s)", logPath)
}

// StopBackground gracefully stops the running daemon via IPC shutdown (falling back to PID signal).
func StopBackground(ctx context.Context, socketAddr string) error {
	client := NewClient(socketAddr)
	if err := client.Shutdown(ctx); err == nil {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if !client.IsRunning(ctx) {
				removePIDFile()
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return nil
	}

	// Fallback: check PID file.
	pidPath, err := DefaultPIDPath()
	if err != nil {
		return fmt.Errorf("daemon is not running on %s", socketAddr)
	}
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("daemon is not running on %s", socketAddr)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		removePIDFile()
		return fmt.Errorf("daemon is not running on %s", socketAddr)
	}
	if err := terminateProcess(pid); err != nil {
		removePIDFile()
		return fmt.Errorf("daemon is not running (stale pid %d): %w", pid, err)
	}
	removePIDFile()
	return nil
}

func removePIDFile() {
	if pidPath, err := DefaultPIDPath(); err == nil {
		_ = os.Remove(pidPath)
	}
}
