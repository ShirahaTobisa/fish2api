// 内嵌 Web 面板:index.html 单文件(无外部依赖),编译时打进二进制。
package main

import (
	_ "embed"
	"net/http"
)

//go:embed index.html
var indexHTML []byte

func handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
