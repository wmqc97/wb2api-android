package panel

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
)

type importRecord struct {
	Auth *auth.Auth
	Err  string
}

// Field names are case/underscore/dash insensitive. Only credential and identity
// fields are merged; unrelated data and array elements never share credentials.
func importKey(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(s))
}

func importField(k string) string {
	switch importKey(k) {
	case "uid", "userid", "accountid":
		return "uid"
	case "accesstoken", "at":
		return "accessToken"
	case "refreshtoken", "rt":
		return "refreshToken"
	case "expiresat", "expireat", "expiration", "expiretime", "expiry":
		return "expiresAt"
	case "expiresin":
		return "expiresIn"
	case "domain":
		return "domain"
	case "realm":
		return "realm"
	case "nickname", "displayname":
		return "nickname"
	case "email":
		return "email"
	case "enterpriseid":
		return "enterpriseId"
	case "devicetoken":
		return "device_token"
	}
	return ""
}

func importText(v any) string {
	switch x := v.(type) {
	case string:
		return strings.TrimSpace(x)
	case json.Number:
		return string(x)
	}
	return ""
}

func importDecodedString(v any) any {
	if s, ok := v.(string); ok && (strings.HasPrefix(strings.TrimSpace(s), "{") || strings.HasPrefix(strings.TrimSpace(s), "[")) {
		d := json.NewDecoder(strings.NewReader(s))
		d.UseNumber()
		var x any
		if d.Decode(&x) == nil {
			var rest any
			if d.Decode(&rest) == io.EOF {
				return x
			}
		}
	}
	return v
}

func importSingleFields(m map[string]any, depth int) (map[string]string, bool, error) {
	fields := map[string]string{}
	if depth > 16 {
		return fields, false, fmt.Errorf("账号嵌套过深")
	}
	put := func(k, v string) error {
		if v == "" {
			return nil
		}
		if k == "realm" {
			if normalized := importRealmAlias(v); normalized != "" {
				v = normalized
			}
		}
		if k == "domain" {
			v = importDomain(v)
		}
		if old := fields[k]; old != "" && old != v {
			return fmt.Errorf("字段 %s 有多个冲突值", k)
		}
		fields[k] = v
		return nil
	}
	for k, v := range m {
		if key := importField(k); key != "" {
			if err := put(key, importText(v)); err != nil {
				return fields, true, err
			}
		}
	}
	// Known single-account fragments may be siblings (wb2api's auth + account).
	for _, key := range []string{"auth", "account", "user", "profile", "credentials", "credential", "tokens", "token", "oauth", "tokeninfo", "accountinfo"} {
		for k, v := range m {
			if importKey(k) != key {
				continue
			}
			child, ok := importDecodedString(v).(map[string]any)
			if !ok {
				continue
			}
			sub, _, err := importSingleFields(child, depth+1)
			if err != nil {
				return fields, true, err
			}
			for k, v := range sub {
				if err := put(k, v); err != nil {
					return fields, true, err
				}
			}
		}
	}
	candidate := fields["accessToken"] != "" || fields["refreshToken"] != ""
	return fields, candidate, nil
}

func importExpiry(fields map[string]string) (int64, error) {
	s := fields["expiresAt"]
	if s == "" {
		if s = fields["expiresIn"]; s != "" {
			n, e := strconv.ParseInt(s, 10, 64)
			if e != nil || n < 0 || n > 10*365*86400 {
				return 0, fmt.Errorf("expiresIn 无效")
			}
			return time.Now().Unix() + n, nil
		}
		return 0, nil
	}
	if n, e := strconv.ParseInt(s, 10, 64); e == nil {
		if n < 0 {
			return 0, fmt.Errorf("expiresAt 无效")
		}
		if n >= 1000000000000 {
			n /= 1000
		}
		return n, nil
	}
	if t, e := time.Parse(time.RFC3339, s); e == nil {
		return t.Unix(), nil
	}
	return 0, fmt.Errorf("expiresAt 需为秒、毫秒时间戳或 RFC3339 日期")
}

func parseImportRecords(raw []byte) ([]importRecord, error) {
	raw = bytes.TrimPrefix(bytes.TrimSpace(raw), []byte{0xef, 0xbb, 0xbf})
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("JSON 格式无效")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fmt.Errorf("JSON 含多余内容")
	}
	records := []importRecord{}
	nodes := 0
	var walk func(any, int) error
	walk = func(v any, depth int) error {
		nodes++
		if depth > 32 || nodes > 100000 || len(records) >= 10000 {
			return fmt.Errorf("JSON 层级或账号数量超出限制")
		}
		v = importDecodedString(v)
		switch x := v.(type) {
		case []any:
			for _, item := range x {
				if err := walk(item, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			f, candidate, err := importSingleFields(x, 0)
			if candidate {
				rec := importRecord{}
				if err == nil && (f["uid"] == "" || f["accessToken"] == "" || f["refreshToken"] == "") {
					err = fmt.Errorf("缺少 uid、accessToken 或 refreshToken；状态/用量导出不含登录凭证，无法导入")
				}
				if err == nil && !validUID(f["uid"]) {
					err = fmt.Errorf("uid 含非法字符")
				}
				if err == nil {
					err = normalizeImportRealm(f)
				}
				expiry := int64(0)
				if err == nil {
					expiry, err = importExpiry(f)
				}
				if err == nil {
					doc := map[string]any{}
					for k, v := range f {
						doc[k] = v
					}
					doc["expiresAt"] = expiry
					if f["nickname"] == "" {
						doc["nickname"] = f["email"]
					}
					b, _ := json.Marshal(doc)
					rec.Auth, err = auth.Parse(b)
				}
				if err != nil {
					rec.Err = err.Error()
				}
				records = append(records, rec)
				return nil
			}
			// Generic export envelopes and UID-keyed dictionaries are traversed, never flattened.
			keys := make([]string, 0, len(x))
			for k := range x {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			before := len(records)
			for _, k := range keys {
				switch child := importDecodedString(x[k]).(type) {
				case map[string]any, []any:
					if err := walk(child, depth+1); err != nil {
						return err
					}
				}
			}
			if before == len(records) && f["uid"] != "" {
				records = append(records, importRecord{Err: "缺少登录凭证（仅有账号信息）"})
			}
		}
		return nil
	}
	if err := walk(root, 0); err != nil {
		return nil, err
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("未找到账号凭证；支持单账号、数组、auth/account 嵌套及 accounts/data 等导出包装")
	}
	return records, nil
}
