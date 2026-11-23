package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/Cinea4678/wecom-push/internal/config"
	"github.com/spf13/cobra"
)

func parseTargetFlags(users, groups, targets []string) ([]config.Target, error) {
	var out []config.Target
	for _, u := range users {
		t, err := config.ParseTargetSpec(config.TargetUser, u)
		if err != nil {
			return nil, fmt.Errorf("--user %q: %w", u, err)
		}
		out = append(out, t)
	}
	for _, g := range groups {
		t, err := config.ParseTargetSpec(config.TargetGroup, g)
		if err != nil {
			return nil, fmt.Errorf("--group %q: %w", g, err)
		}
		out = append(out, t)
	}
	for _, raw := range targets {
		t, err := config.ParseTargetSpec("", raw)
		if err != nil {
			return nil, fmt.Errorf("--target %q: %w", raw, err)
		}
		out = append(out, t)
	}
	return out, nil
}

func newAddCmd(opts *GlobalOptions, use string) *cobra.Command {
	var (
		users       []string
		groups      []string
		targets     []string
		desc        string
		appendMode  bool
		forceMode   bool
		makeDefault bool
	)

	cmd := &cobra.Command{
		Use:   use + " <channel>",
		Short: "Create a notification channel (or append recipients to an existing one)",
		Example: `  # Create channel "ops" targeting a group and two users (with optional labels)
  wecom-push add ops --group wr12345=生产告警群 --user zhangsan=张三 --user lisi

  # Append another user to an existing channel
  wecom-push add ops --append --user wangwu=王五

  # Create a personal channel and set it as default
  wecom-push add me --user zhangyao --default`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			parsed, err := parseTargetFlags(users, groups, targets)
			if err != nil {
				return err
			}

			cfg, cfgPath, err := opts.loadConfig()
			if err != nil {
				return err
			}

			if err := cfg.AddChannel(name, config.Channel{
				Description: strings.TrimSpace(desc),
				Targets:     parsed,
			}, appendMode, forceMode); err != nil {
				return err
			}
			if makeDefault {
				cfg.DefaultChannel = name
			}
			if err := config.Save(cfgPath, cfg); err != nil {
				return err
			}

			ch := cfg.Channels[name]
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"channel":         name,
					"description":     ch.Description,
					"targets":         ch.Targets,
					"default_channel": cfg.DefaultChannel,
				})
			}

			action := "Created"
			if appendMode {
				action = "Updated"
			} else if forceMode {
				action = "Saved"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s channel %q (%d recipient(s)):\n", action, name, len(ch.Targets))
			for _, t := range ch.Targets {
				fmt.Fprintf(cmd.OutOrStdout(), "  - %s\n", t.String())
			}
			if makeDefault {
				fmt.Fprintf(cmd.OutOrStdout(), "Set %q as default channel.\n", name)
			}
			return nil
		},
	}

	cmd.Flags().StringArrayVarP(&users, "user", "u", nil, "user recipient: <userid> or <userid>=<label> (repeatable)")
	cmd.Flags().StringArrayVarP(&groups, "group", "g", nil, "group recipient: <chatid> or <chatid>=<label> (repeatable)")
	cmd.Flags().StringArrayVarP(&targets, "target", "t", nil, "typed recipient: user:<id>[=<label>] or group:<id>[=<label>] (repeatable)")
	cmd.Flags().StringVar(&desc, "desc", "", "optional channel description")
	cmd.Flags().BoolVarP(&appendMode, "append", "a", false, "append recipients to existing channel")
	cmd.Flags().BoolVarP(&forceMode, "force", "f", false, "replace existing channel")
	cmd.Flags().BoolVar(&makeDefault, "default", false, "set as default channel")
	return cmd
}

func newListCmd(opts *GlobalOptions, use string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     use,
		Aliases: []string{"ls"},
		Short:   "List configured channels and their recipients",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, err := opts.loadConfig()
			if err != nil {
				return err
			}

			names := cfg.ChannelNames()
			if opts.JSON {
				type item struct {
					Name        string          `json:"name"`
					Default     bool            `json:"default"`
					Description string          `json:"description,omitempty"`
					Targets     []config.Target `json:"targets"`
				}
				items := make([]item, 0, len(names))
				for _, n := range names {
					ch := cfg.Channels[n]
					items = append(items, item{
						Name:        n,
						Default:     cfg.DefaultChannel == n,
						Description: ch.Description,
						Targets:     ch.Targets,
					})
				}
				return printJSON(cmd.OutOrStdout(), items)
			}

			if len(names) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No channels configured yet. Create one with:")
				fmt.Fprintln(cmd.OutOrStdout(), "  wecom-push add <channel> --user <userid> --group <chatid>")
				return nil
			}

			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "CHANNEL\tDEFAULT\tRECIPIENTS\tDESCRIPTION")
			for _, n := range names {
				ch := cfg.Channels[n]
				def := ""
				if cfg.DefaultChannel == n {
					def = "*"
				}
				parts := make([]string, 0, len(ch.Targets))
				for _, t := range ch.Targets {
					parts = append(parts, t.String())
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", n, def, strings.Join(parts, ", "), ch.Description)
			}
			return tw.Flush()
		},
	}
	return cmd
}

func newRemoveCmd(opts *GlobalOptions, use string) *cobra.Command {
	var (
		users   []string
		groups  []string
		targets []string
	)

	cmd := &cobra.Command{
		Use:     use + " <channel>",
		Aliases: []string{"rm"},
		Short:   "Remove a channel or specific recipients from a channel",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, cfgPath, err := opts.loadConfig()
			if err != nil {
				return err
			}

			toRemove, err := parseTargetFlags(users, groups, targets)
			if err != nil {
				return err
			}

			if len(toRemove) > 0 {
				removed, err := cfg.RemoveTargets(name, toRemove)
				if err != nil {
					return err
				}
				if err := config.Save(cfgPath, cfg); err != nil {
					return err
				}
				if opts.JSON {
					return printJSON(cmd.OutOrStdout(), map[string]any{
						"channel":   name,
						"removed":   removed,
						"remaining": len(cfg.Channels[name].Targets),
					})
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Removed %d recipient(s) from channel %q (%d remaining).\n", removed, name, len(cfg.Channels[name].Targets))
				return nil
			}

			if err := cfg.RemoveChannel(name); err != nil {
				return err
			}
			if err := config.Save(cfgPath, cfg); err != nil {
				return err
			}
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"removed_channel": name,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed channel %q.\n", name)
			return nil
		},
	}

	cmd.Flags().StringArrayVarP(&users, "user", "u", nil, "remove only specified user recipient(s)")
	cmd.Flags().StringArrayVarP(&groups, "group", "g", nil, "remove only specified group recipient(s)")
	cmd.Flags().StringArrayVarP(&targets, "target", "t", nil, "remove only specified typed recipient(s)")
	return cmd
}

func newChannelCmd(opts *GlobalOptions) *cobra.Command {
	chCmd := &cobra.Command{
		Use:     "channel",
		Aliases: []string{"ch"},
		Short:   "Manage notification channels",
	}

	showCmd := &cobra.Command{
		Use:   "show <channel>",
		Short: "Show details of a single channel",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			cfg, _, err := opts.loadConfig()
			if err != nil {
				return err
			}
			ch, ok := cfg.Channels[name]
			if !ok {
				return fmt.Errorf("channel %q not found", name)
			}
			if opts.JSON {
				return printJSON(cmd.OutOrStdout(), map[string]any{
					"name":        name,
					"default":     cfg.DefaultChannel == name,
					"description": ch.Description,
					"targets":     ch.Targets,
				})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Channel    : %s", name)
			if cfg.DefaultChannel == name {
				fmt.Fprint(cmd.OutOrStdout(), " (default)")
			}
			fmt.Fprintln(cmd.OutOrStdout())
			if ch.Description != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Description: %s\n", ch.Description)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Recipients (%d):\n", len(ch.Targets))
			for _, t := range ch.Targets {
				fmt.Fprintf(cmd.OutOrStdout(), "  - %s (chat_type=%d)\n", t.String(), t.Type.ChatType())
			}
			return nil
		},
	}

	var clearDefault bool
	defaultCmd := &cobra.Command{
		Use:   "default [channel]",
		Short: "Set or clear the default channel",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := opts.loadConfig()
			if err != nil {
				return err
			}
			if clearDefault {
				cfg.DefaultChannel = ""
				if err := config.Save(cfgPath, cfg); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Cleared default channel.")
				return nil
			}
			if len(args) == 0 {
				if cfg.DefaultChannel == "" {
					fmt.Fprintln(cmd.OutOrStdout(), "No default channel set.")
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), cfg.DefaultChannel)
				}
				return nil
			}
			name := args[0]
			if _, ok := cfg.Channels[name]; !ok {
				return fmt.Errorf("channel %q not found", name)
			}
			cfg.DefaultChannel = name
			if err := config.Save(cfgPath, cfg); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Default channel set to %q.\n", name)
			return nil
		},
	}
	defaultCmd.Flags().BoolVar(&clearDefault, "clear", false, "clear the default channel")

	chCmd.AddCommand(
		newAddCmd(opts, "add"),
		newListCmd(opts, "list"),
		showCmd,
		newRemoveCmd(opts, "remove"),
		defaultCmd,
	)
	return chCmd
}
