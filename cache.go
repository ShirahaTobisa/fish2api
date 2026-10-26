// 结果缓存:同一段文字 + 同一个声音 + 同样参数 → 复用上次生成的音频。
// 重复请求秒回、不再扣订阅额度、也少打一次上游(连带少取一次人机验证)。
// 纯内存,带总量上限的 LRU + TTL;CACHE_MAX_MB=0 关闭缓存。上限在面板「设置」里能改,保存即生效(configure)。
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
	c := &audioCache{ll: list.New(), idx: map[string]*list.Element{}}
	c.configure()
	return c
}

// configure 按当前环境变量设上限(启动、面板保存设置后各调一次);调小了立刻按新上限淘汰。
func (c *audioCache) configure() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.maxBytes = envIntZero("CACHE_MAX_MB", 256) << 20
	c.maxItem = envIntZero("CACHE_MAX_ITEM_MB", 16) << 20
	c.ttl = time.Duration(envIntZero("CACHE_TTL_SECONDS", 24*3600)) * time.Second
	c.evict()
}

// cacheKey 只取影响输出的字段(不含账号/凭证:同参数输出一致,哪个账号生成都一样)。
func cacheKey(in *webTTSRequest) string {
	b, _ := json.Marshal([]any{in.Text, in.VoiceID, in.Backend, in.Format, in.Prosody, in.Latency, in.Normalize, in.Sampler, in.Extra})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (c *audioCache) get(key string) (*cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.live(key)
	if el == nil {
		return nil, false
	}
	c.ll.MoveToFront(el)
	return el.Value.(*cacheEntry), true
}

// has 缓存里还有没有这条(只看不用,不影响淘汰顺序;请求记录判断能不能回放用)。
func (c *audioCache) has(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.live(key) != nil
}

// live 找没过期的条目(需持锁);过期的顺手删掉。
func (c *audioCache) live(key string) *list.Element {
	el, ok := c.idx[key]
	if !ok || c.maxBytes <= 0 {
		return nil
	}
	if c.ttl > 0 && time.Since(el.Value.(*cacheEntry).added) > c.ttl {
		c.removeElem(el)
		return nil
	}
	return el
}

func (c *audioCache) put(key string, data []byte, mime string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.maxBytes <= 0 || len(data) == 0 || len(data) > c.maxBytes || (c.maxItem > 0 && len(data) > c.maxItem) {
		return
	}
	if el, ok := c.idx[key]; ok {
		c.removeElem(el)
	}
	c.idx[key] = c.ll.PushFront(&cacheEntry{key: key, data: data, mime: mime, added: time.Now()})
	c.bytes += len(data)
	c.evict()
}

// evict 超出总量上限就从最旧的删起;缓存关了(上限 0)就全删(需持锁)。
func (c *audioCache) evict() {
	for c.bytes > max(c.maxBytes, 0) && c.ll.Len() > 0 {
		c.removeElem(c.ll.Back())
	}
}

// itemLimit 单条上限:流式回传时边收边攒,超过就不攒。
func (c *audioCache) itemLimit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.maxItem
}

// clear 清空(面板「设置」里的按钮)。
func (c *audioCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	clear(c.idx)
	c.bytes = 0
}

// stats 当前条数和占用字节。
func (c *audioCache) stats() (items, bytes int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ll.Len(), c.bytes
}

func (c *audioCache) removeElem(el *list.Element) {
	e := el.Value.(*cacheEntry)
	c.ll.Remove(el)
	delete(c.idx, e.key)
	c.bytes -= len(e.data)
}
