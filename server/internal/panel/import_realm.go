package panel

import (
	"fmt"
	"net/url"
	"strings"
)

func importRealmAlias(raw string) string {
	switch importKey(raw) {
	case "global", "intl", "international", "overseas", "oversea", "foreign", "internationaledition", "globaledition", "国际", "国际版", "国际服", "海外", "海外版", "国外":
		return "global"
	case "cn", "china", "domestic", "mainland", "mainlandchina", "zhcn", "国内", "国内版", "国内服", "国服", "中国":
		return "cn"
	}
	return ""
}

func importDomain(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	candidate := s
	if !strings.Contains(candidate, "://") {
		candidate = "https://" + strings.TrimPrefix(candidate, "//")
	}
	if u, err := url.Parse(candidate); err == nil && u.Hostname() != "" {
		return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	}
	return strings.ToLower(strings.TrimSuffix(s, "."))
}

func importDomainRealm(domain string) string {
	belongs := func(base string) bool { return domain == base || strings.HasSuffix(domain, "."+base) }
	if belongs("workbuddy.ai") {
		return "global"
	}
	if belongs("codebuddy.cn") || belongs("workbuddy.cn") || domain == "copilot.tencent.com" {
		return "cn"
	}
	return ""
}

// Domain is a WorkBuddy credential's concrete service identity. An exporter may
// call the same realm intl/global/international, or use its own unknown label.
// Infer from known host suffixes first; never send token probes to imported URLs.
func normalizeImportRealm(fields map[string]string) error {
	domain := importDomain(fields["domain"])
	fields["domain"] = domain
	if realm := importDomainRealm(domain); realm != "" {
		fields["realm"] = realm
		return nil
	}
	if realm := importRealmAlias(fields["realm"]); realm != "" {
		fields["realm"] = realm
		return nil
	}
	if strings.TrimSpace(fields["realm"]) == "" && domain == "" {
		fields["realm"] = "cn"
		return nil
	} // legacy CN exports
	return fmt.Errorf("无法自动识别国内/国际服，请保留凭证中的 WorkBuddy domain 或有效 realm 标记")
}
