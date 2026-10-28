// 管理功能:面板里改设置、请求记录、运行日志(面板「设置 / 请求记录 / 运行日志」页的后端)。
//
// 数据和账号库放在同一个目录(Docker 里是 /data):
//   - config.json:面板里改过的设置 + 使用者 key(见 apikeys.go);
//   - requests.jsonl:请求记录,一行一条,重启后读回来。
//
// 这里的接口都只认管理员密钥(使用者 key 能调什么见 apikeys.go 的 userAllowed)。
package main

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

func adminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/settings", handleSettingsGet)
	mux.HandleFunc("PUT /api/settings", handleSettingsPut)
	mux.HandleFunc("DELETE /api/cache", handleCacheClear)
	mux.HandleFunc("GET /api/logs/requests", handleRequestLog)
	mux.HandleFunc("DELETE /api/logs/requests", handleRequestLogClear)
	mux.HandleFunc("GET /api/logs/requests/{id}/audio", handleRequestAudio)
	mux.HandleFunc("GET /api/logs/server", handleServerLog)
	mux.HandleFunc("DELETE /api/logs/server", handleServerLogClear)
}

// ---------------------------------------------------------------- config.json:设置 + 使用者 key

// settingKeys 面板「设置」页能改的环境变量。保存时直接写进本进程的环境变量,用到的地方都是现读,所以立即生效;
// 面板里清空某项 = 退回启动时的环境变量(没有就是默认值)。监听地址、账号库路径只能改环境变量后重启。
var settingKeys = []string{
	"ACCESS_KEY",
	"TOKEN_VERIFY_INTERVAL_SECONDS", "ACCOUNT_COOLDOWN_SECONDS",
	"CACHE_MAX_MB", "CACHE_MAX_ITEM_MB", "CACHE_TTL_SECONDS",
	"RECAPTCHA_TTL_SECONDS", "RECAPTCHA_KEEPWARM",
	"FISH_API_KEY", "FISH_WEB_TOKEN",
	"REQUEST_LOG_MAX", "REQUEST_LOG_TEXT",
}

var (
	cfgMu   sync.Mutex
	cfgPath string
	cfg     struct {
		Env  map[string]string `json:"env"`  // 面板里改过的设置
		Keys []apiKey          `json:"keys"` // 使用者 key
	}
	origEnv = map[string]string{} // 启动时的环境变量,面板清空某项时退回它(启动后只读)
)

// loadAdmin 启动时读 config.json,把面板里改过的设置盖到环境变量上;导入老的 api_keys.txt;读回请求记录。
func loadAdmin() {
	dir := filepath.Dir(store.path)
	cfgPath = filepath.Join(dir, "config.json")
	cfgMu.Lock()
	if b, err := os.ReadFile(cfgPath); err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			logf("config.json 解析失败,按空配置启动: %v", err)
		}
	}
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}
	for _, k := range settingKeys {
		origEnv[k] = os.Getenv(k)
		if v, ok := cfg.Env[k]; ok {
			os.Setenv(k, v)
		}
	}
	importOldKeys(dir)
	cfgMu.Unlock()
	ttsCache.configure()
	reqLog.load(filepath.Join(dir, "requests.jsonl"))
}

// saveCfg 写回 config.json(需持 cfgMu)。里面有 key,和账号库一样只给本用户读写。
func saveCfg() error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	tmp := cfgPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cfgPath)
}

// saveCfgOr500 保存 config.json(需持 cfgMu);失败时回 500 并返回 false。
func saveCfgOr500(w http.ResponseWriter) bool {
	if err := saveCfg(); err != nil {
		fail(w, 500, "已生效,但写 config.json 失败(重启会丢): "+err.Error())
		return false
	}
	return true
}

func handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	vals := map[string]string{}
	for _, k := range settingKeys {
		vals[k] = os.Getenv(k)
	}
	items, size := ttsCache.stats()
	reqLog.mu.Lock()
	nReq := len(reqLog.items)
	reqLog.mu.Unlock()
	dir, _ := filepath.Abs(filepath.Dir(store.path))
	writeJSON(w, 200, map[string]any{
		"values": vals, "env": origEnv, "requests": nReq,
		"cache": map[string]int{"items": items, "bytes": size},
		"info":  map[string]any{"addr": listenAddr(), "data_dir": dir, "browser": cmp.Or(browserBin, "无")},
	})
}

// handleSettingsPut 收 {变量名: 值};空值 = 退回启动时的环境变量/默认值。
func handleSettingsPut(w http.ResponseWriter, r *http.Request) {
	var in map[string]string
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	for k := range in {
		if !slices.Contains(settingKeys, k) {
			fail(w, 400, "这一项不能在面板里改: "+k)
			return
		}
	}
	cfgMu.Lock()
	// 管理员密钥要放进浏览器的请求头,只能是不带空格的英文字符;也不能和使用者 key 撞上(撞上那把就成了管理员)
	if v := strings.TrimSpace(in["ACCESS_KEY"]); v != "" {
		msg := ""
		switch {
		case len(v) < 8:
			msg = "管理员密钥至少 8 位"
		case strings.ContainsFunc(v, func(r rune) bool { return r <= ' ' || r > '~' }):
			msg = "管理员密钥只能用英文字母、数字和符号,不能有空格和中文"
		case slices.ContainsFunc(cfg.Keys, func(k apiKey) bool { return k.Key == v }):
			msg = "和一把使用者 key 一样了,换一个"
		}
		if msg != "" {
			cfgMu.Unlock()
			fail(w, 400, msg)
			return
		}
	}
	for k, v := range in {
		if v = strings.TrimSpace(v); v == "" || v == origEnv[k] {
			delete(cfg.Env, k)
			v = origEnv[k]
		} else {
			cfg.Env[k] = v
		}
		if v == "" {
			os.Unsetenv(k)
		} else {
			os.Setenv(k, v)
		}
	}
	ok := saveCfgOr500(w)
	cfgMu.Unlock()
	ttsCache.configure()
	reqLog.trim()
	if ok {
		logf("面板修改了设置: %s", strings.Join(slices.Sorted(maps.Keys(in)), ", "))
		handleSettingsGet(w, r)
	}
}

func handleCacheClear(w http.ResponseWriter, r *http.Request) {
	ttsCache.clear()
	logf("面板清空了结果缓存")
	writeJSON(w, 200, map[string]any{"cleared": true})
}

// ---------------------------------------------------------------- 请求记录

// reqRecord 一次合成调用,面板「请求记录」的一行。
type reqRecord struct {
	ID      int64  `json:"id"`
	T       int64  `json:"t"`                 // 开始时间,unix 毫秒
	Key     string `json:"key,omitempty"`     // 调用方的 key 名(没设 ACCESS_KEY 又没带 key 时为空)
	Ep      string `json:"ep"`                // 入口:web / openai / api(官方 API key)/ media(图像视频)
	Account string `json:"account,omitempty"` // 实际用的账号(命中缓存、API key 模式时为空)
	Model   string `json:"model,omitempty"`
	Voice   string `json:"voice,omitempty"` // 声音 ID,或客户端给的声音名字
	Chars   int    `json:"chars"`
	Text    string `json:"text,omitempty"` // 前 200 字;REQUEST_LOG_TEXT=0 时不记
	Format  string `json:"format,omitempty"`
	Status  int    `json:"status"`
	Err     string `json:"err,omitempty"`
	Ms      int64  `json:"ms"`
	Bytes   int    `json:"bytes"`
	Cache   bool   `json:"cache,omitempty"`
	IP      string `json:"ip,omitempty"`

	cacheKey string // 结果缓存的键:缓存里还在就能在面板回放(只在内存里,重启后缓存本来也没了)
}

type recCtx struct{}

// recOf 当前请求的记录(选账号、查缓存时往里补);不记录的请求给个空壳,调用方不用判空。
func recOf(r *http.Request) *reqRecord {
	if rec, ok := r.Context().Value(recCtx{}).(*reqRecord); ok {
		return rec
	}
	return &reqRecord{}
}

var defaultModel = map[string]string{"web": "drama-3", "openai": "drama-3", "api": "s2.1-pro"} // 和各入口不传模型时的默认值一致

// logged 包住合成接口,把每次调用记进请求记录:谁调的、参数、结果、耗时。
func logged(ep string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4<<20))
		r.Body = io.NopCloser(bytes.NewReader(body))
		var in struct {
			Text, Input, Prompt, Voice, Backend, Model, Format string
			VoiceID                                            any    `json:"voice_id"`
			ResponseFormat                                     string `json:"response_format"`
		}
		_ = json.Unmarshal(body, &in)
		text := cmp.Or(in.Text, in.Input, in.Prompt)
		key, _ := r.Context().Value(callerCtx{}).(string)
		rec := &reqRecord{
			T: time.Now().UnixMilli(), Key: key, Ep: ep, IP: clientIP(r), Chars: utf8.RuneCountInString(text),
			Model:  cmp.Or(in.Backend, in.Model, defaultModel[ep]),
			Voice:  cmp.Or(in.Voice, voiceText(in.VoiceID)),
			Format: cmp.Or(in.Format, in.ResponseFormat),
		}
		if os.Getenv("REQUEST_LOG_TEXT") != "0" {
			rec.Text = cut(text, 200)
		}
		rw := &recWriter{ResponseWriter: w, status: 200}
		h(rw, r.WithContext(context.WithValue(r.Context(), recCtx{}, rec)))
		rec.Status, rec.Bytes, rec.Ms = rw.status, rw.n, time.Now().UnixMilli()-rec.T
		rec.Cache = rw.Header().Get("X-Cache") == "HIT"
		if rec.Status >= 400 {
			rec.Err = errText(rw.errBuf)
		}
		reqLog.add(rec)
	}
}

// voiceText 声音字段转成文字:单个 ID 原样,多说话人数组转成 JSON。
func voiceText(v any) string {
	if s, ok := v.(string); ok || v == nil {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// recWriter 记下状态码和字节数;出错时留住响应体开头,用来提取错误信息。
type recWriter struct {
	http.ResponseWriter
	status int
	n      int
	errBuf []byte
}

func (w *recWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *recWriter) Write(p []byte) (int, error) {
	if w.status >= 400 && len(w.errBuf) < 2048 {
		w.errBuf = append(w.errBuf, p[:min(len(p), 2048-len(w.errBuf))]...)
	}
	n, err := w.ResponseWriter.Write(p)
	w.n += n
	return n, err
}

// Unwrap 让 http.ResponseController 找到底层的 Flush(音频边收边发靠它)。
func (w *recWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// errText 从错误响应里取一句人话:本项目和 fish 的错误在 detail / message / error(.message),其余原样截断。
func errText(b []byte) string {
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		if e, ok := m["error"].(map[string]any); ok {
			m["error"] = e["message"]
		}
		for _, k := range []string{"detail", "message", "error"} {
			if s, ok := m[k].(string); ok && s != "" {
				return cut(s, 300)
			}
		}
	}
	return cut(strings.TrimSpace(string(b)), 300)
}

// cut 按字符截断,超出的用 … 表示。
func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}

// clientIP 调用方地址:经反代时取 X-Forwarded-For 的第一个。
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		return strings.TrimSpace(strings.Split(f, ",")[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

// requestLog 内存里留最近 REQUEST_LOG_MAX 条(默认 5000,0 = 不再记),同时一行一条追加进 requests.jsonl;
// 文件行数超出上限一半时按内存整体重写一次,免得越积越大。
type requestLog struct {
	mu    sync.Mutex
	path  string
	items []*reqRecord // 旧 → 新;加进来以后不再改
	next  int64
	lines int // 文件里的行数
}

var reqLog = &requestLog{next: 1}

func logMax() int { return envIntZero("REQUEST_LOG_MAX", 5000) }

func (l *requestLog) load(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.path = path
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		l.lines++
		var rec reqRecord
		if json.Unmarshal(sc.Bytes(), &rec) == nil {
			l.items = append(l.items, &rec)
			l.next = max(l.next, rec.ID+1)
		}
	}
	l.trimLocked()
}

func (l *requestLog) add(rec *reqRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if logMax() <= 0 || l.path == "" {
		return
	}
	rec.ID = l.next
	l.next++
	l.items = append(l.items, rec)
	b, _ := json.Marshal(rec)
	if f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		if _, err = f.Write(append(b, '\n')); err == nil {
			l.lines++
		}
		f.Close()
	}
	l.trimLocked()
}

// trim 设置里调小了保留条数时,立刻删掉多出来的旧记录。
func (l *requestLog) trim() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trimLocked()
}

func (l *requestLog) trimLocked() {
	limit := logMax()
	if limit <= 0 {
		return
	}
	if n := len(l.items) - limit; n > 0 {
		l.items = slices.Delete(l.items, 0, n)
	}
	if l.lines > limit+limit/2 {
		l.rewrite()
	}
}

// rewrite 按内存里的记录重写文件(需持锁)。
func (l *requestLog) rewrite() {
	var buf bytes.Buffer
	for _, rec := range l.items {
		b, _ := json.Marshal(rec)
		buf.Write(b)
		buf.WriteByte('\n')
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o600); err == nil && os.Rename(tmp, l.path) == nil {
		l.lines = len(l.items)
	}
}

// reqGroup 一组记录的汇总(全部 / 某把 key / 某个账号)。
type reqGroup struct {
	Name  string `json:"name"`
	Calls int    `json:"calls"`
	OK    int    `json:"ok"`
	Cache int    `json:"cache"`
	Chars int    `json:"chars"`
	Ms    int64  `json:"ms"` // 总耗时,面板除以次数得平均
	Last  int64  `json:"last"`
}

func (g *reqGroup) add(rec *reqRecord) {
	g.Calls++
	if rec.Status < 400 {
		g.OK++
	}
	if rec.Cache {
		g.Cache++
	}
	g.Chars += rec.Chars
	g.Ms += rec.Ms
	g.Last = max(g.Last, rec.T)
}

// handleRequestLog 查请求记录:from/to(毫秒)限定时间;key/account/status(ok|fail|cache)/q 过滤(key、account 给空值 = 筛空的);
// 返回汇总、按 key 和按账号的分组、趋势(bucket 毫秒一格,从 from 算起)和分页明细(新的在前)。
func handleRequestLog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	num := func(k string, def int64) int64 {
		if n, err := strconv.ParseInt(q.Get(k), 10, 64); err == nil {
			return n
		}
		return def
	}
	from, to, bucket := num("from", 0), num("to", time.Now().UnixMilli()+1), num("bucket", 0)
	limit, offset := int(num("limit", 50)), int(num("offset", 0))
	status, kw := q.Get("status"), strings.ToLower(q.Get("q"))
	var trend [][2]int // 每格 [成功, 失败]
	if bucket > 0 && (to-from)/bucket < 1000 {
		trend = make([][2]int, (to-from+bucket-1)/bucket)
	}
	total, byKey, byAcc := &reqGroup{}, map[string]*reqGroup{}, map[string]*reqGroup{}
	keys, accs := map[string]bool{}, map[string]bool{}
	var hits []*reqRecord
	reqLog.mu.Lock()
	for i := len(reqLog.items) - 1; i >= 0; i-- {
		rec := reqLog.items[i]
		if rec.T < from || rec.T >= to {
			continue
		}
		keys[rec.Key], accs[rec.Account] = true, true
		if q.Has("key") && rec.Key != q.Get("key") || q.Has("account") && rec.Account != q.Get("account") ||
			status == "ok" && rec.Status >= 400 || status == "fail" && rec.Status < 400 || status == "cache" && !rec.Cache ||
			kw != "" && !strings.Contains(strings.ToLower(strings.Join([]string{rec.Text, rec.Voice, rec.Err, rec.Model, rec.IP}, "\n")), kw) {
			continue
		}
		total.add(rec)
		group(byKey, rec.Key).add(rec)
		group(byAcc, rec.Account).add(rec)
		if b := (rec.T - from) / max(bucket, 1); b < int64(len(trend)) {
			trend[b][min(rec.Status/400, 1)]++
		}
		hits = append(hits, rec)
	}
	reqLog.mu.Unlock()
	type view struct {
		*reqRecord
		Play bool `json:"play,omitempty"` // 缓存里还在,能回放
	}
	items := []view{}
	for _, rec := range hits[min(offset, len(hits)):min(offset+limit, len(hits))] {
		items = append(items, view{rec, rec.cacheKey != "" && ttsCache.has(rec.cacheKey)})
	}
	writeJSON(w, 200, map[string]any{
		"total": total, "items": items, "trend": trend, "by_key": sorted(byKey), "by_account": sorted(byAcc),
		"keys": slices.Sorted(maps.Keys(keys)), "accounts": slices.Sorted(maps.Keys(accs)),
	})
}

func group(m map[string]*reqGroup, name string) *reqGroup {
	if m[name] == nil {
		m[name] = &reqGroup{Name: name}
	}
	return m[name]
}

// sorted 分组按调用次数从多到少。
func sorted(m map[string]*reqGroup) []*reqGroup {
	gs := slices.Collect(maps.Values(m))
	slices.SortFunc(gs, func(a, b *reqGroup) int { return b.Calls - a.Calls })
	return gs
}

func handleRequestLogClear(w http.ResponseWriter, r *http.Request) {
	reqLog.mu.Lock()
	reqLog.items = nil
	reqLog.rewrite()
	reqLog.mu.Unlock()
	logf("面板清空了请求记录")
	writeJSON(w, 200, map[string]any{"cleared": true})
}

// handleRequestAudio 回放一条记录的音频(只要结果缓存里还在)。
func handleRequestAudio(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	var key string
	reqLog.mu.Lock()
	if i := slices.IndexFunc(reqLog.items, func(x *reqRecord) bool { return x.ID == id }); i >= 0 {
		key = reqLog.items[i].cacheKey
	}
	reqLog.mu.Unlock()
	e, ok := ttsCache.get(key)
	if !ok {
		fail(w, 404, "这条的音频已经不在缓存里了(过期、被挤掉或服务重启过)")
		return
	}
	w.Header().Set("Content-Type", e.mime)
	_, _ = w.Write(e.data)
}

// ---------------------------------------------------------------- 运行日志

type logLine struct {
	ID  int64  `json:"id"`
	T   int64  `json:"t"`
	Msg string `json:"msg"`
}

// logRing 留最近约 1000 行程序日志给面板「运行日志」页(main 把标准 log 的输出抄一份进来)。
type logRing struct {
	mu    sync.Mutex
	lines []logLine
	next  int64
}

var serverLog = &logRing{}

func (l *logRing) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if len(msg) >= 20 && msg[4] == '/' && msg[19] == ' ' { // 去掉 log 包自带的 "2006/01/02 15:04:05 " 前缀,时间另记
		msg = msg[20:]
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	l.lines = append(l.lines, logLine{l.next, time.Now().UnixMilli(), msg})
	if len(l.lines) > 1200 {
		l.lines = slices.Clone(l.lines[len(l.lines)-1000:])
	}
	return len(p), nil
}

// handleServerLog 返回 id 大于 after 的日志行(面板增量拉);last 是目前最大的 id,比面板手里的小说明服务重启过。
func handleServerLog(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	serverLog.mu.Lock()
	out := []logLine{}
	if i := slices.IndexFunc(serverLog.lines, func(x logLine) bool { return x.ID > after }); i >= 0 {
		out = slices.Clone(serverLog.lines[i:])
	}
	last := serverLog.next
	serverLog.mu.Unlock()
	writeJSON(w, 200, map[string]any{"lines": out, "last": last})
}

func handleServerLogClear(w http.ResponseWriter, r *http.Request) {
	serverLog.mu.Lock()
	serverLog.lines = nil
	serverLog.mu.Unlock()
	writeJSON(w, 200, map[string]any{"cleared": true})
}
