' run-hidden.vbs — 静默启动一个 .cmd，不弹控制台窗口。
'
' 为什么需要：计划任务用 cmd /c 启动包装脚本时会带出一个控制台窗口
' （还会因为没有 chcp 而显示旧版中文注释的乱码）。用 wscript 以
' 窗口样式 0（hidden）拉起，整条进程链都不出现窗口。
'
' 用法：wscript.exe //nologo run-hidden.vbs "D:\...\run-unified-v2.cmd"

Option Explicit

Dim sh, cmd
If WScript.Arguments.Count = 0 Then
    WScript.Quit 1
End If
cmd = WScript.Arguments(0)

Set sh = CreateObject("WScript.Shell")
' 0 = 隐藏窗口，False = 不等待返回（脚本自己会一直跑）
sh.Run """" & cmd & """", 0, False
