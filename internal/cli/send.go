package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Cinea4678/wecom-push/internal/config"
	"github.com/Cinea4678/wecom-push/internal/daemon"
	"github.com/Cinea4678/wecom-push/internal/protocol"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func newSendCmd(opts *GlobalOptions) *cobra.Command {
	var (
		channelName string
		message     string
		filePath    string
		title       string
		users       []string
		groups      []string
		targets     []string
		imagePath   string
		attachPath  string
		cardSpec    string
		forceDirect bool
		forceDaemon bool
		failFast    bool
		timeout     time.Duration
	)

	cmd := &cobra.Command{
		Use:   "send [channel] [message]",
		Short: "Send a message to a configured channel or ad-hoc recipients",
		Example: `  # Send markdown message to a channel
  wecom-push send ops "**Deployment succeeded** on prod-01"

  # Pipe content from stdin with a title
  tail -n 30 app.log | wecom-push send ops --title "Error Logs"

  # Send to the default channel
  wecom-push send "Backup completed"

  # Send ad-hoc to a user or group without creating a channel
  wecom-push send --user zhangsan "Ping!"

  # Send an image or file attachment to a channel
  wecom-push send ops --image ./chart.png
  wecom-push send ops --attachment ./report.pdf

  # Force one-shot direct mode (skip daemon)
  wecom-push send --direct ops "CI build passed"`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if forceDirect && forceDaemon {
				return errors.New("--direct and --daemon cannot be used together")
			}

			cfg, _, err := opts.loadConfig()
			if err != nil {
				return err
			}

			adhocTargets, err := parseTargetFlags(users, groups, targets)
			if err != nil {
				return err
			}

			// Resolve positional arguments cleanly.
			chName := strings.TrimSpace(channelName)
			posMsg := ""
			switch len(args) {
			case 2:
				if chName != "" {
					return errors.New("cannot specify both --channel and two positional arguments")
				}
				chName = args[0]
				posMsg = args[1]
			case 1:
				if chName != "" || len(adhocTargets) > 0 {
					posMsg = args[0]
				} else if _, ok := cfg.Channels[args[0]]; ok {
					chName = args[0]
				} else if cfg.DefaultChannel != "" {
					chName = cfg.DefaultChannel
					posMsg = args[0]
				} else {
					return fmt.Errorf("channel %q not found (run `wecom-push list` to view channels)", args[0])
				}
			case 0:
				if chName == "" && len(adhocTargets) == 0 {
					chName = cfg.DefaultChannel
				}
			}

			// Assemble final recipient list (channel targets + ad-hoc targets, deduplicated).
			recipients := make([]config.Target, 0)
			seen := make(map[string]bool)
			appendUnique := func(list []config.Target) {
				for _, t := range list {
					k := t.Key()
					if !seen[k] {
						seen[k] = true
						recipients = append(recipients, t)
					}
				}
			}

			if chName != "" {
				ch, ok := cfg.Channels[chName]
				if !ok {
					return fmt.Errorf("channel %q not found", chName)
				}
				appendUnique(ch.Targets)
			}
			appendUnique(adhocTargets)

			if len(recipients) == 0 {
				return errors.New("no recipients specified (provide a channel, set a default channel, or pass --user/--group)")
			}

			// Resolve markdown / text message body if any.
			mdContent, err := resolveMessageText(cmd.InOrStdin(), message, posMsg, filePath, title, imagePath == "" && attachPath == "" && cardSpec == "")
			if err != nil {
				return err
			}

			// Resolve template_card JSON if provided.
			var cardJSON json.RawMessage
			if strings.TrimSpace(cardSpec) != "" {
				cardJSON, err = readCardJSON(cardSpec)
				if err != nil {
					return err
				}
			}

			if mdContent == "" && len(cardJSON) == 0 && imagePath == "" && attachPath == "" {
				return errors.New("message content is empty (pass a message argument, -m, -f, stdin, --image, --attachment, or --card)")
			}

			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()

			sockAddr, err := opts.resolveSocketAddr()
			if err != nil {
				return err
			}

			useDaemon := false
			dc := daemon.NewClient(sockAddr)
			if forceDaemon {
				if !dc.IsRunning(ctx) {
					return fmt.Errorf("daemon is not running on %s (start it with `wecom-push daemon start`)", sockAddr)
				}
				useDaemon = true
			} else if !forceDirect {
				useDaemon = dc.IsRunning(ctx)
			}

			// Helper for uploading media via active transport.
			var directClient *protocol.Client
			if !useDaemon {
				bot := cfg.ResolvedBot()
				if err := bot.Validate(); err != nil {
					return err
				}
				directClient, err = protocol.Connect(ctx, protocol.Options{
					WSURL:          bot.WSURL,
					BotID:          bot.ID,
					Secret:         bot.Secret,
					RequestTimeout: timeout,
				})
				if err != nil {
					return fmt.Errorf("connect to WeCom: %w", err)
				}
				defer directClient.Close()
			}

			uploadMedia := func(mediaType, path string) (string, error) {
				absPath, err := filepath.Abs(path)
				if err != nil {
					return "", err
				}
				if useDaemon {
					return dc.Upload(ctx, mediaType, absPath)
				}
				return directClient.UploadMediaFile(ctx, mediaType, absPath)
			}

			var imageMediaID, fileMediaID string
			if imagePath != "" {
				imageMediaID, err = uploadMedia("image", imagePath)
				if err != nil {
					return fmt.Errorf("upload image %s: %w", imagePath, err)
				}
			}
			if attachPath != "" {
				fileMediaID, err = uploadMedia("file", attachPath)
				if err != nil {
					return fmt.Errorf("upload attachment %s: %w", attachPath, err)
				}
			}

			// Build outbound SendItems for all recipients and content payloads.
			items := make([]daemon.SendItem, 0, len(recipients))
			for _, t := range recipients {
				if mdContent != "" {
					items = append(items, daemon.SendItem{
						Target: t,
						Body: protocol.SendMsgBody{
							ChatID:   t.ID,
							ChatType: t.Type.ChatType(),
							MsgType:  "markdown",
							Markdown: &protocol.MarkdownBody{Content: mdContent},
						},
					})
				}
				if len(cardJSON) > 0 {
					items = append(items, daemon.SendItem{
						Target: t,
						Body: protocol.SendMsgBody{
							ChatID:       t.ID,
							ChatType:     t.Type.ChatType(),
							MsgType:      "template_card",
							TemplateCard: cardJSON,
						},
					})
				}
				if imageMediaID != "" {
					items = append(items, daemon.SendItem{
						Target: t,
						Body: protocol.SendMsgBody{
							ChatID:   t.ID,
							ChatType: t.Type.ChatType(),
							MsgType:  "image",
							Image:    &protocol.MediaBody{MediaID: imageMediaID},
						},
					})
				}
				if fileMediaID != "" {
					items = append(items, daemon.SendItem{
						Target: t,
						Body: protocol.SendMsgBody{
							ChatID:   t.ID,
							ChatType: t.Type.ChatType(),
							MsgType:  "file",
							File:     &protocol.MediaBody{MediaID: fileMediaID},
						},
					})
				}
			}

			var resp *daemon.SendResponse
			mode := "direct"
			if useDaemon {
				mode = "daemon"
				resp, err = dc.Send(ctx, daemon.SendRequest{
					Items:    items,
					FailFast: failFast,
				})
				if err != nil {
					return err
				}
			} else {
				resp = &daemon.SendResponse{
					Results: make([]daemon.SendItemResult, 0, len(items)),
				}
				for _, item := range items {
					reqID, sendErr := directClient.SendMessage(ctx, item.Body)
					if sendErr != nil {
						resp.Failed++
						resp.Results = append(resp.Results, daemon.SendItemResult{
							Target: item.Target,
							ReqID:  reqID,
							OK:     false,
							Error:  sendErr.Error(),
						})
						if failFast {
							break
						}
						continue
					}
					resp.Sent++
					resp.Results = append(resp.Results, daemon.SendItemResult{
						Target: item.Target,
						ReqID:  reqID,
						OK:     true,
					})
				}
			}

			if opts.JSON {
				_ = printJSON(cmd.OutOrStdout(), map[string]any{
					"mode":    mode,
					"channel": chName,
					"sent":    resp.Sent,
					"failed":  resp.Failed,
					"results": resp.Results,
				})
			} else {
				for _, r := range resp.Results {
					if r.OK {
						fmt.Fprintf(cmd.OutOrStdout(), "[OK]   %s (via %s)\n", r.Target.String(), mode)
					} else {
						fmt.Fprintf(cmd.ErrOrStderr(), "[FAIL] %s: %s\n", r.Target.String(), r.Error)
					}
				}
			}

			if resp.Failed > 0 {
				return fmt.Errorf("%d of %d message(s) failed", resp.Failed, resp.Sent+resp.Failed)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&channelName, "channel", "c", "", "target channel name")
	cmd.Flags().StringVarP(&message, "message", "m", "", "message content (markdown supported)")
	cmd.Flags().StringVarP(&filePath, "file", "f", "", "read message content from file")
	cmd.Flags().StringVar(&title, "title", "", "optional markdown heading prepended to message")
	cmd.Flags().StringArrayVarP(&users, "user", "u", nil, "ad-hoc user recipient (repeatable)")
	cmd.Flags().StringArrayVarP(&groups, "group", "g", nil, "ad-hoc group recipient (repeatable)")
	cmd.Flags().StringArrayVarP(&targets, "target", "t", nil, "ad-hoc typed recipient user:<id> or group:<id> (repeatable)")
	cmd.Flags().StringVar(&imagePath, "image", "", "upload and send an image file (png/jpg/gif, <=10MB)")
	cmd.Flags().StringVar(&attachPath, "attachment", "", "upload and send a file attachment (<=20MB)")
	cmd.Flags().StringVar(&cardSpec, "card", "", "template_card JSON string or @/path/to/card.json")
	cmd.Flags().BoolVar(&forceDirect, "direct", false, "force one-shot direct WebSocket connection (bypass daemon)")
	cmd.Flags().BoolVar(&forceDaemon, "daemon", false, "require running daemon (fail if daemon is stopped)")
	cmd.Flags().BoolVar(&failFast, "fail-fast", false, "stop sending to remaining recipients after first failure")
	cmd.Flags().DurationVar(&timeout, "timeout", 20*time.Second, "send operation timeout")
	return cmd
}

func resolveMessageText(stdin io.Reader, flagMsg, posMsg, filePath, title string, allowStdin bool) (string, error) {
	if flagMsg != "" && posMsg != "" {
		return "", errors.New("cannot specify both positional message and --message")
	}
	content := flagMsg
	if content == "" {
		content = posMsg
	}

	if filePath != "" {
		b, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("read message file %s: %w", filePath, err)
		}
		content = string(b)
	} else if content == "-" || (content == "" && allowStdin && isReadablePipe(stdin)) {
		b, err := io.ReadAll(io.LimitReader(stdin, protocol.MaxMarkdownBytes+1024))
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		content = string(b)
	}

	content = strings.TrimRight(content, "\r\n")
	if content == "" {
		if title != "" {
			return "### " + strings.TrimSpace(title), nil
		}
		return "", nil
	}
	if strings.TrimSpace(title) != "" {
		content = fmt.Sprintf("### %s\n\n%s", strings.TrimSpace(title), content)
	}
	if len(content) > protocol.MaxMarkdownBytes {
		return "", fmt.Errorf("message exceeds WeCom markdown limit of %d bytes (got %d)", protocol.MaxMarkdownBytes, len(content))
	}
	return content, nil
}

func isReadablePipe(r io.Reader) bool {
	if f, ok := r.(*os.File); ok {
		return !term.IsTerminal(int(f.Fd()))
	}
	// Non-*os.File reader (e.g. bytes.Buffer in tests) is considered piped input.
	return r != nil
}

func readCardJSON(spec string) (json.RawMessage, error) {
	spec = strings.TrimSpace(spec)
	var raw []byte
	if strings.HasPrefix(spec, "@") {
		p := strings.TrimPrefix(spec, "@")
		var err error
		raw, err = os.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("read card file %s: %w", p, err)
		}
	} else {
		raw = []byte(spec)
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON provided for --card")
	}
	return json.RawMessage(raw), nil
}
