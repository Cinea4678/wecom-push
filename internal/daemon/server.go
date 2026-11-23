package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Cinea4678/wecom-push/internal/config"
	"github.com/Cinea4678/wecom-push/internal/protocol"
	"github.com/Cinea4678/wecom-push/internal/version"
)

// StatusResponse summarizes live daemon & WeCom connection status.
type StatusResponse struct {
	Version          string    `json:"version"`
	PID              int       `json:"pid"`
	StartedAt        time.Time `json:"started_at"`
	UptimeSeconds    int64     `json:"uptime_seconds"`
	State            string    `json:"state"` // connecting | connected | disconnected | kicked
	BotID            string    `json:"bot_id"`
	WSURL            string    `json:"ws_url"`
	ConnectedAt      time.Time `json:"connected_at,omitempty"`
	LastPingAt       time.Time `json:"last_ping_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	Reconnects       int64     `json:"reconnects"`
	MessagesSent     int64     `json:"messages_sent"`
	EventsReceived   int64     `json:"events_received"`
	EventSubscribers int       `json:"event_subscribers"`
}

// SendItem is a single target send payload inside SendRequest.
type SendItem struct {
	Target config.Target        `json:"target"`
	Body   protocol.SendMsgBody `json:"body"`
}

// SendRequest asks the daemon to send one or more messages over its active WebSocket session.
type SendRequest struct {
	Items    []SendItem `json:"items"`
	FailFast bool       `json:"fail_fast,omitempty"`
}

// SendItemResult reports delivery status for a single recipient.
type SendItemResult struct {
	Target config.Target `json:"target"`
	ReqID  string        `json:"req_id,omitempty"`
	OK     bool          `json:"ok"`
	Error  string        `json:"error,omitempty"`
}

// SendResponse aggregates per-target delivery results.
type SendResponse struct {
	Results []SendItemResult `json:"results"`
	Sent    int              `json:"sent"`
	Failed  int              `json:"failed"`
}

// RespondRequest replies to an inbound callback frame via aibot_respond_msg.
type RespondRequest struct {
	ReqID   string `json:"req_id"`
	Content string `json:"content"`
}

// UploadRequest uploads a local file via WeCom's chunked WS upload protocol.
type UploadRequest struct {
	Type     string `json:"type"`
	FilePath string `json:"file_path"`
}

// UploadResponse returns the temporary media_id assigned by WeCom.
type UploadResponse struct {
	MediaID string `json:"media_id"`
}

// ServerOptions configures the daemon server.
type ServerOptions struct {
	ConfigPath        string
	ListenAddr        string
	HeartbeatInterval time.Duration
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
	KickedBackoff     time.Duration
	Logger            *slog.Logger
}

// Server maintains a persistent WeCom WebSocket session and exposes a local HTTP/SSE IPC API.
type Server struct {
	opts      ServerOptions
	logger    *slog.Logger
	startedAt time.Time

	mu          sync.RWMutex
	wsClient    *protocol.Client
	state       string
	botID       string
	wsURL       string
	connectedAt time.Time
	lastErr     string

	reconnects     atomic.Int64
	messagesSent   atomic.Int64
	eventsReceived atomic.Int64

	subsMu sync.Mutex
	subs   map[chan protocol.CallbackEvent]struct{}

	reloadCh   chan struct{}
	shutdownCh chan struct{}
	stopOnce   sync.Once
}

// NewServer creates a configured daemon Server.
func NewServer(opts ServerOptions) *Server {
	if opts.HeartbeatInterval <= 0 {
		opts.HeartbeatInterval = 30 * time.Second
	}
	if opts.ReconnectMin <= 0 {
		opts.ReconnectMin = 2 * time.Second
	}
	if opts.ReconnectMax <= 0 {
		opts.ReconnectMax = 60 * time.Second
	}
	if opts.KickedBackoff <= 0 {
		opts.KickedBackoff = 30 * time.Second
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}
	return &Server{
		opts:       opts,
		logger:     logger,
		startedAt:  time.Now(),
		state:      "starting",
		subs:       make(map[chan protocol.CallbackEvent]struct{}),
		reloadCh:   make(chan struct{}, 1),
		shutdownCh: make(chan struct{}),
	}
}

// Run starts the local IPC listener and WeCom WebSocket supervisor until ctx is canceled or /v1/shutdown is called.
func (s *Server) Run(ctx context.Context) error {
	ln, cleanup, err := s.listen()
	if err != nil {
		return err
	}
	defer cleanup()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/send", s.handleSend)
	mux.HandleFunc("POST /v1/respond", s.handleRespond)
	mux.HandleFunc("POST /v1/upload", s.handleUpload)
	mux.HandleFunc("GET /v1/events", s.handleEvents)
	mux.HandleFunc("POST /v1/reload", s.handleReload)
	mux.HandleFunc("POST /v1/shutdown", s.handleShutdown)

	httpSrv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.superviseWS(runCtx)
	}()

	errCh := make(chan error, 1)
	go func() {
		s.logger.Info("daemon listening", "addr", s.opts.ListenAddr)
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	var serveErr error
	select {
	case <-ctx.Done():
		s.logger.Info("daemon received stop signal")
	case <-s.shutdownCh:
		s.logger.Info("daemon received shutdown request")
	case err := <-errCh:
		serveErr = err
	}

	cancel()
	s.closeCurrentWS()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = httpSrv.Shutdown(shutCtx)

	wg.Wait()
	return serveErr
}

func (s *Server) listen() (net.Listener, func(), error) {
	network, target := ParseListenAddr(s.opts.ListenAddr)
	if network == "unix" {
		dir := filepath.Dir(target)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("create socket dir %s: %w", dir, err)
		}
		// Check if another daemon is already actively listening on this socket.
		if conn, err := net.DialTimeout("unix", target, 300*time.Millisecond); err == nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("daemon is already running on %s", target)
		}
		_ = os.Remove(target)
	}

	ln, err := net.Listen(network, target)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s (%s): %w", s.opts.ListenAddr, network, err)
	}

	if network == "unix" {
		if err := os.Chmod(target, 0o600); err != nil {
			_ = ln.Close()
			_ = os.Remove(target)
			return nil, nil, fmt.Errorf("chmod socket %s: %w", target, err)
		}
	}

	cleanup := func() {
		_ = ln.Close()
		if network == "unix" {
			_ = os.Remove(target)
		}
	}
	return ln, cleanup, nil
}

func (s *Server) superviseWS(ctx context.Context) {
	backoff := s.opts.ReconnectMin
	attempt := 0

	for {
		if ctx.Err() != nil {
			return
		}

		cfg, err := config.Load(s.opts.ConfigPath)
		if err != nil {
			s.setState("disconnected", "", "", err.Error())
			s.logger.Error("load config failed", "err", err)
			if !s.waitBackoff(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.opts.ReconnectMax)
			continue
		}

		bot := cfg.ResolvedBot()
		if err := bot.Validate(); err != nil {
			s.setState("disconnected", bot.ID, bot.WSURL, err.Error())
			s.logger.Warn("bot config incomplete; waiting for reload or retry", "err", err)
			if !s.waitBackoff(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.opts.ReconnectMax)
			continue
		}

		s.setState("connecting", bot.ID, bot.WSURL, "")
		if attempt > 0 {
			s.reconnects.Add(1)
		}
		attempt++

		client, err := protocol.Connect(ctx, protocol.Options{
			WSURL:             bot.WSURL,
			BotID:             bot.ID,
			Secret:            bot.Secret,
			HeartbeatInterval: s.opts.HeartbeatInterval,
			OnCallback:        s.broadcastEvent,
		})
		if err != nil {
			s.setState("disconnected", bot.ID, bot.WSURL, err.Error())
			s.logger.Error("connect wecom ws failed", "err", err, "retry_in", backoff)
			if !s.waitBackoff(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff, s.opts.ReconnectMax)
			continue
		}

		// Connected!
		backoff = s.opts.ReconnectMin
		s.mu.Lock()
		s.wsClient = client
		s.state = "connected"
		s.botID = bot.ID
		s.wsURL = bot.WSURL
		s.connectedAt = time.Now()
		s.lastErr = ""
		s.mu.Unlock()
		s.logger.Info("connected to WeCom WebSocket", "bot_id", bot.ID, "ws_url", bot.WSURL)

		// Wait until connection drops, reload is requested, or daemon stops.
		reloaded := false
		select {
		case <-ctx.Done():
			_ = client.Close()
			return
		case <-s.reloadCh:
			reloaded = true
			s.logger.Info("reloading WeCom WebSocket connection")
			_ = client.Close()
		case <-client.Done():
		}

		s.mu.Lock()
		if s.wsClient == client {
			s.wsClient = nil
		}
		s.mu.Unlock()

		if reloaded {
			continue
		}

		discErr := client.Err()
		if errors.Is(discErr, protocol.ErrKickedByNewConnection) {
			s.setState("kicked", bot.ID, bot.WSURL, discErr.Error())
			s.logger.Warn("kicked by another connection with same bot; backing off", "wait", s.opts.KickedBackoff)
			if !s.waitBackoff(ctx, s.opts.KickedBackoff) {
				return
			}
			continue
		}

		errMsg := ""
		if discErr != nil {
			errMsg = discErr.Error()
		}
		s.setState("disconnected", bot.ID, bot.WSURL, errMsg)
		s.logger.Warn("WeCom WebSocket disconnected; scheduling reconnect", "err", errMsg, "retry_in", backoff)
		if !s.waitBackoff(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff, s.opts.ReconnectMax)
	}
}

func (s *Server) waitBackoff(ctx context.Context, d time.Duration) bool {
	// Add up to 20% jitter to avoid thundering herd.
	jitter := time.Duration(rand.Int64N(int64(d/5) + 1))
	timer := time.NewTimer(d + jitter)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-s.reloadCh:
		return true
	case <-timer.C:
		return true
	}
}

func nextBackoff(cur, max time.Duration) time.Duration {
	next := cur * 2
	if next > max {
		return max
	}
	return next
}

func (s *Server) setState(state, botID, wsURL, lastErr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	if botID != "" {
		s.botID = botID
	}
	if wsURL != "" {
		s.wsURL = wsURL
	}
	s.lastErr = lastErr
}

func (s *Server) currentWS() *protocol.Client {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.wsClient
}

func (s *Server) closeCurrentWS() {
	s.mu.Lock()
	c := s.wsClient
	s.wsClient = nil
	s.mu.Unlock()
	if c != nil {
		_ = c.Close()
	}
}

func (s *Server) broadcastEvent(ev protocol.CallbackEvent) {
	s.eventsReceived.Add(1)
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
			// Drop for slow subscribers rather than stalling the WS read loop.
		}
	}
}

func (s *Server) subscribeEvents() (chan protocol.CallbackEvent, func()) {
	ch := make(chan protocol.CallbackEvent, 64)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	return ch, func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	client := s.wsClient
	resp := StatusResponse{
		Version:        version.Version,
		PID:            os.Getpid(),
		StartedAt:      s.startedAt,
		UptimeSeconds:  int64(time.Since(s.startedAt).Seconds()),
		State:          s.state,
		BotID:          s.botID,
		WSURL:          s.wsURL,
		ConnectedAt:    s.connectedAt,
		LastError:      s.lastErr,
		Reconnects:     s.reconnects.Load(),
		MessagesSent:   s.messagesSent.Load(),
		EventsReceived: s.eventsReceived.Load(),
	}
	s.mu.RUnlock()

	if client != nil {
		resp.LastPingAt = client.LastPingAt()
	}

	s.subsMu.Lock()
	resp.EventSubscribers = len(s.subs)
	s.subsMu.Unlock()

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid send request: %v", err))
		return
	}
	if len(req.Items) == 0 {
		writeError(w, http.StatusBadRequest, "no send items provided")
		return
	}

	client := s.currentWS()
	if client == nil {
		writeError(w, http.StatusServiceUnavailable, "daemon is not currently connected to WeCom WebSocket")
		return
	}

	resp := SendResponse{
		Results: make([]SendItemResult, 0, len(req.Items)),
	}
	for _, item := range req.Items {
		reqID, err := client.SendMessage(r.Context(), item.Body)
		if err != nil {
			resp.Failed++
			resp.Results = append(resp.Results, SendItemResult{
				Target: item.Target,
				ReqID:  reqID,
				OK:     false,
				Error:  err.Error(),
			})
			if req.FailFast {
				break
			}
			continue
		}
		s.messagesSent.Add(1)
		resp.Sent++
		resp.Results = append(resp.Results, SendItemResult{
			Target: item.Target,
			ReqID:  reqID,
			OK:     true,
		})
	}

	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRespond(w http.ResponseWriter, r *http.Request) {
	var req RespondRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid respond request: %v", err))
		return
	}
	client := s.currentWS()
	if client == nil {
		writeError(w, http.StatusServiceUnavailable, "daemon is not connected to WeCom WebSocket")
		return
	}
	if err := client.RespondMarkdown(r.Context(), req.ReqID, req.Content); err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	var req UploadRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("invalid upload request: %v", err))
		return
	}
	client := s.currentWS()
	if client == nil {
		writeError(w, http.StatusServiceUnavailable, "daemon is not connected to WeCom WebSocket")
		return
	}
	mediaID, err := client.UploadMediaFile(r.Context(), req.Type, req.FilePath)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, UploadResponse{MediaID: mediaID})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	ch, unsub := s.subscribeEvents()
	defer unsub()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdownCh:
			return
		case ev := <-ch:
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
				return
			}
			flusher.Flush()
		case <-keepalive.C:
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	select {
	case s.reloadCh <- struct{}{}:
	default:
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	s.stopOnce.Do(func() {
		close(s.shutdownCh)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
