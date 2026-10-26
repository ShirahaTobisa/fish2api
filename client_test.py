"""快速自测脚本:调本地 API 合成语音/管理账号。

用法:
  # 账号管理
  python client_test.py --mode accounts                        # 查看账号状态
  python client_test.py --mode add --email you@example.com --password xxx
  python client_test.py --mode add --email you@example.com --interactive   # OAuth 式人工登录
  python client_test.py --mode login --account acc1 --interactive
  python client_test.py --mode credit --account acc1           # 查账号套餐余额

  # 合成(默认走网页会话模式,扣订阅套餐 credits)
  python client_test.py --account acc1 --voice dd43b30d04d9446a94ebe41f301229b5 --out hello.mp3
  python client_test.py --mode api --key <APIKEY>              # API key 模式对照(扣 API 积分)
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("TTS_API_BASE", "http://127.0.0.1:8765")
# 服务器设了 ACCESS_KEY 时:export TTS_ACCESS_KEY=xxx
AUTH = {"Authorization": "Bearer " + os.environ["TTS_ACCESS_KEY"]} if os.environ.get("TTS_ACCESS_KEY") else {}


def req_json(method: str, url: str, payload: dict | None = None, headers: dict | None = None, timeout: int = 60):
    data = json.dumps(payload).encode("utf-8") if payload is not None else None
    req = urllib.request.Request(url, data=data, headers={"Content-Type": "application/json", **AUTH, **(headers or {})}, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read() or b"{}"), resp.headers
    except urllib.error.HTTPError as e:
        try:
            return e.code, json.loads(e.read() or b"{}"), e.headers
        except Exception:
            return e.code, {}, e.headers


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--mode", default="tts",
                    choices=["tts", "api", "credit", "accounts", "add", "login"])
    ap.add_argument("--text", default="你好,这是通过本地 API 调用 Fish Audio 生成的语音。[停顿] 测试成功。")
    ap.add_argument("--voice", default=None, help="音色 ID(网页'选择声音'选的那个)")
    ap.add_argument("--model", default=None, help="api 模式=模型名;web 模式=backend,如 drama-3")
    ap.add_argument("--format", default="mp3", choices=["mp3", "wav", "pcm", "opus"])
    ap.add_argument("--speed", type=float, default=1.0)
    ap.add_argument("--volume", type=float, default=0.0)
    ap.add_argument("--account", default=None, help="账号名(X-Account)")
    ap.add_argument("--token", default=None, help="手动指定网页 JWT")
    ap.add_argument("--key", default=None, help="手动指定 API key")
    ap.add_argument("--email", default=None)
    ap.add_argument("--password", default=None)
    ap.add_argument("--name", default=None, help="账号别名")
    ap.add_argument("--interactive", action="store_true", help="弹浏览器窗口人工登录(OAuth 式)")
    ap.add_argument("--out", default=None, help="输出文件名")
    args = ap.parse_args()

    def acc_headers(extra: dict | None = None) -> dict:
        h = {}
        if args.account:
            h["X-Account"] = args.account
        if args.token:
            h["X-Fish-Web-Token"] = args.token
        if args.key:
            h["X-Fish-Key"] = args.key
        return {**h, **(extra or {})}

    if args.mode == "accounts":
        _, data, _ = req_json("GET", f"{BASE}/api/accounts")
        print(json.dumps(data, ensure_ascii=False, indent=2))
        return 0

    if args.mode == "add":
        status, data, _ = req_json("POST", f"{BASE}/api/accounts", {
            "name": args.name or "", "email": args.email or "", "password": args.password or "",
            "interactive": args.interactive,
        })
        print(f"HTTP {status}", json.dumps(data, ensure_ascii=False, indent=2))
        return 0

    if args.mode == "login":
        if not args.account:
            print("--account 必填", file=sys.stderr)
            return 2
        interactive = "1" if args.interactive else "0"
        status, data, _ = req_json("POST", f"{BASE}/api/accounts/{args.account}/login?interactive={interactive}", timeout=400)
        print(f"HTTP {status}", json.dumps(data, ensure_ascii=False, indent=2))
        return 0

    if args.mode == "credit":
        qs = f"?account={args.account}" if args.account else ""
        status, data, _ = req_json("GET", f"{BASE}/api/web/credit{qs}", headers=acc_headers())
        print(f"HTTP {status}", json.dumps(data, ensure_ascii=False, indent=2))
        return 0

    # ---------- 合成 ----------
    payload: dict = {"text": args.text, "format": args.format}
    if args.voice:
        payload["voice_id"] = args.voice
    if args.model:
        payload["backend" if args.mode == "tts" else "model"] = args.model
    if args.mode == "api":
        payload["prosody"] = {"speed": args.speed, "volume": args.volume}
        url = f"{BASE}/api/tts"
    else:
        payload["prosody"] = {"speed": args.speed, "pitch": 1.0, "volume": args.volume}
        url = f"{BASE}/api/web/tts"

    t0 = time.time()
    req = urllib.request.Request(url, data=json.dumps(payload).encode("utf-8"),
                                 headers={"Content-Type": "application/json", **AUTH, **acc_headers()}, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=300) as resp:
            audio = resp.read()
            task_id = resp.headers.get("X-Task-Id", "")
    except urllib.error.HTTPError as e:
        print(f"HTTP {e.code}: {e.read().decode('utf-8', 'replace')}", file=sys.stderr)
        return 1

    out = args.out or f"tts_test_{args.mode}_{int(time.time())}.{args.format}"
    with open(out, "wb") as f:
        f.write(audio)
    extra = f" | task-id: {task_id}" if task_id else ""
    print(f"HTTP 200 | {len(audio)} bytes | {time.time() - t0:.1f}s{extra} | 已保存: {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
