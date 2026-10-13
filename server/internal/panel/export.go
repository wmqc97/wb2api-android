package panel

import (
	"encoding/json"
	"net/http"
)

// Exports read the current in-memory credential snapshot. They neither refresh
// tokens nor read caller-supplied paths, and require the existing panel auth.
func (p *Panel) accountExport(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if !validUID(uid) {
		writeErr(w, http.StatusBadRequest, "invalid uid")
		return
	}
	a := p.cfg.Pool.AuthByUID(uid)
	if a == nil {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	raw, err := a.ExportCredentials()
	if err != nil {
		writeErr(w, http.StatusConflict, "账号缺少有效凭证，无法导出")
		return
	}
	writeCredentialDownload(w, "workbuddy-"+uid+".json", raw)
}

func (p *Panel) accountsExport(w http.ResponseWriter, r *http.Request) {
	docs := []json.RawMessage{}
	for _, st := range p.cfg.Pool.List() {
		a := p.cfg.Pool.AuthByUID(st.UID)
		if a == nil {
			continue
		}
		raw, err := a.ExportCredentials()
		if err != nil {
			writeErr(w, http.StatusConflict, "部分账号缺少有效凭证，请单独导出其他账号")
			return
		}
		docs = append(docs, json.RawMessage(raw))
	}
	if len(docs) == 0 {
		writeErr(w, http.StatusBadRequest, "账号池为空，没有可导出的凭证")
		return
	}
	raw, err := json.MarshalIndent(docs, "", "  ")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "凭证导出失败")
		return
	}
	writeCredentialDownload(w, "workbuddy-accounts.json", raw)
}

func writeCredentialDownload(w http.ResponseWriter, name string, raw []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(raw)
}
