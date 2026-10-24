package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/maulogger/v2"

	"go.mau.fi/mautrix-discord/config"
	"go.mau.fi/mautrix-discord/database"
)

// Connect persists its heartbeat session, so exercise it with an actual small
// SQLite database rather than replacing the connection or persistence code.
func newConnectionTestUser(t *testing.T) *User {
	t.Helper()
	sqlDB, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	_, err = sqlDB.Exec(`CREATE TABLE "user" (
		mxid TEXT PRIMARY KEY, dcid TEXT, discord_token TEXT, management_room TEXT,
		space_room TEXT, dm_space_room TEXT, read_state_version INTEGER,
		heartbeat_session TEXT
	)`)
	require.NoError(t, err)
	baseDB, err := dbutil.NewWithDB(sqlDB, "sqlite3")
	require.NoError(t, err)
	db := database.New(baseDB, maulogger.Create())
	u := newTestUser()
	u.User = db.User.New()
	u.MXID = "@relay:example.com"
	u.DiscordToken = "Bot test-token"
	u.Insert()
	u.bridge = &DiscordBridge{Config: &config.Config{}, DB: db, usersByID: make(map[string]*User)}
	t.Cleanup(func() {
		u.stopReconnecting()
		u.Lock()
		if u.Session != nil {
			_ = u.Session.Close()
		}
		u.Unlock()
	})
	return u
}

// REST discovery and WebSocket traffic both terminate on the loopback listener.
// These tests are deliberately not parallel: discordgo exposes discovery as a
// package endpoint variable, which is restored after every test.
func useConnectionTestGateway(t *testing.T, gateway http.HandlerFunc) {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/gateway" {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"url": "ws" + strings.TrimPrefix(server.URL, "http") + "/ws",
			})
			return
		}
		gateway(w, r)
	}))
	oldEndpoint := discordgo.EndpointGateway
	discordgo.EndpointGateway = server.URL + "/gateway"
	t.Cleanup(func() {
		discordgo.EndpointGateway = oldEndpoint
		server.Close()
	})
}

func TestStartupFailureKeepsRecovering(t *testing.T) {
	useConnectionTestGateway(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "gateway temporarily unavailable", http.StatusServiceUnavailable)
	})
	u := newConnectionTestUser(t)

	u.startupTryConnect()

	u.reconnectLock.Lock()
	defer u.reconnectLock.Unlock()
	assert.NotNil(t, u.reconnectTimer, "transient startup failures must keep a capped recovery retry scheduled")
	assert.False(t, u.sessionReady)
	assert.False(t, u.manualDisconnect)
}

func TestInitialOp7ReturnsForBackoff(t *testing.T) {
	var attempts atomic.Int32
	useConnectionTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
		_ = conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 3_600_000}})
		var identify heartbeatWireFrame
		if conn.ReadJSON(&identify) != nil {
			return
		}
		if attempts.Add(1) == 1 {
			_ = conn.WriteJSON(map[string]any{"op": 7, "d": nil})
		} else {
			// Bound the old implementation's held-lock five-second retry so a
			// failing regression still joins its goroutine and cleans up.
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "test finished"))
		}
	})
	u := newConnectionTestUser(t)
	result := make(chan error, 1)
	go func() { result <- u.Connect() }()

	select {
	case err := <-result:
		assert.ErrorIs(t, err, discordgo.ErrImmediateDisconnect)
	case <-time.After(time.Second):
		t.Error("initial Op7 did not return for shared backoff; Connect is retrying while holding the user lock")
		select {
		case <-result:
		case <-time.After(7 * time.Second):
			t.Fatal("Connect did not finish after the fixture closed its second attempt")
		}
	}
}

func TestStartupInvalidCredentialsStopsRecovery(t *testing.T) {
	useConnectionTestGateway(t, func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		_ = conn.WriteJSON(map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 3_600_000}})
		var identify heartbeatWireFrame
		if conn.ReadJSON(&identify) == nil {
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(4004, "invalid test token"))
		}
	})
	u := newConnectionTestUser(t)

	u.startupTryConnect()

	require.Eventually(t, func() bool { return !u.IsLoggedIn() }, time.Second, time.Millisecond)
	u.reconnectLock.Lock()
	defer u.reconnectLock.Unlock()
	assert.Nil(t, u.reconnectTimer, "invalid credentials require login, not persistent recovery")
	assert.True(t, u.manualDisconnect)
	assert.False(t, u.sessionReady)
}
