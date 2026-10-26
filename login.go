// 用 rod 驱动真实 Chrome 完成 fish.audio 登录并收割 localStorage 里的 JWT。
//
// fish.audio 的登录(逆向前端得到):
//   - 密码登录 POST /user/token 必须带 reCAPTCHA Enterprise 的分数,自动化浏览器几乎必被拒;
//   - Google / GitHub / Apple 登录走 /user/oauth-*,不要 reCAPTCHA;
//   - token 只在登录那一刻发,没有任何续签接口,过期只能重新登录。
//
// 所以能自动续期的路只有 OAuth:每个账号一个固定的浏览器配置目录,第一次由用户在弹窗里用
// GitHub/Google 登录一次,之后到期由程序在无头浏览器里重新点一次该登录,OAuth 自动跳回就拿到新 token。
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/input"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
)

const authURL = "https://fish.audio/auth/"

const errTextRe = `(reCAPTCHA[^\n]*|incorrect[^\n]*|invalid[^\n]*|wrong[^\n]*|too many[^\n]*|locked[^\n]*|verification failed[^\n]*)`

// 本机有没有 Chrome/Edge:没有就关掉登录功能,只接收 token。
// LookPath 在部分 Linux 发行版上找不到 chromium,再补查几个常见安装路径(Docker 镜像用 apk add chromium)。
var browserBin, hasBrowser = lookBrowser()

func lookBrowser() (string, bool) {
	if bin, ok := launcher.LookPath(); ok {
		return bin, true
	}
	for _, p := range []string{
		"/usr/bin/chromium-browser", "/usr/bin/chromium",
		"/usr/bin/google-chrome-stable", "/usr/bin/google-chrome",
	} {
		if _, err := os.Stat(p); err == nil {
			return p, true
		}
	}
	return "", false
}

// downloadBrowser 精简镜像可以用 rod 运行时自动下载的无头内核(RECAPTCHA_ALLOW_DOWNLOAD=1)。
// 只用于 reCAPTCHA 收割;登录始终要求真实安装的浏览器。
func downloadBrowser() bool { return os.Getenv("RECAPTCHA_ALLOW_DOWNLOAD") == "1" }

// loginDisabled 容器/服务器模式:没有显示器,弹窗登录和无头自动登录都关掉,只收 token。
var loginDisabled = os.Getenv("DISABLE_LOGIN") == "1"

// runningAsRoot Docker 容器里通常是 root,Chromium 不加 --no-sandbox 起不来。
func runningAsRoot() bool { return os.Geteuid() == 0 }

// profileDir 每个账号一个浏览器配置目录(browser-profile/<账号名>),cookie 和 OAuth 登录态都留在里面。
var profileRoot, _ = filepath.Abs("browser-profile")
var unsafePath = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func profileDir(name string) string {
	return filepath.Join(profileRoot, unsafePath.ReplaceAllString(name, "_"))
}

// 启动浏览器:优先用本机安装的 Chrome/Edge(reCAPTCHA 评分更好),去掉 rod 默认的自动化标记。
func newBrowser(profile string, headless bool) (*rod.Browser, func(), error) {
	// Leakless(false):rod 的 leakless.exe 辅助进程常被 Windows Defender 误报拦截,
	// 关闭后副作用仅是主进程异常退出时浏览器进程可能残留。
	l := launcher.New().Leakless(false).UserDataDir(profile).
		Headless(headless).
		Delete("enable-automation"). // 会让 navigator.webdriver=true 并显示"受自动测试软件控制"
		Delete("disable-sync").      // 允许在这个窗口里登录 Google 账号
		Set("disable-blink-features", "AutomationControlled")
	if browserBin != "" {
		l = l.Bin(browserBin)
	} else if downloadBrowser() {
		// 不带 Bin,rod 会自己下载托管内核(首次约 150MB,缓存在 ~/.cache/rod)
	} else {
		return nil, nil, errors.New(
			"没有找到 Chrome/Edge/Chromium:登录需要真实安装的浏览器;" +
				"精简环境可设 RECAPTCHA_ALLOW_DOWNLOAD=1 允许 rod 自动下载无头内核(仅够人机验证收割用)")
	}
	if runningAsRoot() { // Docker 容器里通常以 root 运行
		l = l.Set("no-sandbox").Set("disable-dev-shm-usage")
	}
	if headless { // 容器里没有 GPU,Vulkan/EGL 初始化失败会让 viz 进程反复重启
		l = l.Set("disable-gpu")
	}
	u, err := l.Launch()
	if err != nil {
		return nil, nil, fmt.Errorf("启动浏览器失败: %w", err)
	}
	b := rod.New().ControlURL(u)
	if err := b.Connect(); err != nil {
		return nil, nil, fmt.Errorf("连接浏览器失败: %w", err)
	}
	cleanup := func() { _ = b.Close() }
	return b, cleanup, nil
}

// pollState 读取当前 token(localStorage 没有就看同名 cookie;配置目录里留下的过期旧 token 不算)与页面错误提示。
func pollState(page *rod.Page) (token string, errText string) {
	res, err := page.Eval(fmt.Sprintf(
		`() => { const m = document.body.innerText.match(new RegExp(%q, "i"));
		         const c = document.cookie.match(/(?:^|; )token=([^;]*)/);
		         return { token: localStorage.getItem('token') || (c ? decodeURIComponent(c[1]) : ''), err: m ? m[0] : '' }; }`,
		errTextRe))
	if err != nil || res == nil {
		return "", ""
	}
	token = res.Value.Get("token").Str()
	if exp := jwtExp(token); exp != 0 && exp < float64(time.Now().Unix()) {
		token = ""
	}
	return token, res.Value.Get("err").Str()
}

// injectBanner 交互模式时在页面上放一条提示横幅。
func injectBanner(page *rod.Page, a Account) {
	tip := "密码 / Google / GitHub / Apple 均可"
	if a.Provider != "" {
		tip = "请点「" + a.Provider + "」登录,以后到期会用它自动续期"
	}
	msg, _ := json.Marshal("Fish Audio 账号「" + a.Name + "」登录窗口:请在页面中完成登录(" + tip + "),成功后本窗口会自动关闭。")
	_, _ = page.Eval(fmt.Sprintf(`() => {
		if (document.getElementById('__fa_login_banner')) return;
		const d = document.createElement('div');
		d.id = '__fa_login_banner';
		d.style.cssText = 'position:fixed;top:0;left:0;right:0;z-index:2147483647;background:#1f6feb;color:#fff;padding:14px 24px;font:15px/1.5 system-ui;text-align:center;box-shadow:0 2px 8px rgba(0,0,0,.35)';
		d.textContent = %s;
		document.body.appendChild(d);
	}`, string(msg)))
}

// loginViaBrowser 统一入口,都在账号自己的配置目录里跑:
//   - interactive=true:弹出窗口由用户手动登录(任何方式),只等 token 出现;
//   - 账号设了 Provider(github/google):无头浏览器重新走一遍 OAuth 登录;
//   - 否则:自动填邮箱/密码提交(必过 reCAPTCHA,大概率被拒)。
func loginViaBrowser(a Account, interactive bool) (string, error) {
	headless := !interactive && (a.Provider != "" || os.Getenv("LOGIN_HEADLESS") == "1")
	b, cleanup, err := newBrowser(profileDir(a.Name), headless)
	if err != nil {
		return "", err
	}
	defer cleanup()

	page, err := b.Page(proto.TargetCreateTarget{URL: authURL})
	if err != nil {
		return "", err
	}

	if interactive { // 配置目录里已有有效 token(上次登录过)的话,第一轮轮询就直接拿到
		injectBanner(page, a)
		timeout := time.Duration(interactiveTimeout) * time.Second
		deadline := time.Now().Add(timeout)
		logf("[交互登录] 浏览器窗口已打开,请在窗口里完成账号「%s」的登录(%d 秒内)", a.Name, int(timeout.Seconds()))
		for time.Now().Before(deadline) {
			token, _ := pollState(page)
			if token != "" && tokenOK(token) {
				logf("[交互登录] 已收割到账号「%s」的 token", a.Name)
				return token, nil
			}
			// 原标签页被关掉时找回 fish.audio 标签页;整个浏览器被关掉就不用等到超时了
			if info, e := page.Info(); e != nil || !strings.HasPrefix(info.URL, "https://fish.audio") {
				pages, e := b.Pages()
				if e != nil {
					return "", errors.New("浏览器窗口已被关闭,登录取消")
				}
				if p2, e := pages.FindByURL(`^https://fish\.audio`); e == nil {
					page = p2
				}
			}
			time.Sleep(1 * time.Second)
		}
		return "", fmt.Errorf("%d 秒内未检测到登录成功(未在 localStorage 找到 token)", interactiveTimeout)
	}

	if a.Provider != "" {
		return loginViaOAuth(b, page, a)
	}

	// ---------- 自动填表(SPA 渲染表单需要一点时间) ----------
	emailEl, err := page.Timeout(30 * time.Second).Element(`input[type="email"]`)
	if err != nil {
		return "", fmt.Errorf("登录表单未出现: %w", err)
	}
	if err := emailEl.Input(a.Email); err != nil {
		return "", err
	}
	pwdEl, err := page.Element(`input[type="password"]`)
	if err != nil {
		return "", fmt.Errorf("定位密码输入框失败: %w", err)
	}
	if err := pwdEl.Input(a.Password); err != nil {
		return "", err
	}
	// ElementR 的正则是 JS RegExp,不支持 (?i) 内联标志,用字符类做大小写兼容
	btn, err := page.Timeout(10*time.Second).ElementR("button", `[Ll]og\s*[Ii]n|登录`)
	if err != nil {
		// 找不到按钮时兜底:聚焦密码框按回车提交表单
		logf("未匹配到登录按钮(%v),改用回车提交", err)
		if err := pwdEl.Click(proto.InputMouseButtonLeft, 1); err != nil {
			return "", fmt.Errorf("定位登录按钮失败且回车提交也失败: %w", err)
		}
		if err := page.Keyboard.Press(input.Enter); err != nil {
			return "", err
		}
	} else if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", err
	}

	deadline := time.Now().Add(time.Duration(loginTimeout) * time.Second)
	for time.Now().Before(deadline) {
		token, errText := pollState(page)
		if token != "" && tokenOK(token) {
			return token, nil
		}
		if errText != "" {
			return "", fmt.Errorf("登录被拒绝: %s", errText)
		}
		time.Sleep(1 * time.Second)
	}
	// 超时:截图存档便于排查(登录页当时的状态)
	if shot, serr := page.Screenshot(true, nil); serr == nil {
		_ = os.WriteFile("login-debug.png", shot, 0o644)
		logf("登录超时,页面截图已存 login-debug.png")
	}
	return "", fmt.Errorf("%d 秒内未拿到 token(reCAPTCHA 可能拦截了自动化浏览器,可改用交互登录)", loginTimeout)
}

// loginViaOAuth 在账号自己的配置目录里重新走一遍 GitHub/Google 登录拿新 token:
// 先清掉 fish.audio 的登录态(否则页面直接当已登录,不会再发 token),再点登录按钮,等 OAuth 跳转回来。
func loginViaOAuth(b *rod.Browser, page *rod.Page, a Account) (string, error) {
	old, _ := pollState(page)
	if err := clearFishSession(page); err != nil {
		return "", fmt.Errorf("清理旧登录态失败: %w", err)
	}
	if err := page.Navigate(authURL); err != nil {
		return "", err
	}
	// JS RegExp 没有 (?i),首字母用字符类兼容 Github/github、Google/google
	p := a.Provider
	providerBtnRe := "[" + strings.ToUpper(p[:1]) + strings.ToLower(p[:1]) + "]" + p[1:]
	btn, err := page.Timeout(30*time.Second).ElementR("button", providerBtnRe)
	if err != nil {
		return "", fmt.Errorf("登录页上没找到 %s 按钮: %w", a.Provider, err)
	}
	if err := btn.Click(proto.InputMouseButtonLeft, 1); err != nil {
		return "", err
	}
	deadline := time.Now().Add(time.Duration(loginTimeout) * time.Second)
	for time.Now().Before(deadline) {
		if token, _ := pollState(page); token != "" && token != old && tokenOK(token) {
			logf("[自动续期] 账号「%s」通过 %s 拿到新 token", a.Name, a.Provider)
			return token, nil
		}
		if a.Provider == "google" {
			pickGoogleAccount(b, a.Email)
		}
		time.Sleep(1 * time.Second)
	}
	return "", fmt.Errorf("%d 秒内没拿到新 token:%s 的登录态可能已失效,请用「弹窗登录」重新登录一次", loginTimeout, a.Provider)
}

// clearFishSession 清掉 fish.audio 域的 localStorage 和 cookie(登录态会镜像到 cookie),不动 GitHub/Google 的。
func clearFishSession(page *rod.Page) error {
	if _, err := page.Eval(`() => { localStorage.clear(); sessionStorage.clear(); }`); err != nil {
		return err
	}
	cookies, err := page.Cookies([]string{"https://fish.audio"})
	if err != nil {
		return err
	}
	for _, c := range cookies {
		_ = proto.NetworkDeleteCookies{Name: c.Name, Domain: c.Domain, Path: c.Path}.Call(page)
	}
	return nil
}

// pickGoogleAccount Google 登录会弹一个选账号的小窗口,在里面点和账号邮箱一致的那一项(没有就点第一项)。
func pickGoogleAccount(b *rod.Browser, email string) {
	pages, err := b.Pages()
	if err != nil {
		return
	}
	p, err := pages.FindByURL(`accounts\.google\.com`)
	if err != nil {
		return
	}
	el, err := p.Timeout(3 * time.Second).Element(fmt.Sprintf(`[data-identifier="%s"]`, email))
	if err != nil {
		if el, err = p.Timeout(2 * time.Second).Element(`[data-identifier]`); err != nil {
			return
		}
	}
	_ = el.Click(proto.InputMouseButtonLeft, 1)
}
