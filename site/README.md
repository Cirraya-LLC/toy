# 网站

一个纯静态站，**没有构建工具链、没有 npm、没有框架**。三个文件加一份索引。

它干两件事，**第一件是功能不是装饰**：

1. **挂索引** —— `/index.json` 是客户端真正会去 `fetch` 的地址
2. **落地页 + 包浏览** —— discovery 是这个产品的差异化，那一块运行时读同一份 `/index.json`

---

## 部署到 Cloudflare Pages

在 Pages 里连上这个仓库，然后：

| 设置项 | 填什么 |
| --- | --- |
| 构建命令 | `bash scripts/build-site.sh` |
| 构建输出目录 | `site` |
| 根目录 | 留空（仓库根） |

构建脚本只做一件事：把 `index/toy-index.json` **复制**成 `site/index.json`，
并校验它是合法 JSON、`version` 是 1、条数大于 0。

**必须是复制，不能手抄一份** —— 两份必然漂移，而漂移意味着**网页上写着有的包装不上**。
脚本复制完会 `cmp` 逐字节比对，对不上就整个构建失败：宁可不发布，也不要发布一份
客户端拉下来会炸的索引。

`site/index.json` 在 `.gitignore` 里（它是生成物）。

## 部署完之后要改的**那一个常量**

`internal/store/store.go`：

```go
const DefaultIndexURL = ""    // ← 改成 "https://<你的域名>/index.json"
```

改之前，新用户得自己跑一次 `toy index add`；改之后开箱就能 `toy search`。

**为什么现在是空的**：预置一个还没上线的地址，会让每一个新用户第一次跑
`toy doctor` 就看到一条红叉，而那不是他的问题。「没配索引」是合法状态 ——
`doctor` 报中性说明不报 ✗，`toy install github:owner/repo` 那条路本来就不经过索引。

## 索引更新之后怎么办

```bash
make index-gen ARGS="--repos repos.txt --probe --out index/toy-index.json"
make index-check          # 逐条真装、真跑、真卸 —— 过不了的不许上线
git commit && git push    # Pages 自动重新构建
```

`_headers` 给 `/index.json` 配了 `Cache-Control: max-age=300`，
所以索引更新之后最多五分钟生效，不用等一天。

## 本地预览

```bash
make site && cd site && python3 -m http.server 8099
```

然后可以拿它当真索引用一遍，这是这个站唯一值得验的东西：

```bash
export TOY_HOME=$(mktemp -d)
toy index add local http://127.0.0.1:8099/index.json
toy search disk && toy install muesli/duf && duf --version
```

## 改文案的规矩

**所有文案都要能在 the design notes / `SECURITY.md` / `README.md` 里找到依据。**
尤其这三条，破了任何一条都比不做这个站更糟：

- **不许出现 `curl | bash` 安装脚本。** 那正是这个项目要反的东西。
- **每个包必须标出它是内联条目**（the design notes §5.1）。站上省掉这个标注，
  就等于我们在网页上暗示背书，而客户端却在如实降档 —— 那是我们自己在撒谎，
  而「不撒谎」是这个产品最贵的资产。
- **`✓ 已签名` 必须写清楚它证明不了什么。** 签名消息是
  `id + source + toy.toml 原文`，里面没有一个字节把公钥和仓库 owner 绑起来。
