// codearts-import 从本机已登录的码道 IDE 导入凭据到网关的账号文件。
//
// 为什么需要它：网关不持有 refresh_token 的使用权（那是一次性轮换的，抢用会让
// IDE 掉登录），所以 STS 到期后由 IDE 自己续期、本命令重新读一次即可。
//
// 用法：
//
//	codearts-import                      # 导入/更新 codearts/accounts.json
//	codearts-import -check               # 只报当前凭据还剩多久
//	codearts-import -out <path>          # 指定输出
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"unified-api-v2/internal/codearts"
)

func main() {
	out := flag.String("out", "codearts/accounts.json", "输出路径")
	checkOnly := flag.Bool("check", false, "只检查现有凭据，不导入")
	flag.Parse()

	if *checkOnly {
		check(*out)
		return
	}

	cred, err := codearts.ImportFromIDE()
	if err != nil {
		fmt.Println("导入失败:", err)
		os.Exit(1)
	}

	// 合并与去重由 codearts.SaveCredential 承担（与凭据自愈共用同一份逻辑，
	// 避免「命令能导入、自愈写出重复账号」这类两套实现漂移）。
	if err := codearts.SaveCredential(*out, cred); err != nil {
		fmt.Println("写文件失败:", err)
		os.Exit(1)
	}
	name := codearts.IdentityFromCredential(cred).Name
	if name == "" {
		name = cred.UserName
	}

	fmt.Printf("已导入账号 %s → %s\n", name, *out)
	fmt.Printf("STS 到期: %s（剩 %s）\n", cred.ExpiresAt, remaining(cred.ExpiresAt))
	fmt.Println("提示：网关每次请求都会重读该文件，无需重启。")
}

func check(path string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		fmt.Println("读凭据失败:", err)
		os.Exit(1)
	}
	var doc struct {
		Accounts []codearts.Credential `json:"accounts"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Println("解析失败:", err)
		os.Exit(1)
	}
	if len(doc.Accounts) == 0 {
		fmt.Println("账号文件里没有账号")
		return
	}
	for _, c := range doc.Accounts {
		id := codearts.IdentityFromCredential(c)
		name := id.Name
		if name == "" {
			name = c.UserName
		}
		fmt.Printf("账号 %s | STS 到期 %s（剩 %s）| 可续期材料=%v\n",
			name, c.ExpiresAt, remaining(c.ExpiresAt), c.CanRefresh())
	}
}

func remaining(rfc3339 string) string {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return "未知"
	}
	d := time.Until(t)
	if d <= 0 {
		return "已过期"
	}
	return d.Round(time.Minute).String()
}
