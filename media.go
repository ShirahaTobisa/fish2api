// 图像/视频生成:封装 fish.audio 网页版「图像与视频」工具的上游接口。
//
// 协议(2026-10 逆向 + 实测,不需要 reCAPTCHA,与 TTS /task 不同):
//
//	POST /tools/image-video/price   计价:{modelId,variantKey,mode,count,params}
//	                                → {credits, perItemCredits, pricingVersion, contract}
//	POST /tools/image-video/generate 下单:计价字段 + prompt + clientRequestId
//	                                + expectedCredits + pricingVersion
//	                                → {id(group), results[{id,status,mediaItem}]}
//	GET  /task/group/{group_id}     轮询:status / results[i].status(done) /
//	                                results[i].mediaItem.url(R2 地址,带 token 才能下载)
//
// params 是 snake_case 且必须与计价档位精确匹配,常用:
//
//	文生图 t2i:{"aspect_ratio":"1:1","image_size":"1K"}(nano-banana)
//	文生视频 t2v:{"aspect_ratio":"16:9","resolution":"720P","duration":5}
//	图生视频 i2v:额外要求 startFrame(参考图),暂时只支持 t2i/t2v 纯文本输入。
//
// 资格(实测):图像各套餐可用(free 每天 3 张);视频要 plus 套餐(videoAllowed)。
package main

import (
	"bytes"
	"cmp"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// mediaModels 拉取上游可用模型列表(带可用性与 mode 过滤),原样透传给调用方/面板。
func mediaModels(ctx context.Context, cred *WebCred) (int, []byte, error) {
	resp, err := upRequest(ctx, "GET", "/tools/image-video/models", webHeaders(cred), nil)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, nil
}

// mediaEntitlements 查当前账号的图像/视频资格与每日额度。
func mediaEntitlements(ctx context.Context, cred *WebCred) (int, []byte, error) {
	resp, err := upRequest(ctx, "GET", "/tools/image-video/entitlements", webHeaders(cred), nil)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// mediaPriceBody 计价/下单共用的字段(上游两者请求体同形)。
type mediaPriceBody struct {
	ModelID     string         `json:"modelId"`
	VariantKey  string         `json:"variantKey"`
	Mode        string         `json:"mode"` // image | video
	Count       int            `json:"count"`
	Params      map[string]any `json:"params"`
	DurationSec int            `json:"durationSec,omitempty"`
}

// mediaPrice 计价:credits 与 pricingVersion 是下单必带(expectedCredits/pricingVersion)。
func mediaPrice(ctx context.Context, cred *WebCred, body mediaPriceBody) (int, []byte, error) {
	payload, _ := json.Marshal(body)
	resp, err := upRequest(ctx, "POST", "/tools/image-video/price", webHeaders(cred), payload)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, b, nil
}

// handleWebMediaGenerate POST /api/web/media:图像/视频生成(异步)。
// 请求体:{model, variant, mode, count, params, prompt, wait?(秒,可选同步等待)}
// 流程:计价 → 下单(带 expectedCredits 锁价)→ 返回 group_id;
// wait>0 时轮询到完成,把媒体 URL 直接放响应里;否则拿 group_id 自己查。
func handleWebMediaGenerate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model   string         `json:"model"`
		Variant string         `json:"variant"`
		Mode    string         `json:"mode"`
		Count   int            `json:"count"`
		Params  map[string]any `json:"params"`
		Prompt  string         `json:"prompt"`
		Wait    int            `json:"wait"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if in.Model == "" || in.Variant == "" || in.Prompt == "" {
		fail(w, 400, "model / variant / prompt 必填(variant 如 t2i、t2v;可用模型见 /api/web/media/models)")
		return
	}
	if in.Mode == "" {
		in.Mode = "image"
		if in.Variant == "t2v" || in.Variant == "i2v" {
			in.Mode = "video"
		}
	}
	if in.Count < 1 {
		in.Count = 1
	}
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	// 1) 计价(上游要求下单时带 expectedCredits 与 pricingVersion 锁价)
	priceBody := mediaPriceBody{ModelID: in.Model, VariantKey: in.Variant, Mode: in.Mode, Count: in.Count, Params: in.Params}
	status, pb, err := mediaPrice(ctx, cred, priceBody)
	if err != nil {
		fail(w, 502, "计价请求失败: "+err.Error())
		return
	}
	if status != 200 {
		mediaError(w, status, pb)
		return
	}
	var price struct {
		Credits        int    `json:"credits"`
		PricingVersion string `json:"pricingVersion"`
	}
	if json.Unmarshal(pb, &price) != nil || price.Credits == 0 {
		fail(w, 502, "计价响应解析失败: "+string(pb))
		return
	}

	// 2) 下单
	genBody := map[string]any{
		"modelId":         in.Model,
		"variantKey":      in.Variant,
		"mode":            in.Mode,
		"count":           in.Count,
		"params":          in.Params,
		"prompt":          in.Prompt,
		"clientRequestId": uuid.NewString(),
		"expectedCredits": price.Credits,
		"pricingVersion":  price.PricingVersion,
	}
	payload, _ := json.Marshal(genBody)
	resp, err := upRequest(ctx, "POST", "/tools/image-video/generate", webHeaders(cred), payload)
	if err != nil {
		fail(w, 502, "下单请求失败: "+err.Error())
		return
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	if resp.StatusCode != 200 {
		// 上游 4xx 的 body 形如 {"code":"FREE_IMAGE_DAILY_LIMIT_REACHED",...},转成易读 detail
		var ue struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(b, &ue) != nil || ue.Code == "" {
			mediaError(w, resp.StatusCode, b)
			return
		}
		writeJSON(w, resp.StatusCode, map[string]any{
			"detail": ue.Code + ": " + cmp.Or(ue.Message, "上游拒绝(额度/资格/参数)"),
			"code":   ue.Code,
		})
		return
	}
	var group struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(b, &group) != nil || group.ID == "" {
		fail(w, 502, "下单响应里没有任务 id: "+string(b))
		return
	}

	// 3) wait>0:同步轮询到完成(方便 curl 一把梭);否则立刻返回 group_id
	if in.Wait <= 0 {
		writeJSON(w, 200, map[string]any{
			"group_id": group.ID, "status": "generating",
			"credits_charged": price.Credits,
			"poll":            "GET /api/web/media/" + group.ID,
		})
		return
	}
	result := pollMediaGroup(r.Context(), cred, group.ID, time.Duration(in.Wait)*time.Second)
	writeJSON(w, 200, map[string]any{
		"group_id": group.ID, "credits_charged": price.Credits, "result": result,
	})
}

// handleWebMediaStatus GET /api/web/media/{group_id}:查任务状态/结果。
func handleWebMediaStatus(w http.ResponseWriter, r *http.Request) {
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	wait, _ := time.ParseDuration(r.URL.Query().Get("wait") + "s")
	if wait <= 0 {
		wait = 5 * time.Second
	}
	if wait > 5*time.Minute {
		wait = 5 * time.Minute
	}
	writeJSON(w, 200, pollMediaGroup(r.Context(), cred, r.PathValue("group_id"), wait))
}

// pollMediaGroup 轮询任务组到完成或超时;返回上游最终 JSON。
func pollMediaGroup(ctx context.Context, cred *WebCred, groupID string, maxWait time.Duration) any {
	deadline := time.Now().Add(maxWait)
	for {
		resp, err := upRequest(ctx, "GET", "/task/group/"+groupID, webHeaders(cred), nil)
		if err != nil {
			return map[string]any{"error": err.Error()}
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return map[string]any{"status": resp.StatusCode, "body": json.RawMessage(validMediaJSON(b))}
		}
		var d struct {
			Status  string `json:"status"`
			Results []struct {
				Status    string `json:"status"`
				MediaItem *struct {
					URL  string `json:"url"`
					Type string `json:"type"`
				} `json:"mediaItem"`
				FailureReason string `json:"failureReason"`
			} `json:"results"`
		}
		if json.Unmarshal(b, &d) != nil {
			return map[string]any{"error": "轮询响应解析失败"}
		}
		done := len(d.Results) > 0
		for _, r := range d.Results {
			if r.Status != "done" && r.Status != "failed" {
				done = false
			}
		}
		if done || time.Now().After(deadline) {
			return json.RawMessage(signMediaURLs(b))
		}
		select {
		case <-ctx.Done():
			return map[string]any{"error": "等待超时"}
		case <-time.After(4 * time.Second):
		}
	}
}

// handleWebMediaFile GET /api/web/media/file?url=<R2地址>&sig=<签名>:代理下载结果文件;
// guard 里用 ?sig= 签名放行(15 分钟有效,不需要 Authorization 头)。
func handleWebMediaFile(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	resp, status, err := fetchMediaFile(ctx, r, r.URL.Query().Get("url"))
	if err != nil {
		fail(w, status, err.Error())
		return
	}
	streamBack(w, resp, cmp.Or(resp.Header.Get("Content-Type"), "application/octet-stream"))
}

// fetchMediaFile 带账号 token 取结果文件:R2 的 URL 要登录态才能下,浏览器 <img> 和 API 客户端都直接拿不到,
// 所以由本服务代取。出错时返回该回给调用方的状态码。
func fetchMediaFile(ctx context.Context, r *http.Request, rawURL string) (*http.Response, int, error) {
	if !strings.HasPrefix(rawURL, "https://platform.r2.fish.audio/") {
		return nil, 400, errors.New("缺少合法的 url 参数(仅允许 platform.r2.fish.audio 的结果文件)")
	}
	cred, err := resolveWebCred(r)
	if err != nil {
		return nil, 401, err
	}
	dl, err := http.NewRequestWithContext(ctx, "GET", rawURL, nil)
	if err != nil {
		return nil, 502, err
	}
	dl.Header.Set("Authorization", "Bearer "+cred.Token)
	resp, err := http.DefaultClient.Do(dl)
	if err != nil {
		return nil, 502, fmt.Errorf("下载失败: %w", err)
	}
	return resp, 200, nil
}

// mediaError 上游非 200 的 JSON 错误体原样透传给调用方。
func mediaError(w http.ResponseWriter, status int, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if json.Valid(body) {
		_, _ = w.Write(body)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"message": string(body)})
}

// validMediaJSON 非 JSON 字节转成 JSON 字符串,方便塞进响应体。
func validMediaJSON(b []byte) []byte {
	if json.Valid(b) {
		return b
	}
	c, _ := json.Marshal(string(b))
	return c
}

// handleWebMediaModels GET /api/web/media/models:上游图像/视频模型列表(带 available 与 modes)。
func handleWebMediaModels(w http.ResponseWriter, r *http.Request) {
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	status, b, err := mediaModels(ctx, cred)
	if err != nil {
		fail(w, 502, "上游请求失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// handleWebMediaEntitlements GET /api/web/media/entitlements:资格与每日额度。
func handleWebMediaEntitlements(w http.ResponseWriter, r *http.Request) {
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	status, b, err := mediaEntitlements(ctx, cred)
	if err != nil {
		fail(w, 502, "上游请求失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}

// ---------------------------------------------------------------- 媒体签名(<img> 标签下载用)

// mediaSign 生成 "过期时间.HMAC(path,过期时间)" 签名:<img> 标签带不了 Authorization,
// 面板用签名 URL 让浏览器直接加载;签名只对单个路径 + 15 分钟有效。
func mediaSign(path string) string {
	exp := fmt.Sprintf("%d", time.Now().Add(15*time.Minute).Unix())
	mac := hmacSHA256(mediaSignSecret(), exp+"|"+path)
	return exp + "." + mac
}

// mediaSignVerify 校验签名:路径一致且未过期。
func mediaSignVerify(path, sig string) bool {
	dot := strings.LastIndex(sig, ".")
	if dot <= 0 {
		return false
	}
	exp, mac := sig[:dot], sig[dot+1:]
	var expInt int64
	if _, err := fmt.Sscanf(exp, "%d", &expInt); err != nil || time.Now().Unix() > expInt {
		return false
	}
	expect := hmacSHA256(mediaSignSecret(), exp+"|"+path)
	return subtleCmp(mac, expect)
}

func mediaSignSecret() string {
	if s := os.Getenv("MEDIA_SIGN_SECRET"); s != "" {
		return s
	}
	return "fish2api-media|" + os.Getenv("ACCESS_KEY") // 没单独配时从管理员密钥派生(面板里换了密钥,之前发的签名链接随之失效)
}

func hmacSHA256(secret, msg string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(msg))
	return hex.EncodeToString(m.Sum(nil))
}

func subtleCmp(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// signMediaURLs 给结果 JSON 里每个 mediaItem.url 附一个签名下载地址 sigUrl(15 分钟有效),
// 面板的 <img>/<video> 标签直接用它,绕过 Authorization 头的限制。
func signMediaURLs(body []byte) []byte {
	var d map[string]any
	if json.Unmarshal(body, &d) != nil {
		return body
	}
	results, _ := d["results"].([]any)
	for i, r := range results {
		item, ok := r.(map[string]any)
		if !ok {
			continue
		}
		mi, ok := item["mediaItem"].(map[string]any)
		if !ok {
			continue
		}
		// 签名对象 = 原始未解码的 RequestURI(guard 校验端用 r.URL.RequestURI(),两边字节一致)
		raw := "/api/web/media/file?url=" + url.QueryEscape(mi["url"].(string))
		mi["sigUrl"] = raw + "&sig=" + mediaSign(raw)
		_ = i
	}
	out, err := json.Marshal(d)
	if err != nil {
		return body
	}
	return out
}

// ---------------------------------------------------------------- OpenAI 图像接口(给 new-api 这类客户端用)

// fishImageOpts 文生图的 Fish 参数,和 /api/web/media 的 params 同义。放顶层或 extra_fields 里都行:
// 经 new-api 转发 /v1/images/generations 时只有 extra_fields 会原样保留。
type fishImageOpts struct {
	AspectRatio string         `json:"aspect_ratio"` // 不给就按 size 取最接近的比例
	ImageSize   string         `json:"image_size"`   // 1K / 2K…;不给就看 quality(high / hd 用 2K)
	Params      map[string]any `json:"params"`       // 其他计价档位参数,原样带上
}

// handleOpenAIImages POST /v1/images/generations:OpenAI 格式的文生图。内部走 /api/web/media 同一套
// (计价 → 下单 → 等结果),再换成 OpenAI 的 {created, data:[{url} | {b64_json}]}。
// model 是 Fish 的图像模型(fish- 前缀可带可不带);url 是本服务 15 分钟有效的签名下载链接。
func handleOpenAIImages(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model          string        `json:"model"`
		Prompt         string        `json:"prompt"`
		N              int           `json:"n"`
		Size           string        `json:"size"`
		Quality        string        `json:"quality"`
		ResponseFormat string        `json:"response_format"`
		ExtraFields    fishImageOpts `json:"extra_fields"`
		fishImageOpts
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	imageSize := "1K"
	if in.Quality == "high" || in.Quality == "hd" {
		imageSize = "2K"
	}
	params := map[string]any{
		"aspect_ratio": cmp.Or(in.ExtraFields.AspectRatio, in.AspectRatio, aspectOf(in.Size)),
		"image_size":   cmp.Or(in.ExtraFields.ImageSize, in.ImageSize, imageSize),
	}
	maps.Copy(params, in.Params)
	maps.Copy(params, in.ExtraFields.Params)
	body, _ := json.Marshal(map[string]any{
		"model": strings.TrimPrefix(cmp.Or(in.Model, "nano-banana"), "fish-"), "variant": "t2i", "mode": "image",
		"count": max(in.N, 1), "params": params, "prompt": in.Prompt, "wait": 240,
	})
	// 借 handleWebMediaGenerate 走完整流程(选账号、错误格式、请求记录都一样),结果录下来再换格式
	rec := httptest.NewRecorder()
	inner := r.Clone(r.Context())
	inner.Body = io.NopCloser(bytes.NewReader(body))
	handleWebMediaGenerate(rec, inner)
	if rec.Code != 200 {
		w.Header().Set("Content-Type", rec.Header().Get("Content-Type"))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
		return
	}
	var out struct {
		GroupID string `json:"group_id"`
		Result  struct {
			Results []struct {
				FailureReason string `json:"failureReason"`
				MediaItem     *struct {
					URL    string `json:"url"`
					SigURL string `json:"sigUrl"`
				} `json:"mediaItem"`
			} `json:"results"`
		} `json:"result"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	// 走到这里已经下单扣了额度:之后出错只回 400(生成失败)或 504(超时、取不到图),
	// new-api 默认对这两个不重试,免得它再下一单、重复扣费
	data, reason := []map[string]string{}, ""
	for _, x := range out.Result.Results {
		if x.MediaItem == nil {
			reason = cmp.Or(x.FailureReason, reason)
			continue
		}
		if in.ResponseFormat != "b64_json" {
			data = append(data, map[string]string{"url": publicBase(r) + x.MediaItem.SigURL})
			continue
		}
		resp, _, err := fetchMediaFile(r.Context(), r, x.MediaItem.URL)
		if err != nil {
			fail(w, 504, "图片已生成,但取图失败: "+err.Error())
			return
		}
		b, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || resp.StatusCode != 200 {
			fail(w, 504, fmt.Sprintf("图片已生成,但取图失败: HTTP %d", resp.StatusCode))
			return
		}
		data = append(data, map[string]string{"b64_json": base64.StdEncoding.EncodeToString(b)})
	}
	switch {
	case len(data) > 0:
		writeJSON(w, 200, map[string]any{"created": time.Now().Unix(), "data": data})
	case reason != "":
		fail(w, 400, "图片没生成出来: "+reason)
	default:
		fail(w, 504, "等了 4 分钟还没出图,任务号 "+out.GroupID+",可以用 GET /api/web/media/任务号 再查")
	}
}

// aspectOf 把 OpenAI 的 size(如 1792x1024)换成最接近的常用比例;没给或认不出就 1:1。
func aspectOf(size string) string {
	ws, hs, ok := strings.Cut(size, "x")
	sw, err1 := strconv.ParseFloat(ws, 64)
	sh, err2 := strconv.ParseFloat(hs, 64)
	if !ok || err1 != nil || err2 != nil || sw <= 0 || sh <= 0 {
		return "1:1"
	}
	best, diff := "1:1", math.Inf(1)
	for _, a := range []string{"1:1", "16:9", "9:16", "4:3", "3:4", "3:2", "2:3"} {
		xs, ys, _ := strings.Cut(a, ":")
		x, _ := strconv.ParseFloat(xs, 64)
		y, _ := strconv.ParseFloat(ys, 64)
		if d := math.Abs(math.Log(sw/sh) - math.Log(x/y)); d < diff {
			best, diff = a, d
		}
	}
	return best
}

// publicBase 调用方看到的本服务地址(经反代时取 X-Forwarded-Proto / X-Forwarded-Host),把下载链接写成完整网址。
func publicBase(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return cmp.Or(r.Header.Get("X-Forwarded-Proto"), scheme) + "://" + cmp.Or(r.Header.Get("X-Forwarded-Host"), r.Host)
}

var imgModels struct {
	sync.Mutex
	ids []string
	at  time.Time
}

// imageModels 账号能用的文生图模型,加 fish- 前缀(gpt-image-2 这类名字容易和 new-api 里别的渠道撞);
// 缓存 10 分钟,查不到就不列。topaz/… 这类要先给原图的工具不列。
func imageModels(r *http.Request) []string {
	imgModels.Lock()
	defer imgModels.Unlock()
	if time.Since(imgModels.at) < 10*time.Minute {
		return imgModels.ids
	}
	cred, err := resolveWebCred(r)
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	status, b, err := mediaModels(ctx, cred)
	var list []struct {
		ID        string   `json:"id"`
		Available bool     `json:"available"`
		Modes     []string `json:"modes"`
	}
	if err != nil || status != 200 || json.Unmarshal(b, &list) != nil {
		return nil
	}
	ids := []string{}
	for _, m := range list {
		if m.Available && slices.Contains(m.Modes, "image") && !strings.Contains(m.ID, "/") {
			ids = append(ids, "fish-"+m.ID)
		}
	}
	imgModels.ids, imgModels.at = ids, time.Now()
	return ids
}

// handleWebMediaSchema GET /api/web/media/schema:内嵌的参数档位表
// (每个模型的 mode、variant、参数选项、默认值;来源见 media_schema.go)。
func handleWebMediaSchema(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(mediaSchemaJSON)
}

// handleWebMediaPrice POST /api/web/media/price:计价预览(不下单不扣费)。
// 请求体与 /api/web/media 相同(model/variant/mode/count/params),返回 credits 与 pricingVersion。
func handleWebMediaPrice(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Model   string         `json:"model"`
		Variant string         `json:"variant"`
		Mode    string         `json:"mode"`
		Count   int            `json:"count"`
		Params  map[string]any `json:"params"`
		Wait    int            `json:"wait"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if in.Count < 1 {
		in.Count = 1
	}
	body := mediaPriceBody{ModelID: in.Model, VariantKey: in.Variant, Mode: in.Mode, Count: in.Count, Params: in.Params}
	cred, err := resolveWebCred(r)
	if err != nil {
		fail(w, 401, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	status, b, err := mediaPrice(ctx, cred, body)
	if err != nil {
		fail(w, 502, "计价请求失败: "+err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
