package graph

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/palemoky/chinese-poetry-api/internal/database"
)

// graphqlResponse 是 /graphql 原始响应的最小结构。
type graphqlResponse struct {
	Data   any `json:"data"`
	Errors []struct {
		Message    string           `json:"message"`
		Locations  []map[string]int `json:"locations"`
		Extensions map[string]any   `json:"extensions"`
	} `json:"errors"`
}

// postRaw 直接对 handler 发 POST，返回 HTTP 状态码与解析后的响应，
// 用来断言状态码、错误码与源码位置（gqlgen client 只返回错误字符串）。
func postRaw(t *testing.T, h http.Handler, query string) (int, graphqlResponse) {
	t.Helper()
	body := fmt.Sprintf(`{"query":%q}`, query)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var resp graphqlResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "raw response: %s", rec.Body.String())
	return rec.Code, resp
}

func requireGraphQLError(t *testing.T, status int, resp graphqlResponse, code string) {
	t.Helper()
	require.NotEmpty(t, resp.Errors, "expected a GraphQL error, got data: %v", resp.Data)
	assert.Equal(t, http.StatusUnprocessableEntity, status, "structural limits must return 422")
	assert.Nil(t, resp.Data, "rejected operations must not return any data")
	got, _ := resp.Errors[0].Extensions["code"].(string)
	assert.Equal(t, code, got)
	require.NotEmpty(t, resp.Errors[0].Locations, "error must point at a source location")
	assert.Positive(t, resp.Errors[0].Locations[0]["line"])
	assert.Positive(t, resp.Errors[0].Locations[0]["column"])
}

// defaultLimitServer 与生产默认门槛一致。
func defaultLimitServer(resolver *Resolver) http.Handler {
	return NewServer(resolver, ServerOptions{ComplexityLimit: 5000, Introspection: true})
}

// setupSerializedResolver 创建只允许一个底层连接的内存库。
//
// gqlgen 会并发执行同一请求中的字段，而内存 SQLite 的每个连接都是一个独立的
// 空数据库：并发字段一旦拿到新连接就会看到「no such table」。测试没有外部
// 并发压力，把连接池钉在 1 上即可让所有 goroutine 共用同一份建表结果。
func setupSerializedResolver(t *testing.T) (*Resolver, *database.Repository) {
	t.Helper()
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	db := &database.DB{DB: gormDB}
	require.NoError(t, db.Migrate())
	repo := database.NewRepository(db)
	return NewResolver(db, repo), repo
}

func TestQueryLimitsRejectDeepNesting(t *testing.T) {
	resolver, _ := setupTestResolver(t)

	// poem → author → poems → edges → node → author → poems → edges → node → author → name
	// 共 11 层，超过默认的 10 层上限。注意全部 pageSize 都取 1，
	// 说明这是深度门槛在起作用，而不是复杂度。
	const deep = `{
  poem(id: "1") {
    author {
      poems(pageSize: 1) {
        edges { node {
          author {
            poems(pageSize: 1) {
              edges { node { author { name } } }
            }
          }
        } }
      }
    }
  }
}`
	status, resp := postRaw(t, defaultLimitServer(resolver), deep)
	requireGraphQLError(t, status, resp, errCodeMaxDepth)
	assert.Contains(t, resp.Errors[0].Message, "depth")
}

func TestQueryLimitsRejectAliasFanOut(t *testing.T) {
	resolver, _ := setupTestResolver(t)

	// 同一字段 poem 用 6 个别名重复取，每次只选 title：
	// 复杂度只有约 12，原有的复杂度上限对此完全无感，但它会放大成 6+ 次数据库查询。
	var b strings.Builder
	b.WriteString("{\n")
	for i := 1; i <= 6; i++ {
		fmt.Fprintf(&b, "  a%d: poem(id: %q) { title }\n", i, fmt.Sprint(i))
	}
	b.WriteString("}")

	status, resp := postRaw(t, defaultLimitServer(resolver), b.String())
	requireGraphQLError(t, status, resp, errCodeMaxAliases)
	assert.Contains(t, resp.Errors[0].Message, `"poem"`)
	// 位置应落在第 6 个别名所在的第 7 行，客户端可直接定位
	assert.Equal(t, 7, resp.Errors[0].Locations[0]["line"])
}

// fanoutLevels 描述一棵多层别名扇出查询树：
// 在每个 branch 层级放 5 个同字段别名（恰好压在别名上限上），
// edges/node 是不扇出的连接层，深度共 8 层（低于 10 的深度上限）。
var fanoutLevels = []struct {
	aliasPrefix string
	field       string
	args        string
	branch      bool
}{
	{"a", "poem", `(id: "1")`, true},
	{"b", "author", "", true},
	{"c", "poems", `(pageSize: 1)`, true},
	{"", "edges", "", false},
	{"", "node", "", false},
	{"d", "author", "", true},
	{"e", "poems", `(pageSize: 1)`, true},
}

// fanoutSelection 递归构造 fanoutLevels 描述的选择集，叶子取 totalCount。
// 展开后的字段总数为 7280（5+25+125+125+125+625+3125+3125）。
func fanoutSelection(level int) string {
	spec := fanoutLevels[level]
	last := level == len(fanoutLevels)-1

	if !spec.branch {
		return fmt.Sprintf("%s { %s }", spec.field, fanoutSelection(level+1))
	}

	parts := make([]string, 0, 5)
	for n := 1; n <= 5; n++ {
		inner := "totalCount"
		if !last {
			inner = fanoutSelection(level + 1)
		}
		parts = append(parts, fmt.Sprintf("%s%d: %s%s { %s }", spec.aliasPrefix, n, spec.field, spec.args, inner))
	}
	return strings.Join(parts, " ")
}

func TestQueryLimitsRejectNodeFanOut(t *testing.T) {
	resolver, _ := setupTestResolver(t)

	// 每个选择集内别名数都恰好是 5（不触发别名门槛）、深度只有 8，
	// 但跨层级把扇出连乘起来后字段总数达到 7280。复杂度上限特意放宽，
	// 以证明是节点总数这道独立门槛拦下了它。
	h := NewServer(resolver, ServerOptions{
		ComplexityLimit: 1_000_000,
		MaxDepth:        10,
		MaxAliases:      5,
		MaxNodeCount:    2000,
	})
	status, resp := postRaw(t, h, "{ "+fanoutSelection(0)+" }")
	requireGraphQLError(t, status, resp, errCodeMaxNodeCount)
	assert.Contains(t, resp.Errors[0].Message, "fields")
}

func TestQueryLimitsSmallNodeLimitIsPrecise(t *testing.T) {
	resolver, _ := setupTestResolver(t)

	// 用一个极小的节点上限验证计数精度：poem/id/title 共 3 个字段，
	// 上限 2 时应在第 3 个字段上报错，且与复杂度无关。
	h := NewServer(resolver, ServerOptions{
		ComplexityLimit: 5000,
		MaxNodeCount:    2,
	})
	status, resp := postRaw(t, h, `{ poem(id: "1") { id title } }`)
	requireGraphQLError(t, status, resp, errCodeMaxNodeCount)
}

func TestQueryLimitsCountFragments(t *testing.T) {
	resolver, _ := setupTestResolver(t)

	t.Run("depth through named fragments", func(t *testing.T) {
		// 展开片段后实际深度 11 层
		const q = `{
  poem(id: "1") { ...P1 }
}
fragment P1 on Poem { author { ...P2 } }
fragment P2 on Author { poems(pageSize: 1) { edges { node { ...P3 } } } }
fragment P3 on Poem { author { poems(pageSize: 1) { edges { node { author { name } } } } } }`
		status, resp := postRaw(t, defaultLimitServer(resolver), q)
		requireGraphQLError(t, status, resp, errCodeMaxDepth)
	})

	t.Run("aliases merged across inline and named fragments", func(t *testing.T) {
		// poem 自身只取了 2 次；但片段与内联片段在同一逻辑选择集里
		// 把 author 重复选取了 6 次，应当算作同一个选择集内的别名超限。
		const q = `{
  a1: poem(id: "1") { ...F }
  a2: poem(id: "1") { ...F }
}
fragment F on Poem {
  x1: author { name }
  x2: author { name }
  x3: author { name }
  ... on Poem {
    x4: author { name }
    x5: author { name }
    x6: author { name }
  }
}`
		status, resp := postRaw(t, defaultLimitServer(resolver), q)
		requireGraphQLError(t, status, resp, errCodeMaxAliases)
		assert.Contains(t, resp.Errors[0].Message, `"author"`)
	})
}

func TestQueryLimitsAllowOrdinaryQueries(t *testing.T) {
	resolver, repo := setupSerializedResolver(t)
	createTestData(t, repo)

	t.Run("typical mobile query with two nesting levels", func(t *testing.T) {
		// 手机端常见形态：作者列表 → 每人诗作 → 诗的作者名，深度 9、字段数百级，
		// 所有门槛都应放行并真实返回数据。
		const q = `{
  authors(page: 1, pageSize: 20) {
    totalCount
    edges { node {
      name
      poems(page: 1, pageSize: 20) {
        totalCount
        edges { node { title author { name } } }
      }
    } }
  }
}`
		status, resp := postRaw(t, defaultLimitServer(resolver), q)
		assert.Equal(t, http.StatusOK, status, "body: %v", resp)
		assert.Empty(t, resp.Errors)
		assert.NotNil(t, resp.Data)
	})

	t.Run("aliases exactly at the limit are allowed", func(t *testing.T) {
		var b strings.Builder
		b.WriteString("{\n")
		for i := 1; i <= 5; i++ {
			fmt.Fprintf(&b, "  a%d: poem(id: \"1\") { title }\n", i)
		}
		b.WriteString("}")

		status, resp := postRaw(t, defaultLimitServer(resolver), b.String())
		assert.Equal(t, http.StatusOK, status, "body: %v", resp)
		assert.Empty(t, resp.Errors)
	})

	t.Run("full page at max page size is allowed", func(t *testing.T) {
		// 单层 pageSize=100、字段选全，字段数约百级，必须放行。
		const q = `{ poems(pageSize: 100) {
		  totalCount
		  edges { node { id title content author { name } dynasty { name } type { name } } }
		} }`
		status, resp := postRaw(t, defaultLimitServer(resolver), q)
		assert.Equal(t, http.StatusOK, status, "body: %v", resp)
		assert.Empty(t, resp.Errors)
	})
}

// TestQueryLimitsExecuteNoSQL 验证门槛在 resolver 之前生效：
// 被拦截的请求一条 SQL 都不应发出，自然不可能占用连接池。
func TestQueryLimitsExecuteNoSQL(t *testing.T) {
	gormDB, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := gormDB.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)

	db := &database.DB{DB: gormDB}
	require.NoError(t, db.Migrate())

	var queries int64
	require.NoError(t, gormDB.Callback().Query().Before("gorm:query").Register("test_count_query", func(*gorm.DB) {
		atomic.AddInt64(&queries, 1)
	}))

	repo := database.NewRepository(db)
	resolver := NewResolver(db, repo)
	h := defaultLimitServer(resolver)

	attacks := []string{
		// 深嵌套
		`{ poem(id:"1") { author { poems(pageSize:1){ edges { node { author { poems(pageSize:1){ edges { node { author { name } } } } } } } } } } }`,
		// 同一字段多别名
		`{ a1:poem(id:"1"){title} a2:poem(id:"2"){title} a3:poem(id:"3"){title} a4:poem(id:"4"){title} a5:poem(id:"5"){title} a6:poem(id:"6"){title} }`,
		// 多层别名连乘扇出（每层别名数压在上限，靠节点总数门槛拦截）
		"{ " + fanoutSelection(0) + " }",
	}

	for i, q := range attacks {
		status, resp := postRaw(t, h, q)
		require.Equal(t, http.StatusUnprocessableEntity, status, "attack %d should be rejected", i+1)
		require.NotEmpty(t, resp.Errors, "attack %d", i+1)
		assert.Zero(t, atomic.LoadInt64(&queries),
			"attack %d (%s) triggered SQL queries", i+1, resp.Errors[0].Extensions["code"])
	}

	// 对照：被放行的普通查询确实会查库，证明计数装置本身有效
	status, resp := postRaw(t, h, `{ poems(pageSize: 5) { totalCount } }`)
	require.Equal(t, http.StatusOK, status, "body: %v", resp)
	assert.Positive(t, atomic.LoadInt64(&queries))
}

// TestQueryLimitsAllowFullIntrospection 验证 GraphiQL/Playground 自带的完整
// 内省查询在默认门槛下可用：它的片段较多、ofType 嵌套较深，但字段数与别名
// 都在正常范围内。
func TestQueryLimitsAllowFullIntrospection(t *testing.T) {
	resolver, _ := setupTestResolver(t)
	const q = `query IntrospectionQuery {
  __schema {
    queryType { name }
    mutationType { name }
    types {
      ...FullType
    }
  }
}
fragment FullType on __Type {
  kind
  name
  fields(includeDeprecated: true) {
    name
    args { ...InputValue }
    type { ...TypeRef }
  }
  inputFields { ...InputValue }
  interfaces { ...TypeRef }
  enumValues(includeDeprecated: true) { name }
  possibleTypes { ...TypeRef }
}
fragment InputValue on __InputValue {
  name
  type { ...TypeRef }
  defaultValue
}
fragment TypeRef on __Type {
  kind
  name
  ofType {
    kind
    name
    ofType {
      kind
      name
      ofType { kind name ofType { kind name } }
    }
  }
}`
	status, resp := postRaw(t, defaultLimitServer(resolver), q)
	assert.Equal(t, http.StatusOK, status, "body: %v", resp)
	assert.Empty(t, resp.Errors)
}

func TestNormalizeLimitsFallsBackToDefaults(t *testing.T) {
	got := normalizeLimits(QueryLimits{})
	assert.Equal(t, defaultMaxDepth, got.MaxDepth)
	assert.Equal(t, defaultMaxAliases, got.MaxAliases)
	assert.Equal(t, defaultMaxNodeCount, got.MaxNodeCount)

	got = normalizeLimits(QueryLimits{MaxDepth: 3, MaxAliases: 2, MaxNodeCount: 9})
	assert.Equal(t, 3, got.MaxDepth)
	assert.Equal(t, 2, got.MaxAliases)
	assert.Equal(t, 9, got.MaxNodeCount)
}
