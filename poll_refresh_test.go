package main

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/stretchr/testify/require"
)

func TestPollRefreshCoalescesVotes(t *testing.T) {
	_, user, portal, _ := newRecoveryTest(t)
	user.Session.IsUser = true
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := make(chan int, 2)
	count := 0
	user.Session.Client.Transport = presenceTestTransport(func(req *http.Request) (*http.Response, error) {
		count++
		calls <- count
		if count == 1 {
			close(entered)
			<-release
		}
		return recoverySourceResponse(t, &discordgo.Message{ID: recoveryTestMessageID, ChannelID: portal.Key.ChannelID, Author: &discordgo.User{ID: "2"}, Poll: &discordgo.Poll{Question: discordgo.PollMedia{Text: "question"}}}), nil
	})
	portal.queuePollRefresh(user, recoveryTestMessageID, true)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first read did not start")
	}
	portal.queuePollRefresh(user, recoveryTestMessageID, true)
	portal.queuePollRefresh(user, recoveryTestMessageID, true)
	close(release)
	select {
	case queued := <-portal.discordMessages:
		snapshot, ok := queued.msg.(*discordPollSnapshot)
		require.True(t, ok)
		portal.finishPollRefresh(user, snapshot)
	case <-time.After(2 * time.Second):
		t.Fatal("first snapshot did not arrive")
	}
	select {
	case queued := <-portal.discordMessages:
		snapshot, ok := queued.msg.(*discordPollSnapshot)
		require.True(t, ok)
		require.NotNil(t, snapshot.message)
		portal.finishPollRefresh(user, snapshot)
	case <-time.After(2 * time.Second):
		t.Fatal("coalesced refresh did not arrive")
	}
	require.Len(t, calls, 2)
}

func TestPollRefreshRetriesRateLimit(t *testing.T) {
	_, user, portal, _ := newRecoveryTest(t)
	user.Session.IsUser = true
	attempts := 0
	user.Session.Client.Transport = presenceTestTransport(func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"retry_after":0.01,"message":"limited"}`)), Request: req}, nil
		}
		return recoverySourceResponse(t, &discordgo.Message{ID: recoveryTestMessageID, ChannelID: portal.Key.ChannelID, Author: &discordgo.User{ID: "2"}, Poll: &discordgo.Poll{Question: discordgo.PollMedia{Text: "question"}}}), nil
	})
	portal.queuePollRefresh(user, recoveryTestMessageID, true)
	select {
	case queued := <-portal.discordMessages:
		result, ok := queued.msg.(*discordPollSnapshot)
		require.True(t, ok)
		require.NotNil(t, result.message)
		require.Equal(t, 2, attempts)
	case <-time.After(2 * time.Second):
		t.Fatal("rate-limited poll refresh was not retried")
	}
}

func TestRefreshedPollSurvivesPartialUpdate(t *testing.T) {
	cached := &discordgo.Message{Poll: &discordgo.Poll{Question: discordgo.PollMedia{Text: "old"}}}
	refreshed := &discordgo.Message{Poll: &discordgo.Poll{Question: discordgo.PollMedia{Text: "new"}}}
	retainRefreshedPoll(cached, refreshed)
	mergePartialDiscordMessage(cached, &discordgo.Message{Content: "unrelated edit"})
	require.Equal(t, "new", cached.Poll.Question.Text)
}
