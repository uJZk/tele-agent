# Roadmap

尚未完成、但已经决定要做的工作。完成一项就删除一项；设计与约定写进 `docs/`，不留在这里。

- **兼容性测试没有覆盖的 Claude 功能**：Claude 启动时在 `PATH` 中查找 `code`、`nano`、`vi` 等编辑器，可能用于外部编辑器（Ctrl+G）；按名字启动的编辑器在远端视图的 `PATH` 中没有 shim，功能会失败；IDE 探测（`/bin/sh -c "ps aux | grep …"`）经 `sh` shim 在远端执行，看到的是远端的进程。要用 `strace` 确认它们的调用方式，决定交给本地 exec 代理还是远端，并补上兼容性测试（见[视图切换](docs/claude-code.md#视图切换)、[验证方法](docs/claude-code.md#验证方法)）。
