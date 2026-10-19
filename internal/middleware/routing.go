// 对应【阶段四：多模型路由】
//
// 学习点回顾：body 是个 io.Reader，只能读一次。这里读出来解析 model
// 字段之后，必须把 body "塞回去"，不然后面 ReverseProxy 转发的时候
// body 已经是空的了，下游会收到一个没有内容的请求。
package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"gateway/internal/router"

	"github.com/gin-gonic/gin"
)

func RoutingMiddleware(rt *router.Router, maxBodyBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 学习点：用 http.MaxBytesReader 限制读取上限，防止恶意请求发一个
		// 超大 body 把网关内存撑爆。这是"读取整个 body 到内存"这种做法
		// 必须配套的防御措施。
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{
				"error": "request body too large or unreadable",
			})
			return
		}
		// 关键：读完立刻用一个新的 Reader 把 body 内容还原回去，
		// 后面的 ReverseProxy 还要再读一次完整的 body 转发给下游。
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		var probe struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "invalid json body",
			})
			return
		}
		if probe.Model == "" {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "missing 'model' field in request body",
			})
			return
		}

		route, ok := rt.Resolve(probe.Model)
		if !ok {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{
				"error": "unknown model: " + probe.Model,
			})
			return
		}

		c.Set(BackendContextKey, route.Backend)
		c.Next()
	}
}
