package protocol

import (
	"context"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ErrKickedByNewConnection indicates WeCom pushed disconnected_event because another connection subscribed.
var ErrKickedByNewConnection = errors.New("disconnected by server: another connection subscribed with the same bot")

// ErrClientClosed indicates the WebSocket client is already closed.
var ErrClientClosed = errors.New("websocket client closed")

// Options configures a WeCom WebSocket client connection.
type Options struct {
	WSURL             string
	BotID             string
	Secret            string
	DialTimeout       time.Duration
	RequestTimeout    time.Duration
	HeartbeatInterval time.Duration
	PingTimeout       time.Duration
	OnCallback        func(CallbackEvent)
}

func (o Options) withDefaults() Options {
	if o.WSURL == "" {
		o.WSURL = "wss://openws.work.weixin.qq.com"
	}
	if o.DialTimeout <= 0 {
		o.DialTimeout = 10 * time.Second
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = 15 * time.Second
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = 30 * time.Second
	}
	if o.PingTimeout <= 0 {
		o.PingTimeout = 10 * time.Second
	}
	return o
}

// Client manages a single authenticated WeCom AI Bot WebSocket session.
type Client struct {
	opts Options
	conn *websocket.Conn

	writeMu sync.Mutex

	pendingMu sync.Mutex
	pending   map[string]chan InboundFrame

	closed     chan struct{}
	closeOnce  sync.Once
	closeErrMu sync.Mutex
	closeErr   error

	lastPingAt atomic.Int64
	seq        atomic.Uint64
}

// Connect dials the WeCom WebSocket endpoint, performs aibot_subscribe, and starts heartbeat + read loops.
func Connect(ctx context.Context, opts Options) (*Client, error) {
	opts = opts.withDefaults()
	if opts.BotID == "" || opts.Secret == "" {
		return nil, errors.New("bot_id and secret are required")
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: opts.DialTimeout,
		Proxy:            websocket.DefaultDialer.Proxy,
	}

	dialCtx, cancel := context.WithTimeout(ctx, opts.DialTimeout)
	defer cancel()

	conn, resp, err := dialer.DialContext(dialCtx, opts.WSURL, nil)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dial %s (HTTP %d): %w", opts.WSURL, resp.StatusCode, err)
		}
		return nil, fmt.Errorf("dial %s: %w", opts.WSURL, err)
	}

	c := &Client{
		opts:    opts,
		conn:    conn,
		pending: make(map[string]chan InboundFrame),
		closed:  make(chan struct{}),
	}

	go c.readLoop()

	subCtx, subCancel := context.WithTimeout(ctx, opts.RequestTimeout)
	defer subCancel()

	if _, err := c.call(subCtx, CmdSubscribe, "", SubscribeBody{
		BotID:  opts.BotID,
		Secret: opts.Secret,
	}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("subscribe bot %s: %w", opts.BotID, err)
	}

	go c.heartbeatLoop()
	return c, nil
}

// Done returns a channel that is closed when the underlying WebSocket session terminates.
func (c *Client) Done() <-chan struct{} {
	return c.closed
}

// Err returns the reason the client session ended, if closed.
func (c *Client) Err() error {
	c.closeErrMu.Lock()
	defer c.closeErrMu.Unlock()
	return c.closeErr
}

// LastPingAt returns the timestamp of the most recent successful heartbeat ping.
func (c *Client) LastPingAt() time.Time {
	ns := c.lastPingAt.Load()
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
}

// Close gracefully closes the WebSocket connection.
func (c *Client) Close() error {
	var err error
	c.shutdown(ErrClientClosed, func() {
		c.writeMu.Lock()
		_ = c.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(2*time.Second),
		)
		c.writeMu.Unlock()
		err = c.conn.Close()
	})
	return err
}

func (c *Client) shutdown(reason error, beforeClose func()) {
	c.closeOnce.Do(func() {
		c.closeErrMu.Lock()
		if c.closeErr == nil {
			c.closeErr = reason
		}
		c.closeErrMu.Unlock()

		if beforeClose != nil {
			beforeClose()
		} else {
			_ = c.conn.Close()
		}

		c.pendingMu.Lock()
		for id, ch := range c.pending {
			close(ch)
			delete(c.pending, id)
		}
		c.pendingMu.Unlock()

		close(c.closed)
	})
}

func (c *Client) nextReqID(prefix string) string {
	n := c.seq.Add(1)
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%d_%d_%s", prefix, time.Now().UnixMilli(), n, hex.EncodeToString(b[:]))
}

func (c *Client) writeJSON(v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if err := c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	return c.conn.WriteJSON(v)
}

func (c *Client) call(ctx context.Context, cmd string, reqID string, body any) (InboundFrame, error) {
	select {
	case <-c.closed:
		if err := c.Err(); err != nil {
			return InboundFrame{}, err
		}
		return InboundFrame{}, ErrClientClosed
	default:
	}

	if reqID == "" {
		reqID = c.nextReqID(cmd)
	}

	respCh := make(chan InboundFrame, 1)
	c.pendingMu.Lock()
	select {
	case <-c.closed:
		c.pendingMu.Unlock()
		if err := c.Err(); err != nil {
			return InboundFrame{}, err
		}
		return InboundFrame{}, ErrClientClosed
	default:
		c.pending[reqID] = respCh
	}
	c.pendingMu.Unlock()

	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, reqID)
		c.pendingMu.Unlock()
	}()

	frame := RequestFrame{
		Cmd:     cmd,
		Headers: Headers{ReqID: reqID},
		Body:    body,
	}
	if err := c.writeJSON(frame); err != nil {
		return InboundFrame{}, fmt.Errorf("write %s: %w", cmd, err)
	}

	select {
	case resp, ok := <-respCh:
		if !ok {
			if err := c.Err(); err != nil {
				return InboundFrame{}, err
			}
			return InboundFrame{}, ErrClientClosed
		}
		if resp.ErrCode != nil && *resp.ErrCode != 0 {
			return resp, &APIError{
				Cmd:     cmd,
				ReqID:   reqID,
				ErrCode: *resp.ErrCode,
				ErrMsg:  resp.ErrMsg,
			}
		}
		return resp, nil
	case <-ctx.Done():
		return InboundFrame{}, fmt.Errorf("%s timed out/canceled: %w", cmd, ctx.Err())
	case <-c.closed:
		if err := c.Err(); err != nil {
			return InboundFrame{}, err
		}
		return InboundFrame{}, ErrClientClosed
	}
}

func (c *Client) readLoop() {
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.shutdown(fmt.Errorf("websocket read error: %w", err), nil)
			return
		}

		var frame InboundFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			continue
		}

		// Server-pushed callback frame (has non-empty cmd).
		if frame.Cmd != "" {
			ev := ParseCallbackEvent(data, frame)
			if c.opts.OnCallback != nil {
				go c.opts.OnCallback(ev)
			}
			if frame.Cmd == CmdEventCallback && ev.EventType == EventDisconnected {
				c.shutdown(ErrKickedByNewConnection, nil)
				return
			}
			continue
		}

		// Response frame correlated by headers.req_id.
		if frame.Headers.ReqID != "" {
			c.pendingMu.Lock()
			ch, ok := c.pending[frame.Headers.ReqID]
			if ok {
				delete(c.pending, frame.Headers.ReqID)
			}
			c.pendingMu.Unlock()

			if ok {
				select {
				case ch <- frame:
				default:
				}
			}
		}
	}
}

func (c *Client) heartbeatLoop() {
	ticker := time.NewTicker(c.opts.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.closed:
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(context.Background(), c.opts.PingTimeout)
			err := c.Ping(pingCtx)
			cancel()
			if err != nil {
				c.shutdown(fmt.Errorf("heartbeat ping failed: %w", err), nil)
				return
			}
		}
	}
}

// Ping sends an application-level ping frame and awaits the response.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.call(ctx, CmdPing, "", nil); err != nil {
		return err
	}
	c.lastPingAt.Store(time.Now().UnixNano())
	return nil
}

// SendMessage sends an aibot_send_msg proactive message and returns the request ID.
func (c *Client) SendMessage(ctx context.Context, body SendMsgBody) (string, error) {
	if body.ChatID == "" {
		return "", errors.New("chatid cannot be empty")
	}
	if body.MsgType == "" {
		return "", errors.New("msgtype cannot be empty")
	}
	if body.MsgType == "markdown" && body.Markdown != nil && len(body.Markdown.Content) > MaxMarkdownBytes {
		return "", fmt.Errorf("markdown content exceeds maximum length of %d bytes (got %d)", MaxMarkdownBytes, len(body.Markdown.Content))
	}

	reqID := c.nextReqID("send")
	_, err := c.call(ctx, CmdSendMsg, reqID, body)
	return reqID, err
}

// RespondMarkdown replies to a received callback using its req_id via aibot_respond_msg.
func (c *Client) RespondMarkdown(ctx context.Context, callbackReqID string, content string) error {
	if callbackReqID == "" {
		return errors.New("callback req_id cannot be empty")
	}
	_, err := c.call(ctx, CmdRespondMsg, callbackReqID, RespondMsgBody{
		MsgType: "markdown",
		Markdown: &MarkdownBody{
			Content: content,
		},
	})
	return err
}

// UploadMediaFile uploads a local file via WeCom's 3-step chunked WebSocket upload protocol and returns media_id.
func (c *Client) UploadMediaFile(ctx context.Context, mediaType string, filePath string) (string, error) {
	switch mediaType {
	case "file", "image", "voice", "video":
	default:
		return "", fmt.Errorf("unsupported media type %q (expected file, image, voice, or video)", mediaType)
	}

	f, err := os.Open(filePath)
	if err != nil {
		return "", fmt.Errorf("open media file %s: %w", filePath, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat media file %s: %w", filePath, err)
	}
	size := info.Size()
	if size < MinUploadFileBytes {
		return "", fmt.Errorf("file %s is too small (%d bytes, minimum %d bytes)", filePath, size, MinUploadFileBytes)
	}

	totalChunks := int((size + MaxUploadChunkBytes - 1) / MaxUploadChunkBytes)
	if totalChunks > MaxUploadChunks {
		return "", fmt.Errorf("file %s requires %d chunks, exceeding max %d chunks", filePath, totalChunks, MaxUploadChunks)
	}

	// Compute MD5 for integrity verification.
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash media file: %w", err)
	}
	fileMD5 := hex.EncodeToString(h.Sum(nil))
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", fmt.Errorf("rewind media file: %w", err)
	}

	initResp, err := c.call(ctx, CmdUploadMediaInit, "", UploadMediaInitBody{
		Type:        mediaType,
		Filename:    filepath.Base(filePath),
		TotalSize:   size,
		TotalChunks: totalChunks,
		MD5:         fileMD5,
	})
	if err != nil {
		return "", fmt.Errorf("upload init: %w", err)
	}
	var initBody UploadMediaInitResp
	if err := json.Unmarshal(initResp.Body, &initBody); err != nil || initBody.UploadID == "" {
		return "", fmt.Errorf("invalid upload init response: %s", string(initResp.Body))
	}

	buf := make([]byte, MaxUploadChunkBytes)
	for i := 0; i < totalChunks; i++ {
		n, readErr := io.ReadFull(f, buf)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			return "", fmt.Errorf("read chunk %d: %w", i, readErr)
		}
		if n == 0 {
			break
		}
		encoded := base64.StdEncoding.EncodeToString(buf[:n])
		if _, err := c.call(ctx, CmdUploadMediaChunk, "", UploadMediaChunkBody{
			UploadID:   initBody.UploadID,
			ChunkIndex: i,
			Base64Data: encoded,
		}); err != nil {
			return "", fmt.Errorf("upload chunk %d: %w", i, err)
		}
	}

	finishResp, err := c.call(ctx, CmdUploadMediaFinish, "", UploadMediaFinishBody{
		UploadID: initBody.UploadID,
	})
	if err != nil {
		return "", fmt.Errorf("upload finish: %w", err)
	}
	var finishBody UploadMediaFinishResp
	if err := json.Unmarshal(finishResp.Body, &finishBody); err != nil || finishBody.MediaID == "" {
		return "", fmt.Errorf("invalid upload finish response: %s", string(finishResp.Body))
	}
	return finishBody.MediaID, nil
}
