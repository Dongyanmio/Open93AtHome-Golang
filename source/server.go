package source

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"open93athome-golang/source/Helper"
	"open93athome-golang/source/logger"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zishang520/socket.io/v2/socket"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

var (
	r        *gin.Engine // 将 gin 的路由器也设为全局变量
	clusters []Cluster
)

// corsMiddleware 添加 CORS 头部
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func filterLogs() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		// 获取完整请求的 URL
		fullURL, err := url.QueryUnescape(c.Request.URL.String())
		if err != nil {
			logger.Error("Error decoding URL:", err)
			return
		}

		fullURL = strings.ReplaceAll(fullURL, "%", "%%")

		c.Next()

		latency := time.Since(start)

		clientIP := c.ClientIP()
		method := c.Request.Method
		statusCode := c.Writer.Status()
		userAgent := c.Request.UserAgent()

		// 构造日志输出格式
		logger.Info(fmt.Sprintf(
			"%3d | %13v | %15s | %-7s | %s | %s\n",
			statusCode, // 状态码
			latency,    // 延迟时间
			clientIP,   // 客户端IP
			method,     // 请求方法
			userAgent,  // 用户代理
			fullURL,    // 完整的 URL 包括路径和查询参数
		))
	}
}

func SetupServer(ip string, port string, database *mongo.Client) {
	gin.SetMode(gin.ReleaseMode)

	server := socket.NewServer(nil, nil)

	server.On("connection", func(clients ...any) {
		client := clients[0].(*socket.Socket)

		auth := client.Handshake().Auth

		token, ok := auth.(map[string]interface{})
		if !ok {
			logger.Info("Invalid token type")
			client.Disconnect(true)
		}

		tokenStr, ok := token["token"].(string)
		if !ok || tokenStr == "" {
			logger.Info("Invalid or missing token format")
			client.Disconnect(true)
		}

		// 获取 JWT helper 实例
		jwtHelper, err := Helper.GetInstance()
		if err != nil {
			logger.Error("Error initializing JWT helper")
			client.Disconnect(true)
		}

		if len(strings.Split(tokenStr, ".")) != 3 {
			logger.Error("Malformed JWT token")
			client.Disconnect(true)
		}

		// 验证 token
		claims, err := jwtHelper.VerifyToken(tokenStr, "cluster")
		if err != nil {
			logger.Error("Token verification failed:", err)
			client.Disconnect(true)
		}

		if claims == nil {
			logger.Error("Claims are nil")
			client.Disconnect(true)
			return
		}

		logger.Info("Claims: %v", claims.Claims.(jwt.MapClaims))

		client.On("event", func(datas ...any) {
			logger.Info("%v", datas...)
		})

		client.On("enable", func(datas ...any) {
			logger.Info("%v", datas...)
		})

		client.On("disconnect", func(...any) {
		})
	})

	// 创建路由
	r = gin.New()

	// 添加 CORS 中间件
	r.Use(corsMiddleware())

	// 添加日志过滤中间件
	r.Use(filterLogs())

	// 根路由
	r.GET("/", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"success": "Open93AtHome-Golang",
		})
	})
	openbmclapiAgent := r.Group("/openbmclapi-agent")
	{
		// challenge 路由
		openbmclapiAgent.GET("/challenge", func(c *gin.Context) {
			clusterId := c.Query("clusterId")
			if clusterId == "" {
				c.JSON(http.StatusBadRequest, gin.H{"error": "clusterId is required"})
				return
			}

			// 将 clusterId 转换为 ObjectID
			oid, err := bson.ObjectIDFromHex(clusterId)
			if err != nil {
				logger.Error("Invalid clusterId: %v", err)
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid clusterId"})
				return
			}

			// 从数据库中获取指定的 Cluster
			cluster, err := GetClusterById(database, "93athome", "cluster", oid)
			if err != nil {
				logger.Error("Error getting cluster: %v", err)
				c.JSON(http.StatusNotFound, gin.H{"error": "Cluster not found"})
				return
			}

			if cluster.IsBanned {
				c.JSON(http.StatusForbidden, gin.H{"error": "Cluster is banned"})
				return
			}

			jwtHelper, err := Helper.GetInstance()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Error initializing JWT helper"})
				return
			}

			token, err := jwtHelper.IssueToken(map[string]interface{}{
				"clusterId": clusterId,
			}, "cluster-challenge", 60*5)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Error issuing token"})
				return
			}

			c.JSON(http.StatusOK, gin.H{
				"challenge": token,
			})
		})

		// token 路由
		openbmclapiAgent.POST("/token", func(c *gin.Context) {
			var req struct {
				ClusterId string `json:"clusterId"`
				Signature string `json:"signature"`
				Challenge string `json:"challenge"`
			}
			//我讨厌 form
			contentType := c.Request.Header.Get("Content-Type")
			if strings.HasPrefix(contentType, "application/json") {
				if err := c.ShouldBindJSON(&req); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid JSON request body"})
					return
				}
			} else if strings.HasPrefix(contentType, "application/x-www-form-urlencoded") {
				if err := c.Request.ParseForm(); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse form data"})
					return
				}
				req.ClusterId = c.PostForm("clusterId")
				req.Signature = c.PostForm("signature")
				req.Challenge = c.PostForm("challenge")
			} else if strings.HasPrefix(contentType, "multipart/form-data") {
				if err := c.Request.ParseForm(); err != nil {
					c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse form data"})
					return
				}
				req.ClusterId = c.PostForm("clusterId")
				req.Signature = c.PostForm("signature")
				req.Challenge = c.PostForm("challenge")
			} else {
				c.JSON(http.StatusUnsupportedMediaType, gin.H{"error": "Unsupported Content-Type"})
				return
			}

			jwtHelper, err := Helper.GetInstance()
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Error initializing JWT helper"})
				return
			}

			token, err := jwtHelper.VerifyToken(req.Challenge, "cluster-challenge")
			if err != nil {
				c.JSON(http.StatusForbidden, gin.H{"error": "Invalid challenge token"})
				return
			}

			claims, ok := token.Claims.(jwt.MapClaims)
			if !ok || !token.Valid {
				c.JSON(http.StatusForbidden, gin.H{"error": "Invalid challenge token"})
				return
			}

			clusterIdFromToken, ok := claims["data"].(map[string]interface{})["clusterId"].(string)
			if !ok || clusterIdFromToken != req.ClusterId {
				c.JSON(http.StatusForbidden, gin.H{"error": "Cluster ID mismatch"})
				return
			}

			// 将 clusterId 转换为 ObjectID
			oid, err := bson.ObjectIDFromHex(req.ClusterId)
			if err != nil {
				logger.Error("Invalid clusterId: %v", err)
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid clusterId"})
				return
			}

			// 从数据库中获取指定的 Cluster
			cluster, err := GetClusterById(database, "93athome", "cluster", oid)
			if err != nil {
				logger.Error("Error getting cluster: %v", err)
				c.JSON(http.StatusNotFound, gin.H{"error": "Cluster not found"})
				return
			}

			if !computeSignature(req.Challenge, req.Signature, cluster.ClusterSecret) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Invalid signature"})
				return
			}

			newToken, err := jwtHelper.IssueToken(map[string]interface{}{
				"clusterId": req.ClusterId,
			}, "cluster", 60*60*24)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Error issuing token"})
				return
			}

			c.JSON(http.StatusOK, gin.H{
				"token": newToken,
				"ttl":   1000 * 60 * 60 * 24, // 24小时
			})
		})
	}

	openbmclapi := r.Group("/openbmclapi")
	{
		// configuration 路由
		openbmclapi.GET("/configuration", func(c *gin.Context) {
			if !verifyClusterRequest(c) {
				c.AbortWithStatus(http.StatusForbidden)
				return
			}
			response := gin.H{
				"sync": gin.H{
					"concurrency": 10,
					"source":      "center",
				},
			}
			c.JSON(http.StatusOK, response)
		})

		// files 路由
		openbmclapi.GET("/files", func(c *gin.Context) {
			if !verifyClusterRequest(c) {
				c.JSON(http.StatusForbidden, gin.H{"error": "Unauthorized"})
				return
			}

			filesInfo, err := GetDocuments[FileInfo](database, "93athome", "files", bson.D{})
			if err != nil {
				logger.Error("Error getting files: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get files"})
				return
			}

			// 将 filesInfo 转换为 BMCLAPIObject
			var helperFiles []Helper.BMCLAPIObject
			for _, info := range filesInfo {
				helperFiles = append(helperFiles, Helper.BMCLAPIObject{
					Path:         "/files/" + info.SyncSource + "/" + strings.Replace(info.FileName, "\\", "/", -1),
					Hash:         info.Hash,
					Size:         info.Size,
					LastModified: info.MTime.Time().UnixMilli(),
				})
			}

			avroData, err := GetAvroBytes(helperFiles)
			if err != nil {
				logger.Error("Error generating Avro data: %v", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to generate Avro data"})
				return
			}

			// 在这里处理已授权的请求
			c.Header("Content-Type", "application/octet-stream")
			c.Header("Content-Disposition", "attachment; filename=\"filelist\"")
			c.Data(http.StatusOK, "application/octet-stream", avroData)
		})
	}

	// 下载文件
	r.GET("/files/*filepath", func(c *gin.Context) {
		// 获取整个路径
		fullPath := c.Param("filepath")

		// 拆分路径
		pathSegments := strings.Split(strings.Trim(fullPath, "/"), "/")
		if len(pathSegments) < 2 {
			c.String(http.StatusBadRequest, "Invalid path")
			return
		}

		// 获取 syncSource 和 fileName
		syncSource := pathSegments[0]
		fileName := strings.Join(pathSegments[1:], "/")

		// 从数据库查询文档
		fileRecord, err := GetFileFromDB(database, "93athome", "files", syncSource, fileName)
		if err != nil {
			c.String(http.StatusNotFound, "404 not found")
			return
		}

		// 从数据库的 localDir 字段中获取文件路径
		filePath := fileRecord.LocalDir

		// 打开文件
		file, err := os.Open(filePath)
		if err != nil {
			logger.Error("Error open files: %v", err)
		}
		defer file.Close()

		// 读取文件内容
		fileContent, err := io.ReadAll(file)
		if err != nil {
			c.String(http.StatusInternalServerError, "Error reading file")
			return
		}

		// 获取文件后缀名并根据扩展名设置Content-Type
		ext := filepath.Ext(filePath)
		mimeType := mime.TypeByExtension(ext)
		if mimeType == "" {
			// 如果无法自动识别类型，使用通用的二进制流类型
			mimeType = "application/octet-stream"
		}

		// 设置 Content-Type 并将文件内容作为响应返回
		c.Data(http.StatusOK, mimeType, fileContent)
	})

	// 使用 socketio 处理 WebSocket 请求
	r.GET("/socket.io/*any", gin.WrapH(server.ServeHandler(nil)))
	r.POST("/socket.io/*any", gin.WrapH(server.ServeHandler(nil)))

	logger.Info("Server is running at %s:%s", ip, port)
	// 启动 HTTP 服务器
	if err := r.Run(ip + ":" + port); err != nil {
		logger.Fatal("Server failed to run: %v", err)
	}
}
