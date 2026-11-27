package desktop

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"boxctl/internal/dist"
)

// 下载页（S1.11）：列出管理服务云盘里的文件，阅读 Markdown，下载到用户的「下载」目录。
// 后台服务以系统权限运行，不能替用户写文件、打开文件夹，所以下载和「在文件夹中显示」排队交给托盘外壳执行
// （外壳在用户会话里）：外壳从本机服务取文件内容（本机服务再去管理服务或 OSS 下载），边写边报进度。

const maxFileJobs = 50

// FileJob 是一个下载，或者一个打开文件 / 在文件夹中显示的操作。
type FileJob struct {
	ID         string    `json:"id"`
	Action     string    `json:"action"` // download / reveal / open
	FileID     int64     `json:"file_id"`
	Name       string    `json:"name"`
	FileName   string    `json:"file_name"`
	Status     string    `json:"status"` // pending / running / done / failed / canceled
	Downloaded int64     `json:"downloaded"`
	Total      int64     `json:"total"`
	Path       string    `json:"path,omitempty"` // 下载完成后的本机路径；reveal / open 的目标
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	cancel     bool
}

type fileJobs struct {
	mu   sync.Mutex
	jobs []*FileJob // 新的在后
}

func (f *fileJobs) add(j *FileJob) FileJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	j.ID, j.Status, j.CreatedAt, j.UpdatedAt = newActionID(), "pending", time.Now(), time.Now()
	f.jobs = append(f.jobs, j)
	// 只保留最近的，先丢掉已结束的。
	for len(f.jobs) > maxFileJobs {
		i := 0
		for i < len(f.jobs)-1 && (f.jobs[i].Status == "pending" || f.jobs[i].Status == "running") {
			i++
		}
		f.jobs = append(f.jobs[:i], f.jobs[i+1:]...)
	}
	return *j
}

func (f *fileJobs) find(id string) *FileJob {
	for _, j := range f.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// Pending 返回最早一个待外壳执行的操作。
func (f *fileJobs) Pending() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, j := range f.jobs {
		if j.Status == "pending" {
			return j.ID
		}
	}
	return ""
}

// Downloads 是下载记录，新的在前。
func (f *fileJobs) Downloads() []FileJob {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []FileJob{}
	for i := len(f.jobs) - 1; i >= 0; i-- {
		if f.jobs[i].Action == "download" {
			out = append(out, *f.jobs[i])
		}
	}
	return out
}

var errJobNotFound = errors.New("操作不存在")

func (f *fileJobs) update(id string, fn func(*FileJob) error) (FileJob, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	j := f.find(id)
	if j == nil {
		return FileJob{}, errJobNotFound
	}
	if err := fn(j); err != nil {
		return FileJob{}, err
	}
	j.UpdatedAt = time.Now()
	return *j, nil
}

func (f *fileJobs) remove(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, j := range f.jobs {
		if j.ID == id {
			if j.Status == "running" {
				j.cancel = true // 外壳下次报进度时停下，记录随后删掉
			}
			f.jobs = append(f.jobs[:i], f.jobs[i+1:]...)
			return
		}
	}
}

func fileRoutes(mux *http.ServeMux, authed func(http.HandlerFunc) http.Handler, c *dist.Client, jobs *fileJobs) {
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(v); err != nil {
			writeJSON(w, http.StatusBadRequest, apiErr{"bad_request", "请求内容无效"})
			return false
		}
		return true
	}
	fail := func(w http.ResponseWriter, err error) {
		var in *dist.InputError
		var he *dist.HTTPError
		switch {
		case errors.As(err, &in):
			writeJSON(w, http.StatusBadRequest, apiErr{"invalid", in.Msg})
		case errors.Is(err, errJobNotFound):
			writeJSON(w, http.StatusNotFound, apiErr{"not_found", err.Error()})
		case errors.As(err, &he) && he.Status == http.StatusNotFound:
			writeJSON(w, http.StatusNotFound, apiErr{"not_found", "文件不存在或已下架"})
		default:
			writeJSON(w, http.StatusBadGateway, apiErr{"failed", err.Error()})
		}
	}
	fileID := func(r *http.Request) (int64, error) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			return 0, &dist.InputError{Msg: "文件编号无效"}
		}
		return id, nil
	}

	// ---- 页面 ----
	mux.Handle("GET /api/v1/desktop/files", authed(func(w http.ResponseWriter, r *http.Request) {
		list, err := c.Files(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, list)
	}))
	mux.Handle("GET /api/v1/desktop/files/{id}/content", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := fileID(r)
		if err == nil {
			var text string
			if text, err = c.FileContent(r.Context(), id); err == nil {
				writeJSON(w, http.StatusOK, map[string]string{"content": text})
				return
			}
		}
		fail(w, err)
	}))
	mux.Handle("POST /api/v1/desktop/files/{id}/download", authed(func(w http.ResponseWriter, r *http.Request) {
		id, err := fileID(r)
		if err != nil {
			fail(w, err)
			return
		}
		// 文件名以管理服务为准，不用页面传的。
		list, err := c.Files(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		for _, f := range list {
			if f.ID == id {
				writeJSON(w, http.StatusOK, jobs.add(&FileJob{Action: "download", FileID: f.ID, Name: f.Name, FileName: f.FileName, Total: f.Size}))
				return
			}
		}
		fail(w, &dist.HTTPError{Status: http.StatusNotFound, Msg: "文件不存在"})
	}))
	mux.Handle("GET /api/v1/desktop/files/downloads", authed(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, jobs.Downloads())
	}))
	mux.Handle("POST /api/v1/desktop/files/downloads/{id}/cancel", authed(func(w http.ResponseWriter, r *http.Request) {
		j, err := jobs.update(r.PathValue("id"), func(j *FileJob) error {
			switch j.Status {
			case "pending":
				j.Status = "canceled"
			case "running":
				j.cancel = true
			}
			return nil
		})
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, j)
	}))
	mux.Handle("DELETE /api/v1/desktop/files/downloads/{id}", authed(func(w http.ResponseWriter, r *http.Request) {
		jobs.remove(r.PathValue("id"))
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
	// 打开下载的文件，或在文件夹中显示：路径只用外壳自己报上来的。
	mux.Handle("POST /api/v1/desktop/files/downloads/{id}/{action}", authed(func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if action != "reveal" && action != "open" {
			writeJSON(w, http.StatusNotFound, apiErr{"not_found", "操作不存在"})
			return
		}
		jobs.mu.Lock()
		dl := jobs.find(r.PathValue("id"))
		var path string
		if dl != nil && dl.Action == "download" && dl.Status == "done" {
			path = dl.Path
		}
		jobs.mu.Unlock()
		if path == "" {
			fail(w, &dist.InputError{Msg: "文件还没有下载完成"})
			return
		}
		writeJSON(w, http.StatusOK, jobs.add(&FileJob{Action: action, FileID: dl.FileID, Name: dl.Name, FileName: dl.FileName, Path: path}))
	}))

	// ---- 托盘外壳 ----
	mux.Handle("POST /api/v1/desktop/files/jobs/{id}/claim", authed(func(w http.ResponseWriter, r *http.Request) {
		j, err := jobs.update(r.PathValue("id"), func(j *FileJob) error {
			if j.Status != "pending" {
				return &dist.InputError{Msg: "操作已被认领或已结束"}
			}
			j.Status = "running"
			return nil
		})
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, j)
	}))
	mux.Handle("GET /api/v1/desktop/files/jobs/{id}/blob", authed(func(w http.ResponseWriter, r *http.Request) {
		jobs.mu.Lock()
		j := jobs.find(r.PathValue("id"))
		var id int64
		if j != nil && j.Action == "download" {
			id = j.FileID
		}
		jobs.mu.Unlock()
		if id == 0 {
			fail(w, errJobNotFound)
			return
		}
		resp, err := c.OpenBlob(r.Context(), "/api/v1/client/files/"+strconv.FormatInt(id, 10)+"/download")
		if err != nil {
			fail(w, err)
			return
		}
		defer resp.Body.Close()
		if resp.ContentLength >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, resp.Body)
	}))
	mux.Handle("PUT /api/v1/desktop/files/jobs/{id}/progress", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Downloaded int64 `json:"downloaded"`
			Total      int64 `json:"total"`
		}
		if !decode(w, r, &in) {
			return
		}
		cancel := true // 记录已删掉时也让外壳停下
		_, _ = jobs.update(r.PathValue("id"), func(j *FileJob) error {
			j.Downloaded = in.Downloaded
			if in.Total > 0 {
				j.Total = in.Total
			}
			cancel = j.cancel
			return nil
		})
		writeJSON(w, http.StatusOK, map[string]bool{"cancel": cancel})
	}))
	mux.Handle("POST /api/v1/desktop/files/jobs/{id}/result", authed(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			OK       bool   `json:"ok"`
			Canceled bool   `json:"canceled"`
			Path     string `json:"path"`
			Error    string `json:"error"`
		}
		if !decode(w, r, &in) {
			return
		}
		_, err := jobs.update(r.PathValue("id"), func(j *FileJob) error {
			switch {
			case in.OK:
				j.Status, j.Error = "done", ""
				if j.Action == "download" {
					j.Path, j.Downloaded = in.Path, j.Total
				}
			case in.Canceled:
				j.Status = "canceled"
			default:
				j.Status, j.Error = "failed", clip(in.Error, 400)
			}
			return nil
		})
		if err != nil && !errors.Is(err, errJobNotFound) {
			fail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}))
}
