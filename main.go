// Fish Audio 网页版 TTS → 本地 API(Go 实现)。
//
// 两种模式:
//  1. API key 模式  POST /api/tts      — 官方 /v1/tts,扣钱包里的"API 积分";
//  2. 网页会话模式  POST /api/web/tts  — 网页前端的 POST /task,扣"订阅套餐 credits"。
//
// 网页会话模式支持多账号:accounts.json 保存账号(本地含明文密码,注意保管)。
// 有浏览器的机器用 rod 驱动真实 Chrome 自动登录(或弹出窗口人工登录),按 JWT exp 定期续期;
// 没浏览器的机器(服务器)只接收 token:本地实例登录后通过 PUSH_URL 推送,或在面板里粘贴。
package main

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const upstream = "https://api.fish.audio"

var (
	store     = NewAccountStore()
	accessKey = os.Getenv("ACCESS_KEY")                       // 设了之后 /api/* 都要带 Authorization: Bearer <key>
	pushURL   = strings.TrimRight(os.Getenv("PUSH_URL"), "/") // 本地登录拿到的 token 推送到这台服务器
	pushKey   = os.Getenv("PUSH_KEY")                         // 那台服务器的 ACCESS_KEY
)

// accountCooldown 账号上游 429/5xx 后冷却多久。面板「设置」里能改、保存即生效(见 admin.go),所以现读。
func accountCooldown() time.Duration {
	return time.Duration(envInt("ACCOUNT_COOLDOWN_SECONDS", 30)) * time.Second
}

func logf(format string, args ...any) { log.Printf(format, args...) }

// ---------------------------------------------------------------- 工具

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// fail 错误体同时带 detail(本项目)和 error.message(OpenAI 格式,new-api 这类客户端会显示它)。
func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"detail": msg, "error": map[string]any{"message": msg, "type": "fish_tts_api"}})
}

// guard 设了 ACCESS_KEY 时 /api/* 和 /v1/* 必须带对密钥;/ 和 /health 不拦,面板靠 /health 得知要不要密钥。
// 两级钥匙:ACCESS_KEY=管理员(全部接口);面板「API Keys」里发的=使用者(只能调合成与查询,见 apikeys.go 的 userAllowed)。
// 支持三种传法:Authorization: Bearer <key> / X-Api-Key: <key> / ?key=<key>(OpenAI 客户端通吃)。
// 认出的密钥名放进请求上下文,请求记录里按它统计(没设 ACCESS_KEY 时不拦,但带了 key 照样认名字)。
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !(strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/v1/")) {
			next.ServeHTTP(w, r)
			return
		}
		name, level := keyOf(requestKey(r))
		r = r.WithContext(context.WithValue(r.Context(), callerCtx{}, name))
		if accessKey == "" {
			next.ServeHTTP(w, r)
			return
		}
		// 媒体文件下载走 <img>/<video> 标签,带不了 Authorization 头,放行带有效签名的请求
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/web/media/") && strings.HasSuffix(r.URL.Path, "/file") {
			// 校验对象 = 去掉 &sig=... 之后的 RequestURI(与签名端 raw 字节一致)
			sig := r.URL.Query().Get("sig")
			signed := strings.TrimSuffix(r.URL.RequestURI(), "&sig="+sig)
			if sig != "" && mediaSignVerify(signed, sig) {
				next.ServeHTTP(w, r)
				return
			}
		}
		switch level {
		case 2: // 管理员
			next.ServeHTTP(w, r)
		case 1: // 使用者:只能合成与查询;账号、密钥、设置、记录这些管理接口一律拒绝
			if !userAllowed(r.URL.Path) {
				fail(w, 403, "使用者 API key 只能调合成与查询接口,管理操作需要管理员密钥(ACCESS_KEY)")
				return
			}
			next.ServeHTTP(w, r)
		case -1:
			fail(w, 401, "这把 API key 已在面板里停用")
		default:
			w.Header().Set("WWW-Authenticate", `Bearer realm="fish2api"`)
			fail(w, 401, "需要访问密钥:Authorization: Bearer <key>(管理员用 ACCESS_KEY,使用者用 API key)")
		}
	})
}

// pushToken 把本地拿到的 token 推给 PUSH_URL 指向的服务器;没设 PUSH_URL 就什么都不做。
// 连不上时隔 30 秒再试,最多 5 次(刚开机时服务器可能还没通);密钥不对这类 4xx 不重试。
func pushToken(a Account) {
	if pushURL == "" || a.Token == "" {
		return
	}
	body, _ := json.Marshal(map[string]string{"token": a.Token, "email": a.Email, "team_id": a.TeamID, "workspace_id": a.WorkspaceID})
	for i := 1; ; i++ {
		err := pushOnce(a.Name, body)
		if err == nil {
			logf("账号 %s 的 token 已推送到 %s", a.Name, pushURL)
			return
		}
		logf("账号 %s 的 token 推送到 %s 失败(第 %d 次): %v", a.Name, pushURL, i, err)
		if i >= 5 || strings.HasPrefix(err.Error(), "HTTP 4") {
			return
		}
		time.Sleep(30 * time.Second)
	}
}

func pushOnce(name string, body []byte) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "PUT", pushURL+"/api/accounts/"+url.PathEscape(name)+"/token", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if pushKey != "" {
		req.Header.Set("Authorization", "Bearer "+pushKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// readJSON 解析请求体(最多 4MB)到 v;v 里预先填好的字段就是默认值,请求里给了才覆盖。
func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(v)
}

func upRequest(ctx context.Context, method, path string, headers map[string]string, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, upstream+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return http.DefaultClient.Do(req)
}

// proxyJSON 把上游 GET 的 JSON 响应(含错误)原样转给调用方。
func proxyJSON(w http.ResponseWriter, r *http.Request, path string, headers map[string]string) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	resp, err := upRequest(ctx, "GET", path, headers, nil)
	if err != nil {
		fail(w, 502, "上游请求失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// flushWriter 每写一块就 flush,音频边收边发。
type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	_ = f.rc.Flush()
	return n, err
}

// streamBack 把上游音频流原样回给调用方;上游报错时沿用它的 Content-Type(JSON)。
func streamBack(w http.ResponseWriter, resp *http.Response, mediaType string) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		mediaType = cmp.Or(resp.Header.Get("Content-Type"), "application/json")
	}
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(flushWriter{w, http.NewResponseController(w)}, resp.Body)
}

// streamBackCaching 和 streamBack 一样边收边发,成功(200)时顺便把整段音频存进缓存;
// 客户端中途断开、或音频超过单条上限时不存。
func streamBackCaching(w http.ResponseWriter, resp *http.Response, mediaType, key string) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		streamNon200(w, resp)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	cw := &capWriter{max: ttsCache.itemLimit()}
	_, err := io.Copy(io.MultiWriter(flushWriter{w, http.NewResponseController(w)}, cw), resp.Body)
	if err == nil && !cw.overflow && cw.buf.Len() > 0 {
		ttsCache.put(key, cw.buf.Bytes(), mediaType)
	}
}

func streamNon200(w http.ResponseWriter, resp *http.Response) {
	mediaType := cmp.Or(resp.Header.Get("Content-Type"), "application/json")
	w.Header().Set("Content-Type", mediaType)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(flushWriter{w, http.NewResponseController(w)}, resp.Body)
}

// capWriter 边写边攒,但攒到 max 就停止攒(仍假装全部写入,不打断给客户端的流),用 overflow 标记"太大别缓存"。
type capWriter struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (c *capWriter) Write(p []byte) (int, error) {
	if !c.overflow {
		if c.max > 0 && c.buf.Len()+len(p) > c.max {
			c.overflow = true
			c.buf.Reset() // 太大就别白占内存
		} else {
			c.buf.Write(p)
		}
	}
	return len(p), nil
}

// hasVoice 请求是否明确指定了声音(字符串非空或多说话人数组);没指定时 fish 用账号默认声音,随账号而变,不缓存。
func hasVoice(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return t != ""
	default:
		return true
	}
}

var audioMIME = map[string]string{
	"mp3": "audio/mpeg", "wav": "audio/wav", "pcm": "application/octet-stream", "opus": "audio/opus",
}

func mimeOf(format string) string { return cmp.Or(audioMIME[format], "application/octet-stream") }

// ---------------------------------------------------------------- 凭证解析

// apiKeyHeaders API key 来源:请求头 X-Fish-Key > 环境变量 FISH_API_KEY;都没有返回 nil。
func apiKeyHeaders(r *http.Request) map[string]string {
	key := cmp.Or(r.Header.Get("X-Fish-Key"), os.Getenv("FISH_API_KEY"))
	if key == "" {
		return nil
	}
	return map[string]string{"Authorization": "Bearer " + key}
}

// resolveWebCred 网页 JWT 来源:X-Fish-Web-Token > 账号库(X-Account / ?account= 指定,否则轮询)> FISH_WEB_TOKEN。
func resolveWebCred(r *http.Request) (*WebCred, error) {
	if t := r.Header.Get("X-Fish-Web-Token"); t != "" {
		return &WebCred{Token: t}, nil
	}
	cred, err := store.Pick(cmp.Or(r.Header.Get("X-Account"), r.URL.Query().Get("account")))
	if err == nil {
		recOf(r).Account = cred.Account.Name // 请求记录里记下实际用的账号(见 admin.go)
		return cred, nil
	}
	if t := os.Getenv("FISH_WEB_TOKEN"); t != "" {
		return &WebCred{Token: t}, nil
	}
	return nil, err
}

var errTokenInvalid = errors.New("token 无效或已失效(上游 401)")

// whoami 用 token 调 /user/self 拿用户 ID 和邮箱。网页 token 是不透明的会话串,这是唯一能判断它还有没有效的办法。
func whoami(cred *WebCred) (id, email string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := upRequest(ctx, "GET", "/user/self", webHeaders(cred), nil)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return "", "", errTokenInvalid
	}
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("上游 /user/self 返回 HTTP %d", resp.StatusCode)
	}
	var me struct {
		ID    string `json:"_id"`
		Email string `json:"email"`
	}
	if json.NewDecoder(resp.Body).Decode(&me) != nil || me.ID == "" {
		return "", "", errors.New("上游 /user/self 响应里没有 _id")
	}
	return me.ID, me.Email, nil
}

// tokenOK 浏览器里收割到的 token 先问一下上游认不认(配置目录里可能留着已登出的旧 token);网络错误时放行。
func tokenOK(token string) bool {
	_, _, err := whoami(&WebCred{Token: token})
	return !errors.Is(err, errTokenInvalid)
}

func webHeaders(cred *WebCred) map[string]string {
	h := map[string]string{
		"Authorization": "Bearer " + cred.Token,
		"Content-Type":  "application/json",
		"Origin":        "https://fish.audio",
		"Referer":       "https://fish.audio/",
	}
	if cred.TeamID != "" {
		h["x-team-id"] = cred.TeamID
	}
	if cred.WorkspaceID != "" {
		h["x-workspace-id"] = cred.WorkspaceID
	}
	return h
}

// ---------------------------------------------------------------- 基础端点

func handleHealth(w http.ResponseWriter, r *http.Request) {
	total, usable := store.Stats()
	writeJSON(w, 200, map[string]any{
		"status":               "ok",
		"api_key_configured":   os.Getenv("FISH_API_KEY") != "",
		"web_token_configured": os.Getenv("FISH_WEB_TOKEN") != "",
		"accounts_total":       total,
		"accounts_valid":       usable,
		"browser_login":        hasBrowser && !loginDisabled, // 容器里装了 chromium 也没有显示器,登录仍不可用
		"access_key_required":  accessKey != "",
		"user_api_keys":        keyCount(), // 使用者 key 数量(不回显内容)
		"push_url":             pushURL,
	})
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"models": []map[string]string{
		{"id": "drama-3", "name": "Fish Audio Drama 3", "note": "网页默认,擅长表演/多说话人"},
		{"id": "s2.1-pro", "name": "S2.1 Pro", "note": "官方默认旗舰"},
		{"id": "s2-pro", "name": "S2 Pro", "note": "支持多说话人 model_id 数组"},
		{"id": "s2.1-pro-free", "name": "S2.1 Pro Free", "note": "免费档(仅 API 模式)"},
		{"id": "s1", "name": "S1", "note": "旧模型"},
	}})
}

// ---------------------------------------------------------------- 音色(匿名可查)

func handleVoices(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query() // 查询参数原样转给上游 /model,只补分页默认值
	if q.Get("page_size") == "" {
		q.Set("page_size", "20")
	}
	if q.Get("page_number") == "" {
		q.Set("page_number", "1")
	}
	proxyJSON(w, r, "/model?"+q.Encode(), apiKeyHeaders(r))
}

func handleVoiceDetail(w http.ResponseWriter, r *http.Request) {
	proxyJSON(w, r, "/model/"+url.PathEscape(r.PathValue("id")), apiKeyHeaders(r))
}

// ---------------------------------------------------------------- 模式 1:API key(/v1/tts,扣 API 积分)

type apiTTSRequest struct {
	Text        string         `json:"text"`
	VoiceID     string         `json:"voice_id"`
	Model       string         `json:"model"`
	Format      string         `json:"format"`
	Prosody     map[string]any `json:"prosody"`
	Temperature float64        `json:"temperature"`
	TopP        float64        `json:"top_p"`
	Normalize   bool           `json:"normalize"`
	Language    string         `json:"language"`
	SampleRate  int            `json:"sample_rate"`
	Mp3Bitrate  int            `json:"mp3_bitrate"`
	Latency     string         `json:"latency"`
	ChunkLength int            `json:"chunk_length"`
	Features    []string       `json:"features"`
	Extra       map[string]any `json:"extra"`
}

func handleAPIKeyTTS(w http.ResponseWriter, r *http.Request) {
	headers := apiKeyHeaders(r)
	if headers == nil {
		fail(w, 401, "缺少 API key:设置环境变量 FISH_API_KEY 或请求头 X-Fish-Key(https://fish.audio/app/api-keys/ 创建)")
		return
	}
	in := apiTTSRequest{ // 默认值;prosody 按键合并,缺的键用默认
		Model: "s2.1-pro", Format: "mp3", Prosody: map[string]any{"speed": 1.0, "volume": 0.0},
		Temperature: 0.7, TopP: 0.7, Normalize: true, Mp3Bitrate: 128, Latency: "normal", ChunkLength: 300,
		Features: []string{},
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if in.Text == "" {
		fail(w, 400, "text 不能为空")
		return
	}
	up := map[string]any{
		"text": in.Text, "prosody": in.Prosody, "temperature": in.Temperature, "top_p": in.TopP,
		"normalize": in.Normalize, "format": in.Format, "mp3_bitrate": in.Mp3Bitrate,
		"latency": in.Latency, "chunk_length": in.ChunkLength, "features": in.Features,
	}
	if in.VoiceID != "" {
		up["reference_id"] = in.VoiceID
	}
	if in.Language != "" {
		up["language"] = in.Language
	}
	if in.SampleRate > 0 {
		up["sample_rate"] = in.SampleRate
	}
	maps.Copy(up, in.Extra)
	payload, _ := json.Marshal(up)
	headers["Content-Type"] = "application/json"
	headers["model"] = in.Model
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()
	resp, err := upRequest(ctx, "POST", "/v1/tts", headers, payload)
	if err != nil {
		fail(w, 502, "上游请求失败: "+err.Error())
		return
	}
	streamBack(w, resp, mimeOf(in.Format))
}

// ---------------------------------------------------------------- 模式 2:网页会话(/task,扣订阅套餐 credits)

type webTTSRequest struct {
	Text      string         `json:"text"`
	VoiceID   any            `json:"voice_id"` // string 或 []string(多说话人)
	Backend   string         `json:"backend"`
	Format    string         `json:"format"`
	Prosody   map[string]any `json:"prosody"`
	Latency   string         `json:"latency"`
	Normalize *bool          `json:"normalize"`
	Sampler   map[string]any `json:"sampler"`
	GroupID   string         `json:"group_id"`
	Extra     map[string]any `json:"extra"`
}

// buildTaskBody 拼网页前端同款的 POST /task 请求体。
// recaptcha 非空时带上(2026-10 起上游强制要求,见 recaptcha.go)。
func buildTaskBody(in *webTTSRequest, recaptcha string) []byte {
	params := map[string]any{"text": in.Text, "format": in.Format, "prosody": in.Prosody}
	if in.VoiceID != nil {
		params["model_id"] = in.VoiceID
	}
	if in.Latency != "" {
		params["latency"] = in.Latency
	}
	if in.Normalize != nil {
		params["normalize"] = *in.Normalize
	}
	if in.Sampler != nil {
		params["sampler"] = in.Sampler
	}
	model, _ := in.VoiceID.(string) // 顶层 model 取第一个声音
	if ids, ok := in.VoiceID.([]any); ok && len(ids) > 0 {
		model, _ = ids[0].(string)
	}
	top := map[string]any{
		"type": "tts", "stream": true, "model": model, "backend": in.Backend,
		"group_id": in.GroupID, "parameters": params,
	}
	if recaptcha != "" {
		top["recaptcha"] = recaptcha
	}
	maps.Copy(top, in.Extra)
	b, _ := json.Marshal(top)
	return b
}

// webDefaults 网页合成的默认参数(backend 跟网页一样默认 drama-3);prosody 按键合并,缺的键用默认。
func webDefaults() webTTSRequest {
	return webTTSRequest{
		Format: "mp3", Backend: "drama-3",
		Prosody: map[string]any{"speed": 1.0, "pitch": 1.0, "volume": 0.0, "normalize_loudness": true},
	}
}

func handleWebTTS(w http.ResponseWriter, r *http.Request) {
	in := webDefaults()
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	serveWebTTS(w, r, &in)
}

// postTask 发起一次 /task:自动带上 reCAPTCHA token(recaptchaToken 内部有缓存)。
func postTask(ctx context.Context, cred *WebCred, in *webTTSRequest) (*http.Response, error) {
	token, err := recaptchaToken(false)
	if err != nil {
		return nil, err
	}
	return upRequest(ctx, "POST", "/task", webHeaders(cred), buildTaskBody(in, token))
}

// isRecaptchaRejection 上游 400 且在说 reCAPTCHA 的事(缓存的 token 刚好过期/被风控)。
func isRecaptchaRejection(resp *http.Response) bool {
	if resp.StatusCode != 400 {
		return false
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if strings.Contains(strings.ToLower(string(b)), "recaptcha") {
		return true
	}
	resp.Body = io.NopCloser(bytes.NewReader(b)) // 不是验证的事:把读走的 body 放回去,后面原样回给客户端
	return false
}

// serveWebTTS 选账号、发 /task、401 时重登重试、把音频流回去;/api/web/tts 和 OpenAI 兼容接口共用。
func serveWebTTS(w http.ResponseWriter, r *http.Request, in *webTTSRequest) {
	if in.Text == "" {
		fail(w, 400, "text 不能为空")
		return
	}
	// 结果缓存:同参数命中就直接回放,不打上游、不扣额度、也不取验证分数(没指定声音的不缓存,因默认声音随账号而变)
	canCache := hasVoice(in.VoiceID)
	var key string
	if canCache {
		key = cacheKey(in)
		recOf(r).cacheKey = key // 缓存里还在时,请求记录能回放这条
		if e, ok := ttsCache.get(key); ok {
			w.Header().Set("X-Cache", "HIT")
			w.Header().Set("Content-Type", e.mime)
			_, _ = w.Write(e.data)
			return
		}
	}
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	resp, err := postTask(ctx, cred, in)
	if err != nil {
		fail(w, 502, "请求失败: "+err.Error())
		return
	}
	if isRecaptchaRejection(resp) { // 缓存的 token 刚好被上游判过期:强制换新重试一次
		logf("reCAPTCHA token 被上游拒绝,强制换新重试")
		if _, terr := recaptchaToken(true); terr != nil {
			fail(w, 502, "重新获取人机验证失败: "+terr.Error())
			return
		}
		resp, err = postTask(ctx, cred, in)
		if err != nil {
			fail(w, 502, "请求失败: "+err.Error())
			return
		}
	}
	if err == nil && resp.StatusCode == 401 && cred.Account != nil {
		// 账号库里的 token 被上游拒了:重登一次再试;重登不成就丢掉这个 token,轮询不再选它
		resp.Body.Close()
		name := cred.Account.Name
		logf("账号 %s 的 token 已失效,尝试自动重登", name)
		if cred.Token, err = store.Login(name, false); err != nil {
			store.Invalidate(name, "token 已失效(上游 401),自动重登也失败: "+err.Error())
			store.Save() // 不存的话,重启后死 token 又从文件里读回来、又被当成可用
			fail(w, 401, "token 失效且自动重登失败: "+err.Error())
			return
		}
		resp, err = postTask(ctx, cred, in)
	}
	if err != nil {
		fail(w, 502, "上游请求失败: "+err.Error())
		return
	}
	if cred.Account != nil && (resp.StatusCode == 429 || resp.StatusCode >= 500) {
		store.Cooldown(cred.Account.Name, accountCooldown())
		logf("账号 %s 上游返回 %d,冷却 %s 后再轮到它", cred.Account.Name, resp.StatusCode, accountCooldown())
	}
	if id := resp.Header.Get("task-id"); id != "" {
		w.Header().Set("X-Task-Id", id)
	}
	if canCache {
		streamBackCaching(w, resp, mimeOf(in.Format), key)
	} else {
		streamBack(w, resp, mimeOf(in.Format))
	}
}

// ---------------------------------------------------------------- OpenAI 兼容(给 new-api / 酒馆这类客户端当 TTS 模型用)

// backendAlias OpenAI 的模型名映射到 Fish backend;其他名字去掉 fish- 前缀后原样当 backend。
var backendAlias = map[string]string{"tts-1": "drama-3", "tts-1-hd": "drama-3", "gpt-4o-mini-tts": "drama-3"}

func handleOpenAIModels(w http.ResponseWriter, r *http.Request) {
	var data []map[string]any
	for _, id := range []string{"drama-3", "s2.1-pro", "s2-pro", "s1", "tts-1", "tts-1-hd"} {
		data = append(data, map[string]any{"id": id, "object": "model", "owned_by": "fish-audio"})
	}
	writeJSON(w, 200, map[string]any{"object": "list", "data": data})
}

// handleOpenAISpeech POST /v1/audio/speech:{model, input, voice, response_format, speed} → 音频。
// voice 给 Fish 声音 ID 或声音名字(按名字搜第一个匹配);走网页会话,扣订阅套餐。
func handleOpenAISpeech(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model          string  `json:"model"`
		Input          string  `json:"input"`
		Voice          string  `json:"voice"`
		ResponseFormat string  `json:"response_format"`
		Speed          float64 `json:"speed"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	req := webDefaults()
	req.Text = in.Input
	if backend := strings.TrimPrefix(in.Model, "fish-"); backend != "" {
		req.Backend = cmp.Or(backendAlias[backend], backend)
	}
	if audioMIME[in.ResponseFormat] != "" {
		req.Format = in.ResponseFormat
	}
	if in.Speed > 0 {
		req.Prosody["speed"] = in.Speed
	}
	if in.Voice != "" {
		id, err := resolveVoice(r.Context(), in.Voice)
		if err != nil {
			fail(w, 400, err.Error())
			return
		}
		req.VoiceID = id
	}
	serveWebTTS(w, r, &req)
}

var voiceCache sync.Map // 声音名字 → 声音 ID

// resolveVoice 把客户端给的 voice 变成 Fish 声音 ID:本身是 32 位 hex 就直接用,否则按名字搜一次并缓存。
func resolveVoice(ctx context.Context, voice string) (string, error) {
	if len(voice) == 32 && strings.Trim(voice, "0123456789abcdef") == "" {
		return voice, nil
	}
	if id, ok := voiceCache.Load(voice); ok {
		return id.(string), nil
	}
	resp, err := upRequest(ctx, "GET", "/model?page_size=1&title="+url.QueryEscape(voice), nil, nil)
	if err != nil {
		return "", fmt.Errorf("搜索声音失败: %w", err)
	}
	defer resp.Body.Close()
	var res struct {
		Items []struct {
			ID    string `json:"_id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	if json.NewDecoder(resp.Body).Decode(&res) != nil || len(res.Items) == 0 {
		return "", fmt.Errorf("找不到叫「%s」的声音,可以直接给 32 位声音 ID", voice)
	}
	// 上游搜索是模糊的,瞎写的名字也能搜出东西;给的名字不是标题的一部分就当没找到,免得悄悄用错声音扣积分
	top := res.Items[0]
	if !strings.Contains(strings.ToLower(top.Title), strings.ToLower(voice)) {
		return "", fmt.Errorf("找不到叫「%s」的声音(最接近的是「%s」%s),请用准确的名字或 32 位声音 ID", voice, top.Title, top.ID)
	}
	logf("声音「%s」→ %s(%s)", voice, top.ID, top.Title)
	voiceCache.Store(voice, top.ID)
	return top.ID, nil
}

// ---------------------------------------------------------------- 网页:余额 / 任务状态

func handleWebCredit(w http.ResponseWriter, r *http.Request) {
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	uid := cred.UserID
	if uid == "" { // 账号库里还没记 user_id,或者是请求头直接给的 token
		var err error
		if uid, _, err = whoami(cred); err != nil {
			fail(w, 401, "查不到这个 token 的用户: "+err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	h := webHeaders(cred)
	var pkg, apiCredit any // 两个钱包并行查,省一个来回
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); pkg = wallet(ctx, "/wallet/"+uid+"/package", h) }()          // 订阅套餐(网页用)
	go func() { defer wg.Done(); apiCredit = wallet(ctx, "/wallet/"+uid+"/api-credit", h) }() // API 积分(API key 用)
	wg.Wait()
	writeJSON(w, 200, map[string]any{"user_id": uid, "package": pkg, "api_credit": apiCredit})
}

// wallet 查一个钱包,返回上游 JSON;失败时返回 {error} 或 {status, body}。
func wallet(ctx context.Context, path string, headers map[string]string) any {
	resp, err := upRequest(ctx, "GET", path, headers, nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if !json.Valid(b) {
		b, _ = json.Marshal(string(b))
	}
	if resp.StatusCode != 200 {
		return map[string]any{"status": resp.StatusCode, "body": json.RawMessage(b)}
	}
	return json.RawMessage(b)
}

func handleWebTask(w http.ResponseWriter, r *http.Request) {
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	proxyJSON(w, r, "/task/"+url.PathEscape(r.PathValue("task_id")), webHeaders(cred))
}

// ---------------------------------------------------------------- 账号管理

func handleAccountList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"accounts": store.Views()})
}

func handleAccountAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Account          // name / email / password / token / team_id / workspace_id
		Interactive bool `json:"interactive"` // 保存后立刻弹窗人工登录
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if in.Email == "" && in.Token == "" {
		fail(w, 400, "email 或 token 至少提供一个")
		return
	}
	in.Email = cmp.Or(in.Email, "manual-"+strconv.FormatInt(time.Now().Unix(), 10))
	acc, err := store.Add(in.Account)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	store.Save()
	// 登录策略:给了 token 直接可用(并推送);要求交互或续期方式是 OAuth(第一次必须人工登一次)→ 弹窗;
	// 有密码 → 自动填表试一次;否则等用户手动登录
	switch {
	case acc.Token != "":
		if !verifyAndPush(w, acc.Name) {
			return
		}
	case in.Interactive || acc.Provider != "":
		_, err = store.Login(acc.Name, true)
	case in.Password != "":
		_, err = store.Login(acc.Name, false)
	}
	respondAccount(w, acc.Name, err)
}

// verifyAndPush 粘贴/推送来的 token 先问上游认不认:不认就当场报 400,认就推送到服务器。
func verifyAndPush(w http.ResponseWriter, name string) bool {
	acc, err := store.Verify(name)
	if errors.Is(err, errTokenInvalid) {
		fail(w, 400, "这个 token 上游不认(401):可能复制错了,或者那个浏览器已经退出登录")
		return false
	}
	go pushToken(acc)
	return true
}

// handleAccountPatch 改账号设置,目前只有 provider(自动续期方式)。
func handleAccountPatch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider string `json:"provider"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if in.Provider != "" && in.Provider != "github" && in.Provider != "google" {
		fail(w, 400, "provider 只能是空、github 或 google")
		return
	}
	name := r.PathValue("name")
	if !store.SetProvider(name, in.Provider) {
		fail(w, 404, "账号不存在: "+name)
		return
	}
	store.Save()
	writeJSON(w, 200, store.View(name))
}

// handleAccountToken 直接给账号一个新 token(面板粘贴 / 本地实例推送),账号不存在就新建。
func handleAccountToken(w http.ResponseWriter, r *http.Request) {
	var in Account
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	in.Name, in.Password = r.PathValue("name"), "" // 名字以路径为准;这条路不收密码
	acc, err := store.Upsert(in)
	if err != nil {
		fail(w, 400, err.Error())
		return
	}
	store.Save()
	if verifyAndPush(w, acc.Name) {
		writeJSON(w, 200, store.View(acc.Name))
	}
}

func handleAccountLogin(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	acc := store.Get(name)
	if acc == nil {
		fail(w, 404, "账号不存在: "+name)
		return
	}
	q := r.URL.Query().Get("interactive")
	// 没存密码且没设续期方式时,唯一出路是交互登录;设了 provider 就按非交互走(服务器上是无头 OAuth 续期)
	_, err := store.Login(name, q == "1" || q == "true" || (acc.Password == "" && acc.Provider == ""))
	respondAccount(w, name, err)
}

// respondAccount 返回账号视图;登录失败时附带 warning(账号本身已保存,HTTP 仍为 200)。
func respondAccount(w http.ResponseWriter, name string, err error) {
	view := store.View(name)
	if err != nil {
		view["warning"] = "登录失败: " + err.Error()
	}
	writeJSON(w, 200, view)
}

func handleAccountDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !store.Remove(name) {
		fail(w, 404, "账号不存在: "+name)
		return
	}
	store.Save()
	writeJSON(w, 200, map[string]any{"deleted": name})
}

// ---------------------------------------------------------------- 启动

func listenAddr() string {
	return cmp.Or(os.Getenv("HOST"), "127.0.0.1") + ":" + cmp.Or(os.Getenv("PORT"), "8765")
}

func main() {
	log.SetOutput(io.MultiWriter(os.Stderr, serverLog)) // 日志抄一份给面板「运行日志」页
	loadAdmin()                                         // 面板里改过的设置、使用者 key、请求记录(见 admin.go)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", handleUI)
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /api/models", handleModels)
	mux.HandleFunc("GET /api/voices", handleVoices)
	mux.HandleFunc("GET /api/voices/{id}", handleVoiceDetail)
	mux.HandleFunc("POST /api/tts", logged("api", handleAPIKeyTTS))
	mux.HandleFunc("POST /api/web/tts", logged("web", handleWebTTS))
	mux.HandleFunc("GET /api/web/credit", handleWebCredit)
	mux.HandleFunc("GET /api/web/task/{task_id}", handleWebTask)
	mux.HandleFunc("GET /api/accounts", handleAccountList)
	mux.HandleFunc("POST /api/accounts", handleAccountAdd)
	mux.HandleFunc("POST /api/accounts/{name}/login", handleAccountLogin)
	mux.HandleFunc("PATCH /api/accounts/{name}", handleAccountPatch)
	mux.HandleFunc("PUT /api/accounts/{name}/token", handleAccountToken)
	mux.HandleFunc("DELETE /api/accounts/{name}", handleAccountDelete)
	mux.HandleFunc("GET /api/web/media/models", handleWebMediaModels)
	mux.HandleFunc("GET /api/web/media/entitlements", handleWebMediaEntitlements)
	mux.HandleFunc("POST /api/web/media", logged("media", handleWebMediaGenerate))
	mux.HandleFunc("GET /api/web/media/{group_id}", handleWebMediaStatus)
	mux.HandleFunc("GET /api/web/media/file", handleWebMediaFile)
	mux.HandleFunc("GET /v1/models", handleOpenAIModels)
	mux.HandleFunc("POST /v1/audio/speech", logged("openai", handleOpenAISpeech))
	keyRoutes(mux)
	adminRoutes(mux)

	go store.RefreshLoop() // 始终后台验证 token 有效性、踢掉死 token;有浏览器的机器还会自动重登(见 RefreshLoop)
	Warmup()               // 预热收割浏览器并取一个 token,让首个合成请求免付冷启动(见 recaptcha.go)
	if !hasBrowser {
		logf("本机没有 Chrome/Edge:登录功能关闭,只接收推送/粘贴的 token(服务器模式);token 仍会定期向上游验证")
	}
	if pushURL != "" { // 本地实例启动时把手头可用的 token 都同步一遍
		go func() {
			for _, a := range store.Usable() {
				pushToken(a)
			}
		}()
	}

	addr := listenAddr()
	path, _ := filepath.Abs(store.path)
	logf("Fish Audio TTS API(Go)监听 http://%s,账号库 %s", addr, path)
	log.Fatal(http.ListenAndServe(addr, guard(mux)))
}
