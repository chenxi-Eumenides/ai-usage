package provider

import "strings"

// MaskKey 对 API key 脱敏：
//   - 长度 >= 12：保留前 4 后 4，中间用 **** 掩掉，如 "sk-abc123def456" -> "sk-a****456"
//   - 长度 < 12：全部掩为 "****"
//   - 空串或纯空格：返回空串
func MaskKey(key string) string {
	if strings.TrimSpace(key) == "" {
		return ""
	}
	if len(key) < 12 {
		return "****"
	}
	return key[:4] + "****" + key[len(key)-4:]
}
