package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDiscordRedirect(t *testing.T) {
	for _, tc := range []struct{ path, target string }{
		{"/mautrix-discord/open/@me/123/456", "discord:///channels/@me/123/456"},
		{"/mautrix-discord/open/123/456/789", "discord:///channels/123/456/789"},
		{"/mautrix-discord/open/evil/456/789", ""},
		{"/mautrix-discord/open/@me/456/789?next=https://evil.example", ""},
		{"/mautrix-discord/open/@me/456/789/extra", ""},
		{"/mautrix-discord/open/@me//789", ""},
		{"/other/@me/456/789", ""},
	} {
		w := httptest.NewRecorder()
		discordRedirect(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if tc.target == "" {
			require.Equal(t, http.StatusNotFound, w.Code, tc.path)
			require.Empty(t, w.Header().Get("Location"))
		} else {
			require.Equal(t, http.StatusFound, w.Code, tc.path)
			require.Equal(t, tc.target, w.Header().Get("Location"))
		}
	}
}

func TestDiscordMessageURLRedirect(t *testing.T) {
	require.Equal(t, "https://matrix.example/mautrix-discord/open/@me/123/456", discordMessageURL("", "123", "456", "https://matrix.example"))
	require.Equal(t, "https://discord.com/channels/@me/123/456", discordMessageURL("", "123", "456", "http://insecure.example"))
	require.Empty(t, discordMessageURL("", "evil", "456", "https://matrix.example"))
}

func TestDiscordRedirectOverHTTPS(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(discordRedirect))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Get(server.URL + "/mautrix-discord/open/@me/123/456")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)
	require.Equal(t, "discord:///channels/@me/123/456", resp.Header.Get("Location"))
}
