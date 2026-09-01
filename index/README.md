# 索引

一个 JSON 文件，客户端整份下载后**本地搜索**。里面只存指针，不存实质内容 ——
`source` 那个 git 地址永远是真相。

v1 就用文件，而且 **toy 一个索引都不预置** —— 想搜就自己加一条：

```bash
toy index add local ./index/toy-index.json     # 裸路径会自动补成 file:///绝对路径
toy index add local file:///绝对路径/index/toy-index.json   # 写全了也行
```

以后传到任何静态托管（对象存储、GitHub raw、自己的服务器）只要改一行 URL，
**客户端代码一个字节都不用动** —— 因为它从头到尾只认识"一个 URL"。
真要把它变成所有人的默认索引，改的就是 `internal/store/store.go` 里那个
`DefaultIndexURL` 常量，别处一行都不用碰。

条目里的 `source` 只能是托管方形态（`github:owner/repo`）或者 **`https://`** 的裸地址：
索引是远端内容，放行 `file://` 等于让任何一份索引指使客户端去读本机的文件，
放行明文 `http://` 等于把它指向 `169.254.169.254` 这种只在受害者机器上才有意义的地址。
不合规的条目会被静默跳过并计数（`toy search` 会警告有几条被跳过）。

## 条目长什么样

```json
{
  "id":       "Derrity/tinyserve",
  "source":   "github:Derrity/tinyserve",
  "desc":     "一个很小的静态文件服务器",
  "tags":     ["web", "server"],
  "key":      "a3f9…",
  "installs": 0
}
```

`key` 是作者的 ed25519 公钥，跑过 `toy keygen` 认领之后才有。没有它也能被搜到、被装上。

`sig`（`toy publish` 一起生成）是作者对 `id + source + 那一份 toy.toml 原文` 的签名。
**光有 key 不算已签名** —— 这个文件谁都能 fork、谁都能提 PR，抄一把公钥是零成本的。
客户端验过 `sig` 才会打「✓ 已签名」，验不过就降级成「来源 …」并说明原因。

## 加一条

```bash
cd 你的项目 && toy publish     # 输出一条可以直接粘进来的条目
```

## 我们怎么维护这个索引

### 批量生成条目

`cmd/toy-index` 是维护者用的生成器：喂一个 `owner/repo` 列表，产出可以直接进
这个 JSON 的**内联条目**（§5.1）。它是**独立二进制、不是 `toy` 的子命令** ——
`toy` 客户端永远不用托管方的 REST API（规格 §2），而离线批处理可以（理由见
The design notes §14.11）。

```bash
make index-gen ARGS="--repos index/repos.txt"            # 只补新条目
make index-gen ARGS="--repos index/repos.txt --update"   # 已有的也刷新
make index-gen ARGS="--repos index/repos.txt --probe"    # 顺便解开真包定死 [bin]
export GITHUB_TOKEN=...   # 不带 token 只有 60 次/小时，很快就撞限流
```

列表一行一个 `owner/repo`，`#` 是注释（`index/repos.txt` 还不存在，自己建一个 ——
冷启动的来源是扒 awesome-list，那些列表本来就是这个格式，见 the design notes §11）。
跑完**一定要读那份报告**，尤其最后那一栏「这批条目会往 PATH 上放这些命令名」。

两件它**故意不做**的事，别去掉：

- **不写 `[sha256]`。** 内联条目跟着上游最新的 tag 走，钉死某一版的字节会让
  上游一发新版就变成「哈希对不上，拒绝安装」—— 读起来完全像投毒警告，而实际上
  只是作者正常发了个版。首次安装仍有 TOFU 钉住。
- **不动签过名的条目、不动指针条目。** 前者重新生成会让签名失效（把一个「✓」
  悄悄弄没），后者是上游自己有 `toy.toml`，换成内联是降档不是刷新。`--update`
  也不放行这两种。

### 收录别人的软件：三条政策

绝大多数条目指的是**别人的**仓库，作者根本不知道我们收录了他。所以：

1. **不暗示背书。** 收录只说明"这个包装得上"，不说明我们审计过它的代码、
   也不说明作者和我们有任何关系。生成出来的条目一律是内联档，客户端显示的是
   「打包说明来自索引」的**降档**标识，不是「✓ 已签名」—— 那个勾只属于拿自己的
   钥匙签过的作者本人（§14.3）。desc 原样抄上游的，我们不替它写推荐语。
2. **维护者要求下架就下架，不问理由。** 删掉一条只是不再被搜到 ——
   `toy install github:owner/repo` 照样装得上，任何人 fork 这个文件就是一个完整的
   平行索引。既然下架的代价这么低，就没有任何理由跟人争。
3. **收录时人工过一眼 `[bin]` 左边那个命令名。** 这是整条流水线上唯一不能自动化
   的一步：`reservedLinkNames` 挡住了 `git`、PATH 遮蔽检查挡住了这台机器上已经
   存在的命令，但一个 PATH 上**还不存在**的名字是能被占的（§14.1 那条 critical
   就是这个形状）。生成器的报告最后会把这批名字单独列一遍，就是为了让人看这一眼。

### 条目会烂，所以有回归检查

上游改一次资产命名习惯，那条条目就装不上了 —— 而我们这边一行代码都没动。
`scripts/index-check.sh` 把每一条**真的装一遍、跑一遍、卸一遍**，CI 里每天跑一次
（`index-check.yml`），PR 上只测 diff 里动过的那几条。

```bash
make index-check                             # 全量（要联网，会真的下载）
make index-check ARGS="--only owner/repo"    # 只测一条
```

失败分三类，处理完全不同：**条目** = 改这个 JSON 里那一条；**网络** = 重试，
别动索引；**运行** = 装上了但跑不起来（上游那个 release 坏了，或 `[bin]` 右边的
包内路径猜错了）。

## 这份索引里现在有什么（2026-09-01 验的）

**19 条，全部是内联条目。** 候选是 20 个仓库（`index/repos.txt`），跑的是
`toy-index gen --repos index/repos.txt --probe`，然后**每一条都过了两关**：

1. **地址**：按 toy 真正的拼法（host 的 release 模板 + `git ls-remote --tags` 挑出的
   最高 semver tag + 展开 `{version}`/`{tag}`）算出每个平台的下载地址，逐个
   `curl -sSIL` 打过 —— **122 个地址全部 200**。
   拼法必须和客户端一字不差，否则这个检查证明不了什么：**semver 挑版本时要跟着
   客户端一起忽略预发布版**（`semver.Max` 的默认），不然会去 HEAD 一个客户端根本
   不会装的 tag —— tstack/lnav 的最高 semver tag 就是 `v0.14.1-rc1`，而客户端装的
   是 `v0.14.0`。
2. **真的装**：`scripts/index-check.sh` 在 linux/amd64（glibc）上把 19 条**逐条
   装一遍、跑一遍、卸一遍** —— **19 通过 / 0 失败**。

这一轮同时是 §14.13（剥顶层目录）、§3.1（glibc 兜底 musl）和 i686 那三样东西的
真机验收：ripgrep / fd / bat / delta / eza / zoxide 这批 cargo-dist 项目上一轮
还全部收不了，这一轮**在一台 glibc 机器上装的是 musl 构建、剥掉了那层
`ripgrep-15.2.0-x86_64-unknown-linux-musl/` 目录**，`rg --version` 真的跑起来了。

### 收进来的 19 条

| 仓库 | 命令 | 平台数 | 本机装/跑 |
| --- | --- | --- | --- |
| BurntSushi/ripgrep | `rg` | 6 | ✅ |
| ClementTsang/bottom | `btm` | 9 | ✅ |
| Peltoche/lsd | `lsd` | 9 | ✅ |
| ajeetdsouza/zoxide | `zoxide` | 7 | ✅ |
| bootandy/dust | `dust` | 9 | ✅ |
| charmbracelet/glow | `glow` | 6 | ✅ |
| chmln/sd | `sd` | 7 | ✅ |
| dalance/procs | `procs` | 2 | ✅ |
| dandavison/delta | `delta` | 6 | ✅ |
| eza-community/eza | `eza` | 4 | ✅ |
| jesseduffield/lazygit | `lazygit` | 5 | ✅ |
| junegunn/fzf | `fzf` | 6 | ✅ |
| mikefarah/yq | `yq` | 6 | ✅ |
| muesli/duf | `duf` | 7 | ✅ |
| sharkdp/bat | `bat` | 9 | ✅ |
| sharkdp/fd | `fd` | 9 | ✅ |
| sharkdp/hyperfine | `hyperfine` | 8 | ✅ |
| tstack/lnav | `lnav` | 3 | ✅ |
| wagoodman/dive | `dive` | 4 | ✅ |

### 没收的：XAMPPRocky/tokei

最新 release `v14.0.0` **一个资产都没有**（纯源码 release）。GitHub 自动附的源码包
不算资产，而 toy 不从源码构建（铁律 2），所以这个仓库现在收不了 —— 等它哪天恢复发
二进制再跑一次生成器就行，不用改代码。

### `[bin]` 左边那些名字：人过了一眼（§14.9 第 2 条）

`reservedLinkNames` 挡住了 `git`，PATH 遮蔽检查挡住了这台机器上**已经存在**的命令。
剩下要人看的是「名字太泛」和「和别的项目重名」，机器判不了。看下来四类：

**一、和一个【完全不同的程序】同名 —— 最值得记一句的**

- **`delta`**：Debian 里就有一个 `delta` 包，是「heuristic minimizer of interesting
  files」，跟 git-delta 毫无关系。装了那个包的机器上遮蔽检查会拦住我们（对的），
  但两边的用户会以为是同一个东西。
- **`yq`**：另有一个同名的 `yq`（kislyuk 的 Python 实现，jq 的 YAML 包装），命令行
  语法完全不同。索引里这条是 mikefarah 的 Go 实现。

**二、上游自己就因为撞名被发行版改过名**

Debian 的包描述里写得很直白：`fd` 被改成 `fdfind`、`bat` 被改成 `batcat`，
理由都是 “because of a file name clash”。也就是说 **`fd` 和 `bat` 这两个名字在
真实世界里确实会撞**，只是撞的对象未必在每台机器上都装着 —— 遮蔽检查只在装着的
时候才响。

**三、名字太泛（不撞，但值得记着）**

`sd`（两个字母）、`dust`、`procs`、`dive`、`glow` 都是很普通的词。它们现在不和
任何常见命令冲突，但**先到先得**：谁先装谁占住这个名字。`lsd` 离 `ls` 只差一个
字母，敲错会落到另一个程序上。

**四、命令名和仓库名对不上（不是问题，是发现性问题）**

`ClementTsang/bottom → btm`、`BurntSushi/ripgrep → rg`。搜 “bottom” 的人拿到的
命令叫 `btm`。这两条正是 `--probe` 修出来的：**没有 `--probe` 时命令名取自资产名
前缀**（`ripgrep-15.2.0-….tar.gz` → `ripgrep`），而包里那个文件叫 `rg` ——
写出来就是 `[bin] ripgrep = "rg"`，装完能跑，只是 PATH 上那个名字全世界的文档里
都没有（没有人写过 `ripgrep --version`）。现在探到了就以探到的文件基名为准。

### 已知的坑：裸二进制形态的 `[bin]` 只对一个平台成立

**裸二进制形态已经修好了（2026-09-01）。** 之前 install 把没有压缩后缀的资产
**按原资产名**落进包目录，而资产名每个平台都不一样（`yq_linux_amd64` /
`yq_darwin_arm64`），`[bin]` 却只有一行、不按平台变 —— 于是除了 `[bin]` 恰好写对的
那一个平台，其余每个平台都会软链到一个不存在的文件，**而且是在下载和校验都成功
之后才炸**。`mikefarah/yq` 就是这么在 6 个平台里坏了 5 个，而收录时只在
linux/amd64 上验过，所以没看出来。

现在 `insUnpack` 把单文件落成 `[bin]` 右边那个路径（`yq = "yq"`），每个平台都成立。
生成器那边对应的特判也拆掉了。教训写在 the design notes §14.14。


## 删一条会怎样

**只是不再被搜到。** `toy install github:owner/repo` 照样装得上。
任何人 fork 这个文件就是一个完整的平行索引 —— 没有人能真的封杀一个包，包括我们。
