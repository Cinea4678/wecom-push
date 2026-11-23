package daemon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/Cinea4678/wecom-push/internal/protocol"
)

// Client communicates with a local wecom-push daemon over Unix domain socket or TCP.
type Client struct {
	addr       string
	httpClient *http.Client
	streamHTTP *http.Client
	baseURL    string
}

// NewClient builds a daemon IPC client for the given socket or tcp:// address.
func NewClient(addr string) *Client {
	network, target := ParseListenAddr(addr)
	dialFn := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, target)
	}
	transport := &http.Transport{
		DialContext: dialFn,
	}
	streamTransport := &http.Transport{
		DialContext: dialFn,
	}
	baseURL := "http://wecom-push-daemon"
	if network == "tcp" {
		baseURL = "http://" + target
	}
	return &Client{
		addr:       addr,
		baseURL:    baseURL,
		httpClient: &http.Client{Transport: transport, Timeout: 30 * time.Second},
		streamHTTP: &http.Client{Transport: streamTransport},
	}
}

// IsRunning reports whether a daemon responds to /v1/status within a short probe timeout.
func (c *Client) IsRunning(ctx context.Context) bool {
	probeCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err := c.Status(probeCtx)
	return err == nil
}

// Status fetches live daemon status.
func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var resp StatusResponse
	if err := c.doJSON(ctx, http.MethodGet, "/v1/status", nil, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Send asks the daemon to send one or more messages.
func (c *Client) Send(ctx context.Context, req SendRequest) (*SendResponse, error) {
	var resp SendResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/send", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// Respond replies to a callback req_id via the daemon's WebSocket connection.
func (c *Client) Respond(ctx context.Context, reqID, content string) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/respond", RespondRequest{
		ReqID:   reqID,
		Content: content,
	}, nil)
}

// Upload uploads a local media file via the daemon and returns media_id.
func (c *Client) Upload(ctx context.Context, mediaType, filePath string) (string, error) {
	var resp UploadResponse
	if err := c.doJSON(ctx, http.MethodPost, "/v1/upload", UploadRequest{
		Type:     mediaType,
		FilePath: filePath,
	}, &resp); err != nil {
		return "", err
	}
	return resp.MediaID, nil
}

// Reload signals the daemon to reload configuration and reconnect.
func (c *Client) Reload(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/reload", nil, nil)
}

// Shutdown asks the daemon to stop gracefully.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodPost, "/v1/shutdown", nil, nil)
}

// StreamEvents subscribes to the daemon's SSE /v1/events stream until ctx is canceled or the stream closes.
func (c *Client) StreamEvents(ctx context.Context, onEvent func(protocol.CallbackEvent)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/events", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("daemon events returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		var ev protocol.CallbackEvent
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if onEvent != nil {
			onEvent(ev)
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, in any, out any) error {
	var bodyReader io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var errBody struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(raw, &errBody) == nil && errBody.Error != "" {
			return fmt.Errorf("daemon error (%d): %s", resp.StatusCode, errBody.Error)
		}
		return fmt.Errorf("daemon error (%d): %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	if out != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, out); err != nil {
			return fmt.Errorf("decode daemon response: %w", err)
		}
	}
	return nil
}
