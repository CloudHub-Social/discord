package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
	"go.mau.fi/mautrix-discord/config"
	"go.mau.fi/mautrix-discord/database"
	"go.mau.fi/util/dbutil"
	maulogger "maunium.net/go/maulogger/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/bridge"
	"maunium.net/go/mautrix/crypto/attachment"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestGallerySchemaRoundTrip(t *testing.T) {
	fixture := `{"msgtype":"dm.filament.gallery","body":"Caption","itemtypes":[{"itemtype":"m.image","body":"First","filename":"first.png","url":"mxc://example.org/one","info":{"mimetype":"image/png","w":320,"h":240,"size":123},"page.codeberg.everypizza.msc4193.spoiler":true},{"itemtype":"m.file","body":"notes.txt","url":"mxc://example.org/two","info":{"mimetype":"text/plain","size":5}}]}`
	var raw map[string]any
	require.NoError(t, json.Unmarshal([]byte(fixture), &raw))
	items, err := decodeGallery(raw)
	require.NoError(t, err)
	require.Len(t, items, 2)
	require.Equal(t, event.MsgImage, items[0].Content.MsgType)
	require.Equal(t, "first.png", items[0].Content.FileName)
	require.Equal(t, id.ContentURIString("mxc://example.org/one"), items[0].Content.URL)
	require.Equal(t, 320, items[0].Content.Info.Width)
	require.True(t, items[0].Spoiler)
	require.Equal(t, "notes.txt", items[1].Content.Body)
	encoded, err := encodeGalleryItems([]*ConvertedMessage{{Content: items[0].Content, Extra: map[string]any{gallerySpoilerKey: true}}, {Content: items[1].Content}})
	require.NoError(t, err)
	raw["itemtypes"] = encoded
	again, err := decodeGallery(raw)
	require.NoError(t, err)
	require.Equal(t, items, again)
}

func TestGalleryRejectsUnsupportedAndMissingMedia(t *testing.T) {
	for _, fixture := range []string{
		`{"itemtypes":[]}`,
		`{"itemtypes":[{"itemtype":"m.location","body":"x"}]}`,
		`{"itemtypes":[{"itemtype":"m.file","body":"x"}]}`,
		`{"itemtypes":[{"itemtype":"m.image","url":"mxc://example.org/x"}]}`,
	} {
		var raw map[string]any
		require.NoError(t, json.Unmarshal([]byte(fixture), &raw))
		_, err := decodeGallery(raw)
		require.Error(t, err, fixture)
	}
}

func TestCombineDiscordGalleryPreservesCaptionAndItemOrder(t *testing.T) {
	parts := []*ConvertedMessage{
		{Content: &event.MessageEventContent{MsgType: event.MsgText, Body: "caption", Format: event.FormatHTML, FormattedBody: "<b>caption</b>"}},
		{AttachmentID: "1", Content: &event.MessageEventContent{MsgType: event.MsgImage, Body: "one.png", URL: "mxc://example.org/one", Info: &event.FileInfo{MimeType: "image/png"}}},
		{AttachmentID: "2", Content: &event.MessageEventContent{MsgType: event.MsgVideo, Body: "two.mp4", URL: "mxc://example.org/two", Info: &event.FileInfo{MimeType: "video/mp4"}}, Extra: map[string]any{gallerySpoilerKey: true}},
	}
	result := combineDiscordGallery(parts, 2)
	require.Len(t, result, 1)
	require.Equal(t, galleryAttachmentID, result[0].AttachmentID)
	require.Equal(t, "caption", result[0].Content.Body)
	wrapped := event.Content{Parsed: result[0].Content, Raw: result[0].Extra}
	data, err := json.Marshal(&wrapped)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	require.Equal(t, "dm.filament.gallery", raw["msgtype"])
	items, err := decodeGallery(raw)
	require.NoError(t, err)
	require.Equal(t, "one.png", items[0].Content.Body)
	require.Equal(t, "two.mp4", items[1].Content.Body)
	require.True(t, items[1].Spoiler)
	parts[2].Content = &event.MessageEventContent{MsgType: event.MsgNotice, Body: "upload failed"}
	require.Equal(t, parts, combineDiscordGallery(parts, 2))
}

func TestGalleryEditKeepsItemsInsideNewContent(t *testing.T) {
	content := &event.MessageEventContent{MsgType: galleryMessageType, Body: "new caption"}
	content.SetEdit("$original")
	extra := map[string]any{"itemtypes": []any{map[string]any{"itemtype": "m.file", "body": "file", "url": "mxc://example.org/file"}}}
	wrapped := event.Content{Parsed: content, Raw: galleryEditExtra(extra)}
	data, err := json.Marshal(&wrapped)
	require.NoError(t, err)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(data, &raw))
	_, err = decodeGallery(raw)
	require.NoError(t, err)
	replacement := raw["m.new_content"].(map[string]any)
	require.Equal(t, "dm.filament.gallery", replacement["msgtype"])
	items, err := decodeGallery(replacement)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestDownloadGalleryItemDecryptsAndEnforcesLimit(t *testing.T) {
	plain := []byte("gallery fixture")
	encrypted := attachment.NewEncryptedFile()
	ciphertext := encrypted.Encrypt(plain)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(ciphertext) }))
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@test:example.org", "token")
	require.NoError(t, err)
	client.SpecVersions = &mautrix.RespVersions{}
	intent := &appservice.IntentAPI{Client: client}
	encodedFile, err := json.Marshal(encrypted)
	require.NoError(t, err)
	var receivedFile attachment.EncryptedFile
	require.NoError(t, json.Unmarshal(encodedFile, &receivedFile))
	content := &event.MessageEventContent{File: &event.EncryptedFileInfo{EncryptedFile: receivedFile, URL: "mxc://example.org/file"}}
	got, err := downloadMatrixGalleryItem(intent, content, 1024)
	require.NoError(t, err)
	require.Equal(t, plain, got)
	_, err = downloadMatrixGalleryItem(intent, content, int64(len(ciphertext)-1))
	require.ErrorContains(t, err, "size limit")
	ciphertext[0] ^= 1
	_, err = downloadMatrixGalleryItem(intent, content, 1024)
	require.Error(t, err)
}

func TestPrepareGalleryMultipartPreservesFiles(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/one"):
			_, _ = w.Write([]byte("first"))
		case strings.HasSuffix(r.URL.Path, "/two"):
			_, _ = w.Write([]byte("second"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client, err := mautrix.NewClient(server.URL, "@test:example.org", "token")
	require.NoError(t, err)
	client.SpecVersions = &mautrix.RespVersions{}
	br := &DiscordBridge{Bridge: bridge.Bridge{Bot: &appservice.IntentAPI{Client: client}}}
	portal := &Portal{Portal: &database.Portal{Type: discordgo.ChannelTypeGuildText}, bridge: br}
	items := []galleryItem{
		{Content: &event.MessageEventContent{Body: "alt text", FileName: "first.png", URL: "mxc://example.org/one", Info: &event.FileInfo{MimeType: "image/png"}}},
		{Content: &event.MessageEventContent{Body: "second.txt", URL: "mxc://example.org/two", Info: &event.FileInfo{MimeType: "text/plain"}}, Spoiler: true},
	}
	req, err := portal.prepareMatrixGallery(nil, "channel", "", items, false)
	require.NoError(t, err)
	require.Len(t, req.Files, 2)
	require.Len(t, req.Attachments, 2)
	require.Equal(t, "alt text", req.Attachments[0].Description)
	require.Equal(t, "first.png", req.Files[0].Name)
	require.Equal(t, "SPOILER_second.txt", req.Files[1].Name)
	first, err := io.ReadAll(req.Files[0].Reader)
	require.NoError(t, err)
	require.Equal(t, "first", string(first))
	second, err := io.ReadAll(req.Files[1].Reader)
	require.NoError(t, err)
	require.Equal(t, "second", string(second))
}

func TestGalleryMatrixParsingKeepsUnknownItemFields(t *testing.T) {
	var wrapped event.Content
	require.NoError(t, json.Unmarshal([]byte(`{"msgtype":"dm.filament.gallery","body":"caption","itemtypes":[{"itemtype":"m.file","body":"report.txt","url":"mxc://example.org/file"}],"m.relates_to":{"m.in_reply_to":{"event_id":"$reply"}}}`), &wrapped))
	require.NoError(t, wrapped.ParseRaw(event.EventMessage))
	content, ok := wrapped.Parsed.(*event.MessageEventContent)
	require.True(t, ok)
	require.Equal(t, galleryMessageType, content.MsgType)
	require.Equal(t, id.EventID("$reply"), content.GetRelatesTo().GetNonFallbackReplyTo())
	items, err := decodeGallery(wrapped.Raw)
	require.NoError(t, err)
	require.Len(t, items, 1)
}

func TestPrepareGalleryUserCDNUpload(t *testing.T) {
	var server *httptest.Server
	uploads := make(map[string]string)
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/one"):
			_, _ = w.Write([]byte("first"))
		case strings.HasSuffix(r.URL.Path, "/two"):
			_, _ = w.Write([]byte("second"))
		case strings.HasSuffix(r.URL.Path, "/attachments"):
			var request discordgo.ReqPrepareAttachments
			require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
			require.Len(t, request.Files, 1)
			name := request.Files[0].Name
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"attachments": []any{map[string]any{"id": 1, "upload_url": server.URL + "/upload/" + name, "upload_filename": "remote/" + name}}})
		case strings.HasPrefix(r.URL.Path, "/upload/"):
			data, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			uploads[strings.TrimPrefix(r.URL.Path, "/upload/")] = string(data)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	old := discordgo.EndpointChannels
	discordgo.EndpointChannels = server.URL + "/channels/"
	defer func() { discordgo.EndpointChannels = old }()
	client, err := mautrix.NewClient(server.URL, "@test:example.org", "token")
	require.NoError(t, err)
	client.SpecVersions = &mautrix.RespVersions{}
	br := &DiscordBridge{Bridge: bridge.Bridge{Bot: &appservice.IntentAPI{Client: client}}}
	portal := &Portal{Portal: &database.Portal{Type: discordgo.ChannelTypeGuildText}, bridge: br}
	session, err := discordgo.New("token")
	require.NoError(t, err)
	session.IsUser = true
	session.Client = server.Client()
	sender := &User{Session: session}
	items := []galleryItem{
		{Content: &event.MessageEventContent{Body: "one.png", URL: "mxc://example.org/one", Info: &event.FileInfo{MimeType: "image/png"}}},
		{Content: &event.MessageEventContent{Body: "description", FileName: "two.txt", URL: "mxc://example.org/two", Info: &event.FileInfo{MimeType: "text/plain"}}, Spoiler: true},
	}
	req, err := portal.prepareMatrixGallery(sender, "123", "", items, true)
	require.NoError(t, err)
	require.Empty(t, req.Files)
	require.Len(t, req.Attachments, 2)
	require.Equal(t, "remote/one.png", req.Attachments[0].UploadedFilename)
	require.Equal(t, "remote/SPOILER_two.txt", req.Attachments[1].UploadedFilename)
	require.Equal(t, "description", req.Attachments[1].Description)
	require.Equal(t, map[string]string{"one.png": "first", "SPOILER_two.txt": "second"}, uploads)
}

func TestGalleryEchoReconcilesUnknownSend(t *testing.T) {
	raw, err := sql.Open("sqlite3", ":memory:")
	require.NoError(t, err)
	defer raw.Close()
	base, err := dbutil.NewWithDB(raw, "sqlite3")
	require.NoError(t, err)
	_, err = base.Exec(`CREATE TABLE gallery_pending (mxid TEXT PRIMARY KEY, dc_chan_id TEXT NOT NULL, dc_chan_receiver TEXT NOT NULL, dc_thread_id TEXT NOT NULL, dc_sender TEXT NOT NULL, sender_mxid TEXT NOT NULL, nonce TEXT NOT NULL UNIQUE); CREATE TABLE message (dcid TEXT, dc_attachment_id TEXT, dc_chan_id TEXT, dc_chan_receiver TEXT, dc_sender TEXT, timestamp BIGINT, dc_edit_timestamp BIGINT, dc_thread_id TEXT, mxid TEXT UNIQUE, sender_mxid TEXT, PRIMARY KEY(dcid,dc_attachment_id,dc_chan_id,dc_chan_receiver));`)
	require.NoError(t, err)
	db := database.New(base, maulogger.Create())
	key := database.PortalKey{ChannelID: "123", Receiver: "receiver"}
	pending := database.GalleryPending{MXID: "$event", Channel: key, SenderID: "sender", SenderMXID: "@user:example.org", Nonce: galleryNonce("$event")}
	reserved, err := db.Message.ReserveGallery(pending)
	require.NoError(t, err)
	require.True(t, reserved)
	portal := &Portal{Portal: &database.Portal{Key: key}, bridge: &DiscordBridge{DB: db, Config: &config.Config{}}}
	msg := &discordgo.Message{ID: "100000000000000000", ChannelID: "123", Author: &discordgo.User{ID: "other"}, Nonce: discordgo.StringOrInt(pending.Nonce)}
	require.False(t, portal.reconcileGalleryEcho(msg))
	msg.Author.ID = "sender"
	require.True(t, portal.reconcileGalleryEcho(msg))
	mapped := db.Message.GetByMXID(key, "$event")
	require.NotNil(t, mapped)
	require.Equal(t, msg.ID, mapped.DiscordID)
	require.Equal(t, galleryAttachmentID, mapped.AttachmentID)
	reservation, err := db.Message.PendingGalleryByNonce(key, pending.Nonce, "sender")
	require.NoError(t, err)
	require.Nil(t, reservation)
	require.False(t, portal.reconcileGalleryEcho(msg))
}
