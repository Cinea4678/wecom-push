package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Cinea4678/wecom-push/internal/daemon"
	"github.com/Cinea4678/wecom-push/internal/protocol"
	"github.com/spf13/cobra"
)

func newDebugCmd(opts *GlobalOptions) *cobra.Command {
	var (
		showRaw     bool
		autoReply   bool
		forceDirect bool
		forceDaemon bool
	)

	cmd := &cobra.Command{
		Use:   "debug",
		Short: "Listen for incoming bot messages/events to discover User IDs and Group Chat IDs",
		Long: `Start interactive debug listener mode to discover WeCom User IDs (userid) and
Group Chat IDs (chatid).

How to use:
  1. Run 'wecom-push debug'
  2. In WeCom, send a direct message to your bot (to get your userid), OR
     @mention your bot inside a group chat (to get the group's chatid).
  3. Copy the printed ID (or the suggested 'wecom-push add ...' command).

Note: If the local daemon is already running, 'debug' attaches to the daemon's
event stream without disrupting its WebSocket connection. Otherwise it connects
directly to WeCom.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if forceDirect && forceDaemon {
				return errors.New("--direct and --daemon cannot be used together")
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}

			dc := daemon.NewClient(sockAddr)
			useDaemon := false
			if forceDaemon {
				if !dc.IsRunning(ctx) {
					return fmt.Errorf("daemon is not running on %s (start it with `wecom-push daemon start`)", sockAddr)
				}
				useDaemon = true
			} else if !forceDirect {
				useDaemon = dc.IsRunning(ctx)
			}

			out := cmd.OutOrStdout()
			errOut := cmd.ErrOrStderr()

			if useDaemon {
				if !opts.JSON {
					fmt.Fprintf(errOut, "Attached to running daemon (%s). Waiting for WeCom callbacks... (Ctrl+C to quit)\n", sockAddr)
					fmt.Fprintln(errOut, "Tip: DM the bot to get your User ID, or @bot in a group to get the Group Chat ID.")
				}
				return dc.StreamEvents(ctx, func(ev protocol.CallbackEvent) {
					renderDebugEvent(out, ev, opts.JSON, showRaw)
					if autoReply && ev.Cmd == protocol.CmdMsgCallback && ev.ReqID != "" {
						replyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
						defer cancel()
						if err := dc.Respond(replyCtx, ev.ReqID, formatDebugReply(ev)); err != nil {
							fmt.Fprintf(errOut, "  [warn] auto-reply failed: %v\n", err)
						}
					}
				})
			}

			// Direct mode: establish a temporary WebSocket connection directly to WeCom.
			cfg, _, err := opts.loadConfig()
			if err != nil {
				return err
			}
			bot := cfg.ResolvedBot()
			if err := bot.Validate(); err != nil {
				return err
			}

			if !opts.JSON {
				fmt.Fprintf(errOut, "Connecting directly to %s (bot=%s)...\n", bot.WSURL, bot.ID)
			}

			var client *protocol.Client
			client, err = protocol.Connect(ctx, protocol.Options{
				WSURL:  bot.WSURL,
				BotID:  bot.ID,
				Secret: bot.Secret,
				OnCallback: func(ev protocol.CallbackEvent) {
					renderDebugEvent(out, ev, opts.JSON, showRaw)
					if autoReply && ev.Cmd == protocol.CmdMsgCallback && ev.ReqID != "" && client != nil {
						replyCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
						defer cancel()
						if err := client.RespondMarkdown(replyCtx, ev.ReqID, formatDebugReply(ev)); err != nil {
							fmt.Fprintf(errOut, "  [warn] auto-reply failed: %v\n", err)
						}
					}
				},
			})
			if err != nil {
				return err
			}
			defer client.Close()

			if !opts.JSON {
				fmt.Fprintln(errOut, "Connected! Listening for WeCom messages/events... (Ctrl+C to quit)")
				fmt.Fprintln(errOut, "Tip: DM the bot to get your User ID, or @bot in a group to get the Group Chat ID.")
			}

			select {
			case <-ctx.Done():
				return nil
			case <-client.Done():
				if err := client.Err(); err != nil && !errors.Is(err, protocol.ErrClientClosed) {
					return err
				}
				return nil
			}
		},
	}

	cmd.Flags().BoolVar(&showRaw, "raw", false, "also print full raw JSON callback frames")
	cmd.Flags().BoolVar(&autoReply, "reply", false, "reply back in WeCom chat echoing the discovered chatid/userid")
	cmd.Flags().BoolVar(&forceDirect, "direct", false, "connect directly to WeCom WSS (warning: will kick any existing connection)")
	cmd.Flags().BoolVar(&forceDaemon, "daemon", false, "require running daemon event stream")
	return cmd
}

func renderDebugEvent(w io.Writer, ev protocol.CallbackEvent, asJSON, showRaw bool) {
	if asJSON {
		_ = printLineJSON(w, ev)
		return
	}

	ts := ev.ReceivedAt.Format("15:04:05")
	fmt.Fprintf(w, "\n[%s] %s", ts, ev.Cmd)
	if ev.ChatType != "" {
		fmt.Fprintf(w, " (%s)", ev.ChatType)
	}
	if ev.EventType != "" {
		fmt.Fprintf(w, " event=%s", ev.EventType)
	} else if ev.MsgType != "" {
		fmt.Fprintf(w, " msgtype=%s", ev.MsgType)
	}
	fmt.Fprintln(w)

	if ev.FromUserID != "" {
		fmt.Fprintf(w, "  User ID    : %s\n", ev.FromUserID)
	}
	if ev.ChatID != "" {
		fmt.Fprintf(w, "  Group ID   : %s\n", ev.ChatID)
	}
	if ev.TextContent != "" {
		preview := strings.ReplaceAll(strings.TrimSpace(ev.TextContent), "\n", " ")
		if len(preview) > 120 {
			preview = preview[:120] + "..."
		}
		fmt.Fprintf(w, "  Content    : %s\n", preview)
	}

	if ev.ChatID != "" {
		fmt.Fprintf(w, "  Add Group  : wecom-push add <channel> --group %s\n", ev.ChatID)
	}
	if ev.FromUserID != "" {
		fmt.Fprintf(w, "  Add User   : wecom-push add <channel> --user %s\n", ev.FromUserID)
	}

	if showRaw && len(ev.Raw) > 0 {
		var pretty bytes.Buffer
		if json.Indent(&pretty, ev.Raw, "  ", "  ") == nil {
			fmt.Fprintf(w, "  Raw Frame  :\n  %s\n", pretty.String())
		} else {
			fmt.Fprintf(w, "  Raw Frame  : %s\n", string(ev.Raw))
		}
	}
}

func formatDebugReply(ev protocol.CallbackEvent) string {
	var sb strings.Builder
	sb.WriteString("### wecom-push Debug Info\n")
	if ev.ChatType != "" {
		sb.WriteString(fmt.Sprintf("- **Chat Type**: `%s`\n", ev.ChatType))
	}
	if ev.FromUserID != "" {
		sb.WriteString(fmt.Sprintf("- **User ID**: `%s`\n", ev.FromUserID))
	}
	if ev.ChatID != "" {
		sb.WriteString(fmt.Sprintf("- **Group Chat ID**: `%s`\n", ev.ChatID))
	}
	sb.WriteString("\nQuick add command:\n```sh\n")
	if ev.ChatID != "" {
		sb.WriteString(fmt.Sprintf("wecom-push add <channel> --group %s\n", ev.ChatID))
	} else if ev.FromUserID != "" {
		sb.WriteString(fmt.Sprintf("wecom-push add <channel> --user %s\n", ev.FromUserID))
	}
	sb.WriteString("```")
	return sb.String()
}
