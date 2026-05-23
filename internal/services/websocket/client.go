package websocket

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog/log"
)

const (
	// Time allowed to write a message to the peer
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 60 * time.Second

	// Send pings to peer with this period (must be less than pongWait)
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer
	maxMessageSize = 1024 * 1024
)

func NewClient(userID uuid.UUID, conn *websocket.Conn, hub *Hub) *Client {
	return &Client{
		ID:         uuid.New(),
		UserID:     userID,
		Conn:       conn,
		Send:       make(chan []byte, 256),
		Hub:        hub,
		Subscribed: make(map[string]bool),
		lastPing:   time.Now(),
	}
}

// ReadPump pumps messages from the WebSocket connection to the hub
func (c *Client) ReadPump() {
	defer func() {
		c.Hub.unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		c.lastPing = time.Now()
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Error().
					Err(err).
					Str("clientId", c.ID.String()).
					Msg("WebSocket read error")
			}
			break
		}

		c.handleMessage(message)
	}
}

// WritePump pumps messages from the hub to the WebSocket connection
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// Hub closed the channel
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// Add queued messages to the current WebSocket message
			n := len(c.Send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.Send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// handleMessage processes incoming messages from the client
func (c *Client) handleMessage(message []byte) {
	var msg ClientMessage
	if err := json.Unmarshal(message, &msg); err != nil {
		log.Error().
			Err(err).
			Str("clientId", c.ID.String()).
			Msg("Failed to parse client message")
		return
	}

	switch msg.Type {
	case "SUBSCRIBE":
		c.handleSubscribe(msg.Data)
	case "UNSUBSCRIBE":
		c.handleUnsubscribe(msg.Data)
	case "TYPING_START":
		c.handleTypingStart(msg.Data)
	case "HEARTBEAT":
		c.handleHeartbeat()
	case "PRESENCE_UPDATE":
		c.handlePresenceUpdate(msg.Data)
	case "VOICE_JOIN":
		c.handleVoiceJoin(msg.Data)
	case "VOICE_LEAVE":
		c.handleVoiceLeave(msg.Data)
	case "VOICE_STATE_UPDATE":
		c.handleVoiceStateUpdate(msg.Data)
	case "VOICE_SIGNAL":
		c.handleVoiceSignal(msg.Data)
	default:
		log.Warn().
			Str("type", msg.Type).
			Str("clientId", c.ID.String()).
			Msg("Unknown message type")
	}
}

func (c *Client) handleSubscribe(data json.RawMessage) {
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	channelID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		return
	}

	if !c.canAccessStream(context.Background(), channelID) {
		log.Warn().
			Str("channelId", req.ChannelID).
			Str("userId", c.UserID.String()).
			Msg("User attempted to subscribe to unauthorized channel")
		return
	}

	c.Hub.Subscribe(c, req.ChannelID)
}

func (c *Client) handleUnsubscribe(data json.RawMessage) {
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	c.Hub.Unsubscribe(c, req.ChannelID)
}

func (c *Client) handleTypingStart(data json.RawMessage) {
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	channelID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		return
	}

	if !c.canAccessStream(context.Background(), channelID) {
		return
	}

	c.Hub.SetTyping(context.Background(), req.ChannelID, c.UserID)
}

func (c *Client) canAccessStream(ctx context.Context, channelID uuid.UUID) bool {
	if c.Hub.channelService != nil && c.Hub.channelService.CanAccessChannel(ctx, channelID, c.UserID) {
		return true
	}
	if c.Hub.dmService != nil && c.Hub.dmService.CanAccessConversation(ctx, channelID, c.UserID) {
		return true
	}
	return false
}

func (c *Client) handleHeartbeat() {
	event := &Event{
		Type: EventTypeHeartbeatAck,
		Data: map[string]interface{}{
			"timestamp": time.Now().UnixMilli(),
		},
	}

	data, _ := json.Marshal(event)
	select {
	case c.Send <- data:
	default:
	}
}

func (c *Client) handlePresenceUpdate(data json.RawMessage) {
	var req struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	normalizedStatus, ok := normalizePresenceStatus(req.Status)
	if !ok {
		return
	}

	c.Hub.setUserPresence(context.Background(), c.UserID, normalizedStatus)
}

// SendEvent sends an event directly to this client
func (c *Client) SendEvent(event *Event) {
	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	select {
	case c.Send <- data:
	default:
	}
}

// handleVoiceJoin handles a user joining a voice channel
func (c *Client) handleVoiceJoin(data json.RawMessage) {
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	channelID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		return
	}

	if c.Hub.voiceService == nil {
		return
	}

	// If the user is already in other voice channels, disconnect first and
	// broadcast leave events so channel participant lists stay in sync.
	previousChannelIDs, err := c.Hub.voiceService.DisconnectUser(context.Background(), c.UserID)
	if err != nil {
		log.Error().Err(err).
			Str("userId", c.UserID.String()).
			Msg("Failed to disconnect user from previous voice channels before join")
	}

	for _, previousChannelID := range previousChannelIDs {
		if previousChannelID.String() == req.ChannelID {
			continue
		}
		c.Hub.Broadcast(previousChannelID.String(), &Event{
			Type: EventTypeVoiceLeave,
			Data: map[string]interface{}{
				"channelId": previousChannelID.String(),
				"userId":    c.UserID.String(),
			},
		}, nil)
	}

	state, err := c.Hub.voiceService.JoinChannel(context.Background(), channelID, c.UserID)
	if err != nil {
		log.Error().Err(err).
			Str("channelId", req.ChannelID).
			Str("userId", c.UserID.String()).
			Msg("Failed to join voice channel")
		c.SendEvent(&Event{
			Type: "VOICE_ERROR",
			Data: map[string]interface{}{
				"error": err.Error(),
			},
		})
		return
	}

	// Get user info
	u, _ := c.Hub.userService.GetUserByID(context.Background(), c.UserID)

	// Get current participants for the joining user
	states, _ := c.Hub.voiceService.GetChannelVoiceStates(context.Background(), channelID)

	// Send current state to the joining user
	c.SendEvent(&Event{
		Type: EventTypeVoiceJoin,
		Data: map[string]interface{}{
			"channelId":    req.ChannelID,
			"userId":       c.UserID.String(),
			"state":        state,
			"user":         u,
			"participants": states,
		},
	})

	// Broadcast to others in the channel
	c.Hub.Broadcast(req.ChannelID, &Event{
		Type: EventTypeVoiceJoin,
		Data: map[string]interface{}{
			"channelId": req.ChannelID,
			"userId":    c.UserID.String(),
			"state":     state,
			"user":      u,
		},
	}, &c.ID)
}

// handleVoiceLeave handles a user leaving a voice channel
func (c *Client) handleVoiceLeave(data json.RawMessage) {
	var req struct {
		ChannelID string `json:"channelId"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	channelID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		return
	}

	if c.Hub.voiceService == nil {
		return
	}

	if err := c.Hub.voiceService.LeaveChannel(context.Background(), channelID, c.UserID); err != nil {
		return
	}

	// Broadcast leave to others
	c.Hub.Broadcast(req.ChannelID, &Event{
		Type: EventTypeVoiceLeave,
		Data: map[string]interface{}{
			"channelId": req.ChannelID,
			"userId":    c.UserID.String(),
		},
	}, &c.ID)

}

// handleVoiceStateUpdate handles mute/deafen state updates
func (c *Client) handleVoiceStateUpdate(data json.RawMessage) {
	var req struct {
		ChannelID       string `json:"channelId"`
		IsSelfMuted     *bool  `json:"isSelfMuted"`
		IsSelfDeafened  *bool  `json:"isSelfDeafened"`
		IsScreenSharing *bool  `json:"isScreenSharing"`
		IsWebcamOn      *bool  `json:"isWebcamOn"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	channelID, err := uuid.Parse(req.ChannelID)
	if err != nil {
		return
	}

	if c.Hub.voiceService == nil {
		return
	}

	state, err := c.Hub.voiceService.UpdateVoiceState(context.Background(), channelID, c.UserID, req.IsSelfMuted, req.IsSelfDeafened, req.IsScreenSharing, req.IsWebcamOn)
	if err != nil {
		return
	}

	// Broadcast state update
	c.Hub.Broadcast(req.ChannelID, &Event{
		Type: EventTypeVoiceState,
		Data: map[string]interface{}{
			"channelId": req.ChannelID,
			"userId":    c.UserID.String(),
			"state":     state,
		},
	}, nil)
}

// handleVoiceSignal handles WebRTC signaling (offer/answer/ICE candidates)
func (c *Client) handleVoiceSignal(data json.RawMessage) {
	var req struct {
		ChannelID  string          `json:"channelId"`
		TargetUID  string          `json:"targetUserId"`
		SignalType string          `json:"signalType"` // "offer", "answer", "ice-candidate"
		Signal     json.RawMessage `json:"signal"`
	}
	if err := json.Unmarshal(data, &req); err != nil {
		return
	}

	targetUserID, err := uuid.Parse(req.TargetUID)
	if err != nil {
		return
	}

	// Forward signal to target user
	c.Hub.SendToUser(targetUserID, &Event{
		Type: EventTypeVoiceSignal,
		Data: map[string]interface{}{
			"channelId":    req.ChannelID,
			"fromUserId":   c.UserID.String(),
			"targetUserId": req.TargetUID,
			"signalType":   req.SignalType,
			"signal":       req.Signal,
		},
	})
}
