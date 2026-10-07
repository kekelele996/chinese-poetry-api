package integration

import (
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/99designs/gqlgen/client"
	"github.com/99designs/gqlgen/graphql/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/palemoky/chinese-poetry-api/internal/database"
	"github.com/palemoky/chinese-poetry-api/internal/graph"
	"github.com/palemoky/chinese-poetry-api/internal/graph/generated"
)

// setupLangTestEnv 基于文件型数据库构建 GraphQL 测试客户端。
//
// 这里用文件而非 ":memory:"：每条连到 ":memory:" 的 SQLite 连接都会得到
// 各自独立的库，一旦 gqlgen 并发解析字段，连接池中新增的连接看到的是
// 未迁移的空库，查询会莫名报 "no such table"。
func setupLangTestEnv(t *testing.T) (*client.Client, *database.Repository) {
	gormDB, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "test.db")), &gorm.Config{})
	require.NoError(t, err)

	db := &database.DB{DB: gormDB}
	require.NoError(t, db.Migrate())

	repo := database.NewRepository(db)
	resolver := graph.NewResolver(db, repo)
	srv := handler.NewDefaultServer(generated.NewExecutableSchema(generated.Config{Resolvers: resolver}))

	return client.New(srv), repo
}

// seedVariant 为每个语言变体写入一首标题各不相同的诗，
// 这样一旦查错了表，结果本身就会暴露问题，而不是看上去似乎也说得通。
func seedVariant(t *testing.T, repo *database.Repository, lang database.Lang, title, author string) {
	langRepo := repo.WithLang(lang)

	dynastyID, err := langRepo.GetOrCreateDynasty("唐")
	require.NoError(t, err)
	authorID, err := langRepo.GetOrCreateAuthor(author, dynastyID)
	require.NoError(t, err)

	require.NoError(t, langRepo.InsertPoem(&database.Poem{
		ID:        1,
		Title:     title,
		Content:   datatypes.JSON([]byte(`["床前明月光"]`)),
		AuthorID:  &authorID,
		DynastyID: &dynastyID,
	}))
}

// TestGraphQLLangSelectsVariant 覆盖 lang 参数——它曾经在整个 GraphQL API 中都不起作用。
// 起因是两个缺陷叠加：gqlgen 的 autobind 把枚举字面量直接转成 database.Lang("ZH_HANT")，
// 该取值与两个变体常量都不相等，导致所有表名辅助函数都落到简体分支；
// 而 poems/poem/authors/author 这几个 resolver 干脆从未调用过 WithLang。
func TestGraphQLLangSelectsVariant(t *testing.T) {
	c, repo := setupLangTestEnv(t)
	seedVariant(t, repo, database.LangHans, "简体标题", "李白")
	seedVariant(t, repo, database.LangHant, "繁體標題", "李白傳統")

	t.Run("poems", func(t *testing.T) {
		for lang, want := range map[string]string{"ZH_HANS": "简体标题", "ZH_HANT": "繁體標題"} {
			var resp struct {
				Poems struct {
					Edges []struct {
						Node struct{ Title string }
					}
				}
			}
			require.NoError(t, c.Post(`query { poems(lang: `+lang+`) { edges { node { title } } } }`, &resp))
			require.Len(t, resp.Poems.Edges, 1)
			assert.Equal(t, want, resp.Poems.Edges[0].Node.Title, "lang: %s read the wrong table", lang)
		}
	})

	t.Run("poem by id", func(t *testing.T) {
		for lang, want := range map[string]string{"ZH_HANS": "简体标题", "ZH_HANT": "繁體標題"} {
			var resp struct {
				Poem struct{ Title string }
			}
			require.NoError(t, c.Post(`query { poem(id: "1", lang: `+lang+`) { title } }`, &resp))
			assert.Equal(t, want, resp.Poem.Title)
		}
	})

	t.Run("authors", func(t *testing.T) {
		for lang, want := range map[string]string{"ZH_HANS": "李白", "ZH_HANT": "李白傳統"} {
			var resp struct {
				Authors struct {
					Edges []struct {
						Node struct{ Name string }
					}
				}
			}
			require.NoError(t, c.Post(`query { authors(lang: `+lang+`) { edges { node { name } } } }`, &resp))
			require.Len(t, resp.Authors.Edges, 1)
			assert.Equal(t, want, resp.Authors.Edges[0].Node.Name)
		}
	})

	// Author.poems 没有自己的 lang 参数，曾经不论外层查的是哪个变体都返回简体
	t.Run("nested author poems follow the outer lang", func(t *testing.T) {
		for lang, want := range map[string]string{"ZH_HANS": "简体标题", "ZH_HANT": "繁體標題"} {
			var resp struct {
				Authors struct {
					Edges []struct {
						Node struct {
							Poems struct {
								Edges []struct {
									Node struct{ Title string }
								}
							}
						}
					}
				}
				Poem struct {
					Author struct {
						Poems struct {
							Edges []struct {
								Node struct{ Title string }
							}
						}
					}
				}
			}
			require.NoError(t, c.Post(`query {
				authors(lang: `+lang+`) { edges { node { poems { edges { node { title } } } } } }
				poem(id: "1", lang: `+lang+`) { author { poems { edges { node { title } } } } }
			}`, &resp))
			require.Len(t, resp.Authors.Edges, 1)
			require.Len(t, resp.Authors.Edges[0].Node.Poems.Edges, 1)
			assert.Equal(t, want, resp.Authors.Edges[0].Node.Poems.Edges[0].Node.Title, "authors(lang: %s)", lang)
			require.Len(t, resp.Poem.Author.Poems.Edges, 1)
			assert.Equal(t, want, resp.Poem.Author.Poems.Edges[0].Node.Title, "poem(lang: %s)", lang)
		}
	})

	t.Run("searchPoems", func(t *testing.T) {
		var resp struct {
			SearchPoems struct{ TotalCount int }
		}
		require.NoError(t, c.Post(`query { searchPoems(query: "繁體", lang: ZH_HANT) { totalCount } }`, &resp))
		assert.Equal(t, 1, resp.SearchPoems.TotalCount)

		require.NoError(t, c.Post(`query { searchPoems(query: "繁體", lang: ZH_HANS) { totalCount } }`, &resp))
		assert.Equal(t, 0, resp.SearchPoems.TotalCount, "simplified table has no traditional title")
	})

	t.Run("an unknown enum literal is rejected", func(t *testing.T) {
		var resp struct{}
		err := c.Post(`query { poems(lang: ZH_HANZ) { totalCount } }`, &resp)
		require.Error(t, err)
	})
}

// seedAuthorWithTypedPoems 在指定变体下准备一位「唐」朝作者，并写入 n 首
// 带体裁（ID 11：五言绝句/五言絕句）的诗。简繁两套表各自独立写入，
// ID 保持一致，便于按同一 ID 对比两个变体的响应。
func seedAuthorWithTypedPoems(t *testing.T, repo *database.Repository, lang database.Lang, name string, n int) int64 {
	langRepo := repo.WithLang(lang)

	dynastyID, err := langRepo.GetOrCreateDynasty("唐")
	require.NoError(t, err)
	authorID, err := langRepo.GetOrCreateAuthor(name, dynastyID)
	require.NoError(t, err)

	for i := range n {
		require.NoError(t, langRepo.InsertPoem(&database.Poem{
			ID:        int64(i + 1),
			Title:     poemTitle(lang, i),
			Content:   datatypes.JSON([]byte(`["床前明月光"]`)),
			AuthorID:  &authorID,
			DynastyID: &dynastyID,
			TypeID:    ptrInt64(11),
		}))
	}
	return authorID
}

func poemTitle(lang database.Lang, i int) string {
	if lang == database.LangHant {
		return "繁體詩第" + strconv.Itoa(i+1) + "首"
	}
	return "简体诗第" + strconv.Itoa(i+1) + "首"
}

func ptrInt64(v int64) *int64 { return &v }

// TestGraphQLStatisticsFollowLang 覆盖 statistics 的语言传播：
// statistics resolver 曾经无视 lang 写死简体，于是繁体页面的统计区里
// 嵌套的朝代名、体裁名与分类（如「唐诗」）全部是简体，与正文混排。
func TestGraphQLStatisticsFollowLang(t *testing.T) {
	c, repo := setupLangTestEnv(t)
	seedAuthorWithTypedPoems(t, repo, database.LangHans, "李白", 1)
	seedAuthorWithTypedPoems(t, repo, database.LangHant, "李白", 1)

	const q = `query($lang: Lang) { statistics(lang: $lang) {
		totalPoems
		poemsByDynasty { dynasty { name } count }
		poemsByType { type { name category } count }
	} }`

	type statsResponse struct {
		Statistics struct {
			TotalPoems int
			// 只取计数非 0 的前几项即可，但全集断言更能暴露读错表
			PoemsByDynasty []struct {
				Dynasty struct{ Name string }
				Count   int
			}
			PoemsByType []struct {
				Type struct {
					Name     string
					Category string
				}
				Count int
			}
		}
	}

	expected := map[string]struct {
		seededDynasty   string // 种子朝代里简繁有别的名字
		poemTypeName    string
		poemTypeCategory string
	}{
		"ZH_HANS": {"两汉", "五言绝句", "唐诗"},
		"ZH_HANT": {"兩漢", "五言絕句", "唐詩"},
	}

	for _, lang := range []string{"ZH_HANS", "ZH_HANT"} {
		var resp statsResponse
		require.NoError(t, c.Post(q, &resp, client.Var("lang", lang)))

		want := expected[lang]
		assert.Equal(t, 1, resp.Statistics.TotalPoems, "lang: %s", lang)

		var seededDynastySeen, poemDynastySeen bool
		var poemDynastyCount int
		for _, d := range resp.Statistics.PoemsByDynasty {
			if d.Dynasty.Name == want.seededDynasty {
				seededDynastySeen = true
			}
			if d.Dynasty.Name == "唐" {
				poemDynastySeen = true
				poemDynastyCount = d.Count
			}
		}
		assert.True(t, seededDynastySeen, "lang %s: seeded dynasty %s missing — read the wrong table", lang, want.seededDynasty)
		assert.True(t, poemDynastySeen, "lang %s: dynasty 唐 missing", lang)
		assert.Equal(t, 1, poemDynastyCount, "lang %s: count on 唐", lang)

		var found bool
		for _, ty := range resp.Statistics.PoemsByType {
			if ty.Type.Name == want.poemTypeName {
				found = true
				assert.Equal(t, want.poemTypeCategory, ty.Type.Category, "lang %s: category on %s", lang, want.poemTypeName)
				assert.Equal(t, 1, ty.Count, "lang %s: count on %s", lang, want.poemTypeName)
			}
			// 另一个变体的写法绝不能出现
			assert.NotEqual(t, expected[otherLang(lang)].poemTypeName, ty.Type.Name,
				"lang %s: response leaked the other variant's type name", lang)
		}
		assert.True(t, found, "lang %s: type %s missing — read the wrong table", lang, want.poemTypeName)
	}

	// 不传 lang 时保持现在的简体行为
	var resp statsResponse
	require.NoError(t, c.Post(`query { statistics {
		totalPoems
		poemsByType { type { name category } count }
	} }`, &resp))
	assert.Equal(t, 1, resp.Statistics.TotalPoems)
	var sawHans bool
	for _, ty := range resp.Statistics.PoemsByType {
		if ty.Type.Name == "五言绝句" {
			sawHans = true
			assert.Equal(t, "唐诗", ty.Type.Category)
		}
		assert.NotEqual(t, "五言絕句", ty.Type.Name, "default statistics must stay zh-Hans")
	}
	assert.True(t, sawHans)
}

func otherLang(lang string) string {
	if lang == "ZH_HANS" {
		return "ZH_HANT"
	}
	return "ZH_HANS"
}

// TestGraphQLNestedCountsFollowLang 覆盖挂在 Author/Dynasty/PoetryType 上的
// 计数字段：它们曾经写死简体表。简繁计数取值相同，因此这里验证每个嵌套层级
// 在两个变体下都给出正确数量；读哪套表由语言贯穿契约保证（并经 -race 并发测试）。
func TestGraphQLNestedCountsFollowLang(t *testing.T) {
	c, repo := setupLangTestEnv(t)
	const poemsPerAuthor = 3
	seedAuthorWithTypedPoems(t, repo, database.LangHans, "李白", poemsPerAuthor)
	seedAuthorWithTypedPoems(t, repo, database.LangHant, "李白", poemsPerAuthor)

	const q = `query($lang: Lang) {
		author(id: "1", lang: $lang) {
			poemCount
			dynasty { name poemCount authorCount }
			poems { totalCount }
		}
		dynasties(lang: $lang) { name poemCount authorCount }
		poemTypes(lang: $lang) { name poemCount }
	}`

	type countsResponse struct {
		Author struct {
			PoemCount int
			Dynasty   struct {
				Name        string
				PoemCount   int
				AuthorCount int
			}
			Poems struct{ TotalCount int }
		}
		Dynasties  []struct {
			Name        string
			PoemCount   int
			AuthorCount int
		}
		PoemTypes []struct {
			Name      string
			PoemCount int
		}
	}

	for _, lang := range []string{"ZH_HANS", "ZH_HANT"} {
		var resp countsResponse
		require.NoError(t, c.Post(q, &resp, client.Var("lang", lang)))

		assert.Equal(t, poemsPerAuthor, resp.Author.PoemCount, "lang %s: Author.poemCount", lang)
		assert.Equal(t, "唐", resp.Author.Dynasty.Name)
		assert.Equal(t, poemsPerAuthor, resp.Author.Dynasty.PoemCount, "lang %s: dynasty.poemCount nested in author", lang)
		assert.Equal(t, 1, resp.Author.Dynasty.AuthorCount, "lang %s: dynasty.authorCount nested in author", lang)
		assert.Equal(t, poemsPerAuthor, resp.Author.Poems.TotalCount, "lang %s: poems.totalCount", lang)

		var dynastyCountOK bool
		for _, d := range resp.Dynasties {
			if d.Name == "唐" {
				dynastyCountOK = true
				assert.Equal(t, poemsPerAuthor, d.PoemCount, "lang %s: dynasties[].poemCount", lang)
				assert.Equal(t, 1, d.AuthorCount, "lang %s: dynasties[].authorCount", lang)
			}
		}
		assert.True(t, dynastyCountOK, "lang %s: dynasty 唐 missing", lang)

		var typeCountOK bool
		for _, ty := range resp.PoemTypes {
			if ty.Name == "五言绝句" || ty.Name == "五言絕句" {
				typeCountOK = true
				assert.Equal(t, poemsPerAuthor, ty.PoemCount, "lang %s: poemTypes[].poemCount on %s", lang, ty.Name)
			}
		}
		assert.True(t, typeCountOK, "lang %s: seeded poetry type missing", lang)
	}
}

// TestGraphQLAuthorDetailConsistentScript 覆盖作者详情整屏：
// 顶层作者与嵌套的作品列表、作品的作者/朝代/体裁、数量、分类必须同属一个字形，
// 翻页前后也不能换。这正是线上「同一屏简繁混排」的场景。
func TestGraphQLAuthorDetailConsistentScript(t *testing.T) {
	c, repo := setupLangTestEnv(t)
	const total = 3
	seedAuthorWithTypedPoems(t, repo, database.LangHans, "李白", total)
	seedAuthorWithTypedPoems(t, repo, database.LangHant, "李白", total)

	type detailResponse struct {
		Author struct {
			Name      string
			PoemCount int
			Dynasty   struct {
				Name        string
				PoemCount   int
				AuthorCount int
			}
			Poems struct {
				TotalCount int
				Edges      []struct {
					Node struct {
						Title   string
						Content []string
						Author  struct {
							Name    string
							Dynasty struct{ Name string }
						}
						Dynasty struct {
							Name      string
							PoemCount int
						}
						Type struct {
							Name      string
							Category  string
							PoemCount int
						}
					}
				}
			}
		}
	}

	fetch := func(lang string, page int) detailResponse {
		var resp detailResponse
		q := `query($lang: Lang) { author(id: "1", lang: $lang) {
			name
			poemCount
			dynasty { name poemCount authorCount }
			poems(page: ` + strconv.Itoa(page) + `, pageSize: 2) {
				totalCount
				edges { node {
					title
					content
					author { name dynasty { name } }
					dynasty { name poemCount }
					type { name category poemCount }
				} }
			}
		} }`
		require.NoError(t, c.Post(q, &resp, client.Var("lang", lang)))
		return resp
	}

	// 繁体第 1 页：整屏只能出现繁体字形
	page1 := fetch("ZH_HANT", 1)
	require.Len(t, page1.Author.Poems.Edges, 2)
	assert.Equal(t, total, page1.Author.PoemCount)
	assert.Equal(t, total, page1.Author.Poems.TotalCount)
	assert.Equal(t, "唐", page1.Author.Dynasty.Name)
	assert.Equal(t, total, page1.Author.Dynasty.PoemCount)
	assert.Equal(t, 1, page1.Author.Dynasty.AuthorCount)
	for i, e := range page1.Author.Poems.Edges {
		assert.Equal(t, poemTitle(database.LangHant, i), e.Node.Title, "traditional page 1, poem %d", i)
		assert.Equal(t, []string{"床前明月光"}, e.Node.Content)
		assert.Equal(t, "李白", e.Node.Author.Name)
		assert.Equal(t, "唐", e.Node.Author.Dynasty.Name)
		assert.Equal(t, "唐", e.Node.Dynasty.Name)
		assert.Equal(t, total, e.Node.Dynasty.PoemCount)
		assert.Equal(t, "五言絕句", e.Node.Type.Name)
		assert.Equal(t, "唐詩", e.Node.Type.Category)
		assert.Equal(t, total, e.Node.Type.PoemCount)
	}

	// 繁体第 2 页（独立的一次 GraphQL 请求）：字形不能换回简体
	page2 := fetch("ZH_HANT", 2)
	require.Len(t, page2.Author.Poems.Edges, 1)
	assert.Equal(t, poemTitle(database.LangHant, 2), page2.Author.Poems.Edges[0].Node.Title,
		"traditional page 2 must stay traditional")

	// 简体第 1 页与默认查询：保持简体行为
	for _, lang := range []string{"ZH_HANS", ""} {
		var resp detailResponse
		if lang == "" {
			require.NoError(t, c.Post(`query { author(id: "1") {
				poems(pageSize: 2) { edges { node { title type { name category } } } }
			} }`, &resp))
		} else {
			resp = fetch(lang, 1)
		}
		require.Len(t, resp.Author.Poems.Edges, 2)
		assert.Equal(t, poemTitle(database.LangHans, 0), resp.Author.Poems.Edges[0].Node.Title)
		assert.Equal(t, "五言绝句", resp.Author.Poems.Edges[0].Node.Type.Name)
		assert.Equal(t, "唐诗", resp.Author.Poems.Edges[0].Node.Type.Category)
	}
}

// TestGraphQLConcurrentLangIsolation 并发发起简、繁体请求：
// 语言选择绑定在各自的字段上下文上，任何响应都不能混入另一个变体的文本。
func TestGraphQLConcurrentLangIsolation(t *testing.T) {
	c, repo := setupLangTestEnv(t)
	const total = 6
	seedAuthorWithTypedPoems(t, repo, database.LangHans, "李白", total)
	seedAuthorWithTypedPoems(t, repo, database.LangHant, "李白", total)

	queries := map[string]string{
		"ZH_HANS": `query { author(id: "1", lang: ZH_HANS) {
			poems { edges { node { title type { name category } } } }
		} }`,
		"ZH_HANT": `query { author(id: "1", lang: ZH_HANT) {
			poems { edges { node { title type { name category } } } }
		} }`,
	}

	type result struct {
		Author struct {
			Poems struct {
				Edges []struct {
					Node struct {
						Title string
						Type  struct{ Name, Category string }
					}
				}
			}
		}
	}

	const goroutines = 32
	errCh := make(chan error, goroutines*4)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			lang := []string{"ZH_HANS", "ZH_HANT"}[g%2]
			for range 4 {
				var resp result
				if err := c.Post(queries[lang], &resp); err != nil {
					errCh <- err
					return
				}
				if len(resp.Author.Poems.Edges) != total {
					errCh <- fmt.Errorf("lang %s: got %d edges, want %d", lang, len(resp.Author.Poems.Edges), total)
					return
				}
				for i, e := range resp.Author.Poems.Edges {
					if e.Node.Title != poemTitle(database.Lang(langConst(lang)), i) {
						errCh <- fmt.Errorf("lang %s: title %q leaked from another variant", lang, e.Node.Title)
						return
					}
					wantType, wantCategory := "五言绝句", "唐诗"
					if lang == "ZH_HANT" {
						wantType, wantCategory = "五言絕句", "唐詩"
					}
					if e.Node.Type.Name != wantType || e.Node.Type.Category != wantCategory {
						errCh <- fmt.Errorf("lang %s: type %s/%s leaked from another variant", lang, e.Node.Type.Name, e.Node.Type.Category)
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func langConst(lang string) database.Lang {
	if lang == "ZH_HANT" {
		return database.LangHant
	}
	return database.LangHans
}

// TestSearchPoemsCursors 覆盖 searchPoems 的 connection——它曾是手写构造的：
// 游标在每页内都从 0 开始编号，于是第 2 页首条边的游标与第 1 页的相同；
// 而且没有填 startCursor 与 endCursor，与其他所有 connection 的行为不一致。
func TestSearchPoemsCursors(t *testing.T) {
	c, repo := setupLangTestEnv(t)

	dynastyID, err := repo.GetOrCreateDynasty("唐")
	require.NoError(t, err)
	authorID, err := repo.GetOrCreateAuthor("李白", dynastyID)
	require.NoError(t, err)
	for i := range 4 {
		require.NoError(t, repo.InsertPoem(&database.Poem{
			ID:        int64(i + 1),
			Title:     "春日" + string(rune('A'+i)),
			Content:   datatypes.JSON([]byte(`["春风"]`)),
			AuthorID:  &authorID,
			DynastyID: &dynastyID,
		}))
	}

	page := func(n int) (cursors []string, start, end *string) {
		var resp struct {
			SearchPoems struct {
				Edges []struct {
					Cursor string
					Node   struct{ Title string }
				}
				PageInfo struct {
					StartCursor *string
					EndCursor   *string
				}
			}
		}
		q := `query { searchPoems(query: "春日", page: ` + strconv.Itoa(n) + `, pageSize: 2) {
			edges { cursor node { title } }
			pageInfo { startCursor endCursor }
		} }`
		require.NoError(t, c.Post(q, &resp))

		for _, e := range resp.SearchPoems.Edges {
			cursors = append(cursors, e.Cursor)
		}
		return cursors, resp.SearchPoems.PageInfo.StartCursor, resp.SearchPoems.PageInfo.EndCursor
	}

	first, start1, end1 := page(1)
	second, start2, _ := page(2)

	// 游标对外不透明，但必须能解回它所标识的那一条诗词：
	// 诗词按 id 升序排列，第 1 页是 id 1、2，第 2 页是 id 3、4。
	assert.Equal(t, []string{
		database.EncodePoemCursor(1),
		database.EncodePoemCursor(2),
	}, first)
	assert.Equal(t, []string{
		database.EncodePoemCursor(3),
		database.EncodePoemCursor(4),
	}, second, "page 2 cursors must continue from page 1, not restart at the first poem")

	require.NotNil(t, start1)
	require.NotNil(t, end1)
	require.NotNil(t, start2)
	assert.Equal(t, database.EncodePoemCursor(1), *start1)
	assert.Equal(t, database.EncodePoemCursor(2), *end1)
	assert.Equal(t, database.EncodePoemCursor(3), *start2)
}

// TestPoemsCursorPagination 覆盖 poems 的游标翻页：
// 它不受 page 分页的深度上限约束，因此必须能只靠 endCursor 一路走到底。
func TestPoemsCursorPagination(t *testing.T) {
	c, repo := setupLangTestEnv(t)

	dynastyID, err := repo.GetOrCreateDynasty("唐")
	require.NoError(t, err)
	authorID, err := repo.GetOrCreateAuthor("李白", dynastyID)
	require.NoError(t, err)

	const total = 5
	for i := range total {
		require.NoError(t, repo.InsertPoem(&database.Poem{
			ID:        int64(i + 1),
			Title:     "诗" + strconv.Itoa(i),
			Content:   datatypes.JSON([]byte(`["内容"]`)),
			AuthorID:  &authorID,
			DynastyID: &dynastyID,
		}))
	}

	type poemsResponse struct {
		Poems struct {
			Edges []struct {
				Node struct{ Title string }
			}
			PageInfo struct {
				HasNextPage bool
				EndCursor   *string
			}
			TotalCount int
		}
	}

	// 不带 after 的首次查询也要给出 endCursor，否则没有入口切换到游标翻页
	var resp poemsResponse
	require.NoError(t, c.Post(`query { poems(pageSize: 2) {
		edges { node { title } }
		pageInfo { hasNextPage endCursor }
		totalCount
	} }`, &resp))
	require.True(t, resp.Poems.PageInfo.HasNextPage)
	require.NotNil(t, resp.Poems.PageInfo.EndCursor)

	var visited []string
	for {
		for _, e := range resp.Poems.Edges {
			visited = append(visited, e.Node.Title)
		}
		assert.Equal(t, total, resp.Poems.TotalCount,
			"totalCount is the size of the whole result set, not the rows left after the cursor")

		if !resp.Poems.PageInfo.HasNextPage {
			break
		}
		require.NotNil(t, resp.Poems.PageInfo.EndCursor)

		q := `query { poems(pageSize: 2, after: "` + *resp.Poems.PageInfo.EndCursor + `") {
			edges { node { title } }
			pageInfo { hasNextPage endCursor }
			totalCount
		} }`
		resp = poemsResponse{}
		require.NoError(t, c.Post(q, &resp))
	}

	assert.Equal(t, []string{"诗0", "诗1", "诗2", "诗3", "诗4"}, visited,
		"cursor pagination must visit every poem exactly once, in id order")
}

// TestPoemsCursorRejectsBadInput 覆盖游标参数的错误输入。
func TestPoemsCursorRejectsBadInput(t *testing.T) {
	c, _ := setupLangTestEnv(t)

	t.Run("a cursor this API did not issue is rejected", func(t *testing.T) {
		var resp struct{}
		// 客户端最容易犯的错：把游标当成可以自己构造的行号
		err := c.Post(`query { poems(after: "3") { totalCount } }`, &resp)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cursor")
	})

	t.Run("page and after cannot be combined", func(t *testing.T) {
		var resp struct{}
		q := `query { poems(page: 2, after: "` + database.EncodePoemCursor(1) + `") { totalCount } }`
		err := c.Post(q, &resp)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})
}

// TestAuthorListingTieBreak 覆盖 poem_count 相同的作者的分页场景。
// 仅按 poem_count 排序并非全序，因此某个 LIMIT/OFFSET 窗口
// 究竟返回并列行中的哪几条是不确定的。
func TestAuthorListingTieBreak(t *testing.T) {
	c, repo := setupLangTestEnv(t)

	dynastyID, err := repo.GetOrCreateDynasty("唐")
	require.NoError(t, err)

	// 每位作者都恰好一首诗，于是所有人的 poem_count 全部并列
	const authorCount = 12
	for i := range authorCount {
		name := "作者" + strconv.Itoa(i)
		authorID, err := repo.GetOrCreateAuthor(name, dynastyID)
		require.NoError(t, err)
		require.NoError(t, repo.InsertPoem(&database.Poem{
			ID:        int64(i + 1),
			Title:     "诗" + strconv.Itoa(i),
			Content:   datatypes.JSON([]byte(`["内容"]`)),
			AuthorID:  &authorID,
			DynastyID: &dynastyID,
		}))
	}

	seen := map[string]int{}
	for page := 1; page <= authorCount/3; page++ {
		var resp struct {
			Authors struct {
				Edges []struct {
					Node struct{ Name string }
				}
			}
		}
		q := `query { authors(page: ` + strconv.Itoa(page) + `, pageSize: 3) { edges { node { name } } } }`
		require.NoError(t, c.Post(q, &resp))
		for _, e := range resp.Authors.Edges {
			seen[e.Node.Name]++
		}
	}

	require.Len(t, seen, authorCount, "paging must visit every author exactly once")
	for name, n := range seen {
		assert.Equal(t, 1, n, "%s appeared on more than one page", name)
	}
}

// TestGraphQLCountFields 覆盖 poemCount/authorCount 字段
func TestGraphQLCountFields(t *testing.T) {
	c, repo := setupLangTestEnv(t)
	seedVariant(t, repo, database.LangHans, "简体标题", "李白")

	t.Run("dynasty counts", func(t *testing.T) {
		var resp struct {
			Dynasties []struct {
				Name        string
				PoemCount   int
				AuthorCount int
			}
		}
		require.NoError(t, c.Post(`query { dynasties { name poemCount authorCount } }`, &resp))

		var found bool
		for _, d := range resp.Dynasties {
			if d.Name == "唐" {
				found = true
				assert.Equal(t, 1, d.PoemCount)
				assert.Equal(t, 1, d.AuthorCount)
			}
		}
		assert.True(t, found, "seeded dynasty missing from result")
	})

	t.Run("author poem count", func(t *testing.T) {
		var resp struct {
			Authors struct {
				Edges []struct {
					Node struct {
						Name      string
						PoemCount int
					}
				}
			}
		}
		require.NoError(t, c.Post(`query { authors { edges { node { name poemCount } } } }`, &resp))
		require.Len(t, resp.Authors.Edges, 1)
		assert.Equal(t, 1, resp.Authors.Edges[0].Node.PoemCount)
	})

	t.Run("poetry type poem count", func(t *testing.T) {
		var resp struct {
			PoemTypes []struct {
				Name      string
				PoemCount int
			}
		}
		require.NoError(t, c.Post(`query { poemTypes { name poemCount } }`, &resp))
		assert.NotEmpty(t, resp.PoemTypes, "poetry types are seeded by the schema")
	})
}
