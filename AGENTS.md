# AGENTS.md — mousemacro 项目

## 局域网查询能力（重要）

本项目通过 MCP 连接了本地 shell 服务器（见 `.kilo/kilo.jsonc` 的 `mcp.shell`），它向 AI 暴露以下工具：

- `execute_command(command, argsArray)` — 执行简单命令，**Windows 下可用**。不支持管道、重定向。
- `shell_command(command)` — 需要 bash，**在 Windows 上不可用，禁止使用**。

### 局域网信息脚本

`E:\application\w_cmd\getIp_mcp.bat` 是本机局域网信息脚本，输出本机 IP 与 arp 在线设备表。

当用户提出以下请求时，必须通过 MCP 工具 `execute_command` 运行该脚本，而不是编造数据：

- 查本机 / 局域网 IP
- 查局域网在线设备、在线设备列表、局域网设备
- "查一下局域网在线设备" 等类似表述

推荐调用方式（.bat 经 `cmd /c` 包装，最稳妥）：

```
execute_command(
  command: "cmd",
  argsArray: ["/c", "E:\\application\\w_cmd\\getIp_mcp.bat"]
)
```

也支持直接执行：`execute_command(command: "E:\\application\\w_cmd\\getIp_mcp.bat")`。

### 注意事项

1. 不要使用 `shell_command`（bash 在 Windows 不可用，会失败）。
2. 不要用 PowerShell 思路去猜命令，用上面的固定调用方式。
3. 脚本输出中 arp 的中文表头在 MCP 管道下可能出现乱码（GBK 编码问题，属正常现象），**IP 地址与 MAC 为 ASCII 可正常读取，以 IP/MAC 列为准**。
4. 脚本首行 `Local IP:` 可能为空，是脚本自身已知问题，不影响 arp 在线设备结果，无需向用户强调或自行修复。
