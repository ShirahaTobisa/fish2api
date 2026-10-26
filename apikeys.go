// 使用者 API key:发给调用方(脚本/new-api/酒馆)的钥匙,与管理员的 ACCESS_KEY 分离。
//
//   - ACCESS_KEY(环境变量):管理员,全部权限,面板登录用它;
//   - API_KEYS(环境变量,逗号分隔多把):使用者,只能调合成与只读查询(/api/tts、
//     /api/web/tts、/v1/audio/speech、/v1/models、/api/models、/api/voices*),
//     碰账号管理(/api/accounts*)会被拒;没配 ACCESS_KEY 时视为开放实例,一切照旧不拦。
//
// 兼容三种传法(OpenAI 客户端常见姿势):Authorization: Bearer <key>、X-Api-Key: <key>、?key=<key>。
package main

import (
	"net/http"
	"os"
	"strings"
)

// apiKeys 使用者密钥集合(启动时从 API_KEYS 读入,面板增删会同步写回环境存储文件)。
var apiKeys = newAPIKeySet()

type apiKeySet struct {
	keys map[string]bool
}

func newAPIKeySet() *apiKeySet {
	s := &apiKeySet{keys: map[string]bool{}}
	for _, k := range strings.Split(os.Getenv("API_KEYS"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			s.keys[k] = true
		}
	}
	return s
}

func (s *apiKeySet) list() []string {
	out := make([]string, 0, len(s.keys))
	for k := range s.keys {
		out = append(out, k)
	}
	return out
}

func (s *apiKeySet) add(key string) bool {
	key = strings.TrimSpace(key)
	if key == "" || s.keys[key] {
		return false
	}
	s.keys[key] = true
	return true
}

func (s *apiKeySet) remove(key string) bool {
	if !s.keys[key] {
		return false
	}
	delete(s.keys, key)
	return true
}

func (s *apiKeySet) has(key string) bool { return s.keys[key] }

// requestKey 从请求里取调用方给的 key:Authorization: Bearer > X-Api-Key > ?key=。
func requestKey(r *http.Request) string {
	if v := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); v != "" && v != r.Header.Get("Authorization") {
		return strings.TrimSpace(v)
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		return strings.TrimSpace(v)
	}
	return strings.TrimSpace(r.URL.Query().Get("key"))
}

// isUserKey 判断调用方给的 key 是不是一把有效的使用者 key。
func isUserKey(key string) bool { return key != "" && apiKeys.has(key) }

// isAdminKey 管理员密钥(ACCESS_KEY)。
func isAdminKey(key string) bool { return accessKey != "" && key == accessKey }

// authLevel 鉴权结果:0 无效 1 使用者 2 管理员。
func authLevel(r *http.Request) int {
	k := requestKey(r)
	switch {
	case isAdminKey(k):
		return 2
	case isUserKey(k):
		return 1
	}
	return 0
}
