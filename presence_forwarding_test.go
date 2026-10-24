package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"gopkg.in/yaml.v3"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge/bridgeconfig"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"go.mau.fi/mautrix-discord/config"
	"go.mau.fi/mautrix-discord/database"
)

type presenceTestTransport func(*http.Request) (*http.Response, error)

func (f presenceTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type presenceTestCapture struct {
	mu      sync.Mutex
	patches []map[string]json.RawMessage
	frames  chan []heartbeatWireFrame
}

// Use the existing local gateway handshake, then collect frames up to an
// explicit heartbeat barrier. No sleeps or real Discord credentials are needed.
func newPresenceTestBridge(t *testing.T, isUser, syncPresence, syncStatus bool) (*DiscordBridge, *User, *presenceTestCapture) {
	t.Helper()
	token := "Bot test-token"
	if isUser {
		token = "test-token"
	}
	session, err := discordgo.New(token)
	if err != nil {
		t.Fatal(err)
	}
	session.EventHandler = func(any) {}
	session.ShouldReconnectOnError = false
	dialer := *websocket.DefaultDialer
	dialer.Proxy = nil
	dialer.HandshakeTimeout = 5 * time.Second
	session.Dialer = &dialer

	capture := &presenceTestCapture{frames: make(chan []heartbeatWireFrame, 4)}
	session.Client.Transport = presenceTestTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPatch || req.URL.String() != discordgo.EndpointUserSettings("@me") {
			return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		capture.mu.Lock()
		capture.patches = append(capture.patches, body)
		capture.mu.Unlock()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader("{}")),
			Request:    req,
		}, nil
	})

	cfg := &config.Config{BaseConfig: &bridgeconfig.BaseConfig{}}
	cfg.Homeserver.Domain = "example.com"
	if err := yaml.Unmarshal([]byte("username_template: discord_{{.}}\n"), &cfg.Bridge); err != nil {
		t.Fatal(err)
	}
	cfg.Bridge.SyncMatrixPresenceToDiscord = syncPresence
	cfg.Bridge.SyncMatrixStatusToDiscord = syncStatus
	br := &DiscordBridge{Config: cfg}
	br.Bot = &appservice.IntentAPI{Client: &mautrix.Client{UserID: "@bridge:example.com"}}
	user := newTestUser()
	user.User = &database.User{MXID: "@test:example.com", DiscordID: "1"}
	user.bridge = br
	user.Session = session
	br.usersByMXID = map[id.UserID]*User{user.MXID: user}

	ready := make(chan error, 1)
	accepted := make(chan *websocket.Conn, 1)
	done := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			ready <- err
			return
		}
		accepted <- conn
		defer conn.Close()
		if _, err := exchangeGatewayHeartbeats(conn, isUser); err != nil {
			ready <- err
			return
		}
		if err := conn.SetReadDeadline(time.Time{}); err != nil {
			ready <- err
			return
		}
		ready <- nil
		var frames []heartbeatWireFrame
		for {
			var frame heartbeatWireFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			if frame.Op == 1 && string(frame.D) == "-1" {
				capture.frames <- frames
				frames = nil
			} else {
				frames = append(frames, frame)
			}
		}
	}))
	t.Cleanup(func() {
		user.presenceLock.Lock()
		if user.presenceDebounceTimer != nil {
			user.presenceDebounceTimer.Stop()
		}
		user.presenceLock.Unlock()
		_ = session.Close()
		server.Close()
		select {
		case conn := <-accepted:
			_ = conn.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("presence gateway did not close")
			}
		default:
		}
	})
	session.SetGatewayURL("ws" + strings.TrimPrefix(server.URL, "http"))
	if err := session.Open(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(12 * time.Second):
		t.Fatal("presence gateway handshake timed out")
	}
	return br, user, capture
}

func (c *presenceTestCapture) snapshot(t *testing.T, session *discordgo.Session) ([]heartbeatWireFrame, []map[string]json.RawMessage) {
	t.Helper()
	if err := session.GatewayWriteStruct(map[string]any{"op": 1, "d": -1}); err != nil {
		t.Fatal(err)
	}
	var frames []heartbeatWireFrame
	select {
	case frames = <-c.frames:
	case <-time.After(5 * time.Second):
		t.Fatal("presence gateway barrier timed out")
	}
	c.mu.Lock()
	patches := append([]map[string]json.RawMessage(nil), c.patches...)
	c.mu.Unlock()
	return frames, patches
}

func sendTestMatrixPresence(br *DiscordBridge, user *User, presence event.Presence, text string) {
	br.HandleMatrixPresence(&event.Event{
		Sender: user.MXID,
		Type:   event.EphemeralEventPresence,
		Content: event.Content{Parsed: &event.PresenceEventContent{
			Presence: presence, StatusMessage: text,
		}},
	})
}

func TestMatrixPresenceStatusOnly(t *testing.T) {
	for _, clearStatus := range []bool{false, true} {
		t.Run(fmt.Sprintf("clear_%v", clearStatus), func(t *testing.T) {
			br, user, capture := newPresenceTestBridge(t, true, false, true)
			text := "busy"
			if clearStatus {
				user.matrixStatusEverSet = true
				user.lastMatrixStatusText = "old status"
				text = ""
			}
			sendTestMatrixPresence(br, user, event.PresenceOffline, text)
			frames, patches := capture.snapshot(t, user.Session)
			if len(frames) != 0 {
				t.Errorf("status-only update sent gateway frames: %+v", frames)
			}
			if len(patches) != 1 {
				t.Fatalf("custom-status PATCH count = %d, want 1", len(patches))
			}
			if clearStatus {
				if string(patches[0]["custom_status"]) != "null" {
					t.Errorf("clear custom_status = %s, want null", patches[0]["custom_status"])
				}
			} else {
				var status struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(patches[0]["custom_status"], &status); err != nil {
					t.Fatal(err)
				}
				if status.Text != text {
					t.Errorf("custom-status text = %q, want %q", status.Text, text)
				}
			}
		})
	}
}

func TestMatrixPresenceDisabled(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprintf("pending_%v", pending), func(t *testing.T) {
			br, user, capture := newPresenceTestBridge(t, true, false, false)
			if pending {
				user.pendingDiscordStatus = string(discordgo.StatusOnline)
				user.pendingDiscordStatusText = "queued before disabling"
				user.flushPendingDiscordPresence()
			} else {
				sendTestMatrixPresence(br, user, event.PresenceOnline, "do not send")
			}
			frames, patches := capture.snapshot(t, user.Session)
			if len(frames) != 0 || len(patches) != 0 {
				t.Errorf("disabled forwarding sent %d gateway frames and %d REST patches", len(frames), len(patches))
			}
		})
	}
}

func TestMatrixPresenceStateWire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		presence event.Presence
		text     string
		status   string
		idle     bool
	}{
		{"online", event.PresenceOnline, "ignored", "online", false},
		{"idle", event.PresenceUnavailable, "", "idle", true},
		{"invisible", event.PresenceOffline, "", "invisible", false},
		{"dnd", event.PresenceOnline, "[dnd] ignored", "dnd", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			br, user, capture := newPresenceTestBridge(t, true, true, false)
			// Activities observed on an account are not owned by this bridge
			// session and must never be copied into its outgoing presence.
			user.Session.State.Lock()
			user.Session.State.Presences = []*discordgo.Presence{{
				User:       &discordgo.User{ID: user.DiscordID},
				Activities: []*discordgo.Activity{{Name: "other-session game", Type: discordgo.ActivityTypeGame}},
			}}
			user.Session.State.Unlock()
			before := time.Now().UnixMilli()
			sendTestMatrixPresence(br, user, tc.presence, tc.text)
			frames, patches := capture.snapshot(t, user.Session)
			if len(patches) != 0 {
				t.Errorf("presence-only update changed custom status: %v", patches)
			}
			if len(frames) != 1 || frames[0].Op != 3 {
				t.Fatalf("presence frames = %+v, want one opcode 3", frames)
			}
			var data struct {
				Status     string          `json:"status"`
				AFK        bool            `json:"afk"`
				Since      *int64          `json:"since"`
				Activities json.RawMessage `json:"activities"`
			}
			if err := json.Unmarshal(frames[0].D, &data); err != nil {
				t.Fatal(err)
			}
			if data.Status != tc.status || data.AFK != tc.idle {
				t.Errorf("presence = %s afk=%v, want %s afk=%v", data.Status, data.AFK, tc.status, tc.idle)
			}
			if tc.idle {
				if data.Since == nil || *data.Since < before || *data.Since > time.Now().UnixMilli() {
					t.Errorf("idle since = %v, want current epoch milliseconds", data.Since)
				}
			} else if data.Since != nil {
				t.Errorf("non-idle since = %v, want null", *data.Since)
			}
			if string(data.Activities) != "[]" {
				t.Errorf("bridge session activities = %s, want explicit empty array", data.Activities)
			}
		})
	}
}

func TestMatrixPresenceBotStatusOnly(t *testing.T) {
	br, user, capture := newPresenceTestBridge(t, false, false, true)
	sendTestMatrixPresence(br, user, event.PresenceOffline, "unsupported bot custom status")
	frames, patches := capture.snapshot(t, user.Session)
	if len(frames) != 0 || len(patches) != 0 {
		t.Errorf("bot status-only update sent %d gateway frames and %d REST patches", len(frames), len(patches))
	}
}
