package main

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"strings"
)

// checkSecretPerms 检查凭据类文件的权限，过宽则告警（不改动，避免误伤）。
//
// Windows 上 os.FileMode 的权限位不表达 ACL（Go 只映射只读位），
// 因此那里只做存在性提示，不拿 0666 当「过宽」——否则每次启动都刷一条假告警。
func checkSecretPerms(paths []string) {
	windows := runtime.GOOS == "windows"
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			continue // 未启用/未创建的文件不算问题
		}
		if windows {
			continue
		}
		if fi.Mode().Perm()&0o077 != 0 {
			log.Printf("WARN: 凭据文件权限过宽 %s (%o)，同机其他用户可读，建议 chmod 600", p, fi.Mode().Perm())
		}
	}
}

// codeArtsLoginPort 从 listen 地址里取端口（授权页的 port 参数必须与回调一致）。
func codeArtsLoginPort(listen string) int {
	i := strings.LastIndex(listen, ":")
	if i < 0 {
		return 0
	}
	var p int
	if _, err := fmt.Sscanf(listen[i+1:], "%d", &p); err != nil {
		return 0
	}
	return p
}
