package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Cinea4678/wecom-push/internal/protocol"
	"github.com/gorilla/websocket"
)

func execCLI(t *testing.T, stdin string, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewRootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	if stdin != "" {
		cmd.SetIn(strings.NewReader(stdin))
	} else {
		cmd.SetIn(strings.NewReader(""))
	}
	cmd.SetArgs(args)
	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestCLIInitAddListSendDirectAndRemove(t *testing.T) {
	var (
		mu   sync.Mutex
		sent []protocol.SendMsgBody
	)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	wsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
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
			if req.Cmd == protocol.CmdSendMsg {
				var b protocol.SendMsgBody
				_ = json.Unmarshal(req.Body, &b)
				mu.Lock()
				sent = append(sent, b)
				mu.Unlock()
			}
			_ = conn.WriteJSON(map[string]any{"headers": req.Headers, "errcode": 0, "errmsg": "ok"})
		}
	}))
	defer wsSrv.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	sockPath := filepath.Join(dir, "unused.sock")
	wsURL := "ws" + strings.TrimPrefix(wsSrv.URL, "http")

	// 1. init
	out, _, err := execCLI(t, "", "--config", cfgPath, "--socket", sockPath,
		"init", "--bot-id", "bot_cli", "--secret", "sec_cli_123456", "--ws-url", wsURL)
	if err != nil {
		t.Fatalf("init failed: %v\nout=%s", err, out)
	}

	// 2. add channel with group + user
	out, _, err = execCLI(t, "", "--config", cfgPath, "--socket", sockPath,
		"add", "ops", "--group", "wr_ops=告警群", "--user", "zhangsan=张三", "--desc", "运维频道", "--default")
	if err != nil {
		t.Fatalf("add failed: %v\nout=%s", err, out)
	}

	// 3. list channels
	out, _, err = execCLI(t, "", "--config", cfgPath, "--socket", sockPath, "list")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if !strings.Contains(out, "ops") || !strings.Contains(out, "wr_ops") || !strings.Contains(out, "zhangsan") {
		t.Fatalf("unexpected list output:\n%s", out)
	}

	// 4. send via positional channel + stdin with title
	out, stderr, err := execCLI(t, "Disk usage at 92%", "--config", cfgPath, "--socket", sockPath,
		"send", "ops", "--title", "High Disk")
	if err != nil {
		t.Fatalf("send failed: %v\nout=%s\nerr=%s", err, out, stderr)
	}

	mu.Lock()
	if len(sent) != 2 {
		t.Fatalf("expected 2 sent messages, got %d: %+v", len(sent), sent)
	}
	byID := map[string]protocol.SendMsgBody{}
	for _, s := range sent {
		byID[s.ChatID] = s
	}
	grpMsg, ok := byID["wr_ops"]
	if !ok || grpMsg.ChatType != 2 {
		t.Fatalf("expected group wr_ops with chat_type=2, got %+v", sent)
	}
	usrMsg, ok := byID["zhangsan"]
	if !ok || usrMsg.ChatType != 1 {
		t.Fatalf("expected user zhangsan with chat_type=1, got %+v", sent)
	}
	if grpMsg.Markdown == nil || !strings.Contains(grpMsg.Markdown.Content, "### High Disk") || !strings.Contains(grpMsg.Markdown.Content, "Disk usage at 92%") {
		t.Fatalf("unexpected markdown content: %+v", grpMsg.Markdown)
	}
	mu.Unlock()

	// 5. remove single target from channel
	_, _, err = execCLI(t, "", "--config", cfgPath, "--socket", sockPath, "remove", "ops", "--user", "zhangsan")
	if err != nil {
		t.Fatalf("remove target failed: %v", err)
	}

	// 6. remove entire channel
	_, _, err = execCLI(t, "", "--config", cfgPath, "--socket", sockPath, "remove", "ops")
	if err != nil {
		t.Fatalf("remove channel failed: %v", err)
	}
}
