package util

import "strings"

// ExtractCookieValueByKey 从 Cookie 头中提取指定 key 的值
func ExtractCookieValueByKey(cookie string, key string) string {
	for _, pair := range strings.Split(cookie, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		k, v, found := strings.Cut(pair, "=")
		if !found {
			continue
		}
		if strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// HasAnySuffix path 是否命中任一后缀（"*" 表示全部命中）
func HasAnySuffix(path string, suffixes []string) bool {
	if idx := strings.Index(path, "?"); idx != -1 {
		path = path[:idx]
	}
	for _, s := range suffixes {
		if s == "*" || strings.HasSuffix(path, s) {
			return true
		}
	}
	return false
}
