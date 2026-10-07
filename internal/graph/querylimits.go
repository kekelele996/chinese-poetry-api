package graph

import (
	"context"

	"github.com/99designs/gqlgen/graphql"
	"github.com/99designs/gqlgen/graphql/errcode"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/gqlerror"
)

// 结构性查询门槛的默认值。NewServer 对未配置（<=0）的取值统一回落到这些默认，
// 避免调用方漏配时要么完全没有防护、要么把所有请求拒掉。
const (
	defaultMaxDepth     = 10
	defaultMaxAliases   = 5
	defaultMaxNodeCount = 2000
)

// 被拦截错误的 extensions.code。注册成 KindProtocol 后，
// gqlgen 会对这类错误返回 HTTP 422，而不是当作普通 resolver 错误返回 200。
const (
	errCodeMaxDepth     = "GRAPHQL_MAX_DEPTH_EXCEEDED"
	errCodeMaxAliases   = "GRAPHQL_MAX_ALIASES_EXCEEDED"
	errCodeMaxNodeCount = "GRAPHQL_MAX_NODE_COUNT_EXCEEDED"
)

func init() {
	errcode.RegisterErrorType(errCodeMaxDepth, errcode.KindProtocol)
	errcode.RegisterErrorType(errCodeMaxAliases, errcode.KindProtocol)
	errcode.RegisterErrorType(errCodeMaxNodeCount, errcode.KindProtocol)
}

// QueryLimits 是在 resolver 执行之前生效的结构性查询门槛。
//
// 复杂度上限（FixedComplexityLimit）只能按「代价」估算，而每个字段在本服务里
// 基本对应一次独立的数据库查询（没有 DataLoader 做批合并）：
//   - 同一字段换一批别名重复选取时，每次代价只有 1~2，几千个别名也能低于复杂度上限，
//     却会放大成几千次查询；
//   - 层层嵌套即便被分页加权的复杂度挡住，也缺少一个能直接告诉客户端「层数超限」
//     的、与具体字段位置绑定的错误。
//
// 这里补上深度、同字段别名数与字段总数三道硬门槛。它们作为
// OperationContextMutator 运行在文档解析与校验之后、任何 resolver 执行之前，
// 被拦下的请求一次数据库连接都不会占用。
type QueryLimits struct {
	// MaxDepth 是 fragment 展开后允许的最大字段嵌套层数，根查询字段算第 1 层。
	MaxDepth int
	// MaxAliases 是同一个逻辑选择集内，同一字段允许被重复选取（含别名）的次数。
	MaxAliases int
	// MaxNodeCount 是一次操作在 fragment 展开后允许选取的字段总数。
	MaxNodeCount int
}

var (
	_ graphql.HandlerExtension        = QueryLimits{}
	_ graphql.OperationContextMutator = QueryLimits{}
)

func (q QueryLimits) ExtensionName() string { return "QueryLimits" }

func (q QueryLimits) Validate(graphql.ExecutableSchema) error { return nil }

func (q QueryLimits) MutateOperationContext(_ context.Context, opCtx *graphql.OperationContext) *gqlerror.Error {
	op := opCtx.Doc.Operations.ForName(opCtx.OperationName)
	if op == nil {
		return nil
	}
	return analyzeQuery(opCtx.Doc, op, q)
}

// limitFrame 是 AST 遍历时的显式栈帧。显式栈而不是递归：
// 能通过语法校验的文档仍可能嵌套得非常深，递归遍历会把同样的深度再压一遍调用栈。
type limitFrame struct {
	selections ast.SelectionSet
	depth      int
	// aliases 统计当前逻辑选择集内每个字段被选取的次数。
	// fragment 展开与内联片段不产生新的选择集，因此与父帧共用同一个 map；
	// 进入字段自身的子选择集时才换新 map。
	aliases map[string]int
	// visited 记录到达该帧的展开路径上已经出现过的 fragment，
	// 用来在 fragment 成环时中断展开（正常情况下校验规则已经先拒绝成环文档）。
	visited []string
}

// analyzeQuery 展开 fragment 统计深度、别名数与字段总数，超限时返回带源码位置的错误。
func analyzeQuery(doc *ast.QueryDocument, op *ast.OperationDefinition, limits QueryLimits) *gqlerror.Error {
	fragments := make(map[string]*ast.FragmentDefinition, len(doc.Fragments))
	for _, frag := range doc.Fragments {
		fragments[frag.Name] = frag
	}

	nodes := 0
	stack := []limitFrame{{
		selections: op.SelectionSet,
		depth:      1,
		aliases:    make(map[string]int),
	}}

	for len(stack) > 0 {
		// 取栈顶；逆序压入子帧可以保持按文档顺序从左到右的遍历，
		// 让报错位置稳定落在最靠前的违规字段上。
		last := len(stack) - 1
		frame := stack[last]
		stack = stack[:last]

		var nested []limitFrame
		for _, sel := range frame.selections {
			switch s := sel.(type) {
			case *ast.Field:
				nodes++
				if nodes > limits.MaxNodeCount {
					return locatedLimitError(
						errCodeMaxNodeCount, s.Position,
						"query selects %d fields, which exceeds the node count limit of %d; "+
							"request fewer fields per operation or split it into smaller queries",
						nodes, limits.MaxNodeCount,
					)
				}

				if frame.depth > limits.MaxDepth {
					return locatedLimitError(
						errCodeMaxDepth, s.Position,
						"query nesting depth is %d, which exceeds the depth limit of %d; "+
							"flatten the selection or nest fewer levels",
						frame.depth, limits.MaxDepth,
					)
				}

				count := frame.aliases[s.Name] + 1
				frame.aliases[s.Name] = count
				if count > limits.MaxAliases {
					return locatedLimitError(
						errCodeMaxAliases, s.Position,
						"field %q is selected %d times (via aliases) in one selection set, "+
							"which exceeds the alias limit of %d; select it once or use a list field",
						s.Name, count, limits.MaxAliases,
					)
				}

				if len(s.SelectionSet) > 0 {
					nested = append(nested, limitFrame{
						selections: s.SelectionSet,
						depth:      frame.depth + 1,
						aliases:    make(map[string]int),
						visited:    frame.visited,
					})
				}

			case *ast.InlineFragment:
				// 内联片段作用于同一个对象，不增加嵌套层数，别名计数并入当前选择集。
				nested = append(nested, limitFrame{
					selections: s.SelectionSet,
					depth:      frame.depth,
					aliases:    frame.aliases,
					visited:    frame.visited,
				})

			case *ast.FragmentSpread:
				// 展开路径上已有同名 fragment：成环或菱形重复展开，跳过即可。
				if contains(frame.visited, s.Name) {
					continue
				}
				frag := fragments[s.Name]
				if frag == nil {
					// 未定义 fragment 属于校验错误，正常情况下到不了这里。
					continue
				}
				visited := append(append(make([]string, 0, len(frame.visited)+1), frame.visited...), s.Name)
				nested = append(nested, limitFrame{
					selections: frag.SelectionSet,
					depth:      frame.depth,
					aliases:    frame.aliases,
					visited:    visited,
				})
			}
		}

		// 逆序压栈：保证文档中最靠左的子选择集最先被处理。
		for i := len(nested) - 1; i >= 0; i-- {
			stack = append(stack, nested[i])
		}
	}

	return nil
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// locatedLimitError 构造带行列位置的 GraphQL 错误，客户端可直接定位到违规字段。
func locatedLimitError(code string, pos *ast.Position, format string, args ...any) *gqlerror.Error {
	var err *gqlerror.Error
	if pos != nil {
		err = gqlerror.ErrorPosf(pos, format, args...)
	} else {
		err = gqlerror.Errorf(format, args...)
	}
	errcode.Set(err, code)
	return err
}

// normalizeLimits 把未配置的取值回落到默认门槛。
func normalizeLimits(l QueryLimits) QueryLimits {
	if l.MaxDepth <= 0 {
		l.MaxDepth = defaultMaxDepth
	}
	if l.MaxAliases <= 0 {
		l.MaxAliases = defaultMaxAliases
	}
	if l.MaxNodeCount <= 0 {
		l.MaxNodeCount = defaultMaxNodeCount
	}
	return l
}
