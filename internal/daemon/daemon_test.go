package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Cinea4678/wecom-push/internal/config"
	"github.com/Cinea4678/wecom-push/internal/protocol"
	"github.com/gorilla/websocket"
)

func TestDaemonServerSendAndSSE(t *testing.T) {
	var (
		mu       sync.Mutex
		writeMu  sync.Mutex
		sent     []protocol.SendMsgBody
		lastConn *websocket.Conn
	)
	writeConnJSON := func(c *websocket.Conn, v any) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return c.WriteJSON(v)
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		lastConn = conn
		mu.Unlock()
		defer conn.Close()

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				Cmd     string           `json:"cmd"`
				Headers protocol.Headers `json:"headers"`
				Body    json.RawMessage  `json:"body"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}
			switch req.Cmd {
			case protocol.CmdSubscribe, protocol.CmdPing, protocol.CmdRespondMsg:
				_ = writeConnJSON(conn, map[string]any{"headers": req.Headers, "errcode": 0, "errmsg": "ok"})
			case protocol.CmdSendMsg:
				var body protocol.SendMsgBody
				_ = json.Unmarshal(req.Body, &body)
				mu.Lock()
				sent = append(sent, body)
				mu.Unlock()
				_ = writeConnJSON(conn, map[string]any{"headers": req.Headers, "errcode": 0, "errmsg": "ok"})
			}
		}
	}))
	defer wsSrv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	sockPath := filepath.Join(dir, "d.sock")

	cfg := &config.Config{
		Bot: config.BotConfig{
			ID:     "bot_daemon",
			Secret: "secret_daemon",
			WSURL:  "ws" + strings.TrimPrefix(wsSrv.URL, "http"),
		},
	}
	if err := config.Save(cfgPath, cfg); err != nil {
		t.Fatalf("save config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	srv := NewServer(ServerOptions{
		ConfigPath:        cfgPath,
		ListenAddr:        sockPath,
		HeartbeatInterval: 200 * time.Millisecond,
		Logger:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	doneCh := make(chan error, 1)
	go func() {
		doneCh <- srv.Run(ctx)
	}()

	client := NewClient(sockPath)
	// Wait for daemon to connect to mock WS.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := client.Status(ctx)
		if err == nil && st.State == "connected" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	st, err := client.Status(ctx)
	if err != nil || st.State != "connected" {
		t.Fatalf("daemon not connected: st=%+v err=%v", st, err)
	}

	// Send via daemon to 2 targets.
	sendResp, err := client.Send(ctx, SendRequest{
		Items: []SendItem{
			{
				Target: config.Target{Type: config.TargetGroup, ID: "grp_1"},
				Body: protocol.SendMsgBody{
					ChatID:   "grp_1",
					ChatType: 2,
					MsgType:  "markdown",
					Markdown: &protocol.MarkdownBody{Content: "alert 1"},
				},
			},
			{
				Target: config.Target{Type: config.TargetUser, ID: "usr_1"},
				Body: protocol.SendMsgBody{
					ChatID:   "usr_1",
					ChatType: 1,
					MsgType:  "markdown",
					Markdown: &protocol.MarkdownBody{Content: "alert 1"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if sendResp.Sent != 2 || sendResp.Failed != 0 {
		t.Fatalf("unexpected sendResp: %+v", sendResp)
	}

	// Stream events via SSE and verify callback delivery without kicking WS.
	evCtx, evCancel := context.WithCancel(ctx)
	evCh := make(chan protocol.CallbackEvent, 1)
	go func() {
		_ = client.StreamEvents(evCtx, func(ev protocol.CallbackEvent) {
			evCh <- ev
		})
	}()

	// Wait until subscriber is registered.
	for i := 0; i < 40; i++ {
		st, _ := client.Status(ctx)
		if st != nil && st.EventSubscribers >= 1 {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	mu.Lock()
	conn := lastConn
	mu.Unlock()
	if conn == nil {
		t.Fatal("expected active mock WS connection")
	}
	_ = writeConnJSON(conn, map[string]any{
		"cmd":     protocol.CmdMsgCallback,
		"headers": map[string]any{"req_id": "sse_req_1"},
		"body": map[string]any{
			"chatid":   "grp_live",
			"chattype": "group",
			"from":     map[string]any{"userid": "alice"},
			"msgtype":  "text",
			"text":     map[string]any{"content": "@bot ping"},
		},
	})

	select {
	case ev := <-evCh:
		if ev.ChatID != "grp_live" || ev.FromUserID != "alice" {
			t.Fatalf("unexpected SSE event: %+v", ev)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for SSE event")
	}
	evCancel()

	// Graceful shutdown via IPC.
	if err := client.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown failed: %v", err)
	}
	select {
	case err := <-doneCh:
		if err != nil {
			t.Fatalf("daemon Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for daemon exit")
	}
}
