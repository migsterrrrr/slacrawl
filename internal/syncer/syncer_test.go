package syncer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/openclaw/slacrawl/internal/config"
	"github.com/openclaw/slacrawl/internal/store"
)

func TestParseSourceAliases(t *testing.T) {
	cases := map[string]Source{
		"":          SourceAPI,
		"api":       SourceAPI,
		"bot":       SourceAPI,
		"user":      SourceUser,
		"user-api":  SourceUser,
		"desktop":   SourceDesktop,
		"wiretap":   SourceDesktop,
		"mcp":       SourceMCP,
		"connector": SourceMCP,
		"all":       SourceAll,
		"hybrid":    SourceAll,
	}
	for input, want := range cases {
		got, err := ParseSource(input)
		require.NoError(t, err, input)
		require.Equal(t, want, got, input)
	}
}

func TestParseSourceRejectsUnknown(t *testing.T) {
	_, err := ParseSource("github")
	require.ErrorContains(t, err, "unsupported source")
}

func TestParseSourceProvider(t *testing.T) {
	source, err := ParseSource(" Provider:Archive ")
	require.NoError(t, err)
	require.Equal(t, Source("provider:archive"), source)
	name, ok := ProviderName(source)
	require.True(t, ok)
	require.Equal(t, "archive", name)

	for _, input := range []string{"provider:", "provider:bad:name", "provider:bad/name"} {
		_, err := ParseSource(input)
		require.ErrorContains(t, err, "unsupported source")
	}
}

func TestRunWithTokensRoutesUserSource(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "slacrawl.db"))
	require.NoError(t, err)
	defer func() { require.NoError(t, st.Close()) }()

	_, err = RunWithTokens(context.Background(), config.Default(), st, Options{Source: SourceUser}, config.Tokens{})
	require.ErrorContains(t, err, "SLACK_USER_TOKEN is required for user-only sync")
}

func TestDesktopOptionsForSourceAllClearsInheritedWorkspace(t *testing.T) {
	opts := desktopOptionsForSourceAll(Options{Source: SourceAll, WorkspaceID: "T111"})
	require.Empty(t, opts.WorkspaceID)

	opts = desktopOptionsForSourceAll(Options{Source: SourceAll, WorkspaceID: "T222", WorkspaceSet: true})
	require.Equal(t, "T222", opts.WorkspaceID)
}
