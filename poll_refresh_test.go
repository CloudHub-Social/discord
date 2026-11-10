package main

import (
	"net/http"
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
