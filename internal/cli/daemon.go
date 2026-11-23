package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Cinea4678/wecom-push/internal/daemon"
	"github.com/spf13/cobra"
)

func newDaemonCmd(opts *GlobalOptions) *cobra.Command {
	daemonCmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the background WeCom long-connection daemon",
		Long: `The background daemon maintains a single persistent WebSocket connection to
WeCom (with 30s heartbeat & automatic reconnect) and exposes a local Unix socket
API. Running the daemon avoids WeCom's aibot_subscribe rate limit, prevents
concurrent CLI invocations from kicking each other, and lets 'wecom-push debug'
inspect live callbacks seamlessly.`,
	}

	var (
		heartbeat     time.Duration
		kickedBackoff time.Duration
		logLevel      string
	)
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground (ideal for systemd / launchd / Docker)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := opts.resolveConfigPath()
			if err != nil {
				return err
			}
			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}

			level := slog.LevelInfo
			switch strings.ToLower(logLevel) {
			case "debug":
				level = slog.LevelDebug
			case "warn":
				level = slog.LevelWarn
			case "error":
				level = slog.LevelError
			}
			logger := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level}))

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			srv := daemon.NewServer(daemon.ServerOptions{
				ConfigPath:        cfgPath,
				ListenAddr:        sockAddr,
				HeartbeatInterval: heartbeat,
				KickedBackoff:     kickedBackoff,
				Logger:            logger,
			})
			return srv.Run(ctx)
		},
	}
	runCmd.Flags().DurationVar(&heartbeat, "heartbeat", 30*time.Second, "WeCom WebSocket ping interval")
	runCmd.Flags().DurationVar(&kickedBackoff, "kicked-backoff", 30*time.Second, "wait duration before reconnecting after disconnected_event")
	runCmd.Flags().StringVar(&logLevel, "log-level", "info", "log level: debug, info, warn, error")

	var (
		noWait  bool
		logPath string
	)
	startCmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := opts.resolveConfigPath()
			if err != nil {
				return err
			}
			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			st, err := daemon.StartBackground(ctx, cfgPath, sockAddr, logPath, !noWait)
			if err != nil {
				return err
			}
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), st)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Daemon started (pid=%d, state=%s, socket=%s)\n", st.PID, st.State, sockAddr)
			if st.LastError != "" {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: %s\n", st.LastError)
			}
			return nil
		},
	}
	startCmd.Flags().BoolVar(&noWait, "no-wait", false, "return as soon as IPC socket is listening without waiting for WeCom handshake")
	startCmd.Flags().StringVar(&logPath, "log-file", "", "daemon log file path (default ~/.local/state/wecom-push/daemon.log)")

	stopCmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := daemon.StopBackground(ctx, sockAddr); err != nil {
				return err
			}
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{"stopped": true, "socket": sockAddr})
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Daemon stopped.")
			return nil
		},
	}

	restartCmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart the background daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := opts.resolveConfigPath()
			if err != nil {
				return err
			}
			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()

			_ = daemon.StopBackground(ctx, sockAddr)
			st, err := daemon.StartBackground(ctx, cfgPath, sockAddr, "", true)
			if err != nil {
				return err
			}
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), st)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Daemon restarted (pid=%d, state=%s)\n", st.PID, st.State)
			return nil
		},
	}

	statusCmd := &cobra.Command{
		Use:   "status",
		Short: "Show daemon connection status and metrics",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			st, err := daemon.NewClient(sockAddr).Status(ctx)
			if err != nil {
				if opts.JSON {
					return printJSON(cmd.OutOrStdout(), map[string]any{
						"running": false,
						"socket":  sockAddr,
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Daemon is not running (socket: %s)\n", sockAddr)
				return nil
			}
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), st)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Status          : running (%s)\n", st.State)
			fmt.Fprintf(cmd.OutOrStdout(), "PID             : %d\n", st.PID)
			fmt.Fprintf(cmd.OutOrStdout(), "Version         : %s\n", st.Version)
			fmt.Fprintf(cmd.OutOrStdout(), "Socket          : %s\n", sockAddr)
			fmt.Fprintf(cmd.OutOrStdout(), "Uptime          : %s\n", (time.Duration(st.UptimeSeconds) * time.Second).String())
			fmt.Fprintf(cmd.OutOrStdout(), "Bot ID          : %s\n", emptyFallback(st.BotID, "(not configured)"))
			fmt.Fprintf(cmd.OutOrStdout(), "WebSocket URL   : %s\n", st.WSURL)
			if !st.ConnectedAt.IsZero() {
				fmt.Fprintf(cmd.OutOrStdout(), "Connected Since : %s\n", st.ConnectedAt.Format(time.RFC3339))
			}
			if !st.LastPingAt.IsZero() {
				fmt.Fprintf(cmd.OutOrStdout(), "Last Heartbeat  : %s\n", st.LastPingAt.Format(time.RFC3339))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Messages Sent   : %d\n", st.MessagesSent)
			fmt.Fprintf(cmd.OutOrStdout(), "Events Received : %d\n", st.EventsReceived)
			fmt.Fprintf(cmd.OutOrStdout(), "Debug Listeners : %d\n", st.EventSubscribers)
			fmt.Fprintf(cmd.OutOrStdout(), "Reconnects      : %d\n", st.Reconnects)
			if st.LastError != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Last Error      : %s\n", st.LastError)
			}
			return nil
		},
	}

	reloadCmd := &cobra.Command{
		Use:   "reload",
		Short: "Reload configuration and reconnect the daemon's WebSocket session",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			if err := daemon.NewClient(sockAddr).Reload(ctx); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Reload signal sent to daemon.")
			return nil
		},
	}

	var tailLines int
	logsCmd := &cobra.Command{
		Use:   "logs",
		Short: "Show recent background daemon logs",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := daemon.DefaultLogPath()
			if err != nil {
				return err
			}
			f, err := os.Open(p)
			if err != nil {
				return fmt.Errorf("open daemon log %s: %w", p, err)
			}
			defer f.Close()

			var lines []string
			sc := bufio.NewScanner(f)
			for sc.Scan() {
				lines = append(lines, sc.Text())
			}
			if err := sc.Err(); err != nil && err != io.EOF {
				return err
			}
			if tailLines > 0 && len(lines) > tailLines {
				lines = lines[len(lines)-tailLines:]
			}
			for _, l := range lines {
				fmt.Fprintln(cmd.OutOrStdout(), l)
			}
			return nil
		},
	}
	logsCmd.Flags().IntVarP(&tailLines, "lines", "n", 50, "number of trailing lines to show (0 for all)")

	serviceCmd := &cobra.Command{
		Use:   "service <systemd|launchd>",
		Short: "Print a systemd user unit or macOS launchd plist for running the daemon automatically",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				exe = "/usr/local/bin/wecom-push"
			}
			logFile, _ := daemon.DefaultLogPath()
			switch strings.ToLower(args[0]) {
			case "systemd":
				fmt.Fprintf(cmd.OutOrStdout(), `[Unit]
Description=wecom-push WeCom AI Bot Long-Connection Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=%s daemon run
Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
`, exe)
			case "launchd":
				fmt.Fprintf(cmd.OutOrStdout(), `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>cc.cinea.wecom-push</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>daemon</string>
    <string>run</string>
  </array>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <true/>
  <key>StandardOutPath</key>
  <string>%s</string>
  <key>StandardErrorPath</key>
  <string>%s</string>
</dict>
</plist>
`, exe, logFile, logFile)
			default:
				return fmt.Errorf("unsupported service format %q (expected systemd or launchd)", args[0])
			}
			return nil
		},
	}

	daemonCmd.AddCommand(runCmd, startCmd, stopCmd, restartCmd, statusCmd, reloadCmd, logsCmd, serviceCmd)
	return daemonCmd
}
