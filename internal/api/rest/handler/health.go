package handler

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/palemoky/chinese-poetry-api/internal/database"
)

// StatsHandler 返回全库的整体统计数据。
//
// 本接口不接受任何查询参数，尤其是 lang：GetStatistics 统计的行数在简繁两套表中一致。
// 探针接口（/livez、/readyz、/api/v1/health）则有意保持宽松，因为探针常会附加
// 防缓存参数，若一并拒绝，会让本来健康的服务在健康检查中被判为异常。
func StatsHandler(repo *database.Repository) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !checkQueryParams(c) {
			return
		}

		stats, err := repo.GetStatistics()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{
				"error": "failed to get statistics",
			})
			return
		}

		c.JSON(http.StatusOK, stats)
	}
}
