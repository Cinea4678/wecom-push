package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type mockWeComServer struct {
	server   *httptest.Server
	mu       sync.Mutex
	writeMu  sync.Mutex
	sent     []SendMsgBody
	conns    []*websocket.Conn
	failAuth bool
}

func (m *mockWeComServer) writeJSON(conn *websocket.Conn, v any) error {
	m.writeMu.Lock()
	defer m.writeMu.Unlock()
	return conn.WriteJSON(v)
}

func newMockWeComServer(t *testing.T, failAuth bool) *mockWeComServer {
	t.Helper()
	m := &mockWeComServer{failAuth: failAuth}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}

	m.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		m.mu.Lock()
		m.conns = append(m.conns, conn)
		m.mu.Unlock()
		defer conn.Close()

		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var req struct {
				Cmd     string          `json:"cmd"`
				Headers Headers         `json:"headers"`
				Body    json.RawMessage `json:"body"`
			}
			if err := json.Unmarshal(data, &req); err != nil {
				return
			}

			switch req.Cmd {
			case CmdSubscribe:
				errCode := 0
				errMsg := "ok"
				if m.failAuth {
					errCode = 40014
					errMsg = "invalid secret"
				}
				_ = m.writeJSON(conn, map[string]any{
					"headers": req.Headers,
					"errcode": errCode,
					"errmsg":  errMsg,
				})
			case CmdPing:
				_ = m.writeJSON(conn, map[string]any{
					"headers": req.Headers,
					"errcode": 0,
					"errmsg":  "ok",
				})
			case CmdSendMsg:
				var body SendMsgBody
				_ = json.Unmarshal(req.Body, &body)
				m.mu.Lock()
				m.sent = append(m.sent, body)
				m.mu.Unlock()
				_ = m.writeJSON(conn, map[string]any{
					"headers": req.Headers,
					"errcode": 0,
					"errmsg":  "ok",
				})
			case CmdUploadMediaInit:
				_ = m.writeJSON(conn, map[string]any{
					"headers": req.Headers,
					"body":    map[string]any{"upload_id": "up_001"},
					"errcode": 0,
					"errmsg":  "ok",
				})
			case CmdUploadMediaChunk:
				_ = m.writeJSON(conn, map[string]any{
					"headers": req.Headers,
					"errcode": 0,
					"errmsg":  "ok",
				})
			case CmdUploadMediaFinish:
				_ = m.writeJSON(conn, map[string]any{
					"headers": req.Headers,
					"body": map[string]any{
						"type":       "file",
						"media_id":   "media_xyz",
						"created_at": "1700000000",
					},
					"errcode": 0,
					"errmsg":  "ok",
				})
			}
		}
	}))
	t.Cleanup(m.server.Close)
	return m
}

func (m *mockWeComServer) wsURL() string {
	return "ws" + strings.TrimPrefix(m.server.URL, "http")
}

func (m *mockWeComServer) pushFrame(v any) error {
	m.mu.Lock()
	if len(m.conns) == 0 {
		m.mu.Unlock()
		return errors.New("no active connection")
	}
	conn := m.conns[len(m.conns)-1]
	m.mu.Unlock()
	return m.writeJSON(conn, v)
}

func TestClientSendHeartbeatCallbackAndUpload(t *testing.T) {
	mock := newMockWeComServer(t, false)
	cbCh := make(chan CallbackEvent, 2)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := Connect(ctx, Options{
		WSURL:             mock.wsURL(),
		BotID:             "bot_test",
		Secret:            "secret_test",
		HeartbeatInterval: 80 * time.Millisecond,
		OnCallback: func(ev CallbackEvent) {
			cbCh <- ev
		},
	})
	if err != nil {
		t.Fatalf("Connect failed: %v", err)
	}
	defer client.Close()

	// Send markdown message.
	reqID, err := client.SendMessage(ctx, SendMsgBody{
		ChatID:   "wr_group_1",
		ChatType: 2,
		MsgType:  "markdown",
		Markdown: &MarkdownBody{Content: "**hello**"},
	})
	if err != nil || reqID == "" {
		t.Fatalf("SendMessage failed: reqID=%q err=%v", reqID, err)
	}

	mock.mu.Lock()
	if len(mock.sent) != 1 || mock.sent[0].ChatID != "wr_group_1" || mock.sent[0].ChatType != 2 {
		t.Fatalf("unexpected sent payload: %+v", mock.sent)
	}
	mock.mu.Unlock()

	// Verify heartbeat updates LastPingAt.
	time.Sleep(150 * time.Millisecond)
	if client.LastPingAt().IsZero() {
		t.Fatal("expected heartbeat ping to update LastPingAt")
	}

	// Verify upload flow.
	tmpFile := filepath.Join(t.TempDir(), "sample.txt")
	if err := os.WriteFile(tmpFile, []byte("hello wecom upload"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	mediaID, err := client.UploadMediaFile(ctx, "file", tmpFile)
	if err != nil || mediaID != "media_xyz" {
		t.Fatalf("UploadMediaFile failed: mediaID=%q err=%v", mediaID, err)
	}

	// Simulate server message callback.
	if err := mock.pushFrame(map[string]any{
		"cmd":     CmdMsgCallback,
		"headers": map[string]any{"req_id": "cb_1"},
		"body": map[string]any{
			"msgid":    "msg_1",
			"aibotid":  "bot_test",
			"chatid":   "wr_group_1",
			"chattype": "group",
			"from":     map[string]any{"userid": "zhangsan"},
			"msgtype":  "text",
			"text":     map[string]any{"content": "@bot hi"},
		},
	}); err != nil {
		t.Fatalf("pushFrame: %v", err)
	}

	select {
	case ev := <-cbCh:
		if ev.ChatID != "wr_group_1" || ev.FromUserID != "zhangsan" || ev.ChatType != "group" {
			t.Fatalf("unexpected callback event: %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for callback event")
	}

	// Simulate disconnected_event (kicked by another connection).
	if err := mock.pushFrame(map[string]any{
		"cmd":     CmdEventCallback,
		"headers": map[string]any{"req_id": "cb_kick"},
		"body": map[string]any{
			"msgtype": "event",
			"event":   map[string]any{"eventtype": EventDisconnected},
		},
	}); err != nil {
		t.Fatalf("pushFrame disconnect: %v", err)
	}

	select {
	case <-client.Done():
		if !errors.Is(client.Err(), ErrKickedByNewConnection) {
			t.Fatalf("expected ErrKickedByNewConnection, got %v", client.Err())
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for client disconnect")
	}
}

func TestClientAuthFailure(t *testing.T) {
	mock := newMockWeComServer(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	_, err := Connect(ctx, Options{
		WSURL:  mock.wsURL(),
		BotID:  "bot_test",
		Secret: "bad_secret",
	})
	if err == nil {
		t.Fatal("expected auth error, got nil")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.ErrCode != 40014 {
		t.Fatalf("expected APIError 40014, got %v", err)
	}
}
