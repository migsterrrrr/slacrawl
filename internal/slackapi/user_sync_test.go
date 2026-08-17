package slackapi

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
)

func TestSyncUserMirrorsJoinedChannelsDMsAndThreadsWithoutJoining(t *testing.T) {
	server := newUserOnlySlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{User: "xoxp-user-only"}, server.URL(), server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }
	client.now = func() time.Time { return time.Date(2026, 8, 17, 9, 30, 0, 0, time.UTC) }
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	require.NoError(t, client.SyncUser(context.Background(), st, SyncOptions{Concurrency: 1}))

	channels, err := st.Channels(context.Background(), "T123", "", 20)
	require.NoError(t, err)
	kindByID := make(map[string]string, len(channels))
	for _, channel := range channels {
		kindByID[channel.ID] = channel.Kind
	}
	require.Equal(t, map[string]string{
		"CARCHIVED": "public_channel",
		"CJOINED":   "public_channel",
		"CPRIVATE":  "private_channel",
		"D123":      "im",
		"G123":      "mpim",
	}, kindByID)
	require.NotContains(t, kindByID, "CUNJOINED")

	rows, err := st.QueryReadOnly(context.Background(), `
select channel_id, text, source_name, source_rank
from messages
order by channel_id, ts
`)
	require.NoError(t, err)
	require.Len(t, rows, 6)
	for _, row := range rows {
		require.NotEqual(t, "CUNJOINED", row["channel_id"])
		require.Equal(t, SourceUser, row["source_name"])
		require.Equal(t, int64(1), row["source_rank"])
	}
	require.Equal(t, 0, server.calls("conversations.join"))
	require.Empty(t, server.unexpectedTokens())

	coverage, err := st.GetSyncState(context.Background(), "doctor", "threads", "coverage")
	require.NoError(t, err)
	require.Equal(t, "full", coverage)
	lastSync, err := st.GetSyncState(context.Background(), SourceUser, "workspace", "T123")
	require.NoError(t, err)
	require.Equal(t, "2026-08-17T09:30:00Z", lastSync)
}

func TestDoctorSupportsReadOnlyUserTokenWithoutBot(t *testing.T) {
	server := newUserOnlySlackServer(t)
	defer server.Close()

	client := NewWithOptions(config.Tokens{User: "xoxp-user-only"}, server.URL(), server.Client())
	client.sleep = func(context.Context, time.Duration) error { return nil }
	diagnostics, err := client.Doctor(context.Background())
	require.NoError(t, err)
	require.False(t, diagnostics.BotConfigured)
	require.True(t, diagnostics.UserConfigured)
	require.True(t, diagnostics.UserAuthAvailable)
	require.True(t, diagnostics.UserReadOnly)
	require.Equal(t, "T123", diagnostics.UserAuthTeamID)
	require.Equal(t, "Test Team", diagnostics.UserAuthTeam)
	require.Equal(t, "full", diagnostics.ThreadCoverage)
	require.True(t, diagnostics.DMsIncluded)
	require.Empty(t, diagnostics.DMsMissingScope)
	require.Empty(t, diagnostics.UserScopeError)
}

func TestSyncUserRefusesBotCredentials(t *testing.T) {
	client := New(config.Tokens{Bot: "xoxb-not-allowed", User: "xoxp-user-only"})
	st := mustStore(t)
	defer func() { require.NoError(t, st.Close()) }()

	err := client.SyncUser(context.Background(), st, SyncOptions{})
	require.ErrorContains(t, err, "refuses SLACK_BOT_TOKEN")
}

func TestValidateUserOnlyScopeSet(t *testing.T) {
	allReadScopes := strings.Join(append(append([]string{}, requiredUserChannelScopes...), requiredUserDMSScopes...), ",")
	tests := []struct {
		name       string
		auth       *slack.AuthTestResponse
		includeDMs bool
		errorText  string
	}{
		{
			name:       "all required read scopes",
			auth:       userAuthWithScopes(allReadScopes + ",search:read,users:read.email"),
			includeDMs: true,
		},
		{
			name:       "DM scopes optional when DMs disabled",
			auth:       userAuthWithScopes(strings.Join(requiredUserChannelScopes, ",")),
			includeDMs: false,
		},
		{
			name:       "missing DM scope",
			auth:       userAuthWithScopes(strings.ReplaceAll(allReadScopes, "im:history,", "")),
			includeDMs: true,
			errorText:  "missing required OAuth scopes: im:history",
		},
		{
			name:       "write scope",
			auth:       userAuthWithScopes(allReadScopes + ",chat:write"),
			includeDMs: true,
			errorText:  "refuses non-read OAuth scopes: chat:write",
		},
		{
			name:       "management scope",
			auth:       userAuthWithScopes(allReadScopes + ",channels:manage"),
			includeDMs: true,
			errorText:  "refuses non-read OAuth scopes: channels:manage",
		},
		{
			name: "bot token",
			auth: &slack.AuthTestResponse{
				BotID:  "B123",
				Header: http.Header{"X-Oauth-Scopes": []string{allReadScopes}},
			},
			includeDMs: true,
			errorText:  "authenticated a bot token",
		},
		{
			name:       "missing scope header",
			auth:       &slack.AuthTestResponse{},
			includeDMs: true,
			errorText:  "did not report OAuth scopes",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateUserOnlyScopeSet(test.auth, test.includeDMs)
			if test.errorText == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.errorText)
		})
	}
}

func userAuthWithScopes(scopes string) *slack.AuthTestResponse {
	return &slack.AuthTestResponse{
		Team:   "Test Team",
		TeamID: "T123",
		User:   "alice",
		UserID: "U123",
		Header: http.Header{"X-Oauth-Scopes": []string{scopes}},
	}
}

type userOnlySlackServer struct {
	server *httptest.Server
	mu     sync.Mutex
	counts map[string]int
	tokens []string
}

func newUserOnlySlackServer(t *testing.T) *userOnlySlackServer {
	t.Helper()
	mock := &userOnlySlackServer{counts: map[string]int{}}
	mock.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		values := mustFormValues(r)
		token := values.Get("token")
		if token == "" {
			token = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		mock.mu.Lock()
		mock.counts[r.URL.Path]++
		mock.tokens = append(mock.tokens, token)
		mock.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/auth.test":
			scopes := append(append([]string{}, requiredUserChannelScopes...), requiredUserDMSScopes...)
			w.Header().Set("X-OAuth-Scopes", strings.Join(scopes, ","))
			_, _ = w.Write([]byte(`{"ok":true,"team":"Test Team","team_id":"T123","user":"alice","user_id":"U123"}`))
		case "/conversations.list":
			switch values.Get("types") {
			case "public_channel,private_channel":
				_, _ = w.Write([]byte(`{"ok":true,"channels":[
					{"id":"CJOINED","name":"joined","is_channel":true,"is_member":true},
					{"id":"CUNJOINED","name":"not-joined","is_channel":true,"is_member":false},
					{"id":"CPRIVATE","name":"private","is_channel":true,"is_private":true,"is_member":true},
					{"id":"CARCHIVED","name":"archive","is_channel":true,"is_archived":true,"is_member":true}
				],"response_metadata":{"next_cursor":""}}`))
			case "im,mpim":
				_, _ = w.Write([]byte(`{"ok":true,"channels":[
					{"id":"D123","is_im":true,"is_private":true,"user":"U456"},
					{"id":"G123","is_mpim":true,"is_private":true,"members":["U123","U456"]}
				],"response_metadata":{"next_cursor":""}}`))
			default:
				http.Error(w, "unexpected conversation types", http.StatusBadRequest)
			}
		case "/conversations.history":
			channelID := values.Get("channel")
			if channelID == "CUNJOINED" {
				t.Errorf("user-only sync requested history for unjoined channel")
			}
			replyFields := ""
			if channelID == "CJOINED" {
				replyFields = `,"reply_count":1,"latest_reply":"1710000001.000200"`
			}
			//nolint:gosec // Test server echoes a controlled channel ID.
			_, _ = fmt.Fprintf(w, `{"ok":true,"messages":[{"type":"message","channel":%q,"user":"U123","text":%q,"ts":"1710000000.000100"%s}],"response_metadata":{"next_cursor":""}}`, channelID, "message-"+channelID, replyFields)
		case "/conversations.replies":
			_, _ = w.Write([]byte(`{"ok":true,"messages":[{"type":"message","channel":"CJOINED","user":"U456","text":"thread reply","thread_ts":"1710000000.000100","ts":"1710000001.000200"}],"response_metadata":{"next_cursor":""}}`))
		case "/users.list":
			_, _ = w.Write([]byte(`{"ok":true,"members":[
				{"id":"U123","name":"alice","profile":{"display_name":"alice"}},
				{"id":"U456","name":"bob","profile":{"display_name":"bob"}}
			],"response_metadata":{"next_cursor":""}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	return mock
}

func (m *userOnlySlackServer) Close() {
	m.server.Close()
}

func (m *userOnlySlackServer) URL() string {
	return m.server.URL + "/"
}

func (m *userOnlySlackServer) Client() *http.Client {
	return m.server.Client()
}

func (m *userOnlySlackServer) calls(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts["/"+method]
}

func (m *userOnlySlackServer) unexpectedTokens() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var unexpected []string
	for _, token := range m.tokens {
		if token != "xoxp-user-only" {
			unexpected = append(unexpected, token)
		}
	}
	return unexpected
}
