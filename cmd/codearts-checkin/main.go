// codearts-checkin 执行 CodeArts 每日签到。
//
// 用法：
//
//	codearts-checkin -dry-run   # 只列活动，不领取（安全）
//	codearts-checkin           # 执行签到（幂等：已领会如实报 already-claimed）
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"unified-api-v2/internal/codearts"
)

func main() {
	credPath := flag.String("accounts", "codearts/accounts.json", "凭据文件")
	dryRun := flag.Bool("dry-run", false, "只列活动，不领取")
	timeout := flag.Duration("timeout", 30*time.Second, "单次请求超时")
	flag.Parse()

	client := codearts.NewClient(*credPath)
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	acts, err := client.Activities(ctx)
	if err != nil {
		fmt.Println("查询活动列表失败:", err)
		os.Exit(1)
	}
	fmt.Printf("活动 %d 条：\n", len(acts))
	for _, a := range acts {
		claimable := "否"
		if a.Claimable {
			claimable = "是"
		}
		fmt.Printf("  - type=%-12s status=%-10s 可领=%-2s 积分=%-6d %s\n",
			a.Type, a.Status, claimable, a.Amount, a.Title)
	}

	if *dryRun {
		fmt.Println("\n（dry-run，未领取）")
		return
	}

	res, err := client.ClaimDailyCheckin(ctx)
	if err != nil {
		fmt.Println("\n签到失败:", err)
		os.Exit(1)
	}
	switch res.Kind {
	case codearts.ClaimClaimed:
		fmt.Printf("\n✅ 已领取")
		if res.Credit > 0 {
			fmt.Printf(" +%d 积分", res.Credit)
		}
		fmt.Println()
		if res.Message != "" {
			fmt.Println("   " + res.Message)
		}
	case codearts.ClaimAlready:
		fmt.Println("\n今天已经领过了（幂等，无副作用）")
	case codearts.ClaimInactive:
		fmt.Println("\n当前不可领取:", res.Message)
	default:
		fmt.Println("\n失败:", res.Message)
		os.Exit(1)
	}
}
