package slackapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"github.com/openclaw/slacrawl/internal/store"
)

var requiredUserChannelScopes = []string{
	"channels:history",
	"channels:read",
	"groups:history",
	"groups:read",
	"users:read",
}

var requiredUserDMSScopes = []string{
	"im:history",
	"im:read",
	"mpim:history",
	"mpim:read",
}

// SyncUser mirrors only conversations the authenticated user has joined. It
// refuses bot credentials and non-read OAuth scopes so this source cannot join
// channels or perform write actions in Slack.
func (c *Client) SyncUser(ctx context.Context, st *store.Store, opts SyncOptions) error {
	if strings.TrimSpace(c.tokens.Bot) != "" || c.bot != nil {
		return errors.New("user-only sync refuses SLACK_BOT_TOKEN; disable the bot token source")
	}
	if c.user == nil || strings.TrimSpace(c.tokens.User) == "" {
		return errors.New("SLACK_USER_TOKEN is required for user-only sync")
	}

	auth, err := c.authTest(ctx, c.user)
	if err != nil {
		return err
	}
	if err := validateUserOnlyScopeSet(auth, c.includeDMs); err != nil {
		return err
	}
	workspaceID, err := authenticatedWorkspaceID(auth, opts.WorkspaceID)
	if err != nil {
		return err
	}

	now := c.now()
	if err := st.UpsertWorkspace(ctx, store.Workspace{
		ID:           workspaceID,
		Name:         auth.Team,
		EnterpriseID: auth.EnterpriseID,
		RawJSON:      store.MarshalRaw(auth),
		UpdatedAt:    now,
	}); err != nil {
		return err
	}

	users, err := c.getUsers(ctx, c.user)
	if err != nil {
		return err
	}
	userByID := make(map[string]slack.User, len(users))
	for _, user := range users {
		userByID[user.ID] = user
		if _, err := c.skipUserCollision(ctx, st, workspaceID, user, now); err != nil {
			return err
		}
	}

	channels, err := c.fetchChannelsWithClient(ctx, c.user, workspaceID)
	if err != nil {
		return err
	}
	channels = selectUserConversations(channels, opts, true)
	threadRepliesSkipped := newThreadSkipTracker()
	userSource := channelSyncSource{
		historyClient: c.user,
		token:         c.tokens.User,
		sourceName:    SourceUser,
		sourceRank:    1,
		allowJoin:     false,
		threadSkip:    threadRepliesSkipped,
	}
	if err := c.syncChannelsWithSource(ctx, st, workspaceID, channels, opts, now, true, userSource); err != nil {
		return err
	}

	if c.includeDMs {
		dms, err := c.fetchDMs(ctx, workspaceID)
		if err != nil {
			return err
		}
		for index := range dms {
			dms[index].Name = dmChannelName(dms[index], userByID)
		}
		dms = selectUserConversations(dms, opts, false)
		if err := c.syncChannelsWithSource(ctx, st, workspaceID, dms, opts, now, true, userSource); err != nil {
			return err
		}
	}

	if err := c.setThreadCoverage(ctx, st, workspaceID, opts, true, threadRepliesSkipped); err != nil {
		return err
	}
	return st.SetSyncState(ctx, SourceUser, "workspace", workspaceID, now.Format(time.RFC3339))
}

func selectUserConversations(channels []slack.Channel, opts SyncOptions, requireMembership bool) []slack.Channel {
	allow := make(map[string]struct{}, len(opts.Channels))
	for _, id := range opts.Channels {
		allow[strings.TrimSpace(id)] = struct{}{}
	}
	excluded := excludedChannelNames(opts.ExcludeChannels)
	selected := make([]slack.Channel, 0, len(channels))
	for _, channel := range channels {
		if requireMembership && !channel.IsMember {
			continue
		}
		if len(allow) > 0 {
			if _, ok := allow[channel.ID]; !ok {
				continue
			}
		}
		if channelExcluded(channel, excluded) {
			continue
		}
		selected = append(selected, channel)
	}
	return selected
}

func validateUserOnlyScopeSet(auth *slack.AuthTestResponse, includeDMs bool) error {
	if auth == nil {
		return errors.New("user-only sync could not inspect Slack authentication")
	}
	if strings.TrimSpace(auth.BotID) != "" {
		return errors.New("user-only sync requires a user token, but Slack authenticated a bot token")
	}
	scopes := oauthScopes(auth.Header)
	if len(scopes) == 0 {
		return errors.New("Slack did not report OAuth scopes; refusing user-only sync")
	}

	granted := make(map[string]struct{}, len(scopes))
	var nonRead []string
	for _, scope := range scopes {
		granted[scope] = struct{}{}
		if !isReadOnlyUserScope(scope) {
			nonRead = append(nonRead, scope)
		}
	}
	if len(nonRead) > 0 {
		return fmt.Errorf("user-only sync refuses non-read OAuth scopes: %s", strings.Join(nonRead, ", "))
	}

	required := append([]string{}, requiredUserChannelScopes...)
	if includeDMs {
		required = append(required, requiredUserDMSScopes...)
	}
	var missing []string
	for _, scope := range required {
		if _, ok := granted[scope]; !ok {
			missing = append(missing, scope)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("user-only sync is missing required OAuth scopes: %s", strings.Join(missing, ", "))
	}
	return nil
}

func oauthScopes(header http.Header) []string {
	seen := map[string]struct{}{}
	for _, value := range header.Values("X-OAuth-Scopes") {
		for _, scope := range strings.Split(value, ",") {
			scope = strings.ToLower(strings.TrimSpace(scope))
			if scope != "" {
				seen[scope] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(seen))
	for scope := range seen {
		out = append(out, scope)
	}
	sort.Strings(out)
	return out
}

func isReadOnlyUserScope(scope string) bool {
	scope = strings.ToLower(strings.TrimSpace(scope))
	if strings.HasSuffix(scope, ":read") || strings.Contains(scope, ":read.") || strings.HasSuffix(scope, ":history") {
		return true
	}
	switch scope {
	case "openid", "profile", "email", "identify", "identity.basic", "identity.email", "identity.team", "identity.avatar":
		return true
	default:
		return false
	}
}
