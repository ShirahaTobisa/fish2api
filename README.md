# Fish Audio 网页版 TTS → REST API(Go)

把 https://fish.audio 网页版的能力封装成 REST API,自带管理面板,目标是用账号的**订阅套餐 credits**(与 API 积分是两个独立钱包)。

> **2026-10-25 晚已修复**:网页合成 `POST /task` 现在每次都要带 reCAPTCHA Enterprise 人机验证
> (缺失 400 "Missing reCAPTCHA Enterprise",伪造 400 "reCAPTCHA verification failed";网页前端的
> 做法是 `grecaptcha.enterprise.execute(sitekey, {action:"playground"})`)。已实现自动收割:rod 无头浏览器
> 常驻打开一个 fish.audio 页面,执行同一段脚本拿 token,缓存 100 秒(token 实测约 120 秒有效、期内可复用),
> 被上游判过期时自动换新重试。**不需要任何登录态**;无头模式已做指纹处理(真 Chrome 内核 + 抹掉
> `HeadlessChrome` UA 标记),实测连续 14/14 通过(含每请求全新收割的 8/8)。注意:登录(`action=login`)
> 的风控阈值和这个不是一回事,密码自动登录无头仍会被拒。单次收割约 1~2 秒,常驻后均摊几乎为零。
> 启动时自动预热(拉起收割浏览器并先取一个 token,首个请求免付冷启动);收割页优先用同域名的
> `robots.txt` 纯文本页(sitekey 只绑域名,页面加载 1 秒内、常驻零 CPU),失败自动退回 SPA 页。
> 香港 2026-10-26 实测:容器启动后 9 秒预热完成,首个请求 1.6 秒,缓存命中 0.002 秒。
> 收割依赖浏览器:本地模式用本机 Chrome;**服务器(Docker)镜像里已带 Alpine chromium,同样能收**。
> 数据中心 IP 的评分已实测通过(香港,2026-10-25):无头 Chromium 收割 token → 合成 200,热请求约 2 秒。
> 注意一个已修的坑:enterprise.js 的引导层先就位(只有 `ready`),render/execute 是 payload 异步加载后才挂上的,
> 必须轮询等完整 API,否则在数据中心网络下必现 "render is not a function"。
> API key 模式(`/api/tts`)不受影响。

## 原理(逆向结论)

- 网页登录态 = `localStorage["token"]`(同时镜像到同名 cookie),是个不透明的会话串(UUID),**不是 JWT,里面没有过期时间**;
  前端以 `Authorization: Bearer <token>` 调 `api.fish.audio`;`GET /user/self` 能查用户 ID,也是判断 token 还有没有效的唯一办法(401 = 失效);
- 网页"生成语音"调 `POST /task`(`{"type":"tts","stream":true,"backend":"drama-3","parameters":{...}}`),响应是音频流,任务 ID 在响应头 `task-id`;
- 两个钱包:`/wallet/{uid}/package` = 订阅套餐(网页用,字段 `balance / total / type`)vs `/wallet/{uid}/api-credit` = API 积分(API key 用);
- 密码登录 `POST /user/token` 要求人机验证(自动化浏览器几乎必被拒);Google / GitHub / Apple 登录走 `/user/oauth-*`,不要求;
- 没有任何续签接口,token 失效只能重新登录。

程序按机器能力分两种角色:

| 角色 | 条件 | 能做什么 |
|---|---|---|
| **本地** | 机器上有 Chrome/Edge | 弹窗登录(你自己在窗口里登,任何方式)、自动续期(见下)、每 10 分钟向上游验证 token;设了 `PUSH_URL` 就把每次拿到的 token 推送到服务器。每个账号一个独立的浏览器配置目录 `browser-profile/<账号名>` |
| **服务器** | 没有浏览器(Docker 里) | 只接收 token(书签 / 本地推送 / 面板粘贴),对外提供 API 和面板;登录按钮自动隐藏 |


**自动续期**(已在香港服务器实测跑通全流程):token 没有过期时间,后台每 10 分钟调一次 `/user/self` 验证;失效了就按账号的「续期方式」重新登录——
`github` / `google`:在该账号的配置目录里起一个无头浏览器,清掉 fish.audio 的登录态后点对应的登录按钮,OAuth 跳回就拿到新 token
(第一次要你在「弹窗登录」里用它登一次,让配置目录里留下 GitHub/Google 的登录态;同邮箱的 OAuth 登录通常会关联到已有账号,登完看余额在不在);
密码方式:自动填表,要过人机验证,基本会被拦。一次失败后 10 分钟内不再重试。

服务器模式(`DISABLE_LOGIN=1`)下弹窗登录不可用,但 **provider 无头续期可用**:把本地登录过的
`browser-profile/<账号名>` 目录拷到服务器 `/data/browser-profile/` 下、面板里把续期方式设为对应 provider,
token 失效后服务器就会自己续;profile 里 GitHub/Google 的登录态一般能活几个月,到期再本地弹窗登一次并重新拷贝。
注意 `ElementR` 的正则是 JS RegExp,没有 `(?i)`,大小写用字符类处理。

## 本地运行

```bash
go build -o fish2api.exe .     # 需要 Go 1.22+
./fish2api.exe                 # http://127.0.0.1:8765 打开面板
```

面板里「添加账号」:填邮箱 + 密码走自动登录;勾「弹出浏览器人工登录」走交互登录;或直接粘贴 token / 用书签发过来。
粘贴/推送来的 token 会先问上游认不认,不认当场报错。要同步给服务器就带着环境变量启动:

```powershell
$env:PUSH_URL="https://tts.example.com"; $env:PUSH_KEY="服务器的ACCESS_KEY"; .\fish2api.exe
```

启动时会把手头可用的 token 全推一遍,之后每次登录成功都推;服务器连不上会隔 30 秒重试几次。

## 服务器部署(Docker + 反代)

```bash
scp -r . server:/srv/fish2api        # 源码放过去(.dockerignore 已排除 exe / accounts.json / browser-profile)
cd /srv/fish2api
echo "ACCESS_KEY=$(openssl rand -hex 24)" > .env
docker compose up -d --build             # 或在 Dockge 里导入这个目录
```

镜像 = Go 二进制 + Alpine 的 chromium(约 +200MB):容器里的 chromium 专门收割网页合成要求的
reCAPTCHA token,所以**服务器也能走套餐 credits 合成**。容器以 `DISABLE_LOGIN=1` 运行
(没有显示器,面板上的登录按钮自动隐藏),token 从本地电脑推送/粘贴过来;`/data/accounts.json` 只有 token。
容器监听 `127.0.0.1:8765`,Caddy 反代一行:

```
tts.example.com {
    reverse_proxy 127.0.0.1:8765
}
```

**部署后自测**(香港节点 2026-10-25 已实测通过,其他机房照此验一次):

```bash
curl -X POST https://tts.example.com/api/web/tts \
  -H "Authorization: Bearer $ACCESS_KEY" -H "Content-Type: application/json" \
  -d '{"text":"server test","backend":"drama-3"}' --output test.mp3
# 200 且 test.mp3 以 ID3 开头 = 全通;
# 400 "reCAPTCHA verification failed" = 谷歌不给这个数据中心 IP 的无头浏览器打高分 → 换日本那台再试,或退回 API key 模式
```

部署完打开域名,面板会要 `ACCESS_KEY`;然后给账号拿 token:最简单是「更新 token」里的书签,
或在本地电脑用上面的 `PUSH_URL` 方式登录推送,或 F12 → Application → Local Storage 复制 `token` 粘贴。

## 结果缓存与可靠性

- **结果缓存**:同一段文字 + 同一个声音 + 同样参数,命中缓存直接回放(响应头 `X-Cache: HIT`),不打上游、不扣额度、也不取人机验证。没指定声音的请求不缓存(默认声音随账号而变)。相同参数会一直回放同一段音频,想每次不同就改文字/参数或 `CACHE_MAX_MB=0` 关掉。
- **后台验证**:无论有没有浏览器,服务都每 `TOKEN_VERIFY_INTERVAL_SECONDS` 向上游验证一次各账号的 token,提前踢掉失效的,免得真实请求撞上死 token 白跑一趟。
- **失败冷却**:账号在 `/task` 上被上游 429 或 5xx 时,`ACCOUNT_COOLDOWN_SECONDS` 内轮询跳过它,换下一个账号。

## 面板

照 CPAMP(CPA-Manager-Plus)的思路做的管理面板,深浅色、手机可用:

- **仪表盘**:今日调用、成功率、缓存命中、字数、平均耗时、套餐余额合计,按小时的成功/失败柱状图,账号概况,最近请求;
- **请求记录**:服务端记下每一次合成(面板、new-api、脚本都算):时间、哪把 key 调的、用了哪个账号、模型、声音、字数、前 200 字原文、
  成败和失败原因、耗时、是否命中缓存、来源 IP。按 今天 / 昨天 / 7 天 / 30 天 / 全部 看,能按 key、账号、结果筛选和搜文字,
  可切「明细 / 按 Key / 按账号」汇总;点一行看详情,结果缓存里还在的能直接回放。存在数据目录的 `requests.jsonl`,重启不丢;
- **运行日志**:程序日志(token 验证、续期、人机验证、推送……)在面板里看,不用 `docker logs`;只留最近约 1000 行;
- **账号**:添加、更新 token(书签)、续期方式、余额;
- **API Keys**:给 new-api / 酒馆 / 朋友各发一把使用者 key,有名字、能停用 / 改名 / 吊销,显示调用次数和最后使用时间;
- **设置**:缓存、冷却、token 验证间隔、人机验证、官方 API key、请求记录保留条数这些,改了保存立即生效(存在数据目录的 `config.json`,
  比环境变量优先;清空某项 = 退回环境变量/默认值);监听地址、端口、`ACCESS_KEY` 只能改环境变量后重启;
- 工具页:语音合成(声音搜索 + 试听、标签插入、记住上次选择)、图像/视频、生成历史(存在浏览器里,可回放/下载)。

## 端点

两级密钥(三种姿势通吃:`Authorization: Bearer <key>`、`X-Api-Key: <key>`、`?key=<key>`):

- **`ACCESS_KEY`(管理员)**:全部接口,面板登录也用它;
- **使用者 API key**(面板「API Keys」页发放,`sk-fish-` 开头,每把有名字):**只能调合成与查询**
  (`/v1/*`、`/api/tts`、`/api/models`、`/api/voices*`、`/api/web/*`,含图像/视频生成),
  账号、key、设置、记录这些管理接口返回 403;停用的 key 返回 401。存在 `data/config.json`;
  老版本的 `data/api_keys.txt` 第一次启动时自动导入(起名「旧 key N」),原文件改名为 `.imported` 留底;
  不再读 `API_KEYS` 环境变量;
- `ACCESS_KEY` 没设置时不拦任何请求(本机内网随意用),但带了 key 的请求照样按 key 名记进请求记录。

| 方法 | 路径 | 权限 | 说明 |
|---|---|---|---|
| GET | `/health` | 无 | 状态、账号数、使用者 key 数量、是否能登录 |
| GET | `/api/models` | 管理员/使用者 | 模型列表 |
| GET | `/api/voices?title=&language=&page_size=&page_number=` | 管理员/使用者 | 音色搜索(参数原样转给上游 `/model`) |
| GET | `/api/voices/{voice_id}` | 管理员/使用者 | 音色详情 |
| POST | `/api/tts` | 管理员/使用者 | 官方 /v1/tts,需 `X-Fish-Key` 或 `FISH_API_KEY`,**扣 API 积分** |
| POST | `/api/web/tts` | 管理员/使用者 | 网页 /task,**扣订阅套餐 credits**,响应头带 `X-Task-Id` |
| GET | `/api/web/credit?account=` | 管理员/使用者 | 两个钱包余额 |
| GET | `/api/web/task/{task_id}?account=` | 管理员/使用者 | 任务状态 |
| GET / POST | `/api/keys` | 仅管理员 | 使用者 key 列表 / 发放(`{"name":"给谁用"}`,key 自动生成 `sk-fish-…`) |
| PATCH / DELETE | `/api/keys/{name}` | 仅管理员 | 改名 `{"name"}`、启用停用 `{"enabled"}` / 吊销 |
| GET / PUT | `/api/settings` | 仅管理员 | 面板「设置」:读 / 改(`{"变量名":"值"}`,空值 = 退回环境变量/默认值),立即生效 |
| DELETE | `/api/cache` | 仅管理员 | 清空结果缓存 |
| GET / DELETE | `/api/logs/requests` | 仅管理员 | 请求记录:`from`/`to`(毫秒)、`key`、`account`、`status`(ok/fail/cache)、`q`、`limit`/`offset`、`bucket`(趋势每格毫秒);返回汇总、按 key/账号分组、趋势和明细 / 清空 |
| GET | `/api/logs/requests/{id}/audio` | 仅管理员 | 回放这条的音频(结果缓存里还在才有) |
| GET / DELETE | `/api/logs/server?after=` | 仅管理员 | 运行日志(增量拉)/ 清空面板里的这份 |
| GET / POST | `/api/accounts` | 仅管理员 | 账号列表 / 添加账号(`name,email,password,token,provider,interactive`) |
| POST | `/api/accounts/{name}/login?interactive=1` | 仅管理员 | 立即登录(1=弹窗人工);没浏览器的机器返回错误 |
| PUT | `/api/accounts/{name}/token` | 仅管理员 | 给账号写入新 token(`{"token":...}`),不存在就新建;推送、粘贴、书签都走它,上游不认的 token 直接 400 |
| PATCH | `/api/accounts/{name}` | 仅管理员 | 改续期方式 `{"provider":"github" / "google" / ""}` |
| DELETE | `/api/accounts/{name}` | 仅管理员 | 删除账号 |
| GET | `/v1/models` | 管理员/使用者 | OpenAI 格式的模型列表(new-api「获取模型」用) |
| POST | `/v1/audio/speech` | 管理员/使用者 | OpenAI 兼容 TTS,走网页会话;Fish 参数放 `metadata`(见「接入 new-api」) |
| POST | `/v1/images/generations` | 管理员/使用者 | OpenAI 兼容文生图,内部走 `/api/web/media` 同步等结果 |

**账号选择**:`/api/web/*` 带 `X-Account: <账号名>`(或 `?account=`)用指定账号;不带时在有效账号间轮询;
再退到 `X-Fish-Web-Token` 头 / `FISH_WEB_TOKEN` 环境变量;都没有返回 401。

## 图像/视频生成(网页「图像与视频」工具的封装)

上游接口 `/tools/image-video/*`,**不需要 reCAPTCHA**,扣订阅套餐 credits。
模型:`GET /api/web/media/models`(图像:nano-banana/seedream/gemini-3-pro-image/gpt-image…;
视频:kling/veo/seedance/minimax…);资格与每日额度:`GET /api/web/media/entitlements`
(free 套餐每天 3 张图,**视频要 plus**)。

`POST /api/web/media`(管理员或使用者 key):

```json
{"model":"nano-banana","variant":"t2i","mode":"image","count":1,
 "params":{"aspect_ratio":"1:1","image_size":"1K"},
 "prompt":"a cat","wait":240}
```

- `variant`:`t2i`(文生图)/ `t2v`(文生视频);`params` 键为 snake_case 且必须与
  上游计价档位精确匹配(`aspect_ratio`/`image_size`/`resolution`/`duration`),不匹配报 400;
- `wait=0` 只拿 `group_id`(异步);`wait>0` 同步轮询到完成,`result.results[i].mediaItem`
  里有 `url`(R2 原始地址,需 token)与 `sigUrl`(15 分钟签名的代理地址,`<img>/<video>` 可直接加载);
- `GET /api/web/media/{group_id}` 查状态;`GET /api/web/media/file?url=<R2>&sig=<签名>` 代理下载。

**合成请求体**(`POST /api/web/tts`,仅 `text` 必填;`backend` 不填默认 drama-3,跟网页一样;`prosody` 只给部分键时其余用默认值):

```json
{
  "text": "你好,[停顿]这是 <whisper>悄悄话</whisper> 测试。",
  "voice_id": "dd43b30d04d9446a94ebe41f301229b5",
  "backend": "drama-3",
  "format": "mp3",
  "prosody": {"speed": 1.0, "pitch": 1.0, "volume": 0.0, "normalize_loudness": true},
  "latency": "normal",
  "normalize": true
}
```

调用中 token 失效(上游 401)时,服务会自动重登一次并重放请求(需本地,且账号有密码或续期方式);
重登不成就丢弃该 token、账号标记 `login_failed`,轮询不再选它,拿到新 token 后恢复。

## 接入 new-api(当 OpenAI 的语音和图像模型用)

服务实现了 OpenAI 的 `POST /v1/audio/speech`(语音)和 `POST /v1/images/generations`(文生图),在 new-api 里加一个 **OpenAI 类型渠道**就能用:

- Base URL 填这个服务的地址,密钥填面板「API Keys」页发的一把使用者 key;
- 模型:语音 `drama-3`、`s2.1-pro`、`s2-pro`、`s1`,也认 `tts-1` / `tts-1-hd`(等于 drama-3);
  图像是账号能用的文生图模型加 `fish-` 前缀(如 `fish-nano-banana`、`fish-gpt-image-2`,免得和别的渠道的同名模型撞)。点「获取模型」会自动填;
- 记得在 new-api 里给这些模型设价格(或放进倍率为 0 的分组自己用),没有倍率的模型 new-api 会拒绝。

**语音**,请求体就是 OpenAI 的格式:

```json
{"model": "drama-3", "input": "你好 [停顿] 世界", "voice": "丁真", "response_format": "mp3", "speed": 1.0,
 "metadata": {"prosody": {"pitch": 1.1, "volume": 3}, "latency": "balanced"}}
```

- `voice` 给 Fish 的 32 位声音 ID,或直接写声音名字(按名字搜,标题得包含这个名字,否则报错并提示最接近的那个,避免悄悄用错声音);
  多人对话把几个声音用逗号隔开;`response_format` 支持 mp3 / wav / opus / pcm;
- Fish 自己的参数和 `/api/web/tts` 同名:`prosody`(speed / pitch / volume / normalize_loudness,给几个改几个)、`latency`、`normalize`、`sampler`、`group_id`、`extra`。
  直接调本服务时放顶层或 `metadata` 里都行;**经 new-api 转发时只有 `metadata` 会原样保留**(new-api 会丢掉它不认识的字段)。

**图像**,OpenAI 格式,同步等出图(约 1 分钟):

```json
{"model": "fish-nano-banana", "prompt": "一只橘猫趴在窗台上晒太阳", "n": 1, "size": "1792x1024",
 "response_format": "url", "extra_fields": {"image_size": "2K"}}
```

- `size` 换成最接近的比例(1:1 / 16:9 / 9:16 / 4:3 / 3:4 / 3:2 / 2:3),`quality` 为 high / hd 时用 2K;
  也能直接给 Fish 的 `aspect_ratio`、`image_size`、`params`,经 new-api 时放 `extra_fields` 里;
- 返回 `{"created", "data": [{"url"}]}`,url 是本服务 15 分钟有效的签名下载链接;`response_format: "b64_json"` 时直接给图片 base64;
- 视频要 plus 套餐,还没接。

酒馆这类客户端选 OpenAI 兼容的 TTS / 生图提供商、地址填 new-api 即可。

## 配置(环境变量)

标 ✎ 的也能在面板「设置」里改,保存即生效,面板里的值比环境变量优先(存在数据目录的 `config.json`)。

| 变量 | 默认 | 说明 |
|---|---|---|
| `HOST` / `PORT` | 127.0.0.1 / 8765 | 监听地址(Docker 里 HOST=0.0.0.0) |
| `ACCESS_KEY` | - | 设了之后 `/api/*`、`/v1/*` 必须带密钥,公网部署必设 |
| `PUSH_URL` / `PUSH_KEY` | - | 本地实例把 token 推送到这个服务器(它的地址和 ACCESS_KEY) |
| `FISH_API_KEY` ✎ | - | API key 模式 |
| `FISH_WEB_TOKEN` ✎ | - | 手动网页 token(账号库之外的兜底) |
| `TOKEN_VERIFY_INTERVAL_SECONDS` ✎ | 600 | 多久向上游验证一次 token 还有效 |
| `TOKEN_CHECK_INTERVAL_SECONDS` | 60 | 巡检周期 |
| `TOKEN_REFRESH_MARGIN_SECONDS` | 1800 | 只对带过期时间的 JWT 有意义:剩余低于该值触发重登 |
| `LOGIN_HEADLESS` | 0 | 1=密码自动登录用无头浏览器;交互登录始终有头;OAuth 续期始终无头 |
| `LOGIN_TIMEOUT_SECONDS` | 90 | 自动登录超时 |
| `LOGIN_INTERACTIVE_TIMEOUT_SECONDS` | 300 | 交互登录等待人工完成的时间 |
| `RECAPTCHA_TTL_SECONDS` ✎ | 100 | 人机验证 token 缓存时长(官方有效期约 120 秒;调 1 可测试每次全新收割) |
| `RECAPTCHA_KEEPWARM` ✎ | 0 | 1=快到期就自动换新,合成永远不用等验证;代价是更频繁地请求 Google |
| `RECAPTCHA_HEADED` | 0 | 1=人机验证改用有头窗口收割(排查风控问题时用,平时不需要) |
| `RECAPTCHA_ALLOW_DOWNLOAD` | 0 | 1=没找到浏览器时允许 rod 自动下载无头内核(仅够人机验证收割;正式镜像已带 chromium,不需要) |
| `DISABLE_LOGIN` | 0 | 1=服务器模式:关闭登录(容器无显示器),面板隐藏登录按钮,只收推送/粘贴的 token |
| `CACHE_MAX_MB` ✎ | 256 | 结果缓存总量上限(MB);0=关闭缓存 |
| `CACHE_MAX_ITEM_MB` ✎ | 16 | 单条音频超过该大小就不进缓存 |
| `CACHE_TTL_SECONDS` ✎ | 86400 | 一条缓存最多留多久;0=不过期 |
| `ACCOUNT_COOLDOWN_SECONDS` ✎ | 30 | 账号被上游 429/5xx 后冷却多久,期间轮询跳过它(X-Account 指定的不受影响) |
| `REQUEST_LOG_MAX` ✎ | 5000 | 请求记录保留条数;0=不再记录 |
| `REQUEST_LOG_TEXT` ✎ | 1 | 0=请求记录只记字数,不记文字内容(默认记前 200 字) |
| `ACCOUNTS_FILE` | accounts.json | 账号库路径;`config.json`(面板设置 + 使用者 key)和 `requests.jsonl`(请求记录)放在同一个目录 |

## 文件

`main.go` 接口与启动 · `accounts.go` 账号库/验证/续期 · `login.go` 浏览器登录 · `recaptcha.go` 网页合成的人机验证(自动收割 reCAPTCHA token)·
`apikeys.go` 使用者 key 与鉴权 · `admin.go` 面板设置、请求记录、运行日志 · `media.go` 图像/视频生成 · `cache.go` 结果缓存 · `index.html` 面板(编译时打进二进制) ·
`client_test.py` 命令行自测脚本 · `Dockerfile` / `docker-compose.yml` 服务器部署。

## 安全与注意事项

- 本地 `accounts.json` 里的密码是**明文**(0600 权限)、`browser-profile/` 里有登录 cookie,都勿提交版本库;服务器那份只有 token,`PUT .../token` 不收密码;
- 公网一定设 `ACCESS_KEY`,容器只绑 127.0.0.1 交给反代做 HTTPS;
- 这是非官方自动化,请只用于自己的账号、控制频率,账号风险自担;
- **Windows 本机会在安全日志里看到 4625 登录失败记录**:Chromium 用全新配置目录首次启动时,会拿空密码试登一次 Windows 账户(账户有密码就记一条失败,这是 Chromium 源码里的已知行为,不是入侵)。收割浏览器的配置目录已持久化,这个检查一生只发生一次;重建目录(目录被强杀残留锁住时)会再发生一次。平时用的 Chrome 只试一次是同样的机制。Linux/容器无此行为。

官方文档:https://docs.fish.audio
