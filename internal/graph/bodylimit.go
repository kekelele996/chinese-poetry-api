package graph

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// defaultMaxBodyBytes 是 GraphQL 请求体的默认大小上限（1 MiB）。
// 正常查询只有几 KB，1 MiB 对上百个字段的大查询也留了几十倍余量。
const defaultMaxBodyBytes int64 = 1 << 20

// errCodeRequestBodyTooLarge 是请求体超限时返回的 GraphQL 错误码。
const errCodeRequestBodyTooLarge = "GRAPHQL_REQUEST_TOO_LARGE"

// MaxBytesHandler 把 GraphQL 请求体限制在 limit 字节以内。
//
// gqlgen 的 POST transport 用 io.ReadAll 读整个请求体且没有任何大小限制，
// 一个超大的请求体在进入解析前就能吃掉大量内存。这里分两步兜底：
//   - Content-Length 已知且超限时，不读取请求体，直接返回 413；
//   - 其余情况（分块传输或 Content-Length 不实）套上 http.MaxBytesReader，
//     读到上限即中断，实际占用内存不会超过 limit。
//
// 返回的错误仍是标准 GraphQL 错误结构（errors + code），客户端可以统一处理。
func MaxBytesHandler(next http.Handler, limit int64) http.Handler {
	if limit <= 0 {
		limit = defaultMaxBodyBytes
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength > limit {
			writeRequestBodyTooLarge(w, limit)
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

func writeRequestBodyTooLarge(w http.ResponseWriter, limit int64) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusRequestEntityTooLarge)

	// 与 gqlgen 普通错误保持同样的响应外形：{"errors":[{...}]}
	payload := map[string]any{
		"errors": []map[string]any{
			{
				"message": "request body exceeds the GraphQL size limit of " +
					strconv.FormatInt(limit, 10) + " bytes; send a smaller query or use persisted queries",
				"extensions": map[string]any{"code": errCodeRequestBodyTooLarge},
			},
		},
	}
	_ = json.NewEncoder(w).Encode(payload)
}
