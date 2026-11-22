package main

import (
	"encoding/json"
	"log"
	"os"
	"reflect"
	"time"

	"unified-api-v2/internal/server"
)

// configWatchInterval 轮询间隔。与 auths 目录监听（5s）同口径：
// 不引第三方 fsnotify，代价是最多 5s 的延迟 —— 对「改别名」这种低频操作足够。
const configWatchInterval = 5 * time.Second

// watchConfig 轮询 config.json，变化时热载**别名表**，其余变更只提示需重启。
//
// 为什么不全量热载：listen / providers 接线 / api_key 变更会改变服务形态，
// 半途换掉只会产生「看起来生效了但连接被拒」这类更难查的问题。宁可明确要求重启。
func watchConfig(h *server.Handler, path string, boot Config) {
	fi, err := os.Stat(path)
	if err != nil {
		log.Printf("配置热载未启用（%s 不可读: %v）", path, err)
		return
	}
	last := fi.ModTime()
	log.Printf("配置热载已启用：%s（每 %s 检查；只热载 aliases，其余变更需重启）",
		path, configWatchInterval)

	go func() {
		t := time.NewTicker(configWatchInterval)
		defer t.Stop()
		for range t.C {
			fi, err := os.Stat(path)
			if err != nil || !fi.ModTime().After(last) {
				continue
			}
			last = fi.ModTime()

			raw, err := os.ReadFile(path)
			if err != nil {
				log.Printf("配置热载：读文件失败，保留旧配置: %v", err)
				continue
			}
			var next Config
			if err := json.Unmarshal(raw, &next); err != nil {
				// 解析失败**绝不**替换：用户可能正在编辑（半截 JSON）。
				log.Printf("配置热载：解析失败，保留旧配置: %v", err)
				continue
			}
			h.ReloadAliases(next.Aliases)
			warnRestartRequired(boot, next)
		}
	}()
}

// warnRestartRequired 把这些字段的变更显式说出来，避免用户以为已经生效。
func warnRestartRequired(boot, next Config) {
	var items []string
	if boot.Listen != next.Listen {
		items = append(items, "listen")
	}
	if boot.APIKey != next.APIKey {
		items = append(items, "api_key")
	}
	if boot.AuthDir != next.AuthDir {
		items = append(items, "auth_dir")
	}
	if !reflect.DeepEqual(boot.Providers, next.Providers) {
		items = append(items, "providers")
	}
	if boot.StateFile != next.StateFile {
		items = append(items, "state_file")
	}
	if len(items) > 0 {
		log.Printf("WARN: 以下字段已变更但**必须重启**才生效：%v", items)
	}
}
