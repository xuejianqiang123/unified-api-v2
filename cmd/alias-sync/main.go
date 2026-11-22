// alias-sync 对比「上游模型目录」与「config.json 里已暴露的别名」，只输出 diff。
//
// 刻意**不写文件**：别名的取舍涉及路由与成本，机器给建议、人来拍板。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"time"

	"unified-api-v2/internal/codearts"
)

type aliasDoc struct {
	Aliases map[string]struct {
		Model           string            `json:"model"`
		ModelByProvider map[string]string `json:"model_by_provider"`
	} `json:"aliases"`
}

func main() {
	cfgPath := flag.String("config", "config.json", "config.json 路径")
	credPath := flag.String("accounts", "codearts/accounts.json", "codearts 凭据路径")
	fromFile := flag.String("from", "", "从本地保存的 /v1/model/builtin 响应读取（接口参数未确认时的离线模式）")
	flag.Parse()

	raw, err := os.ReadFile(*cfgPath)
	if err != nil {
		fmt.Println("读取配置失败:", err)
		os.Exit(1)
	}
	var doc aliasDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Println("解析配置失败:", err)
		os.Exit(1)
	}

	// 已暴露给 codearts 的上游模型名集合
	covered := map[string]bool{}
	for _, a := range doc.Aliases {
		if m, ok := a.ModelByProvider["codearts"]; ok && m != "" {
			covered[m] = true
			continue
		}
		// 未显式声明映射时，规范名本身就是 codearts 的模型名（仅对小写同名的那些成立）
		covered[a.Model] = true
	}

	var models []codearts.BuiltinModel
	if *fromFile != "" {
		raw, err := os.ReadFile(*fromFile)
		if err != nil {
			fmt.Println("读取模型目录文件失败:", err)
			os.Exit(1)
		}
		var doc struct {
			Models []codearts.BuiltinModel `json:"builtinModels"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			fmt.Println("解析模型目录失败:", err)
			os.Exit(1)
		}
		models = doc.Models
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		cli := codearts.NewClient(*credPath)
		var err error
		models, err = cli.BuiltinModels(ctx)
		if err != nil {
			fmt.Println("拉取上游模型目录失败:", err)
			fmt.Println("提示 1：凭据过期时先跑 go run ./cmd/codearts-import（或在面板点「导入」）")
			fmt.Println("提示 2：用 -from <本地响应.json> 走离线模式（接口参数尚待确认）")
			os.Exit(1)
		}
	}

	var missing []codearts.BuiltinModel
	for _, m := range models {
		if !m.Enable {
			continue
		}
		if !covered[m.ModelID] {
			missing = append(missing, m)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].ModelID < missing[j].ModelID })

	fmt.Printf("上游可用模型 %d 个，已建别名覆盖 %d 个\n", len(models), len(models)-len(missing))
	if len(missing) == 0 {
		fmt.Println("没有遗漏。")
		return
	}
	fmt.Printf("\n未建别名的模型（%d 个）：\n", len(missing))
	for _, m := range missing {
		fmt.Printf("  %-28s ctx=%-9d out=%-7d img=%-5v %s\n",
			m.ModelID, m.ContextWindow, m.MaxTokens, m.SupportsImages, m.ModelName)
	}
	fmt.Println("\n以上仅为建议，不会自动写入 config.json。")
}
