package api //nolint:revive

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bluenviron/mediamtx/internal/defs"
	"github.com/bluenviron/mediamtx/internal/servers/rtp"
	"github.com/gin-gonic/gin"
)

func (a *API) onOpenRTPServer(ctx *gin.Context) {
	var req struct {
		Port       int    `json:"port"`       // 可选，0 表示自动选择
		StreamPath string `json:"streamPath"` // 必填
		SDP        string `json:"sdp"`        // 必填
		Timeout    int    `json:"timeout"`    // 可选，超时时间（秒），默认 30
	}

	err := ctx.ShouldBindJSON(&req)
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}

	if req.StreamPath == "" {
		a.writeError(ctx, http.StatusBadRequest, errors.New("streamPath is required"))
		return
	}

	if req.SDP == "" {
		a.writeError(ctx, http.StatusBadRequest, errors.New("sdp is required"))
		return
	}

	// Remove leading and trailing slashes to match MediaMTX path naming rules
	streamPath := strings.Trim(req.StreamPath, "/")
	if streamPath == "" {
		a.writeError(ctx, http.StatusBadRequest, errors.New("streamPath cannot be empty after removing slashes"))
		return
	}

	timeout := 30 * time.Second
	if req.Timeout > 0 {
		timeout = time.Duration(req.Timeout) * time.Second
	}

	port, err := a.RTPServer.OpenServer(req.Port, streamPath, req.SDP, timeout)
	if err != nil {
		a.writeError(ctx, http.StatusInternalServerError, err)
		return
	}

	ctx.JSON(http.StatusOK, gin.H{
		"status": "ok",
		"port":   port,
	})
}

func (a *API) onCloseRTPServer(ctx *gin.Context) {
	portStr := ctx.Param("port")
	port, err := strconv.Atoi(portStr)
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, errors.New("invalid port"))
		return
	}

	err = a.RTPServer.CloseServer(port)
	if err != nil {
		if errors.Is(err, rtp.ErrServerNotFound) {
			a.writeError(ctx, http.StatusNotFound, err)
		} else {
			a.writeError(ctx, http.StatusInternalServerError, err)
		}
		return
	}

	a.writeOK(ctx)
}

func (a *API) onListRTPServers(ctx *gin.Context) {
	servers, err := a.RTPServer.ListServers()
	if err != nil {
		a.writeError(ctx, http.StatusInternalServerError, err)
		return
	}

	data := &struct {
		ItemCount int                      `json:"itemCount"`
		PageCount int                      `json:"pageCount"`
		Items     []*defs.APIRTPServerInfo `json:"items"`
	}{
		Items: servers,
	}

	data.ItemCount = len(data.Items)
	pageCount, err := paginate(&data.Items, ctx.Query("itemsPerPage"), ctx.Query("page"))
	if err != nil {
		a.writeError(ctx, http.StatusBadRequest, err)
		return
	}
	data.PageCount = pageCount

	ctx.JSON(http.StatusOK, data)
}
