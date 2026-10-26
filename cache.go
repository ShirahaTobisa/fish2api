// 结果缓存:同一段文字 + 同一个声音 + 同样参数 → 复用上次生成的音频。
// 重复请求秒回、不再扣订阅额度、也少打一次上游(连带少取一次人机验证)。
// 纯内存,带总量上限的 LRU + TTL;CACHE_MAX_MB=0 关闭缓存。
// 注意:相同参数会一直回放同一段音频,想每次不同就改文字/参数或关缓存。
package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
)

type cacheEntry struct {
	key   string
	data  []byte
	mime  string
	added time.Time
}

type audioCache struct {
	mu       sync.Mutex
	ll       *list.List // 队首最新,队尾最旧
	idx      map[string]*list.Element
	bytes    int
	maxBytes int           // 总量上限;<=0 关闭
	maxItem  int           // 单条上限;超过的不缓存
	ttl      time.Duration // 一条最多留多久;<=0 不过期
}

var ttsCache = newAudioCache()

func newAudioCache() *audioCache {
	return &audioCache{
		ll:       list.New(),
		idx:      map[string]*list.Element{},
		maxBytes: envIntZero("CACHE_MAX_MB", 256) << 20,
		maxItem:  envIntZero("CACHE_MAX_ITEM_MB", 16) << 20,
		ttl:      time.Duration(envIntZero("CACHE_TTL_SECONDS", 24*3600)) * time.Second,
	}
}

// cacheKey 只取影响输出的字段(不含账号/凭证:同参数输出一致,哪个账号生成都一样)。
func cacheKey(in *webTTSRequest) string {
	b, _ := json.Marshal([]any{in.Text, in.VoiceID, in.Backend, in.Format, in.Prosody, in.Latency, in.Normalize, in.Sampler, in.Extra})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *audioCache) get(key string) (*cacheEntry, bool) {
	if c.maxBytes <= 0 {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.idx[key]
	if !ok {
		return nil, false
	}
	e := el.Value.(*cacheEntry)
	if c.ttl > 0 && time.Since(e.added) > c.ttl {
		c.removeElem(el)
		return nil, false
	}
	c.ll.MoveToFront(el)
	return e, true
}

func (c *audioCache) put(key string, data []byte, mime string) {
	if c.maxBytes <= 0 || len(data) == 0 || len(data) > c.maxBytes || (c.maxItem > 0 && len(data) > c.maxItem) {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[key]; ok {
		c.removeElem(el)
	}
	c.idx[key] = c.ll.PushFront(&cacheEntry{key: key, data: data, mime: mime, added: time.Now()})
	c.bytes += len(data)
	for c.bytes > c.maxBytes {
		back := c.ll.Back()
		if back == nil {
			break
		}
		c.removeElem(back)
	}
}

func (c *audioCache) removeElem(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.idx, e.key)
	c.bytes -= len(e.data)
}
