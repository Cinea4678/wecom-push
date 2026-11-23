package protocol

import (
	"encoding/json"
	"fmt"
	"time"
)

// Official WeCom AI Bot WebSocket command names (doc path/101463).
const (
	CmdSubscribe          = "aibot_subscribe"
	CmdPing               = "ping"
	CmdSendMsg            = "aibot_send_msg"
	CmdRespondMsg         = "aibot_respond_msg"
	CmdRespondWelcomeMsg  = "aibot_respond_welcome_msg"
	CmdRespondUpdateMsg   = "aibot_respond_update_msg"
	CmdMsgCallback        = "aibot_msg_callback"
	CmdEventCallback      = "aibot_event_callback"
	CmdUploadMediaInit    = "aibot_upload_media_init"
	CmdUploadMediaChunk   = "aibot_upload_media_chunk"
	CmdUploadMediaFinish  = "aibot_upload_media_finish"
	EventDisconnected     = "disconnected_event"
	EventEnterChat        = "enter_chat"
	EventTemplateCard     = "template_card_event"
	EventFeedback         = "feedback_event"
	MaxMarkdownBytes      = 20480
	MaxUploadChunkBytes   = 512 * 1024 // 512KB before base64
	MaxUploadChunks       = 100
	MinUploadFileBytes    = 5
)

// Headers carries request correlation metadata.
type Headers struct {
	ReqID string `json:"req_id"`
}

// RequestFrame is the outbound WebSocket JSON frame.
type RequestFrame struct {
	Cmd     string  `json:"cmd"`
	Headers Headers `json:"headers"`
	Body    any     `json:"body,omitempty"`
}

// InboundFrame represents any JSON frame received from WeCom WSS.
type InboundFrame struct {
	Cmd     string          `json:"cmd,omitempty"`
	Headers Headers         `json:"headers,omitempty"`
	Body    json.RawMessage `json:"body,omitempty"`
	ErrCode *int            `json:"errcode,omitempty"`
	ErrMsg  string          `json:"errmsg,omitempty"`
}

// APIError represents a non-zero errcode returned by WeCom.
type APIError struct {
	Cmd     string
	ReqID   string
	ErrCode int
	ErrMsg  string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("wecom %s failed (errcode=%d, errmsg=%q, req_id=%s)", e.Cmd, e.ErrCode, e.ErrMsg, e.ReqID)
}

// SubscribeBody is the payload for aibot_subscribe.
type SubscribeBody struct {
	BotID  string `json:"bot_id"`
	Secret string `json:"secret"`
}

// FeedbackConfig optionally enables user feedback callbacks.
type FeedbackConfig struct {
	ID string `json:"id"`
}

// MarkdownBody is the markdown message content payload.
type MarkdownBody struct {
	Content  string          `json:"content"`
	Feedback *FeedbackConfig `json:"feedback,omitempty"`
}

// MediaBody references an uploaded temporary media asset.
type MediaBody struct {
	MediaID string `json:"media_id"`
}

// VideoBody references an uploaded video asset with optional metadata.
type VideoBody struct {
	MediaID     string `json:"media_id"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
}

// SendMsgBody is the payload for aibot_send_msg (proactive push).
type SendMsgBody struct {
	ChatID       string          `json:"chatid"`
	ChatType     uint32          `json:"chat_type,omitempty"`
	MsgType      string          `json:"msgtype"`
	Markdown     *MarkdownBody   `json:"markdown,omitempty"`
	TemplateCard json.RawMessage `json:"template_card,omitempty"`
	File         *MediaBody      `json:"file,omitempty"`
	Image        *MediaBody      `json:"image,omitempty"`
	Voice        *MediaBody      `json:"voice,omitempty"`
	Video        *VideoBody      `json:"video,omitempty"`
}

// RespondMsgBody is the payload for aibot_respond_msg (reply to callback).
type RespondMsgBody struct {
	MsgType  string        `json:"msgtype"`
	Markdown *MarkdownBody `json:"markdown,omitempty"`
}

// UploadMediaInitBody initializes chunked media upload.
type UploadMediaInitBody struct {
	Type        string `json:"type"` // file | image | voice | video
	Filename    string `json:"filename"`
	TotalSize   int64  `json:"total_size"`
	TotalChunks int    `json:"total_chunks"`
	MD5         string `json:"md5,omitempty"`
}

// UploadMediaInitResp is the body returned by aibot_upload_media_init.
type UploadMediaInitResp struct {
	UploadID string `json:"upload_id"`
}

// UploadMediaChunkBody uploads a single base64-encoded chunk.
type UploadMediaChunkBody struct {
	UploadID   string `json:"upload_id"`
	ChunkIndex int    `json:"chunk_index"`
	Base64Data string `json:"base64_data"`
}

// UploadMediaFinishBody finalizes a chunked upload.
type UploadMediaFinishBody struct {
	UploadID string `json:"upload_id"`
}

// UploadMediaFinishResp is the body returned by aibot_upload_media_finish.
type UploadMediaFinishResp struct {
	Type      string          `json:"type"`
	MediaID   string          `json:"media_id"`
	CreatedAt json.RawMessage `json:"created_at"`
}

// CallbackBody models common fields in aibot_msg_callback and aibot_event_callback.
type CallbackBody struct {
	MsgID      string `json:"msgid,omitempty"`
	CreateTime int64  `json:"create_time,omitempty"`
	AIBotID    string `json:"aibotid,omitempty"`
	ChatID     string `json:"chatid,omitempty"`
	ChatType   string `json:"chattype,omitempty"` // "single" | "group"
	From       *struct {
		UserID string `json:"userid,omitempty"`
	} `json:"from,omitempty"`
	MsgType string `json:"msgtype,omitempty"`
	Text    *struct {
		Content string `json:"content,omitempty"`
	} `json:"text,omitempty"`
	Voice *struct {
		Content string `json:"content,omitempty"`
	} `json:"voice,omitempty"`
	Event *struct {
		EventType string `json:"eventtype,omitempty"`
	} `json:"event,omitempty"`
}

// CallbackEvent is a normalized inbound callback event emitted to debug/SSE listeners.
type CallbackEvent struct {
	ReceivedAt  time.Time       `json:"received_at"`
	Cmd         string          `json:"cmd"`
	ReqID       string          `json:"req_id,omitempty"`
	MsgID       string          `json:"msgid,omitempty"`
	AIBotID     string          `json:"aibotid,omitempty"`
	ChatID      string          `json:"chatid,omitempty"`
	ChatType    string          `json:"chattype,omitempty"`
	FromUserID  string          `json:"from_userid,omitempty"`
	MsgType     string          `json:"msgtype,omitempty"`
	EventType   string          `json:"eventtype,omitempty"`
	TextContent string          `json:"text_content,omitempty"`
	Raw         json.RawMessage `json:"raw"`
}

// ParseCallbackEvent extracts structured fields from an inbound callback frame.
func ParseCallbackEvent(raw []byte, frame InboundFrame) CallbackEvent {
	ev := CallbackEvent{
		ReceivedAt: time.Now(),
		Cmd:        frame.Cmd,
		ReqID:      frame.Headers.ReqID,
		Raw:        append(json.RawMessage(nil), raw...),
	}
	if len(frame.Body) > 0 {
		var body CallbackBody
		if err := json.Unmarshal(frame.Body, &body); err == nil {
			ev.MsgID = body.MsgID
			ev.AIBotID = body.AIBotID
			ev.ChatID = body.ChatID
			ev.ChatType = body.ChatType
			ev.MsgType = body.MsgType
			if body.From != nil {
				ev.FromUserID = body.From.UserID
			}
			if body.Text != nil {
				ev.TextContent = body.Text.Content
			} else if body.Voice != nil {
				ev.TextContent = body.Voice.Content
			}
			if body.Event != nil {
				ev.EventType = body.Event.EventType
			}
		}
	}
	return ev
}
