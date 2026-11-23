package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Cinea4678/wecom-push/internal/config"
	"github.com/Cinea4678/wecom-push/internal/daemon"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newInitCmd(opts *GlobalOptions) *cobra.Command {
	var (
		botID  string
		secret string
		wsURL  string
	)

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Configure the shared WeCom AI Bot identity (BotID + Secret)",
		Long: `Configure the BotID and long-connection Secret shared by all channels.
If flags are omitted in an interactive terminal, you will be prompted securely
(the Secret input is hidden and will not appear in shell history).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := opts.loadConfig()
			if err != nil {
				return err
			}

			isTTY := term.IsTerminal(int(os.Stdin.Fd()))
			reader := bufio.NewReader(cmd.InOrStdin())

			if strings.TrimSpace(botID) == "" {
				if !isTTY && cfg.Bot.ID == "" {
					return fmt.Errorf("--bot-id is required in non-interactive mode")
				}
				if isTTY {
					prompt := "WeCom Bot ID"
					if cfg.Bot.ID != "" {
						prompt += fmt.Sprintf(" [%s]", cfg.Bot.ID)
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s: ", prompt)
					line, _ := reader.ReadString('\n')
					botID = strings.TrimSpace(line)
				}
				if botID == "" {
					botID = cfg.Bot.ID
				}
			}

			if strings.TrimSpace(secret) == "" {
				if !isTTY && cfg.Bot.Secret == "" {
					return fmt.Errorf("--secret is required in non-interactive mode")
				}
				if isTTY {
					prompt := "WeCom Bot Secret (input hidden)"
					if cfg.Bot.Secret != "" {
						prompt += " [press Enter to keep existing]"
					}
					fmt.Fprintf(cmd.OutOrStdout(), "%s: ", prompt)
					raw, err := term.ReadPassword(int(os.Stdin.Fd()))
					fmt.Fprintln(cmd.OutOrStdout())
					if err != nil {
						return fmt.Errorf("read secret: %w", err)
					}
					secret = strings.TrimSpace(string(raw))
				}
				if secret == "" {
					secret = cfg.Bot.Secret
				}
			}

			cfg.Bot.ID = strings.TrimSpace(botID)
			cfg.Bot.Secret = strings.TrimSpace(secret)
			if cmd.Flags().Changed("ws-url") {
				cfg.Bot.WSURL = strings.TrimSpace(wsURL)
			}

			if err := cfg.Bot.Validate(); err != nil {
				return err
			}
			if err := config.Save(cfgPath, cfg); err != nil {
				return err
			}

			// Notify running daemon if reachable.
			reloaded := false
			if sockAddr, err := opts.resolveSocketAddr(); err == nil {
				dc := daemon.NewClient(sockAddr)
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				if dc.Reload(ctx) == nil {
					reloaded = true
				}
				cancel()
			}

			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"config_path":     cfgPath,
					"bot_id":          cfg.Bot.ID,
					"secret_masked":   config.MaskSecret(cfg.Bot.Secret),
					"ws_url":          cfg.ResolvedBot().WSURL,
					"daemon_reloaded": reloaded,
				})
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Saved Bot configuration to %s\n", cfgPath)
			fmt.Fprintf(cmd.OutOrStdout(), "  Bot ID : %s\n", cfg.Bot.ID)
			fmt.Fprintf(cmd.OutOrStdout(), "  Secret : %s\n", config.MaskSecret(cfg.Bot.Secret))
			fmt.Fprintf(cmd.OutOrStdout(), "  WS URL : %s\n", cfg.ResolvedBot().WSURL)
			if reloaded {
				fmt.Fprintln(cmd.OutOrStdout(), "  Daemon : reloaded active connection")
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nNext steps:")
			fmt.Fprintln(cmd.OutOrStdout(), "  1. Run `wecom-push debug` and send a message to the bot (or @bot in a group) to get IDs")
			fmt.Fprintln(cmd.OutOrStdout(), "  2. Create a channel: `wecom-push add ops --group <chatid> --user <userid>`")
			fmt.Fprintln(cmd.OutOrStdout(), "  3. Push a message:   `wecom-push send ops \"Hello from wecom-push\"`")
			return nil
		},
	}

	cmd.Flags().StringVar(&botID, "bot-id", "", "WeCom AI Bot ID")
	cmd.Flags().StringVar(&secret, "secret", "", "WeCom AI Bot long-connection Secret")
	cmd.Flags().StringVar(&wsURL, "ws-url", "", "Custom WebSocket URL (default wss://openws.work.weixin.qq.com)")
	return cmd
}

func newConfigCmd(opts *GlobalOptions) *cobra.Command {
	cfgCmd := &cobra.Command{
		Use:   "config",
		Short: "Inspect configuration and paths",
	}

	showCmd := &cobra.Command{
		Use:   "show",
		Short: "Show current configuration (with Secret masked)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := opts.loadConfig()
			if err != nil {
				return err
			}
			resolved := cfg.ResolvedBot()
			sockAddr, _ := opts.resolveSocketAddr()

			out := map[string]any{
				"config_path":     cfgPath,
				"socket_addr":     sockAddr,
				"bot_id":          resolved.ID,
				"secret_masked":   config.MaskSecret(resolved.Secret),
				"ws_url":          resolved.WSURL,
				"default_channel": cfg.DefaultChannel,
				"channel_count":   len(cfg.Channels),
			}
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), out)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Config file    : %s\n", cfgPath)
			fmt.Fprintf(cmd.OutOrStdout(), "Daemon socket  : %s\n", sockAddr)
			fmt.Fprintf(cmd.OutOrStdout(), "Bot ID         : %s\n", emptyFallback(resolved.ID, "(not set)"))
			fmt.Fprintf(cmd.OutOrStdout(), "Bot Secret     : %s\n", config.MaskSecret(resolved.Secret))
			fmt.Fprintf(cmd.OutOrStdout(), "WebSocket URL  : %s\n", resolved.WSURL)
			fmt.Fprintf(cmd.OutOrStdout(), "Default channel: %s\n", emptyFallback(cfg.DefaultChannel, "(none)"))
			fmt.Fprintf(cmd.OutOrStdout(), "Channels       : %d\n", len(cfg.Channels))
			return nil
		},
	}

	pathCmd := &cobra.Command{
		Use:   "path",
		Short: "Print resolved config file path",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := opts.resolveConfigPath()
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}

	cfgCmd.AddCommand(showCmd, pathCmd)
	return cfgCmd
}

func emptyFallback(s, fb string) string {
	if strings.TrimSpace(s) == "" {
		return fb
	}
	return s
}
