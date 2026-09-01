# 更新日志

格式参照 [Keep a Changelog](https://keepachangelog.com/zh-CN/1.1.0/)，
版本号遵循 [语义化版本](https://semver.org/lang/zh-CN/)。

## [未发布]

## [0.1.0] - 2026-09-01

首个版本。

### 新增

- **十一个子命令**：`install` / `uninstall` / `update` / `list` / `search` /
  `info` / `index` / `keygen` / `publish` / `serve` / `doctor`。
- **四条安装路径**：索引名（`Derrity/tinyserve`）、托管方前缀
  （`github:owner/repo`）、其它托管方（`gitlab:x/y`，加三行配置就多一家）、
  裸 `toy.toml` 地址。
- **版本发现走 `git ls-remote`**，不碰任何托管方的 REST API —— 所以不会被任何
  一家的限流卡住，也不需要为每家写一套适配器。
- **`toy.toml` 只写 release 资产名**，不写 URL、不写版本号、不写托管方。
  打个 tag 就是发新版，这个文件一个字都不用改。
- **两种包形态**：release 二进制（解压后软链）和单文件脚本（下载 + `chmod +x`
  + 软链）。两者都不构建、都不执行。
- **平台候选回退**：`[artifact]` 的 key 可以带第三段（`linux/amd64/musl`），
  客户端按从具体到通用的顺序试。musl 检测看 `/lib/ld-musl-*.so.1`，
  arm 代次读 `/proc/cpuinfo`，Rosetta 是静态规则，全部在纯函数里。
  装到的不是为这台机器构建的那个时会明说。
- **`toy init`**：从 `dist/` 里的资产文件名反推一份 `toy.toml`，把版本号换成
  `{version}` 占位符，并现算 `[sha256]`（把安装侧从 TOFU 直接升到强制校验）。
  猜出来的部分会在结尾明说是猜的。
- **四档信任等级**，安装完成那一行如实打印：作者签名 / 托管方账号 /
  打包说明来自索引 / 未签名。含义见 `SECURITY.md`。
- **TOFU 钥匙环**：artifact 哈希和作者公钥首次安装即钉住，之后变了拒绝安装。
  模型抄的是 SSH 的 `known_hosts`。
- **索引**：一个 JSON 文件，`https://` 和本地路径一样用，可以配多个。
  条目只存指向仓库的指针 —— 从索引里删掉一条只是「不再被搜到」。
- **`toy serve`**：把本机缓存变成一个镜像。内容是哈希寻址的，所以镜像不需要被
  信任；真的走了镜像时安装会明说是从哪拿的。
- **`toy publish` / `toy keygen`**：ed25519 签名，两步都是可选的。
- **安装清单**（`~/.toy/installed/`）：`uninstall` 和 `update` 的依据。

### 安全

首发前跑了一轮对抗审计：八个攻击面并行找出 33 条发现，经三视角对抗证伪后确认
16 条、推翻 17 条。完整复盘见 [`docs/audit-2026-08.md`](docs/audit-2026-08.md)。

**critical —— 铁律 1 被完整绕过：** 一个包可以声明 `[bin] git = "payload"`，
装完 `~/.toy/bin/git` 就在 PATH 上，而 toy 自己靠 `git ls-remote` 发现版本 ——
下一条 toy 命令就执行了这个包里的文件。遮蔽检查挡不住它：在一台还没装 git 的机器上
（macOS 没装 Xcode CLT、slim 容器、CI 基础镜像，而裸地址那条安装路径压根不需要
git），`LookPath` 找不到东西就放行了。修在两半，缺一半都不成立 ——
`store` 的 `reservedLinkNames` 无条件拒绝（**全项目唯一一条 `--force` 也不放行**
的检查），`host` 的 `lookupGit` 自己走 PATH 解析出绝对路径并跳过 `$TOY_HOME/bin`。

其余确认并修掉的：

- 索引改一行 `source`，`toy update` 就把已装包换成另一个仓库的内容 —— 没有确认、
  没有 TOFU 警告、信任等级也不变。作者公钥的 TOFU 钉在从 source 推出来的 id 上，
  所以改 source 等于换身份。
- `toy search --json` 把「索引里有一把 key」直接报成 `signed: true`，一次签名都没验。
- `toy list` 的信任列能被一个包的 `name` 整列顶出屏幕，并伪造成「✓ 已签名」。
- 输出转义漏掉裸字节 `0x9b`（8 位形式的 CSI），能在按 8 位 C1 解释的终端上把上一行
  的信任等级擦掉重写。同一个码位写成合法 UTF-8 时却会被正确转义 —— 结果取决于
  字符串里别的地方有没有控制字符。
- `✓ 已签名` 后面印仓库 owner，是在替一把不知道是谁的钥匙背书。
- `uninstall` 会删掉别的包的命令；两个包会互相删对方的 `pkg` 目录。
- `[artifact]` / `[script]` 的值可以写成 `file:///…`，于是 toy 去读本机任意文件
  并把它装进 PATH、缓存进 `toy serve` 对外提供的目录。
- HTTP 重定向静默把 https 降级成 http（判据要扫整条 via 链）；artifact 下载没有
  体积上限（判据必须是实际写入的字节）；URL 里的凭据漏进 error 对象 —— 含一条
  `net/http` 自己的：`CheckRedirect` 失败时它把原始 `Location` 塞回
  `url.Error.URL` 而不走抹除。
- 解压：符号链接的三段式逃逸（`x -> "."`，`x/up -> ".."`），因为词法校验按词法父
  目录解析、内核按真实父目录解析；GNU 稀疏文件被当成非常规成员静默丢弃，装完是
  一条不存在的命令；不认识的 tar 成员类型声明了内容时，`continue` 掉等于让 reader
  把那些字节从 gzip 流里解出来丢掉，字节上限数不到。
- 指纹从 8 个 hex 提到至少 128 位：实测约 3.6 万次 keygen/秒/核，32 位外推约
  33 core-hour —— 要挡的不是碰撞是**原像**。
- 私钥权限的判据改成「group/other 一位都不能有」而不是「恰好 0600」——
  原来那句提示是在教用户把只读的私钥改成可写。
- 上游删掉最新 tag 导致的**静默降级**现在会说一句。
- 失败的重装会抹掉正在工作的那一版（清单还在、`list` 照样列，只是软链悬空）。

### 变更

- **`--force` 拆成两件事**（三个独立视角各自撞到）：`--reinstall` 管日常重装，
  不碰任何安全检查；`--force` 只管接受安全检查报出的变化，**并逐条打印放行了什么**。
- **TOFU 改成按资产名钉**，不按 `ref@version` 钉：同一个版本在 musl 和 glibc 机器上
  是两份不同的字节，只按版本钉的话共享 `$HOME` 会被误报成投毒。
- **v1 不预置任何索引**（`DefaultIndexURL` 是空的）。公共索引还没上线，塞一个拉不到
  的地址等于让每个新用户第一次 `doctor` 就吃一条红叉。「没有索引」是合法状态：
  `doctor` 报中性说明而不是 ✗，`install github:owner/repo` 照常能装。
- 索引查找改成大小写不敏感；`pkg` 目录的每一段加去歧义后缀（APFS 上 `Foo` 和 `foo`
  是同一个目录，不加后缀会让两个不同的包共用一个安装目录，卸一个会删掉另一个的
  可执行文件）。

### 移除

- 删掉 `internal/store` 里 613 行手写 TOML 解析（依赖补回来之后不需要了）。

[未发布]: https://github.com/Cirraya-LLC/toy/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Cirraya-LLC/toy/releases/tag/v0.1.0
