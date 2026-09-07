package server

// handler_proxy.go —— 产物下载代理：把 chatgpt.com 的鉴权直链（图片 / Code Interpreter 沙箱文件）
// 通过服务端带 token 代理给客户端，避免前端直连内部地址被 403。
//
// 挂了产物存储（ArtifactStore）之后，这两个路由是升级前旧链接的兼容别名：
//  1. 映射里有 → 走存储（缓存直出 / 按映射回源），不再依赖内存会话；
//  2. 映射里没有但会话还活着 → 用会话下载，顺手登记映射 + 写缓存（回填），下次就不用会话了；
//  3. 都没有 → 404。

import (
	"net/http"
	"path"

	"github.com/gin-gonic/gin"
)

const proxyDefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

// HandleImageProxy 处理图片流式代理请求
func (h *ChatHandler) HandleImageProxy(c *gin.Context) {
	convID := c.Query("conv_id")
	fileID := c.Query("file_id")
	if convID == "" || fileID == "" {
		c.String(http.StatusBadRequest, "Missing conv_id or file_id")
		return
	}

	artifactID := ChatGPTImageArtifactID(fileID)
	if h.store != nil {
		if rec, ok := h.store.Lookup(artifactID); ok {
			h.store.Serve(c, rec.ID+"."+rec.Ext)
			return
		}
	}

	entry, ok := h.session.GetSession(convID)
	if !ok {
		c.String(http.StatusNotFound, "Session not found or expired")
		return
	}

	if h.store != nil {
		// 回填：这次还得靠会话下载，但从此以后都走存储。
		data, mimeType, err := entry.client.DownloadFileByFileID(convID, fileID)
		if err == nil && len(data) > 0 {
			ext := ArtifactExtForMime(mimeType)
			if ext == "" {
				ext = "png"
			}
			rec := ArtifactRecord{
				ID: artifactID, Ext: ext, Mime: mimeType, Kind: ArtifactKindImage,
				Provider: chatGPTArtifactProvider, AuthID: entry.authID,
				Locator: ArtifactLocator{ConvID: convID, FileID: fileID},
			}
			if h.store.Record(rec) == nil && h.store.PutBytes(artifactID, data, mimeType) == nil {
				h.store.Serve(c, artifactID+"."+ext)
				return
			}
			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
			c.Header("Cache-Control", "public, max-age=31536000")
			c.Data(http.StatusOK, mimeType, data)
			return
		}
	}

	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = proxyDefaultUserAgent
	}
	if err := entry.client.ProxyImageByFileID(fileID, convID, c.Writer, userAgent); err != nil {
		c.String(http.StatusInternalServerError, "Proxy image failed: %v", err)
	}
}

// HandlePDFProxy 代理下载 Code Interpreter 生成的 PDF（及其它沙箱文件）
func (h *ChatHandler) HandlePDFProxy(c *gin.Context) {
	convID := c.Query("conv_id")
	msgID := c.Query("msg_id")
	sandboxPath := c.Query("sandbox_path")
	if convID == "" || msgID == "" || sandboxPath == "" {
		c.String(http.StatusBadRequest, "Missing conv_id, msg_id or sandbox_path")
		return
	}

	artifactID := SandboxArtifactID(convID, msgID, sandboxPath)
	if h.store != nil {
		if rec, ok := h.store.Lookup(artifactID); ok {
			h.store.Serve(c, rec.ID+"."+rec.Ext)
			return
		}
	}

	entry, ok := h.session.GetSession(convID)
	if !ok {
		c.String(http.StatusNotFound, "Session not found or expired")
		return
	}

	if h.store != nil {
		data, mimeType, err := entry.client.DownloadSandboxFile(convID, msgID, sandboxPath)
		if err == nil && len(data) > 0 {
			name := path.Base(sandboxPath)
			ext := ArtifactExtForName(name)
			if ext == "" {
				ext = ArtifactExtForMime(mimeType)
			}
			if ext == "" {
				ext = "bin"
			}
			rec := ArtifactRecord{
				ID: artifactID, Ext: ext, Mime: mimeType, Name: name, Kind: ArtifactKindFile,
				Provider: chatGPTArtifactProvider, AuthID: entry.authID,
				Locator: ArtifactLocator{ConvID: convID, MessageID: msgID, SandboxPath: sandboxPath},
			}
			if h.store.Record(rec) == nil && h.store.PutBytes(artifactID, data, mimeType) == nil {
				h.store.Serve(c, artifactID+"."+ext)
				return
			}
			if mimeType == "" {
				mimeType = "application/octet-stream"
			}
			c.Data(http.StatusOK, mimeType, data)
			return
		}
	}

	userAgent := c.GetHeader("User-Agent")
	if userAgent == "" {
		userAgent = proxyDefaultUserAgent
	}
	if err := entry.client.ProxyPDFBySandboxPath(convID, msgID, sandboxPath, c.Writer, userAgent); err != nil {
		c.String(http.StatusInternalServerError, "Proxy PDF failed: %v", err)
	}
}
