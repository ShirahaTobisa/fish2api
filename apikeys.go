// 使用者 API key:发给调用方(脚本/new-api/酒馆)的钥匙,与管理员的 ACCESS_KEY 分离。
//
//   - ACCESS_KEY(环境变量,面板「设置」里也能改):管理员,全部权限,面板登录用它;
//   - 使用者 key(面板「API Keys」页发放,存在 data/config.json):有名字、能停用,
//     只能调合成与查询(见 userAllowed),碰账号、密钥、设置、记录这些管理接口会被拒;
//     没配 ACCESS_KEY 时视为开放实例,一切照旧不拦;
//   - 老版本存在 data/api_keys.txt(一行一把)的 key 第一次启动时自动导入,原文件改名留底。
//
// 兼容三种传法(OpenAI 客户端常见姿势):Authorization: Bearer <key>、X-Api-Key: <key>、?key=<key>。
package main

import (
	"cmp"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// apiKey 一把使用者 key。调用次数、最后使用时间从请求记录里统计,这里不存。
type apiKey struct {
	Name    string `json:"name"`
	Key     string `json:"key"`
	Enabled bool   `json:"enabled"`
	Created int64  `json:"created"` // unix 毫秒
}

const adminKeyName = "管理员" // 请求记录里 ACCESS_KEY 显示的名字

type callerCtx struct{} // 请求上下文里放调用方的 key 名(guard 放,请求记录取)

func keyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/keys", handleKeyList)
	mux.HandleFunc("POST /api/keys", handleKeyAdd)
	mux.HandleFunc("PATCH /api/keys/{name}", handleKeyPatch)
	mux.HandleFunc("DELETE /api/keys/{name}", handleKeyDelete)
}

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

// keyOf 认出 key:管理员 (adminKeyName, 2);启用的使用者 key (名字, 1);停用的 (名字, -1);认不出 ("", 0)。
func keyOf(key string) (string, int) {
	if key == "" {
		return "", 0
	}
	if ak := os.Getenv("ACCESS_KEY"); ak != "" && subtle.ConstantTimeCompare([]byte(key), []byte(ak)) == 1 {
		return adminKeyName, 2
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	for _, k := range cfg.Keys {
		if subtle.ConstantTimeCompare([]byte(key), []byte(k.Key)) == 1 {
			if !k.Enabled {
				return k.Name, -1
			}
			return k.Name, 1
		}
	}
	return "", 0
}

// userAllowed 使用者 key 能调的接口:合成(语音/图像/视频)、查声音和模型、查余额和任务;其余都是管理接口。
func userAllowed(path string) bool {
	if strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/compat/") || strings.HasPrefix(path, "/api/voices") || strings.HasPrefix(path, "/api/web/") {
		return true
	}
	return path == "/api/tts" || path == "/api/models"
}

func keyCount() int {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	return len(cfg.Keys)
}

// importOldKeys 把老版本 api_keys.txt 里的 key 并进来(起名「旧 key N」),原文件改名为 .imported 留底(需持 cfgMu)。
func importOldKeys(dir string) {
	old := cmp.Or(os.Getenv("API_KEYS_FILE"), filepath.Join(dir, "api_keys.txt"))
	b, err := os.ReadFile(old)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		k := strings.TrimSpace(line)
		if k != "" && !slices.ContainsFunc(cfg.Keys, func(x apiKey) bool { return x.Key == k }) {
			cfg.Keys = append(cfg.Keys, apiKey{Name: "旧 key " + strconv.Itoa(len(cfg.Keys)+1), Key: k, Enabled: true, Created: time.Now().UnixMilli()})
		}
	}
	if err := saveCfg(); err != nil {
		logf("导入 %s 失败: %v", old, err)
		return
	}
	_ = os.Rename(old, old+".imported")
	logf("已把 %s 里的使用者 key 导入 config.json,原文件改名为 .imported", old)
}

func handleKeyList(w http.ResponseWriter, r *http.Request) {
	cfgMu.Lock()
	keys := slices.Clone(cfg.Keys)
	cfgMu.Unlock()
	writeJSON(w, 200, map[string]any{"keys": keys})
}

// handleKeyAdd 发放新 key:{"name":"给谁用"},key 自动生成(sk-fish- 开头)。
func handleKeyAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	k := apiKey{Name: strings.TrimSpace(in.Name), Key: "sk-fish-" + randHex(20), Enabled: true, Created: time.Now().UnixMilli()}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	if msg := badKeyName(k.Name, ""); msg != "" {
		fail(w, 400, msg)
		return
	}
	cfg.Keys = append(cfg.Keys, k)
	if saveCfgOr500(w) {
		logf("发放使用者 API key「%s」", k.Name)
		writeJSON(w, 200, k)
	}
}

// handleKeyPatch 改名 {"name"} / 启用停用 {"enabled"}。
func handleKeyPatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    *string `json:"name"`
		Enabled *bool   `json:"enabled"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	cfgMu.Lock()
	defer cfgMu.Unlock()
	i := keyIndex(r.PathValue("name"))
	if i < 0 {
		fail(w, 404, "没有这把 key: "+r.PathValue("name"))
		return
	}
	k := &cfg.Keys[i]
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if msg := badKeyName(name, k.Name); msg != "" {
			fail(w, 400, msg)
			return
		}
		k.Name = name
	}
	if in.Enabled != nil {
		k.Enabled = *in.Enabled
	}
	if saveCfgOr500(w) {
		writeJSON(w, 200, *k)
	}
}

func handleKeyDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfgMu.Lock()
	defer cfgMu.Unlock()
	i := keyIndex(name)
	if i < 0 {
		fail(w, 404, "没有这把 key: "+name)
		return
	}
	cfg.Keys = slices.Delete(cfg.Keys, i, i+1)
	if saveCfgOr500(w) {
		logf("吊销使用者 API key「%s」", name)
		writeJSON(w, 200, map[string]any{"deleted": name})
	}
}

// keyIndex 按名字找 key(需持 cfgMu),找不到返回 -1。
func keyIndex(name string) int {
	return slices.IndexFunc(cfg.Keys, func(k apiKey) bool { return k.Name == name })
}

// badKeyName 检查名字(需持 cfgMu):不能空、不能重名、不能和管理员撞名;old 是改名前的名字。
func badKeyName(name, old string) string {
	switch {
	case name == "":
		return "名字不能为空:写上给谁用的,请求记录里按它统计"
	case name == adminKeyName:
		return "「" + adminKeyName + "」留给 ACCESS_KEY,换个名字"
	case name != old && keyIndex(name) >= 0:
		return "已经有叫「" + name + "」的 key 了"
	}
	return ""
}

// randHex 生成 n 字节的随机 hex。
func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // Go 1.24 起 crypto/rand.Read 不会失败
	return hex.EncodeToString(b)
}
