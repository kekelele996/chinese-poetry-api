package graph

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func decodeJSON(t *testing.T, raw []byte, v any) {
	t.Helper()
	require.NoError(t, json.Unmarshal(raw, v), "raw response: %s", raw)
}

func TestMaxBytesHandlerRejectsLargeContentLength(t *testing.T) {
	resolver, _ := setupTestResolver(t)
	h := MaxBytesHandler(NewServer(resolver, ServerOptions{ComplexityLimit: 5000}), 64)

	// Content-Length 已知超限：不应读取请求体，直接 413 + GraphQL 错误结构
	body := strings.Repeat("x", 128)
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
	assert.Contains(t, rec.Header().Get("Content-Type"), "application/json")

	var resp graphqlResponse
	decodeJSON(t, rec.Body.Bytes(), &resp)
	require.Len(t, resp.Errors, 1)
	assert.Equal(t, errCodeRequestBodyTooLarge, resp.Errors[0].Extensions["code"])
	assert.Contains(t, resp.Errors[0].Message, "64")
}

func TestMaxBytesHandlerAllowsSmallBody(t *testing.T) {
	resolver, _ := setupTestResolver(t)
	h := MaxBytesHandler(NewServer(resolver, ServerOptions{ComplexityLimit: 5000}), 4096)

	// __typename 不需要内省权限，任何情况下都能成功，用来验证请求确实透传到了 gqlgen
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestMaxBytesHandlerStopsOversizedChunkedBody(t *testing.T) {
	resolver, _ := setupTestResolver(t)
	h := MaxBytesHandler(NewServer(resolver, ServerOptions{ComplexityLimit: 5000}), 64)

	// ContentLength=-1 模拟分块传输：头里看不到大小，靠 MaxBytesReader 在读到上限时中断。
	body := strings.NewReader(strings.Repeat("x", 4096))
	req := httptest.NewRequest(http.MethodPost, "/graphql", body)
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// gqlgen 把读取失败包成 errors 返回；关键信息是 body too large，
	// 且服务内存占用在读取 64 字节后即被切断。
	var resp graphqlResponse
	decodeJSON(t, rec.Body.Bytes(), &resp)
	require.NotEmpty(t, resp.Errors)
	assert.Contains(t, resp.Errors[0].Message, "too large")
}

func TestMaxBytesHandlerZeroLimitUsesDefault(t *testing.T) {
	resolver, _ := setupTestResolver(t)
	h := MaxBytesHandler(NewServer(resolver, ServerOptions{ComplexityLimit: 5000}), 0)

	// 正常小查询应在默认上限下放行
	req := httptest.NewRequest(http.MethodPost, "/graphql", strings.NewReader(`{"query":"{ __typename }"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusOK, rec.Code)
}
