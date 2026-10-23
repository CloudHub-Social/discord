package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
)

type heartbeatWireFrame struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

type heartbeatFixtureResult struct {
	frames [2]heartbeatWireFrame
	err    error
}

func TestDiscordGatewayHeartbeatCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name   string
		token  string
		isUser bool
	}{
		{name: "bot", token: "Bot test-token"},
		{name: "user", token: "test-token", isUser: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, err := discordgo.New(tc.token)
			if err != nil {
				t.Fatal(err)
			}
			if session.IsUser != tc.isUser {
				t.Fatalf("IsUser = %v, want %v", session.IsUser, tc.isUser)
			}
			session.EventHandler = func(any) {}
			session.ShouldReconnectOnError = false

			// Keep this connection local even if a proxy is configured.
			dialer := *websocket.DefaultDialer
			dialer.Proxy = nil
			dialer.HandshakeTimeout = 5 * time.Second
			session.Dialer = &dialer

			results := make(chan heartbeatFixtureResult, 1)
			accepted := make(chan *websocket.Conn, 1)
			handlerDone := make(chan struct{})
			upgrader := websocket.Upgrader{
				// User sessions send Discord's Origin header. This
				// server exists only on httptest's loopback listener.
				CheckOrigin: func(*http.Request) bool { return true },
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer close(handlerDone)
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					results <- heartbeatFixtureResult{err: fmt.Errorf("upgrade: %w", err)}
					return
				}
				accepted <- conn
				defer conn.Close()

				frames, err := exchangeGatewayHeartbeats(conn, tc.isUser)
				results <- heartbeatFixtureResult{frames: frames, err: err}
				if err != nil {
					return
				}
				// Keep the peer alive until Session.Close sends its close
				// frame, avoiding an unrelated disconnect during assertions.
				for {
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
				}
			}))
			t.Cleanup(func() {
				_ = session.Close()
				server.Close()
				// httptest.Server does not own hijacked WebSockets.
				select {
				case conn := <-accepted:
					_ = conn.Close()
					select {
					case <-handlerDone:
					case <-time.After(5 * time.Second):
						t.Error("fake gateway did not finish after close")
					}
				default:
					// Upgrade never completed.
				}
			})

			// Bypass Discord's REST gateway discovery entirely.
			session.SetGatewayURL("ws" + strings.TrimPrefix(server.URL, "http"))
			if err := session.Open(); err != nil {
				t.Fatalf("open local gateway: %v", err)
			}
			var result heartbeatFixtureResult
			select {
			case result = <-results:
			case <-time.After(12 * time.Second):
				t.Fatal("timed out waiting for local gateway heartbeats")
			}
			if result.err != nil {
				t.Fatal(result.err)
			}
			// Capture both paths before asserting so one mismatch cannot
			// hide a regression in the other heartbeat send path.
			for i, name := range []string{"periodic", "requested"} {
				t.Run(name, func(t *testing.T) {
					assertGatewayHeartbeat(t, result.frames[i], tc.isUser)
				})
			}
		})
	}
}

func exchangeGatewayHeartbeats(conn *websocket.Conn, isUser bool) ([2]heartbeatWireFrame, error) {
	var frames [2]heartbeatWireFrame
	deadline := time.Now().Add(10 * time.Second)
	if err := conn.SetReadDeadline(deadline); err != nil {
		return frames, err
	}
	if err := conn.SetWriteDeadline(deadline); err != nil {
		return frames, err
	}
	// The first periodic heartbeat is immediate. A one-hour interval
	// prevents a timer tick from satisfying the requested-heartbeat check.
	if err := conn.WriteJSON(map[string]any{
		"op": 10,
		"d":  map[string]any{"heartbeat_interval": 3_600_000},
	}); err != nil {
		return frames, fmt.Errorf("send HELLO: %w", err)
	}
	var identify heartbeatWireFrame
	if err := conn.ReadJSON(&identify); err != nil {
		return frames, fmt.Errorf("read IDENTIFY: %w", err)
	}
	if identify.Op != 2 {
		return frames, fmt.Errorf("IDENTIFY opcode = %d, want 2", identify.Op)
	}
	if err := conn.WriteJSON(map[string]any{
		"op": 0,
		"t":  "READY",
		"s":  42,
		"d": map[string]any{
			"v":          9,
			"session_id": "heartbeat-test-session",
			"user": map[string]any{
				"id":            "1",
				"username":      "heartbeat-test",
				"discriminator": "0001",
				"bot":           !isUser,
			},
			"guilds":           []any{},
			"private_channels": []any{},
		},
	}); err != nil {
		return frames, fmt.Errorf("send READY: %w", err)
	}
	if isUser {
		// User sessions emit Op41 after READY and before heartbeats.
		var sessionUpdate heartbeatWireFrame
		if err := conn.ReadJSON(&sessionUpdate); err != nil {
			return frames, fmt.Errorf("read user session update: %w", err)
		}
		if sessionUpdate.Op != 41 {
			return frames, fmt.Errorf("user session update opcode = %d, want 41", sessionUpdate.Op)
		}
	}
	if err := conn.ReadJSON(&frames[0]); err != nil {
		return frames, fmt.Errorf("read periodic heartbeat: %w", err)
	}
	if err := conn.WriteJSON(map[string]any{"op": 11, "d": nil}); err != nil {
		return frames, fmt.Errorf("send heartbeat ACK: %w", err)
	}
	if err := conn.WriteJSON(map[string]any{"op": 1, "d": nil}); err != nil {
		return frames, fmt.Errorf("request heartbeat: %w", err)
	}
	if err := conn.ReadJSON(&frames[1]); err != nil {
		return frames, fmt.Errorf("read requested heartbeat: %w", err)
	}
	return frames, nil
}

func assertGatewayHeartbeat(t *testing.T, frame heartbeatWireFrame, isUser bool) {
	t.Helper()
	if !isUser {
		if frame.Op != 1 {
			t.Fatalf("bot heartbeat opcode = %d, want 1; d=%s", frame.Op, frame.D)
		}
		var sequence int64
		if err := json.Unmarshal(frame.D, &sequence); err != nil {
			t.Fatalf("bot heartbeat d must be a numeric sequence: %v", err)
		}
		if sequence != 42 {
			t.Errorf("bot heartbeat sequence = %d, want 42", sequence)
		}
		return
	}
	if frame.Op != 40 {
		t.Fatalf("user heartbeat opcode = %d, want 40; d=%s", frame.Op, frame.D)
	}
	var data struct {
		Seq int64 `json:"seq"`
		Qos struct {
			Active  bool     `json:"active"`
			Ver     int      `json:"ver"`
			Reasons []string `json:"reasons"`
		} `json:"qos"`
	}
	if err := json.Unmarshal(frame.D, &data); err != nil {
		t.Fatal(err)
	}
	if data.Seq != 42 {
		t.Errorf("user heartbeat sequence = %d, want 42", data.Seq)
	}
	if !data.Qos.Active || data.Qos.Ver != 26 ||
		len(data.Qos.Reasons) != 1 || data.Qos.Reasons[0] != "foregrounded" {
		t.Errorf("user QoS payload changed: %+v", data.Qos)
	}
}
