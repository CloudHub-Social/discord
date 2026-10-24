package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.mau.fi/mautrix-discord/database"
)

func TestConnectedRequiresReadySession(t *testing.T) {
	u := newTestUser()
	u.Session = &discordgo.Session{}
	t.Cleanup(u.stopReconnecting)

	assert.False(t, u.Connected(), "allocating a session does not establish a Discord connection")
	u.onConnectionEstablished()
	assert.True(t, u.Connected(), "READY establishes the current session")
}

func TestConnectedClearsOnDisconnect(t *testing.T) {
	u := newTestUser()
	u.Session = &discordgo.Session{}
	t.Cleanup(u.stopReconnecting)
	u.onConnectionEstablished()
	require.True(t, u.Connected())

	u.disconnectedHandler(&discordgo.Disconnect{})

	assert.False(t, u.Connected(), "the retained session pointer must not report a disconnected socket as connected")
	assert.NotNil(t, u.reconnectTimer, "automatic recovery remains scheduled")
}

func TestConnectedClearsOnManualTeardown(t *testing.T) {
	u := newTestUser()
	u.Session = &discordgo.Session{}
	t.Cleanup(u.stopReconnecting)
	u.onConnectionEstablished()

	u.stopReconnecting()

	assert.False(t, u.Connected(), "manual teardown immediately withdraws connection readiness")
	assert.Nil(t, u.reconnectTimer)
}

func TestProvisioningPingReportsDisconnectedSession(t *testing.T) {
	u := newTestUser()
	u.User = &database.User{MXID: "@relay:example.com", DiscordToken: "Bot test-token"}
	u.Session = &discordgo.Session{}
	t.Cleanup(u.stopReconnecting)
	u.onConnectionEstablished()
	u.disconnectedHandler(&discordgo.Disconnect{})
	request := httptest.NewRequest(http.MethodGet, "/v1/ping", nil)
	request = request.WithContext(context.WithValue(request.Context(), "user", u))
	response := httptest.NewRecorder()

	(&ProvisioningAPI{}).ping(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	var body respPing
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	assert.True(t, body.Discord.LoggedIn, "stored credentials remain available for recovery")
	assert.False(t, body.Discord.Connected, "health response reflects the dropped socket")
}

func TestProvisioningDisconnectCancelsBackoff(t *testing.T) {
	u := newTestUser()
	var err error
	u.Session, err = discordgo.New("Bot test-token")
	require.NoError(t, err)
	u.Session.EventHandler = func(any) {}
	t.Cleanup(u.stopReconnecting)
	u.onConnectionEstablished()
	u.disconnectedHandler(&discordgo.Disconnect{})
	request := httptest.NewRequest(http.MethodPost, "/v1/disconnect", nil)
	request = request.WithContext(context.WithValue(request.Context(), "user", u))
	response := httptest.NewRecorder()

	(&ProvisioningAPI{}).disconnect(response, request)

	assert.Equal(t, http.StatusOK, response.Code, "a manual disconnect must cancel recovery even while the socket is down")
	assert.True(t, u.manualDisconnect)
	assert.Nil(t, u.reconnectTimer)
	assert.Nil(t, u.Session)
}
