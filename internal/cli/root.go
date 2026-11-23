package cli

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/Cinea4678/wecom-push/internal/config"
	"github.com/Cinea4678/wecom-push/internal/daemon"
	"github.com/spf13/cobra"
)

// GlobalOptions holds persistent CLI flags shared by all subcommands.
type GlobalOptions struct {
	ConfigPath string
	SocketAddr string
	JSON       bool
}

func (g *GlobalOptions) resolveConfigPath() (string, error) {
	return config.ResolvePath(g.ConfigPath)
}

func (g *GlobalOptions) resolveSocketAddr() (string, error) {
	return daemon.ResolveSocketAddr(g.SocketAddr)
}

func (g *GlobalOptions) loadConfig() (*config.Config, string, error) {
	p, err := g.resolveConfigPath()
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.Load(p)
	if err != nil {
		return nil, "", err
	}
	return cfg, p, nil
}

// NewRootCmd constructs the complete Cobra command tree.
func NewRootCmd() *cobra.Command {
	opts := &GlobalOptions{}

	root := &cobra.Command{
		Use:   "wecom-push",
		Short: "Send messages to WeCom (企业微信) users & groups via AI Bot WebSocket",
		Long: `wecom-push is a lightweight CLI + optional background daemon for pushing
notifications to WeCom (企业微信) users and groups over the official AI Bot
WebSocket long-connection protocol (wss://openws.work.weixin.qq.com).

All channels share one Bot identity (BotID + Secret), and each channel holds a
list of target users and/or groups. When the daemon is running, send and debug
commands automatically reuse its persistent WebSocket connection; otherwise
they seamlessly fall back to direct mode.`,
		SilenceUsage: true,
	}

	root.PersistentFlags().StringVar(&opts.ConfigPath, "config", "", "config file path (default ~/.config/wecom-push/config.yaml)")
	root.PersistentFlags().StringVar(&opts.SocketAddr, "socket", "", "daemon socket path or tcp://host:port")
	root.PersistentFlags().BoolVar(&opts.JSON, "json", false, "output structured JSON")

	root.AddCommand(
		newInitCmd(opts),
		newConfigCmd(opts),
		newAddCmd(opts, "add"),
		newListCmd(opts, "list"),
		newRemoveCmd(opts, "remove"),
		newChannelCmd(opts),
		newSendCmd(opts),
		newDebugCmd(opts),
		newDaemonCmd(opts),
		newVersionCmd(opts),
	)

	return root
}

// Execute runs the root CLI command.
func Execute() error {
	return NewRootCmd().Execute()
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func printLineJSON(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
