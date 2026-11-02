package projectpkg

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/li904338203/tabtv/backend/internal/transport/errcode"
	"github.com/li904338203/tabtv/backend/internal/transport/httpx"
)

// Handler 挂载导出与导入两个端点。
type Handler struct {
	svc   *Service
	owner func(*http.Request) Owner
}

// NewHandler 创建 Handler。owner 给出当前请求的本机主体。
func NewHandler(svc *Service, owner func(*http.Request) Owner) *Handler {
	return &Handler{svc: svc, owner: owner}
}

// Mount：POST /projects/{projectId}/package（回 zip 字节）、POST /projects/import（multipart，字段 file）。
func (h *Handler) Mount(r chi.Router, requireUser, wsGuard func(http.Handler) http.Handler) {
	r.Group(func(r chi.Router) {
		r.Use(requireUser, wsGuard)
		r.Post("/projects/{projectId}/package", h.export)
		r.Post("/projects/import", h.importPackage)
	})
}

func (h *Handler) export(w http.ResponseWriter, r *http.Request) {
	pid, err := strconv.ParseInt(chi.URLParam(r, "projectId"), 10, 64)
	if err != nil {
		httpx.Fail(w, r, httpx.Err(errcode.InvalidArgument))
		return
	}
	file, name, err := h.svc.Export(r.Context(), h.owner(r).WorkspaceID, pid)
	if errors.Is(err, ErrNotFound) {
		httpx.Fail(w, r, httpx.Err(errcode.NotFound))
		return
	}
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
		return
	}
	defer os.Remove(file)
	f, err := os.Open(file)
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil {
		w.Header().Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("X-Filename", url.PathEscape(name))
	_, _ = io.Copy(w, f)
}

func (h *Handler) importPackage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, MaxTotalBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "请以 multipart/form-data 上传项目包（字段 file）"))
		return
	}
	var tmp string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "上传中断："+err.Error()))
			return
		}
		if part.FormName() != "file" {
			_ = part.Close()
			continue
		}
		f, err := os.CreateTemp("", "tabtv-import-*.zip")
		if err != nil {
			httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
			return
		}
		tmp = f.Name()
		_, err = io.Copy(f, part)
		_ = f.Close()
		_ = part.Close()
		if err != nil {
			_ = os.Remove(tmp)
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				httpx.Fail(w, r, httpx.Errf(errcode.PayloadTooLarge, "项目包过大"))
				return
			}
			httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "上传中断："+err.Error()))
			return
		}
		break
	}
	if tmp == "" {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, "缺少项目包文件（字段 file）"))
		return
	}
	defer os.Remove(tmp)
	res, err := h.svc.Import(r.Context(), h.owner(r), tmp)
	if IsBadPackage(err) {
		httpx.Fail(w, r, httpx.Errf(errcode.InvalidArgument, err.Error()))
		return
	}
	if err != nil {
		httpx.Fail(w, r, httpx.Wrap(errcode.Internal, err))
		return
	}
	httpx.JSON(w, http.StatusCreated, res)
}
