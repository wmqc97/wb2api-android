// import.go 面板账号批量导入，兼容本程序与第三方 JSON 凭证导出。
//
// 吸收上游 9371f7d：Add Account 对话框新增「导入 JSON」标签页——本地浏览器登录
// 一次一个账号，迁移/换机时得重复 N 次；cockpit tools 的导出文件里已含
// uid/accessToken/refreshToken/domain，直接批量落盘进池即可。
//
// 设计边界：只做「把给定凭证安全落盘 + 进池 + 收尾签到」，不做任何凭证派生或
// 猜测（缺字段的条目跳过并逐条说明原因，不静默丢弃也不整批失败）。
package panel

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

// importCockpit 保留旧端点名称，接收多种 JSON 凭证格式并批量导入账号。
//
//	POST /panel/api/import/cockpit
//	Content-Type: multipart/form-data
//	Body: file=<json>
//
// 返回 {ok, total, imported, skipped, errors}。校验失败（非 multipart / 非法
// JSON / 空数组）返回 400——不能静默成功，否则前端显示「成功 0 个」而用户不知道
// 文件坏了。单条坏数据只进 skipped + errors，不拖垮整批。
func (p *Panel) importCockpit(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, (32<<20)+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, http.StatusBadRequest, "parse form: "+err.Error())
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field: "+err.Error())
		return
	}
	defer file.Close()

	raw, err := io.ReadAll(io.LimitReader(file, (32<<20)+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read file: "+err.Error())
		return
	}

	if len(raw) > 32<<20 {
		writeErr(w, http.StatusRequestEntityTooLarge, "JSON 文件超过 32 MiB")
		return
	}
	accounts, err := parseImportRecords(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := os.MkdirAll(p.cfg.AuthDir, 0700); err != nil {
		writeErr(w, http.StatusInternalServerError, "无法创建账号目录")
		return
	}

	var imported, skipped int
	var errs []string

	seen := map[string]bool{}
	for i, acc := range accounts {
		if acc.Err != "" {
			skipped++
			errs = append(errs, fmt.Sprintf("第 %d 条：%s", i+1, acc.Err))
			continue
		}
		a := acc.Auth
		uid := a.UID
		if seen[uid] {
			skipped++
			errs = append(errs, fmt.Sprintf("第 %d 条：重复 uid，已保留首个有效条目", i+1))
			continue
		}
		seen[uid] = true
		realm := auth.ResolveRealm(a.RealmStored(), a.DomainValue())
		a.FilePath = filepath.Join(p.cfg.AuthDir, fmt.Sprintf("workbuddy-%s.json", uid))

		// realm 落盘（global 显式写；cn 走 domain 推断），与面板登录路径同口径。
		if realm == "global" {
			if _, err := auth.BackfillRealmFor(a, "global"); err != nil {
				skipped++
				errs = append(errs, fmt.Sprintf("uid=%s: set realm failed: %v", uid, err))
				continue
			}
		} else {
			_, _ = a.BackfillRealm()
		}

		if err := a.SaveAtomic(); err != nil {
			skipped++
			errs = append(errs, fmt.Sprintf("uid=%s: save auth failed: %v", uid, err))
			continue
		}

		p.cfg.Pool.Add(a)
		p.cfg.Pool.Revive(uid)

		imported++
		p.queueAccountSetup(a)
	}

	log.Printf("panel: cockpit import finished total=%d imported=%d skipped=%d", len(accounts), imported, skipped)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"total":               len(accounts),
		"imported":            imported,
		"skipped":             skipped,
		"errors":              errs,
		"maintenance_pending": imported > 0 && p.cfg.Upstream != nil,
	})
}
