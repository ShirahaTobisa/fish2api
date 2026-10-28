# fish2api

fish2api 是一个用 Docker 部署的 Fish Audio 网关。它把 [fish.audio](https://fish.audio) 网页版的语音合成、图像/视频生成封装成 API，走账号的**订阅套餐 credits**（网页上用的那份额度），不花 API 积分。管理员在面板里放 Fish 账号的网页 token，给 new-api、酒馆、脚本各发一把使用者 key；对外提供 OpenAI 兼容接口，可以直接当 new-api 的一个渠道用。

项目重点是“能用网页额度、好接入、好管理”：网页合成每次要求的人机验证由服务自动处理，多账号轮询、结果缓存、请求记录、运行日志、在线改设置都在面板里。

## 功能

- 管理面板：`/`，仪表盘、请求记录、运行日志、账号、API Keys、设置，外加语音合成、图像/视频、生成历史三个试用页；深浅色、手机可用
- 两级密钥：`ACCESS_KEY` 是管理员密钥，什么都能做；面板发的使用者 key（`sk-fish-…`）只能合成和查询，有名字、能停用
- 网页合成：`POST /api/web/tts`，扣订阅套餐 credits；网页要求的 reCAPTCHA Enterprise 由容器里的无头 Chromium 自动取，不用人管
- OpenAI 兼容：`/v1/audio/speech`（语音）、`/v1/images/generations`（文生图）、`/v1/models`
- Fish 官方接口同款：`/compat/v1/audio/speech`、`/model`，原来直连 `api.fish.audio` 的插件换个地址和 key 就能用；允许浏览器跨域调用
- 图像/视频：`POST /api/web/media`，封装网页「图像与视频」工具；视频要 plus 套餐
- 官方 API 模式：`POST /api/tts`，用 Fish 官方 API key，扣 API 积分
- 多账号：可用账号间轮询；被上游限流或报错的账号冷却一段时间；每 10 分钟向上游验证 token，失效的提前踢掉；GitHub / Google 登录的账号能自动续期
- 结果缓存：同文字 + 同声音 + 同参数直接回放，不扣额度、秒回
- 请求记录：每次合成谁调的、用了哪个账号、成没成功、花了多久，都能在面板查和汇总；缓存里还在的音频能回放
- 在线设置：缓存、冷却、验证间隔、人机验证等，在面板改完保存马上生效
- 持久化：账号、设置、key、请求记录都在 `/data`

Fish 的网页 token 等同账号登录态，拿到 token 就能用这个账号的额度。公网部署一定要设 `ACCESS_KEY`；这是非官方自动化，请只用于自己的账号、控制频率，账号风险自担。

## 目录结构

```text
.
├─ main.go              # 接口、鉴权、网页合成、OpenAI 语音接口、启动
├─ accounts.go          # 账号库、token 验证、自动续期
├─ recaptcha.go         # 网页合成要的人机验证（无头 Chromium 自动取）
├─ media.go             # 图像/视频生成、OpenAI 文生图接口
├─ apikeys.go           # 使用者 key 和鉴权
├─ admin.go             # 面板设置、请求记录、运行日志
├─ cache.go             # 结果缓存
├─ login.go             # 浏览器登录（旧本地模式用）
├─ index.html           # 管理面板（编译时打进程序）
├─ Dockerfile           # 镜像：Go 程序 + Alpine chromium
├─ docker-compose.yml   # Docker Compose 部署
└─ client_test.py       # 命令行自测脚本
```

## 环境变量

标 ✎ 的也能在面板「设置」里改，保存马上生效，面板里的值比环境变量优先（存在 `/data/config.json`；在面板里清空 = 退回环境变量或默认值）。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `ACCESS_KEY` ✎ | 无 | 管理员密钥。设了之后 `/api/*`、`/v1/*` 都要带密钥，公网部署必设。 |
| `HOST` / `PORT` | 镜像里 `0.0.0.0` / `8765` | 监听地址。 |
| `ACCOUNTS_FILE` | 镜像里 `/data/accounts.json` | 账号库路径；`config.json`、`requests.jsonl` 放在同一个目录。 |
| `DISABLE_LOGIN` | 镜像里 `1` | 服务器模式：容器没有显示器，关掉弹窗登录，token 靠书签或粘贴送进来。 |
| `TOKEN_VERIFY_INTERVAL_SECONDS` ✎ | `600` | 多久向上游验证一次 token 还能用。 |
| `TOKEN_CHECK_INTERVAL_SECONDS` | `60` | 后台巡检周期。 |
| `ACCOUNT_COOLDOWN_SECONDS` ✎ | `30` | 账号被上游 429/5xx 后冷却多久，期间轮询跳过它（`X-Account` 指定的不受影响）。 |
| `CACHE_MAX_MB` ✎ | `256` | 结果缓存总量上限（MB），`0` 关闭缓存。 |
| `CACHE_MAX_ITEM_MB` ✎ | `16` | 单条音频超过这个大小就不缓存。 |
| `CACHE_TTL_SECONDS` ✎ | `86400` | 一条缓存最多留多久，`0` 不过期。 |
| `RECAPTCHA_TTL_SECONDS` ✎ | `100` | 一次人机验证复用多久（官方有效期约 120 秒）。 |
| `RECAPTCHA_KEEPWARM` ✎ | `0` | `1` = 快到期就自动换新，合成永远不用等验证；代价是更频繁地请求 Google。 |
| `REQUEST_LOG_MAX` ✎ | `5000` | 请求记录保留条数，`0` 不再记录。 |
| `REQUEST_LOG_TEXT` ✎ | `1` | `0` = 请求记录只记字数，不记文字内容（默认记前 200 字）。 |
| `FISH_API_KEY` ✎ | 无 | 官方 API key，`/api/tts` 用。 |
| `FISH_WEB_TOKEN` ✎ | 无 | 兜底网页 token：账号库里没有可用账号时用它。 |
| `MEDIA_SIGN_SECRET` | 从 `ACCESS_KEY` 派生 | 图片/视频签名下载链接用的密钥，一般不用设。 |
| `RECAPTCHA_ALLOW_DOWNLOAD` | `0` | `1` = 找不到浏览器时让 rod 自己下载无头内核；正式镜像自带 chromium，不需要。 |

旧的本地模式（在电脑上直接跑、弹窗登录、把 token 推给服务器）已不再维护，相关变量 `PUSH_URL` / `PUSH_KEY`、`LOGIN_HEADLESS`、`LOGIN_TIMEOUT_SECONDS`、`LOGIN_INTERACTIVE_TIMEOUT_SECONDS`、`RECAPTCHA_HEADED`、`TOKEN_REFRESH_MARGIN_SECONDS` 只在那种模式下有用。

## 部署（Docker Compose）

```bash
git clone https://github.com/ShirahaTobisa/fish2api.git
cd fish2api
echo "ACCESS_KEY=$(openssl rand -hex 24)" > .env
docker compose up -d --build
```

- 镜像 = Go 程序 + Alpine 的 chromium（约 +200MB），chromium 专门用来取网页合成要的人机验证；
- 容器只监听 `127.0.0.1:8765`，交给反代做 HTTPS。反代也跑在 Docker 里的话，在同目录建 `docker-compose.override.yml` 把 `fish2api` 加进反代所在的网络（这个文件不进仓库），反代就能按容器名访问 `fish2api:8765`；
- 数据都在 `./data`，升级时拉新代码再 `docker compose up -d --build`，数据不受影响。

Caddy 示例：

```text
tts.example.com {
    reverse_proxy 127.0.0.1:8765
}
```

部署后自测（`200` 且文件以 `ID3` 开头就是全通）：

```bash
curl -X POST https://tts.example.com/api/web/tts \
  -H "Authorization: Bearer $ACCESS_KEY" -H "Content-Type: application/json" \
  -d '{"text":"server test"}' --output test.mp3
```

如果返回 `400 "reCAPTCHA verification failed"`，说明 Google 不给这个机房 IP 的无头浏览器打高分，换一台服务器试，或改用官方 API 模式。

## 首次使用流程

1. 打开域名，输入 `.env` 里的 `ACCESS_KEY`。想换成好记的，在「设置」里改，保存马上生效。
2. 「账号」→「+ 添加账号」，再点「更新 token」：把弹窗里的书签按钮拖到书签栏，用你平时的浏览器登录 fish.audio，在它的页面上点一下书签，token 就自动发过来。也可以在 fish.audio 按 F12 → Application → Local Storage 复制 `token` 粘贴进去。
3. 「API Keys」→「+ 发放新 Key」，写上给谁用（比如 new-api、酒馆）。
4. 「语音合成」里试一句，「请求记录」里能看到这一条。
5. 按下面「接入 new-api」把它加成渠道。

## 账号与 token

- 网页 token 是不透明的会话串（UUID），没有过期时间，只能靠向上游 `/user/self` 验证（401 = 失效）。服务每 10 分钟验证一次，失效的会被标成 `login_failed`、不再参与轮询，拿到新 token 后恢复。
- 合成时上游回 401：设了续期方式的账号会先重新登录、再重放这次请求；不行就丢掉这个 token（存盘），这次请求返回 401，下一次请求换别的账号。
- **自动续期**：账号的「续期方式」设成 GitHub / Google 时，token 失效后服务会用无头浏览器重新走一遍 OAuth 登录。服务器上没法弹窗，要先在电脑上用这个账号登录一次，把配置目录 `browser-profile/<账号名>` 拷到服务器的 `/data/browser-profile/` 下；里面 GitHub/Google 的登录态一般能撑几个月。邮箱密码登录要过人机验证，基本会被拦。
- **选账号**：`/api/web/*` 带 `X-Account: <账号名>`（或 `?account=`）用指定账号；不带就在可用账号间轮询；再不行用请求头 `X-Fish-Web-Token` 或 `FISH_WEB_TOKEN`；都没有返回 401。

## 鉴权

三种传法都认：`Authorization: Bearer <key>`、`X-Api-Key: <key>`、`?key=<key>`。

| 密钥 | 能做什么 |
| --- | --- |
| `ACCESS_KEY`（管理员） | 所有接口，登录面板也用它 |
| 使用者 key（`sk-fish-…`） | 只能调合成和查询：`/v1/*`、`/compat/*`、`/api/tts`、`/api/models`、`/api/voices*`、`/api/web/*`（含图像/视频）；碰账号、key、设置、记录这些管理接口返回 `403`；停用的 key 返回 `401` |

`/`、`/health` 和公开声音库 `/model` 不要密钥。没设 `ACCESS_KEY` 时不拦任何请求（只适合内网），带了 key 的请求照样按 key 名记进请求记录。

浏览器里的插件可以直接跨域调用：所有接口都带 `Access-Control-Allow-Origin: *`，预检请求直接放行。鉴权靠请求头里的 key，不用 cookie，所以别的网站拿不到你的 key 也调不了。

## 语音合成

### 网页合成（扣订阅套餐 credits）

```text
POST /api/web/tts
```

```bash
curl -X POST https://tts.example.com/api/web/tts \
  -H "Authorization: Bearer sk-fish-xxxx" \
  -H "Content-Type: application/json" \
  -H "X-Account: acc1" \
  -d '{
    "text": "你好，[停顿]这是 <whisper>悄悄话</whisper> 测试。",
    "voice_id": "dd43b30d04d9446a94ebe41f301229b5",
    "backend": "drama-3",
    "format": "mp3",
    "prosody": {"speed": 1.0, "pitch": 1.0, "volume": 0, "normalize_loudness": true},
    "latency": "normal",
    "normalize": true
  }' --output out.mp3
```

| 字段 | 默认值 | 说明 |
| --- | --- | --- |
| `text` | 必填 | 要念的文字，可以带网页同款标签（见下）。 |
| `voice_id` | 账号默认声音 | Fish 的 32 位声音 ID；给数组 = 多人对话。不填时用账号默认声音，这种请求不缓存。 |
| `backend` | `drama-3` | 模型：`drama-3`（网页默认，擅长表演）、`s2.1-pro`、`s2-pro`、`s1`。 |
| `format` | `mp3` | `mp3`、`wav`、`opus`、`pcm`。 |
| `prosody` | `speed 1.0`、`pitch 1.0`、`volume 0`、`normalize_loudness true` | 语速、音调、音量（dB）、响度均衡；给几个改几个，其余用默认。 |
| `latency` | 不传 | 延迟模式，原样交给 Fish（官方取值 `normal`、`balanced`、`low`）。 |
| `normalize` | 不传 | 是否先规范化文字（数字、符号读法）。 |
| `sampler` | 不传 | 采样参数，原样交给 Fish 网页接口，本服务不解析。 |
| `group_id` | 空 | 网页的分组字段，一般不用。 |
| `extra` | 不传 | 兜底：对象里的键原样并进发给 Fish 的请求顶层。 |

返回音频流；响应头 `X-Task-Id` 是 Fish 的任务号，`X-Cache: HIT / MISS` 表示有没有命中缓存（没指定声音时没有这个头）。出错时返回 JSON：`{"detail": "原因", "error": {"message": "原因"}}`。

**文字标签**：文字原样交给 Fish，网页上能用的写法这里都能用，比如 `[停顿]`、`[笑着说]`、`[叹气]`、`[语气加重]`、`<whisper>悄悄话</whisper>`、`<strong>重读</strong>`；S2 系列模型也认句首用方括号写的英文语气描述。

**声音从哪找**：`GET /api/voices?title=名字&language=zh&page_size=20` 搜公开声音库（参数原样转给 Fish 的 `/model`），返回里的 `_id` 就是 `voice_id`；面板「语音合成」页能直接搜和试听。

### OpenAI 兼容语音

```text
POST /v1/audio/speech
```

```json
{
  "model": "drama-3",
  "input": "你好 [停顿] 世界",
  "voice": "丁真",
  "response_format": "mp3",
  "speed": 1.0,
  "metadata": {"prosody": {"pitch": 1.1, "volume": 3}, "latency": "balanced"}
}
```

| 字段 | 说明 |
| --- | --- |
| `model` | `drama-3`、`s2.1-pro`、`s2-pro`、`s1`；`tts-1`、`tts-1-hd`、`gpt-4o-mini-tts` 都当 `drama-3`；带 `fish-` 前缀也行。不填用 `drama-3`。 |
| `input` | 要念的文字，同上可以带标签。 |
| `voice` | Fish 的 32 位声音 ID，或直接写声音名字：按名字搜，标题里得包含这个名字，否则报错并提示最接近的那个，免得悄悄用错声音扣额度。多人对话用逗号隔开几个声音。 |
| `response_format` | `mp3`（默认）、`wav`、`opus`、`pcm`。 |
| `speed` | 语速。 |
| `metadata` | Fish 自己的参数，和网页合成同名：`prosody`、`latency`、`normalize`、`sampler`、`group_id`、`extra`。直接调本服务时放顶层、或像官方那样放 `provider.options["fish-audio"]` 也行；**经 new-api 转发时只有 `metadata` 会原样保留**。 |

OpenAI 的 `instructions` 字段 Fish 没有对应功能，会被忽略；要控制语气请在文字里写标签。

### Fish 官方兼容接口（插件用）

```text
POST /compat/v1/audio/speech
```

和 Fish 官方的 `https://api.fish.audio/compat/v1/audio/speech` 同一个路径、同一种请求体。原来直连官方的插件，把地址 `https://api.fish.audio` 换成本服务的地址、key 换成「API Keys」里发的使用者 key，就改走网页额度：

```json
{
  "model": "fish-audio/s2.1-pro",
  "input": "[开心] 你好呀",
  "voice": "dd43b30d04d9446a94ebe41f301229b5",
  "response_format": "mp3",
  "provider": {"options": {"fish-audio": {"prosody": {"volume": 2}, "latency": "balanced"}}}
}
```

- `model` 认官方写法 `fish-audio/…`；官方的免费档（如 `fish-audio/s2.1-pro-free`）在网页上没有，换成同名的网页模型（`s2.1-pro`），扣订阅额度；也认 `drama-3` 这类网页模型名。
- `voice` 是 Fish 声音 ID，留空或写 `default` 用账号默认声音；也能写声音名字、用逗号写多个，规则同上。
- `response_format` 不写时和官方一样给 `pcm`。
- Fish 参数放 `provider.options["fish-audio"]`，照办的有 `prosody`、`latency`、`normalize`（同网页合成）；优先级和官方一样：`provider.options` 高于 `metadata`，高于顶层。
- **参考音频克隆（`references`）暂不支持**，收到会回 `400`；官方 API 的 `temperature`、`top_p`、`chunk_length`、`sample_rate` 和 `X-Fish-*` 请求头网页这条路没有，忽略。

声音库也是官方同款路径，插件搜声音、查声音走同一个地址就行（公开，不要密钥）：

```text
GET /model?title=名字&language=zh&page_size=20
GET /model/{声音ID}
```

查「我自己的声音」（`self=true`）、建声音、删声音这些要官方 API key 的操作本服务不支持；插件要用这几项，还得填官方地址和官方 key。

### 官方 API 模式（扣 API 积分）

```text
POST /api/tts
```

用 Fish 官方 API key：请求头 `X-Fish-Key`，或设置里的 `FISH_API_KEY`。字段：`text`（必填）、`voice_id`、`model`（默认 `s2.1-pro`）、`format`（`mp3`）、`prosody`（`speed`、`volume`）、`temperature`（`0.7`）、`top_p`（`0.7`）、`normalize`（`true`）、`latency`（`normal`）、`chunk_length`（`300`）、`mp3_bitrate`（`128`）、`sample_rate`、`language`、`features`、`extra`。

## 图像 / 视频

上游是网页的「图像与视频」工具，不需要人机验证，扣订阅套餐 credits。免费套餐每天 3 张图，视频要 plus 套餐。账号能用哪些模型：`GET /api/web/media/models`；资格和每日余量：`GET /api/web/media/entitlements`。

### OpenAI 兼容文生图

```text
POST /v1/images/generations
```

```json
{
  "model": "fish-nano-banana",
  "prompt": "一只橘猫趴在窗台上晒太阳",
  "n": 1,
  "size": "1792x1024",
  "response_format": "url",
  "extra_fields": {"image_size": "2K"}
}
```

| 字段 | 说明 |
| --- | --- |
| `model` | Fish 的图像模型，`GET /v1/models` 列出的 `fish-…` 那些（如 `fish-nano-banana`、`fish-seedream-5-0`、`fish-gpt-image-2`）；前缀可带可不带，不填用 `nano-banana`。 |
| `prompt` | 画面描述。 |
| `n` | 张数，默认 `1`；每个模型的上限由 Fish 决定。 |
| `size` | 换成最接近的比例：`1:1`、`16:9`、`9:16`、`4:3`、`3:4`、`3:2`、`2:3`；不给或认不出用 `1:1`。 |
| `quality` | `high` 或 `hd` 时用 2K，其他用 1K。 |
| `response_format` | `url`（默认）或 `b64_json`。 |
| `extra_fields` | Fish 自己的参数：`aspect_ratio`、`image_size`、`params`（其他计价档位参数原样带上），优先于上面的 `size` / `quality`。直接调本服务时放顶层也行；**经 new-api 转发时只有 `extra_fields` 会原样保留**。 |

同步等出图（约 1 分钟，最多 4 分钟），返回：

```json
{"created": 1793000000, "data": [{"url": "https://tts.example.com/api/web/media/file?url=…&sig=…"}]}
```

`url` 是本服务的签名下载链接，15 分钟内不用密钥就能打开；`b64_json` 直接给图片内容。

出错时：还没下单的错误原样返回（比如 `429 FREE_IMAGE_DAILY_LIMIT_REACHED` 今天额度用完）；已经下单扣了额度之后，生成失败回 `400`、超时或取不到图回 `504`——这两个状态 new-api 默认不重试，免得重复下单扣费。

### 原生图像 / 视频接口

```text
POST /api/web/media
```

```json
{"model": "nano-banana", "variant": "t2i", "count": 1,
 "params": {"aspect_ratio": "1:1", "image_size": "1K"},
 "prompt": "a cat", "wait": 240}
```

| 字段 | 说明 |
| --- | --- |
| `model` | 模型 ID（不带 `fish-` 前缀），见 `/api/web/media/models`。 |
| `variant` | `t2i` 文生图、`t2v` 文生视频。 |
| `mode` | `image` / `video`，不填按 `variant` 推断。 |
| `count` | 张数，默认 `1`。 |
| `params` | 计价档位参数，键用下划线写法，必须和 Fish 的计价档位完全对上（`aspect_ratio`、`image_size`、`resolution`、`duration`），对不上会报 400。 |
| `prompt` | 画面描述。 |
| `wait` | `0` = 下单后马上返回任务号；大于 0 = 最多等这么多秒，把结果一起返回。 |

返回 `group_id`、`credits_charged`；等到结果时 `result.results[i].mediaItem` 里有 `url`（Fish 的原始地址，要登录态）和 `sigUrl`（本服务的签名代理地址）。查进度：`GET /api/web/media/{group_id}?wait=秒`；下载：`GET /api/web/media/file?url=…&sig=…`。

## 接入 new-api

在 new-api 后台加一个渠道：

| 项 | 填什么 |
| --- | --- |
| 类型 | OpenAI |
| 地址 | 本服务的地址，如 `https://tts.example.com` |
| 密钥 | 面板「API Keys」里发的一把使用者 key |
| 模型 | 点「获取模型」自动填：语音 `drama-3`、`s2.1-pro`、`s2-pro`、`s1`、`tts-1`、`tts-1-hd`，图像 `fish-…` |
| 价格 | 给这些模型设价格，或放进倍率为 0 的分组自己用；没有价格的模型 new-api 会拒绝（开了自用模式除外） |

new-api 转发时会把请求拆开重拼，它不认识的字段都会丢掉，「透传请求体」开关对语音接口也不起作用。所以：

- 语音的 Fish 参数（音调、音量、延迟模式等）放 `metadata`；
- 图像的 Fish 参数（比例、分辨率）放 `extra_fields`；
- 多人对话的几个声音用逗号写在 `voice` 里。

图像模型统一带 `fish-` 前缀，免得和 new-api 里别的渠道的同名模型（如 `gpt-image-2`）抢请求。

经 new-api 调用的示例：

```bash
curl https://new-api.example.com/v1/audio/speech \
  -H "Authorization: Bearer sk-你的new-api令牌" \
  -H "Content-Type: application/json" \
  -d '{"model":"drama-3","input":"测试一下。","voice":"dd43b30d04d9446a94ebe41f301229b5",
       "metadata":{"prosody":{"volume":2}}}' --output out.mp3
```

酒馆这类客户端：TTS 选 OpenAI 兼容，地址填 new-api（或本服务）的 `/v1/audio/speech`，模型填 `drama-3`，声音填 Fish 声音 ID 或名字；生图同理选 OpenAI 兼容，模型填 `fish-…`。

## 管理 API

需要管理员密钥 `ACCESS_KEY`：

| 方法 | 路径 | 说明 |
| --- | --- | --- |
| GET | `/health` | 状态、账号数、使用者 key 数、是否要密钥（不用密钥） |
| GET / POST | `/api/accounts` | 账号列表 / 添加账号（`name`、`email`、`token`、`provider`） |
| PUT | `/api/accounts/{name}/token` | 写入新 token（`{"token": "…"}`），账号不存在就新建；上游不认的 token 直接 400 |
| PATCH | `/api/accounts/{name}` | 改续期方式 `{"provider": "github" / "google" / ""}` |
| DELETE | `/api/accounts/{name}` | 删除账号 |
| GET | `/api/web/credit?account=` | 账号的两个钱包余额（订阅套餐、API 积分） |
| GET | `/api/web/task/{task_id}?account=` | 查 Fish 合成任务 |
| GET / POST | `/api/keys` | 使用者 key 列表 / 发放（`{"name": "给谁用"}`，key 自动生成） |
| PATCH / DELETE | `/api/keys/{name}` | 改名 `{"name"}`、启用停用 `{"enabled"}` / 吊销 |
| GET / PUT | `/api/settings` | 读 / 改设置（`{"变量名": "值"}`，空值 = 退回环境变量或默认值），马上生效 |
| DELETE | `/api/cache` | 清空结果缓存 |
| GET / DELETE | `/api/logs/requests` | 请求记录：`from` / `to`（毫秒）、`key`、`account`、`status`（`ok` / `fail` / `cache`）、`q`、`limit` / `offset`、`bucket`；返回汇总、按 key 和按账号的分组、趋势、明细 / 清空 |
| GET | `/api/logs/requests/{id}/audio` | 回放这条的音频（结果缓存里还在才有） |
| GET / DELETE | `/api/logs/server?after=` | 运行日志（增量拉）/ 清空面板里的这份 |

## 数据存储

都在容器的 `/data`（compose 里映射到 `./data`）：

```text
/data/accounts.json       # 账号和网页 token
/data/config.json         # 面板里改过的设置（含管理员密钥）+ 使用者 key
/data/requests.jsonl      # 请求记录，一行一条
/data/browser-profile/    # 人机验证用的浏览器配置；OAuth 续期的账号配置也放这里
```

备份把这个目录打包就行（`browser-profile` 可以不备）。`accounts.json`、`config.json` 里都是能直接用的凭证，别提交到仓库、别公开。旧版本的 `api_keys.txt` 会在第一次启动时自动导入 `config.json`（起名「旧 key N」），原文件改名为 `api_keys.txt.imported` 留底。

## 结果缓存与可靠性

- **结果缓存**：同一段文字 + 同一个声音 + 同样参数，直接回放上次的音频（响应头 `X-Cache: HIT`），不打上游、不扣额度、也不取人机验证。没指定声音的请求不缓存。想每次都重新生成就改文字/参数，或在设置里把缓存上限设成 `0`。
- **后台验证**：每 `TOKEN_VERIFY_INTERVAL_SECONDS` 向上游验证一次各账号的 token，提前踢掉失效的。
- **失败冷却**：账号被上游 429 或 5xx 时，`ACCOUNT_COOLDOWN_SECONDS` 内轮询跳过它。
- **人机验证**：容器启动时先预热，取到的验证复用 `RECAPTCHA_TTL_SECONDS` 秒，被上游判过期就自动换新重试一次。

## 原理（逆向结论）

- 网页登录态 = `localStorage["token"]`（同时写在同名 cookie 里），以 `Authorization: Bearer <token>` 调 `api.fish.audio`；`GET /user/self` 能查用户 ID，也是判断 token 还有没有效的唯一办法。
- 网页「生成语音」调 `POST /task`（`{"type": "tts", "stream": true, "backend": "drama-3", "parameters": {...}}`），响应是音频流，任务号在响应头 `task-id`。
- 2026-10 起 `/task` 每次都要带 reCAPTCHA Enterprise 分数（`action: playground`），缺了回 400 "Missing reCAPTCHA Enterprise"。服务在无头 Chromium 里打开 fish.audio 的 `robots.txt`，执行和网页同样的脚本取分数；要等 enterprise.js 的完整接口加载完再调用，否则数据中心网络下会报 "render is not a function"。
- 两个钱包：`/wallet/{uid}/package` 是订阅套餐（网页用，`balance` / `total` / `type`），`/wallet/{uid}/api-credit` 是 API 积分（官方 API key 用）。
- 密码登录 `POST /user/token` 也要人机验证，自动化浏览器几乎必被拒；Google / GitHub / Apple 登录走 `/user/oauth-*`，不要求。没有续签接口，token 失效只能重新登录。

## 安全建议

- 公网一定设 `ACCESS_KEY`，容器只绑 `127.0.0.1`，交给反代做 HTTPS。
- 在面板里改过的管理员密钥忘了：到服务器上看 `data/config.json` 里的 `ACCESS_KEY`；把这一项删掉再重启，就退回 `.env` 里的。
- 给每个用途单独发一把使用者 key，泄露了在面板里停用或吊销，不用换管理员密钥。
- `/data` 里是账号 token 和 key，按密码对待；带 `?key=` 的链接也别公开分享。
- 非官方自动化，控制调用频率，账号风险自担。

官方文档：https://docs.fish.audio
