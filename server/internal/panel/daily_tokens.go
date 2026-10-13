package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"regexp"
	"strings"
)

var tokenLimitPattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([KMB]?)$`)

func parseTokenLimit(raw json.RawMessage) (int64, error) {
	s := strings.TrimSpace(string(raw))
	if strings.HasPrefix(s, `"`) {
		if err := json.Unmarshal(raw, &s); err != nil {
			return 0, fmt.Errorf("每日上限无效")
		}
	}
	m := tokenLimitPattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(s)))
	if m == nil {
		return 0, fmt.Errorf("请输入 Token 数，例如 100M、500K、100000000；0 表示不限")
	}
	v, ok := new(big.Rat).SetString(m[1])
	if !ok {
		return 0, fmt.Errorf("每日上限无效")
	}
	scale := int64(1)
	switch m[2] {
	case "K":
		scale = 1000
	case "M":
		scale = 1000000
	case "B":
		scale = 1000000000
	}
	v.Mul(v, new(big.Rat).SetInt64(scale))
	if !v.IsInt() || !v.Num().IsInt64() {
		return 0, fmt.Errorf("Token 数必须为整数")
	}
	n := v.Num().Int64()
	if n < 0 || n > 9000000000000000 {
		return 0, fmt.Errorf("每日上限超出范围")
	}
	return n, nil
}

func (p *Panel) accountDailyTokens(w http.ResponseWriter, r *http.Request) {
	uid := r.PathValue("uid")
	if _, ok := p.cfg.Pool.Status(uid); !ok {
		writeErr(w, http.StatusNotFound, "account not found")
		return
	}
	var req struct {
		Model string          `json:"model"`
		Limit json.RawMessage `json:"limit"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	if err := d.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid daily limit request")
		return
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		writeErr(w, http.StatusBadRequest, "unexpected trailing data")
		return
	}
	limit, err := parseTokenLimit(req.Limit)
	if err == nil {
		err = p.cfg.Pool.SetDailyTokenLimit(uid, req.Model, limit)
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	st, _ := p.cfg.Pool.Status(uid)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "daily_token_usage": st.DailyTokenUsage, "daily_token_reset_at": st.DailyTokenResetAt})
}
