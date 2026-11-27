package desktop

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestFileJobsLifecycle(t *testing.T) {
	jobs := &fileJobs{}
	mux := http.NewServeMux()
	fileRoutes(mux, func(h http.HandlerFunc) http.Handler { return h }, nil, jobs)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
		return rec
	}
	dl := jobs.add(&FileJob{Action: "download", FileID: 7, Name: "说明", FileName: "a.zip", Total: 100})
	if jobs.Pending() != dl.ID {
		t.Fatal("新下载应待外壳执行")
	}
	// 没下载完不能打开。
	if rec := do("POST", "/api/v1/desktop/files/downloads/"+dl.ID+"/reveal", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("未完成时不能在文件夹中显示：%d", rec.Code)
	}
	if rec := do("POST", "/api/v1/desktop/files/jobs/"+dl.ID+"/claim", ""); rec.Code != http.StatusOK {
		t.Fatalf("认领失败：%d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/api/v1/desktop/files/jobs/"+dl.ID+"/claim", ""); rec.Code == http.StatusOK {
		t.Fatal("不能重复认领")
	}
	if jobs.Pending() != "" {
		t.Fatal("认领后不再待执行")
	}
	rec := do("PUT", "/api/v1/desktop/files/jobs/"+dl.ID+"/progress", `{"downloaded":50,"total":100}`)
	var p struct{ Cancel bool }
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if p.Cancel || jobs.Downloads()[0].Downloaded != 50 {
		t.Fatalf("进度不对：%s", rec.Body)
	}
	do("POST", "/api/v1/desktop/files/jobs/"+dl.ID+"/result", `{"ok":true,"path":"/Users/x/Downloads/a.zip"}`)
	if d := jobs.Downloads()[0]; d.Status != "done" || d.Path == "" || d.Downloaded != 100 {
		t.Fatalf("完成状态不对：%+v", d)
	}
	rec = do("POST", "/api/v1/desktop/files/downloads/"+dl.ID+"/reveal", "")
	if rec.Code != http.StatusOK || jobs.Pending() == "" {
		t.Fatalf("在文件夹中显示应排队：%d %s", rec.Code, rec.Body)
	}
	if len(jobs.Downloads()) != 1 {
		t.Fatal("下载记录里不应出现打开文件夹的操作")
	}

	// 取消：进行中的下载在下次报进度时停下；删除记录后也让外壳停下。
	dl2 := jobs.add(&FileJob{Action: "download", FileID: 8, FileName: "b.zip"})
	do("POST", "/api/v1/desktop/files/jobs/"+dl2.ID+"/claim", "")
	do("POST", "/api/v1/desktop/files/downloads/"+dl2.ID+"/cancel", "")
	rec = do("PUT", "/api/v1/desktop/files/jobs/"+dl2.ID+"/progress", `{"downloaded":1}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if !p.Cancel {
		t.Fatal("取消后外壳应停下")
	}
	do("DELETE", "/api/v1/desktop/files/downloads/"+dl2.ID, "")
	rec = do("PUT", "/api/v1/desktop/files/jobs/"+dl2.ID+"/progress", `{"downloaded":2}`)
	_ = json.Unmarshal(rec.Body.Bytes(), &p)
	if !p.Cancel {
		t.Fatal("记录删除后外壳应停下")
	}
}
