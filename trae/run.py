"""Trae provider 的无窗口启动器。

为什么需要它：原先用 `cmd /c "pythonw.exe -m uvicorn ... >> log"` 启动，
cmd 会等命令结束，而 uvicorn 是常驻进程——于是 cmd 一直挂着，控制台窗口
也就一直在（实测 PID 49472 就是这层 cmd）。

pythonw.exe 本身没有控制台，所以这里由 Python 自己把 stdout/stderr 接到
日志文件，再启动 uvicorn，整条链路没有任何控制台窗口。
"""

import os
import sys

# UPSTREAM_MODE=ide 是硬性要求：本机 Trae CN 账号在 remote 通道下无模型权限。
os.environ.setdefault("UPSTREAM_MODE", "ide")

# 切到脚本所在目录，保证 data/ 与 src/ 的相对路径与 run.cmd 一致。
BASE = os.path.dirname(os.path.abspath(__file__))
os.chdir(BASE)
sys.path.insert(0, BASE)

os.makedirs("data", exist_ok=True)
log = open(os.path.join("data", "server.err.log"), "a", encoding="utf-8", buffering=1)
sys.stdout = log
sys.stderr = log

import uvicorn  # noqa: E402  （必须放在重定向之后，确保导入期的日志也进文件）

if __name__ == "__main__":
    uvicorn.run(
        "src.main:app",
        host="127.0.0.1",
        port=8000,
        log_level="warning",
        access_log=False,
    )
