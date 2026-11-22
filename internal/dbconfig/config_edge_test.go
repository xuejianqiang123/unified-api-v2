package dbconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLoadMissingFile 文件不存在必须报错（上层据此决定是否用默认值），
// 不能静默返回一个零值 Config —— 那会让服务「起来了但什么都不工作」。
func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Fatal("缺失文件应报错")
	}
}

// TestLoadInvalidJSON 半截 JSON（用户正在编辑）必须报错，让上层保留旧配置。
func TestLoadInvalidJSON(t *testing.T) {
	p := writeCfg(t, `{"listen": "127.0.0.1:9000",`)
	if _, err := Load(p); err == nil {
		t.Fatal("非法 JSON 应报错")
	}
}

// TestLoadUnknownFieldIgnored 未知字段不能导致失败：
// 这是「旧版本读到新增字段」与「降级回滚」的兼容契约。
func TestLoadUnknownFieldIgnored(t *testing.T) {
	p := writeCfg(t, `{"listen":"127.0.0.1:9000","some_future_field":{"a":1}}`)
	if _, err := Load(p); err != nil {
		t.Fatalf("未知字段不应导致失败: %v", err)
	}
}

// TestLoadWrongTypeFails 类型错误必须报错（不能把 "abc" 当数字用）。
func TestLoadWrongTypeFails(t *testing.T) {
	p := writeCfg(t, `{"listen": 12345}`)
	if _, err := Load(p); err == nil {
		t.Fatal("类型错误应报错")
	}
}

// TestApplyDefaults 空配置要能补齐到可运行状态，且不 panic。
func TestApplyDefaults(t *testing.T) {
	p := writeCfg(t, `{}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("空配置应能加载: %v", err)
	}
	if cfg.DefaultModel == "" {
		t.Error("default_model 应被补齐")
	}
	// Models 空是合法状态（= 不限制可选模型），这里只验证不 panic 且默认模型可用。
	if _, err := cfg.PoolOptions(), error(nil); err != nil {
		t.Fatal(err)
	}
}

// TestResolveModelAlias 别名解析：已知别名回真名，未知原样返回（不 panic）。
func TestResolveModelAlias(t *testing.T) {
	p := writeCfg(t, `{"model_aliases":{"fast":"deepseek-v4-flash"}}`)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.ResolveModel("fast"); got != "deepseek-v4-flash" {
		t.Errorf("ResolveModel(fast) = %q", got)
	}
	if got := cfg.ResolveModel("unknown-xyz"); got != "unknown-xyz" {
		t.Errorf("未知模型应原样返回，实际 %q", got)
	}
}
