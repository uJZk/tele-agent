# 命令行、启动流程与安装

## 命令形式

```bash
tele [tele 选项] <别名>[:<目录>] [claude 参数...]

tele dev                  # 在远端家目录启动 Claude
tele dev:proj             # ~/proj（相对路径按远端家目录解析）
tele dev:/srv/app         # 绝对路径
tele dev:proj --resume    # 别名之后的参数原样传给 claude
tele dev -p "…"           # 同上
```

- **`<别名>[:<目录>]`** 沿用 scp、rsync 的 `host:path` 写法。省略目录时是**远端用户的家目录**；相对路径按远端家目录解析；末尾的 `/` 会被去掉。目录不存在时直接报错退出，不自动创建。这个目录就是 Claude 的工作目录，也是 Claude 识别项目的依据。
- **参数边界**：tele 自己的选项必须写在别名**之前**，别名之后的所有内容都原样传给 Claude Code。这与 `ssh [选项] host [命令]`、`docker run [选项] 镜像 [参数]` 的惯例一致，不需要 `--`，也不会和 claude 的参数冲突。
- **切分规则**：按**第一个** `:` 切分，所以别名中不能含 `:`，目录中可以含。
- **一个 tele 进程只对应一台主机**，运行期间不切换。要换主机，就退出后用另一个别名重新启动。

**其它命令**：

| 命令 | 作用 |
|---|---|
| `tele host add <别名> …` / `tele host confirm` / `tele host ls` / `tele host rm` | 登记主机、配对，查看状态（连通性、RTT、时钟偏差、操作系统信息） |
| `tele doctor [别名]` | 本地检查，指定别名时再检查到该主机的连通性（见[预检与修复策略](#预检与修复策略)） |
| `tele server install` / `run` / `uninstall` | 远端服务 |

所有子命令名都是保留字，不能用作主机别名。

## 启动流程

1. 解析 `<别名>[:<目录>]`，读取别名配置，建立会话（SS2022 + 会话层）。连接不上时报错退出，并提示运行 `tele doctor <别名>`。
2. 从服务端获取目标主机的信息：主机名、操作系统和发行版、内核、架构、远端用户、`$HOME`、登录 shell 及其 PATH；解析工作目录，并确认它存在。
3. 按[命名空间的构建](filesystem.md#命名空间的构建)构建命名空间：挂载 telefs（远端的 `/`），准备远端视图（本地集合、`HOME`）。
4. 生成系统提示词文件、shim 目录和 teleswitch 库，启动 CONNECT 代理，准备好[注入的环境](claude-code.md#注入的环境)。
5. 在本地视图中 exec `claude`，别名之后的参数原样透传；teleswitch 在 `main` 之前切换到远端视图，并 `chdir` 到工作目录。
6. Claude 退出后清理会话：远端进程按租约规则处理（见[断线语义](transport.md#断线语义)），卸载 FUSE。

**已知限制：会话历史不按主机隔离**。Claude 以 cwd 路径作为 `~/.claude/projects/` 下的项目键，不同主机上的相同路径会共用会话历史，`--resume` 时会一起列出。tele 不改变这一行为。

## 安装与配对

```bash
# 本地：生成配对串（包含 SS2022 PSK、端口、一次性 token），同时打印远端的安装步骤
tele host add myhost --endpoint 203.0.113.5:8443
# → 输出：tele server install --pair 'tele1:…'

# 远端（普通用户即可；systemd --user + loginctl enable-linger，或由管理员安装为系统服务）
# 从项目的 GitHub Releases 下载与远端架构对应的 tele 二进制，放到 ~/.local/bin/tele，并加上可执行权限
tele server install --pair 'tele1:…'
# → 检查 NTP、inotify 上限，放通端口；输出回执串 'tele1r:…'

# 本地
tele host confirm myhost 'tele1r:…'
```

本地能通过 SSH 登录远端时，可以用 `tele host add --ssh user@host` 一步完成：通过 SSH 上传 `tele` 二进制并执行 `tele server install --pair …`，远端不需要预先安装 tele。

远端服务默认以 `systemd --user` 运行。

## 配置与状态文件

| 路径 | 内容 |
|---|---|
| `~/.config/tele/`（本地） | 主机别名、PSK（0600） |
| `~/.config/tele/`（远端） | 服务端配置、PSK（0600）、安装清单 `install-manifest.json` |
| `~/.cache/tele/s/<sid>/`（远端） | 会话目录：scratch 文件、溢出到磁盘的命令输出 |

## 预检与修复策略

**原则**：先检查全部项目，再按类别处理，而不是一律报错或一律自动修改：

- 只影响当前用户、可以撤销的操作 → **自动执行**；
- 涉及系统范围或需要提权的操作 → **展示将要执行的确切命令，征得同意后执行**；
- 无法修复的问题 → **报错**，并给出原因和指引。

一律报错时，用户要逐条手动修复，安装体验差；一律自动修改，会在用户不知情时改动防火墙、sysctl、linger 这类系统设置，也会在非交互场景（CI、由 Claude 调用）下卡在提权提示上。

**流程**：

1. `tele server install`（以及本地的 `tele doctor`）先做**只读预检**，输出一张清单：✅ 通过 / 🔧 可自动修复 / 🔐 需同意或提权 / ❌ 无法修复 / ⚠️ 警告。
2. 🔧 项自动执行。
3. 🔐 项逐条展示**确切命令**，由用户确认（`[y/N]`）。
4. 最后重新预检，确认结果。
5. 存在 ❌ 项，或必需的 🔐 项没有得到同意时，以非零状态退出，并打印需要手动执行的命令。

**远端检查项**：

| 检查项 | 类别 | 处理 |
|---|---|---|
| 二进制与配置目录（`~/.local/bin`、`~/.config/tele`），PSK 文件权限 0600 | 🔧 | 直接创建和设置 |
| `systemd --user` 单元的安装、启用、启动 | 🔧 | 直接执行；没有 user manager 时降级为 ⚠️，改为提示前台运行或安装为系统服务 |
| linger（`loginctl enable-linger $USER`） | 🔐 | 没有 linger 时，用户登出后服务会停止。polkit 的 `set-self-linger` 在活跃会话中通常允许，在 SSH 等非活跃会话中可能需要认证。拒绝时降级为 ⚠️，并说明后果 |
| 监听端口可达（本机防火墙：firewalld、ufw、nft） | 🔐 | 生成对应前端的放行命令，经同意后用 sudo 执行；云安全组无法检测，只给出提示 |
| `fs.inotify.max_user_watches` 不足（按项目文件数估算） | 🔐 | 写入 `/etc/sysctl.d/90-tele.conf` 并执行 `sysctl --system`；拒绝时降级为 ⚠️：变更推送会退回短 TTL |
| 时钟同步（SS2022 要求误差在 30 秒以内） | 误差已超限为 ❌；NTP 未启用为 🔐 | 启用 `timedatectl set-ntp true` 需要同意；误差已经超限时直接报错，因为连接会被拒绝 |
| `bash`、`rg`、`git` 是否可用 | ⚠️ | 缺少时对应的 Claude 功能会失败（见 [shim](exec.md#shim)） |
| 内核版本、`/proc/sys/fs/inotify` 可用 | ❌ | 报错，并说明最低要求 |

**本地检查项**（`tele doctor`；首次运行 `tele <别名>` 时自动执行）：

| 检查项 | 类别 | 处理 |
|---|---|---|
| `/dev/fuse` 存在且可读写 | 权限不足为 🔐；不存在为 ❌ | 发行版默认是 0666；异常时给出 `modprobe fuse` 或 udev 规则的建议 |
| 非特权 userns 可用（`user.max_user_namespaces`、Ubuntu 的 AppArmor 限制） | 🔐 | 安装随包附带的 AppArmor profile（需要 sudo），而不是全局关闭限制；拒绝时报错 |
| Claude Code 版本在已验证列表中 | ⚠️ | 未验证的版本给出警告，但仍然允许运行 |
| 与目标主机的时钟偏差 | ⚠️ / ❌ | 同上表 |

**通用约定**：

- **非交互**时（没有 TTY，例如在 CI 中，或由 Claude 在 Bash 里调用）**绝不提权**：🔧 项照常执行，🔐 项全部视为未同意，打印需要手动执行的命令。
- `--yes` 表示同意所有 🔐 项，用于自动化部署；`--check` 只做预检；`--print-commands` 只打印命令，不执行。
- 所有改动记入安装清单，`tele server uninstall` 据此逐项回滚。
- 每一步都是幂等的，可以重复运行。
