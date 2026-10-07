package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, defaultTrustedProxies, cfg.Server.TrustedProxies)
	assert.Equal(t, 5000, cfg.GraphQL.ComplexityLimit)
	assert.True(t, cfg.GraphQL.Introspection)
	assert.Equal(t, 10, cfg.GraphQL.MaxDepth)
	assert.Equal(t, 5, cfg.GraphQL.MaxAliases)
	assert.Equal(t, 2000, cfg.GraphQL.MaxNodeCount)
	assert.Equal(t, int64(1<<20), cfg.GraphQL.MaxBodyBytes)
}

func TestLoadGraphQLLimitsFromEnv(t *testing.T) {
	t.Setenv("GRAPHQL_COMPLEXITY_LIMIT", "1234")
	t.Setenv("GRAPHQL_MAX_DEPTH", "7")
	t.Setenv("GRAPHQL_MAX_ALIASES", "3")
	t.Setenv("GRAPHQL_MAX_NODE_COUNT", "456")
	t.Setenv("GRAPHQL_MAX_BODY_BYTES", "7890")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, 1234, cfg.GraphQL.ComplexityLimit)
	assert.Equal(t, 7, cfg.GraphQL.MaxDepth)
	assert.Equal(t, 3, cfg.GraphQL.MaxAliases)
	assert.Equal(t, 456, cfg.GraphQL.MaxNodeCount)
	assert.Equal(t, int64(7890), cfg.GraphQL.MaxBodyBytes)
}

func TestLoadRejectsNonPositiveGraphQLLimits(t *testing.T) {
	for name, env := range map[string]string{
		"complexity_limit": "GRAPHQL_COMPLEXITY_LIMIT",
		"max_depth":        "GRAPHQL_MAX_DEPTH",
		"max_aliases":      "GRAPHQL_MAX_ALIASES",
		"max_node_count":   "GRAPHQL_MAX_NODE_COUNT",
		"max_body_bytes":   "GRAPHQL_MAX_BODY_BYTES",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(env, "0")
			_, err := Load("")
			require.Error(t, err)
			assert.Contains(t, err.Error(), name)
		})
	}
}

func TestLoadTrustedProxiesFromEnv(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", " 10.0.0.1, 192.168.0.0/16 ,")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, []string{"10.0.0.1", "192.168.0.0/16"}, cfg.Server.TrustedProxies)

	t.Setenv("TRUSTED_PROXIES", "none")
	cfg, err = Load("")
	require.NoError(t, err)
	assert.Empty(t, cfg.Server.TrustedProxies)
}

func TestLoadRejectsInvalidTrustedProxy(t *testing.T) {
	t.Setenv("TRUSTED_PROXIES", "not-an-ip")
	_, err := Load("")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not-an-ip")
}

func TestLoadConfigFile(t *testing.T) {
	cfg, err := Load("../../config.yaml")
	require.NoError(t, err)
	assert.Equal(t, 5000, cfg.GraphQL.ComplexityLimit)
	assert.Equal(t, 10, cfg.GraphQL.MaxDepth)
	assert.Equal(t, 5, cfg.GraphQL.MaxAliases)
	assert.Equal(t, 2000, cfg.GraphQL.MaxNodeCount)
	assert.Equal(t, int64(1048576), cfg.GraphQL.MaxBodyBytes)
	assert.Equal(t, defaultTrustedProxies, cfg.Server.TrustedProxies)
}
