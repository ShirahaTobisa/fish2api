// 图像/视频模型的参数档位表(从 fish.audio 网页前端逆向提取,2026-10)。
// 每个模型:mode(image/video)、默认 variant(t2i/t2v)、各参数的合法选项与默认值。
// 上游计价按 params 精确匹配档位,不合法直接 400 —— 面板的下拉与 /api/web/media/schema 用它。
package main

import (
	_ "embed"
	"encoding/json"
)

//go:embed media-schema.json
var mediaSchemaJSON []byte

// mediaSchema 模型名 → 参数定义。
var mediaSchema = func() map[string]mediaModelSchema {
	var m map[string]mediaModelSchema
	_ = json.Unmarshal(mediaSchemaJSON, &m)
	return m
}()

type mediaModelSchema struct {
	Mode     string              `json:"mode"`
	Variant  string              `json:"variant"`
	Params   map[string][]string `json:"params"`
	Defaults map[string]any      `json:"defaults"`
}
