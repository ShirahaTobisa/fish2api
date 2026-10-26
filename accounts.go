// 多账号管理与网页 token 的获取/续期。
//
// 登录态机制(逆向 fish.audio 前端 + 实测得到):
//   - 网页登录态 = localStorage["token"](同时镜像到同名 cookie),是一个不透明的会话串(UUID),
//     不是 JWT,里面没有过期时间;只能定期调 /user/self 看它还认不认(401 = 失效);
//   - 登录接口 POST /user/token 强制要求 Google reCAPTCHA Enterprise 分数,自动化浏览器几乎必被拒;
//     Google / GitHub / Apple 登录走 /user/oauth-*,不要 reCAPTCHA;
//   - 没有任何续签接口,token 失效只能重新登录(见 login.go);
//   - 没有浏览器的机器(服务器)上登录功能关闭,只接收 token:本地实例登录后推送(PUSH_URL),或面板/书签送来。
package main

import (
	"cmp"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------- 环境配置

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}

// envIntZero 和 envInt 类似,但允许 0(用于"设 0 表示关闭"的数值配置)。
func envIntZero(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return def
}

var (
	refreshMargin      = envInt("TOKEN_REFRESH_MARGIN_SECONDS", 1800)     // 带过期时间的 token 剩余多少秒触发重登
	checkInterval      = envInt("TOKEN_CHECK_INTERVAL_SECONDS", 60)       // 后台巡检周期
	loginTimeout       = envInt("LOGIN_TIMEOUT_SECONDS", 90)              // 自动登录超时
	interactiveTimeout = envInt("LOGIN_INTERACTIVE_TIMEOUT_SECONDS", 300) // 交互登录等待人工操作的超时
)

// verifyEvery 多久向上游验证一次 token 还有效。面板「设置」里能改、保存即生效(见 admin.go),所以现读。
func verifyEvery() int { return envInt("TOKEN_VERIFY_INTERVAL_SECONDS", 600) }

// 自动登录失败后隔多久才再试,否则每轮巡检都会弹一次浏览器
const retryGap = 10 * time.Minute

// ---------------------------------------------------------------- JWT 工具(fish 现在发的是不透明 token,这里只是兼容万一是 JWT 的情况)

func jwtExp(token string) float64 {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return 0
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	exp, _ := m["exp"].(float64)
	return exp
}

// ---------------------------------------------------------------- 账号存储

type Account struct {
	Name          string    `json:"name"`
	Email         string    `json:"email"`
	Password      string    `json:"password,omitempty"`
	Token         string    `json:"token,omitempty"`
	Exp           float64   `json:"exp,omitempty"` // 只有 JWT 才有;0 = 不透明 token,有效性靠上游验证
	UserID        string    `json:"user_id,omitempty"`
	TeamID        string    `json:"team_id,omitempty"`
	WorkspaceID   string    `json:"workspace_id,omitempty"`
	Status        string    `json:"status"`             // ok / no_token / login_failed
	Provider      string    `json:"provider,omitempty"` // 自动续期方式:"" 密码 / github / google
	LastLogin     float64   `json:"last_login,omitempty"`
	LastCheck     float64   `json:"last_check,omitempty"` // 上次向上游验证通过的时间
	LastError     string    `json:"last_error,omitempty"`
	lastAttempt   time.Time // 上次自动登录失败的时间,用于退避
	cooldownUntil time.Time // 上游 429/5xx 后的冷却截止,轮询期间跳过它
}

// WebCred 一次请求用的网页凭证快照;Account 非 nil 表示来自账号库(上游 401 时可重登)。
type WebCred struct {
	Token       string
	UserID      string
	TeamID      string
	WorkspaceID string
	Account     *Account
}

// AccountStore 账号库。账号字段的读写都在 mu 内进行,对外只给快照/视图。
type AccountStore struct {
	mu       sync.Mutex
	loginMu  sync.Mutex // 同一时间只开一个登录浏览器
	path     string
	rr       int
	Accounts []*Account `json:"accounts"`
}

func NewAccountStore() *AccountStore {
	s := &AccountStore{path: cmp.Or(os.Getenv("ACCOUNTS_FILE"), "accounts.json")}
	if b, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(b, s)
	}
	return s
}

func (s *AccountStore) Save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := s.path + ".tmp"
	err := os.WriteFile(tmp, b, 0o600)
	if err == nil {
		err = os.Rename(tmp, s.path)
	}
	if err != nil {
		logf("保存账号库失败: %v", err)
	}
}

// find 按名字找账号(需持锁)。
func (s *AccountStore) find(name string) *Account {
	for _, a := range s.Accounts {
		if a.Name == name {
			return a
		}
	}
	return nil
}

func (s *AccountStore) Get(name string) *Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.find(name)
}

// Add 新增账号,只取 a 的基础字段:name 为空取邮箱前缀;给了 token 则直接可用。返回存下的快照。
func (s *AccountStore) Add(a Account) (Account, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.add(a)
}

func (s *AccountStore) add(a Account) (Account, error) {
	n := &Account{
		Name: cmp.Or(a.Name, strings.SplitN(a.Email, "@", 2)[0]), Email: a.Email, Password: a.Password,
		Provider: a.Provider, TeamID: a.TeamID, WorkspaceID: a.WorkspaceID, Status: "no_token",
	}
	if s.find(n.Name) != nil {
		return Account{}, fmt.Errorf("账号名已存在: %s", n.Name)
	}
	if a.Token != "" {
		setToken(n, a.Token)
	}
	s.Accounts = append(s.Accounts, n)
	return *n, nil
}

// Upsert 用新 token 更新账号,不存在就新建。面板粘贴、书签、本地实例推送都走这里。
func (s *AccountStore) Upsert(a Account) (Account, error) {
	if a.Token == "" {
		return Account{}, errors.New("token 不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.find(a.Name)
	if n == nil {
		return s.add(a)
	}
	n.Email, n.TeamID, n.WorkspaceID = cmp.Or(a.Email, n.Email), cmp.Or(a.TeamID, n.TeamID), cmp.Or(a.WorkspaceID, n.WorkspaceID)
	setToken(n, a.Token)
	return *n, nil
}

// Usable 所有可用账号的快照(本地实例启动时整体推送给服务器用)。
func (s *AccountStore) Usable() []Account {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Account
	for _, a := range s.Accounts {
		if s.usable(a) {
			out = append(out, *a)
		}
	}
	return out
}

func (s *AccountStore) Remove(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	before := len(s.Accounts)
	s.Accounts = slices.DeleteFunc(s.Accounts, func(a *Account) bool { return a.Name == name })
	return len(s.Accounts) < before
}

// SetProvider 改账号的自动续期方式;账号不存在返回 false。
func (s *AccountStore) SetProvider(name, provider string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.find(name)
	if a != nil {
		a.Provider = provider
	}
	return a != nil
}

// Invalidate 上游明确拒绝(401)后丢掉 token,让轮询不再选到它。
func (s *AccountStore) Invalidate(name, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.find(name); a != nil {
		a.Token, a.Exp, a.Status, a.LastError = "", 0, "login_failed", reason
	}
}

// Cooldown 给账号加一段冷却,期间轮询跳过它(上游 429/5xx 时用);用 X-Account 指定名字的请求不受影响。
func (s *AccountStore) Cooldown(name string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.find(name); a != nil {
		a.cooldownUntil = time.Now().Add(d)
	}
}

// setToken 写入新 token 并标记可用(需持锁)。
func setToken(a *Account, token string) {
	a.Token, a.Exp, a.Status = token, jwtExp(token), "ok"
	a.LastLogin, a.LastCheck, a.LastError = float64(time.Now().Unix()), 0, ""
}

// usable 有 token 且(带过期时间的话)还没到期(需持锁);不透明 token 失效时由 Verify/401 处理清掉。
func (s *AccountStore) usable(a *Account) bool {
	return a.Token != "" && (a.Exp == 0 || a.Exp-float64(time.Now().Unix()) > 30)
}

// Pick 选择账号:指定名字 > 可用账号轮询。返回的是快照,可在锁外使用。
func (s *AccountStore) Pick(name string) (*WebCred, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var a *Account
	if name != "" {
		if a = s.find(name); a == nil {
			return nil, fmt.Errorf("账号不存在: %s", name)
		}
		if a.Token == "" {
			return nil, fmt.Errorf("账号 %s 还没有 token,先登录或粘贴一个", name)
		}
	} else {
		var pool, cooling []*Account
		now := time.Now()
		for _, x := range s.Accounts {
			if !s.usable(x) {
				continue
			}
			if now.Before(x.cooldownUntil) {
				cooling = append(cooling, x)
			} else {
				pool = append(pool, x)
			}
		}
		if len(pool) == 0 { // 都在冷却就还是从冷却里挑,总比直接失败强
			pool = cooling
		}
		if len(pool) == 0 {
			return nil, errors.New("没有可用账号:用 X-Account 指定账号、X-Fish-Web-Token 直接给 token,或先添加账号并登录")
		}
		s.rr++
		a = pool[s.rr%len(pool)]
	}
	return &WebCred{Token: a.Token, UserID: a.UserID, TeamID: a.TeamID, WorkspaceID: a.WorkspaceID, Account: a}, nil
}

// Stats 账号总数与可用数。
func (s *AccountStore) Stats() (total, usable int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.Accounts {
		if s.usable(a) {
			usable++
		}
	}
	return len(s.Accounts), usable
}

// View 单个账号的对外视图(不暴露密码和 token 明文);账号不存在时返回空视图。
func (s *AccountStore) View(name string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.find(name); a != nil {
		return s.view(a)
	}
	return map[string]any{}
}

func (s *AccountStore) Views() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]map[string]any, 0, len(s.Accounts))
	for _, a := range s.Accounts {
		out = append(out, s.view(a))
	}
	return out
}

func (s *AccountStore) view(a *Account) map[string]any {
	v := map[string]any{
		"name": a.Name, "email": a.Email, "has_password": a.Password != "", "provider": a.Provider,
		"status": a.Status, "usable": s.usable(a), "last_error": a.LastError, "user_id": a.UserID,
		"team_id": a.TeamID, "workspace_id": a.WorkspaceID,
		"expires_at": rfc3339(a.Exp), "last_login": rfc3339(a.LastLogin), "last_check": rfc3339(a.LastCheck),
		"seconds_left": nil, "token_preview": "",
	}
	if a.Token != "" {
		v["token_preview"] = a.Token[:min(8, len(a.Token))] + "…"
	}
	if a.Token != "" && a.Exp > 0 {
		v["seconds_left"] = int(a.Exp - float64(time.Now().Unix())) // 负数 = 已过期
	}
	return v
}

// rfc3339 把 unix 秒转成 UTC 时间串;0 返回 nil。
func rfc3339(ts float64) any {
	if ts <= 0 {
		return nil
	}
	return time.Unix(int64(ts), 0).UTC().Format(time.RFC3339)
}

// ---------------------------------------------------------------- 验证 / 登录 / 续期

// Verify 向上游验证账号的 token:通过就记下 user_id、邮箱和验证时间;上游 401 就丢掉 token。
// 网络错误不改状态。返回验证后的快照。
func (s *AccountStore) Verify(name string) (Account, error) {
	a := s.Get(name)
	if a == nil {
		return Account{}, fmt.Errorf("账号不存在: %s", name)
	}
	s.mu.Lock()
	cred := &WebCred{Token: a.Token, TeamID: a.TeamID, WorkspaceID: a.WorkspaceID}
	s.mu.Unlock()
	if cred.Token == "" {
		return *a, errors.New("没有 token")
	}
	id, email, err := whoami(cred)
	s.mu.Lock()
	switch {
	case err == nil:
		a.UserID, a.LastCheck, a.Status, a.LastError = id, float64(time.Now().Unix()), "ok", ""
		if a.Email == "" || strings.HasPrefix(a.Email, "manual-") {
			a.Email = email
		}
	case errors.Is(err, errTokenInvalid):
		a.Token, a.Exp, a.Status, a.LastError = "", 0, "login_failed", "token 已失效(上游 401)"
	}
	snap := *a
	s.mu.Unlock()
	if err == nil || errors.Is(err, errTokenInvalid) {
		s.Save()
	}
	return snap, err
}

// Login 登录并把结果写回账号库:interactive=true 弹窗人工,否则按账号的续期方式/密码自动登录。
func (s *AccountStore) Login(name string, interactive bool) (string, error) {
	a := s.Get(name)
	if a == nil {
		return "", fmt.Errorf("账号不存在: %s", name)
	}
	s.mu.Lock()
	acc := *a // 快照,锁外用
	s.mu.Unlock()
	var token string
	var err error
	switch {
	case !hasBrowser:
		err = errors.New("这台机器没有 Chrome/Edge,不能登录;请在本地电脑登录后把 token 推送或粘贴过来")
	case loginDisabled && interactive:
		// 容器里没有显示器,弹窗登录开不了;但 OAuth 无头续期(default 分支)仍然可用
		err = errors.New("服务器模式(DISABLE_LOGIN=1)没有显示器,打不开登录窗口;" +
			"请用面板的书签更新 token,或在本地电脑登录一次后把 browser-profile/<账号名> 拷到服务器," +
			"并把账号续期方式设为 github/google,之后服务器就能自动续期")
	case !interactive && loginDisabled && acc.Provider == "" && acc.Password == "":
		err = errors.New("没有 token 也没有续期方式:先用面板书签更新 token," +
			"或在本地电脑登录一次并把 browser-profile/<账号名> 拷过来、把续期方式设为 github/google")
	case !interactive && acc.Password == "" && acc.Provider == "":
		err = errors.New("没有密码也没设续期方式,无法自动登录,请改用弹窗登录")
	default:
		s.loginMu.Lock()
		token, err = loginViaBrowser(acc, interactive)
		s.loginMu.Unlock()
	}
	s.mu.Lock()
	if err != nil {
		a.Status, a.LastError, a.lastAttempt = "login_failed", err.Error(), time.Now()
	} else {
		setToken(a, token)
	}
	s.mu.Unlock()
	if err != nil {
		s.Save()
		return "", err
	}
	snap, _ := s.Verify(name) // 顺便拿 user_id;Verify 自己会 Save
	go pushToken(snap)
	return token, nil
}

// due 需要自动重登的账号:能自动登录(有密码或设了续期方式)、没有 token 或(JWT)临期、且距上次失败已过退避时间。
func (s *AccountStore) due() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, a := range s.Accounts {
		if (a.Password != "" || a.Provider != "") && time.Since(a.lastAttempt) >= retryGap &&
			(a.Token == "" || (a.Exp > 0 && a.Exp-float64(time.Now().Unix()) <= float64(refreshMargin))) {
			names = append(names, a.Name)
		}
	}
	return names
}

// staleChecks 有 token 但太久没向上游验证过的账号。
func (s *AccountStore) staleChecks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for _, a := range s.Accounts {
		if a.Token != "" && float64(time.Now().Unix())-a.LastCheck >= float64(verifyEvery()) {
			names = append(names, a.Name)
		}
	}
	return names
}

// RefreshLoop 后台协程:定期验证 token 是否还有效,失效/缺失的账号能自动登录的就重登。
func (s *AccountStore) RefreshLoop() {
	for range time.Tick(time.Duration(checkInterval) * time.Second) {
		for _, name := range s.staleChecks() {
			if _, err := s.Verify(name); err != nil {
				logf("账号 %s 的 token 验证: %v", name, err)
			}
		}
		if !hasBrowser {
			continue // 服务器(没浏览器)只做验证、踢掉死 token,不自动登录
		}
		for _, name := range s.due() {
			logf("账号 %s 没有可用 token,自动重登", name)
			if _, err := s.Login(name, false); err != nil {
				logf("账号 %s 自动登录失败: %v", name, err)
			}
		}
	}
}
