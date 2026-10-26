// 人机验证(reCAPTCHA Enterprise)token 的收割与缓存。
//
// 2026-10 起网页合成 POST /task 每次都必须带 recaptcha 字段(缺失 400 "Missing reCAPTCHA
// Enterprise",伪造 400 "reCAPTCHA verification failed")。网页前端的流程是在页面里跑
// grecaptcha.enterprise.execute(SITEKEY, {action:"playground"}) 拿 token 再随请求提交。
//
// 因此这里用 rod 在无头浏览器里打开一个 fish.audio 页面,执行同一段脚本收割 token。
// 实测要点:
//   - 不需要任何登录态,任何 fish.audio 页面都能执行,headless 即可通过(评分够);
//   - token 约 120 秒有效期,期内可复用(同一 token 连续两次 /task 都 200),所以缓存到 100 秒;
//   - 收割耗时约 1~2 秒,浏览器常驻,token 临期才换新。
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

const (
	recaptchaSitekey = "6LfR4RwqAAAAAEvptRw9zohw7HeDU6NCqtAnJk1i" // fish.audio 全站同一个 key
	recaptchaAction  = "playground"                               // 网页合成用的 action(前端 beta 代码里 Va("playground"))
)

// 缓存时长:官方有效期约 120s,默认保守用 100s;可用 RECAPTCHA_TTL_SECONDS 调小做测试(面板里能改,所以现读)。
func recaptchaTTL() time.Duration {
	return time.Duration(envInt("RECAPTCHA_TTL_SECONDS", 100)) * time.Second
}

var (
	recaptchaMu   sync.Mutex
	recaptchaTok  string
	recaptchaAt   time.Time
	recaptchaPage *rod.Page
	recaptchaKill func()
)

// recaptchaToken 返回一个可用的 reCAPTCHA token,临期自动换新;force=true 强制重新收割。
// 需要本机有 Chrome/Edge(在无头浏览器里收割,与账号登录态无关)。
func recaptchaToken(force bool) (string, error) {
	recaptchaMu.Lock()
	defer recaptchaMu.Unlock()
	if !force && recaptchaTok != "" && time.Since(recaptchaAt) < recaptchaTTL() {
		return recaptchaTok, nil
	}
	token, err := harvestRecaptcha()
	if err != nil { // 页面/浏览器可能被导航或关掉了,重建一次再试
		resetRecaptcha()
		if token, err = harvestRecaptcha(); err != nil {
			return "", err
		}
	}
	recaptchaTok, recaptchaAt = token, time.Now()
	return token, nil
}

// recaptchaProfile 收割浏览器的持久化配置目录。
// 注意(Windows 特有):Chromium 用全新配置目录首次启动时,会拿空密码试登一次 Windows 账户,
// 账户有密码就会在安全日志里记一条 4625 登录失败;结果缓存在配置目录里,之后不再试。
// 所以这里用固定目录而不是每次临时目录,把这件事限制在只发生一次;Linux/容器没有这个行为。
var recaptchaDir = filepath.Join(profileRoot, "_recaptcha")

// harvestRecaptcha 在常驻页面里执行 reCAPTCHA;页面不可用就重建。
func harvestRecaptcha() (string, error) {
	if recaptchaPage != nil {
		if info, e := recaptchaPage.Info(); e == nil && info != nil {
			t0 := time.Now()
			tok, err := execRecaptcha(recaptchaPage)
			if err == nil {
				logf("reCAPTCHA token 收割成功(%.1fs,长度 %d)", time.Since(t0).Seconds(), len(tok))
				return tok, nil
			}
			logf("复用收割页面失败(%.1fs): %v", time.Since(t0).Seconds(), err)
		}
	}
	resetRecaptcha()
	if !hasBrowser && !downloadBrowser() {
		return "", errors.New(
			"网页合成每次都要 reCAPTCHA 人机验证,需要在无头浏览器里自动获取,但没有找到浏览器;" +
				"安装 Chrome/Edge/Chromium,或设 RECAPTCHA_ALLOW_DOWNLOAD=1 允许 rod 自动下载无头内核," +
				"或改用 API key 模式(/api/tts)")
	}
	// RECAPTCHA_HEADED=1 可改用有头窗口收割(排查风控问题时用,平时不需要)
	headed := os.Getenv("RECAPTCHA_HEADED") == "1"
	b, cleanup, dir, err := openRecaptchaBrowser(headed)
	if err != nil {
		return "", err
	}
	// 整条收割链路 150 秒硬超时:CDP 调用在浏览器抽风时可能永远不返回,必须兜住
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()
	// 候选收割页:sitekey 只绑定域名不挑页面,轻量纯文本页(robots.txt)加载 1 秒内、
	// 常驻时后台零 CPU;不行再退回完整 SPA 页(加载要十几秒)
	harvestPages := []string{"https://fish.audio/robots.txt", "https://fish.audio/auth/"}
	var page *rod.Page
	var tok string
	var lastErr error
	for _, u := range harvestPages {
		p, err := b.Context(ctx).Page(proto.TargetCreateTarget{URL: u})
		if err != nil {
			lastErr = fmt.Errorf("创建页面失败(%s): %w", u, err)
			continue
		}
		p = p.Context(ctx)
		normalizeUA(p) // 无头 Chrome 的 UA 带 "HeadlessChrome",抹掉这个明显标记
		if _, err := p.Timeout(20 * time.Second).Element("body"); err != nil {
			lastErr = fmt.Errorf("页面加载失败(%s): %w", u, err)
			_ = p.Close()
			continue
		}
		tok, err = execRecaptcha(p)
		if err != nil {
			lastErr = fmt.Errorf("收割失败(%s): %w", u, err)
			if shot, serr := p.Screenshot(true, nil); serr == nil {
				_ = os.WriteFile(filepath.Join(profileRoot, "recaptcha-debug.png"), shot, 0o644)
			}
			_ = p.Close()
			continue
		}
		logf("[recaptcha] 收割页:%s", u)
		page = p
		break
	}
	if page == nil {
		cleanup()
		return "", lastErr
	}
	kill := func() {
		cancel()
		cleanup()
		if dir != recaptchaDir {
			_ = os.RemoveAll(dir)
		}
	}
	recaptchaPage, recaptchaKill = page, kill
	return tok, nil
}

// openRecaptchaBrowser 打开收割浏览器:优先持久化目录;目录被强杀残留的孤儿进程锁住时
// 清掉重建(丢一次空密码检查的缓存);实在不行退回一次性临时目录。
func openRecaptchaBrowser(headed bool) (b *rod.Browser, cleanup func(), dir string, err error) {
	if b, cleanup, err = newBrowser(recaptchaDir, !headed); err == nil {
		return b, cleanup, recaptchaDir, nil
	}
	firstErr := err
	_ = os.RemoveAll(recaptchaDir) // 锁也可能来自目录里的残留文件,能删多少删多少
	if b, cleanup, err = newBrowser(recaptchaDir, !headed); err == nil {
		logf("recaptcha 配置目录曾被锁住(疑似强杀残留),已重建: %v", firstErr)
		return b, cleanup, recaptchaDir, nil
	}
	// 最后的退路:临时目录(这次运行结束删掉,下次启动会再触发一次空密码检查)
	tmp, terr := os.MkdirTemp("", "fish-recaptcha-")
	if terr != nil {
		return nil, nil, "", firstErr
	}
	if b, cleanup, err = newBrowser(tmp, !headed); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, nil, "", err
	}
	logf("recaptcha 持久目录不可用,本次退回临时目录: %v", firstErr)
	return b, cleanup, tmp, nil
}

// normalizeUA 把无头浏览器的 User-Agent 改成正常 Chrome 的(去掉 Headless 字样),
// 降低 reCAPTCHA 风控的可见特征。浏览器内核版本号保持一致,不会出现版本撒谎的破绽。
func normalizeUA(page *rod.Page) {
	v, err := proto.BrowserGetVersion{}.Call(page)
	if err != nil {
		return
	}
	ua := strings.Replace(v.Product, "Headless", "", 1)
	_ = proto.EmulationSetUserAgentOverride{UserAgent: ua}.Call(page)
}

// execRecaptcha 在页面上下文里执行 reCAPTCHA,拆成四步各自限时 + 日志:
// reCAPTCHA 的 execute 在异常环境里可能永远不 resolve,没有超时就会把整个请求挂死。
// 用显式渲染(render=explicit + enterprise.render)而不是 render=<sitekey> 的自动渲染:
// 自动渲染在 Alpine 的 Chromium 里曾稳定出现 "Invalid site key or not loaded"(widget 未注册)。
func execRecaptcha(page *rod.Page) (string, error) {
	loaded, err := page.Timeout(15 * time.Second).Eval(`() => !!(window.grecaptcha && window.grecaptcha.enterprise)`)
	if err != nil {
		return "", fmt.Errorf("探测 grecaptcha 失败: %w", err)
	}
	if !loaded.Value.Bool() {
		logf("[recaptcha] 注入 enterprise.js")
		if _, err := page.Timeout(40 * time.Second).Eval(`async () => {
			await new Promise((res, rej) => {
				const s = document.createElement('script');
				s.src = 'https://www.recaptcha.net/recaptcha/enterprise.js?render=explicit';
				s.onload = res; s.onerror = () => rej(new Error('recaptcha script load failed'));
				document.head.appendChild(s);
			});
		}`); err != nil {
			return "", fmt.Errorf("reCAPTCHA 脚本加载失败: %w", err)
		}
	} else {
		logf("[recaptcha] grecaptcha 已存在,跳过注入")
	}
	if _, err := page.Timeout(30 * time.Second).Eval(`async () => { await grecaptcha.enterprise.ready(); }`); err != nil {
		return "", fmt.Errorf("等待 reCAPTCHA ready 失败: %w", err)
	}
	// 诊断:转储 enterprise 命名空间的形态,降级/残缺加载时一眼可见
	if dbg, derr := page.Timeout(10 * time.Second).Eval(`() => {
		const e = window.grecaptcha.enterprise;
		return {
			methods: Object.getOwnPropertyNames(e).slice(0, 30),
			clients: Object.keys((window.___grecaptcha_cfg || {}).clients || {}),
			scripts: [...document.scripts].filter(s => s.src.includes('recaptcha')).map(s => s.src.split('?')[0]),
			ua: navigator.userAgent,
		};
	}`); derr == nil {
		logf("[recaptcha] 环境诊断: methods=%v clients=%v scripts=%v ua=%s",
			dbg.Value.Get("methods").String(), dbg.Value.Get("clients").String(),
			dbg.Value.Get("scripts").String(), dbg.Value.Get("ua").String())
	}
	logf("[recaptcha] ready,等待完整 API 并渲染执行")
	// ready() 只代表引导层就位,render/execute 是 payload 异步加载后才挂上的(数据中心到
	// gstatic 可能要好几秒),必须轮询等完整 API,否则 render 直接 "not a function"。
	res, err := page.Timeout(60 * time.Second).Eval(fmt.Sprintf(`async () => {
		const SITEKEY = %q;
		for (let i = 0; i < 60; i++) {
			if (typeof grecaptcha.enterprise.render === 'function' && typeof grecaptcha.enterprise.execute === 'function') break;
			await new Promise(r => setTimeout(r, 500));
		}
		if (typeof grecaptcha.enterprise.render !== 'function') throw new Error('reCAPTCHA enterprise 完整 API 30 秒未加载完');
		let holder = document.getElementById('__fa_rc_holder');
		if (!holder) {
			holder = document.createElement('div');
			holder.id = '__fa_rc_holder';
			document.body.appendChild(holder);
		}
		if (window.__fa_rc_widget === undefined) {
			window.__fa_rc_widget = grecaptcha.enterprise.render(holder, {
				sitekey: SITEKEY,
				size: 'invisible',
				'error-callback': (e) => { window.__fa_rc_error = String(e); },
			});
		}
		if (window.__fa_rc_error) throw new Error('widget 渲染出错: ' + window.__fa_rc_error);
		return await grecaptcha.enterprise.execute(window.__fa_rc_widget, {action: %q});
	}`, recaptchaSitekey, recaptchaAction))
	if err != nil {
		return "", fmt.Errorf("reCAPTCHA execute 失败: %w", err)
	}
	token := res.Value.Str()
	if token == "" {
		return "", errors.New("reCAPTCHA 返回了空 token")
	}
	return token, nil
}

func resetRecaptcha() {
	if recaptchaKill != nil {
		recaptchaKill()
		recaptchaKill = nil
	}
	recaptchaPage = nil
	recaptchaTok = ""
}

// Warmup 启动预热:拉起收割浏览器并先取一个 token,让首个合成请求免付冷启动
// (浏览器启动 + 页面加载 + reCAPTCHA payload 加载,冷启动要 50 秒以上)。
// RECAPTCHA_KEEPWARM=1 时还会按缓存周期持续换新 token,首请求永远零等待;
// 代价是每 TTL-10s 打一次 reCAPTCHA 接口,持续跑可能引来风控关注,默认关闭(面板里能随时开关,每轮现读)。
func Warmup() {
	if !hasBrowser && !downloadBrowser() {
		return
	}
	go func() {
		time.Sleep(2 * time.Second) // 等主服务先起稳
		if _, err := recaptchaToken(false); err != nil {
			logf("[recaptcha] 启动预热失败(不影响运行,首个请求会自动重试): %v", err)
			return
		}
		logf("[recaptcha] 启动预热完成,收割浏览器已常驻")
	}()
	go func() {
		for {
			time.Sleep(max(recaptchaTTL()-10*time.Second, 10*time.Second))
			if os.Getenv("RECAPTCHA_KEEPWARM") != "1" {
				continue
			}
			if _, err := recaptchaToken(true); err != nil {
				logf("[recaptcha] keepwarm 换新失败: %v", err)
			}
		}
	}()
}
