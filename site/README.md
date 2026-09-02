# 网站

一个纯静态站，**没有 npm、没有框架、没有字体 CDN、没有第三方请求**。
构建步骤只有一个 python3 heredoc（在 `scripts/build-site.sh` 里）。

它干两件事，**第一件是功能不是装饰**：

1. **挂索引** —— `/index.json` 是客户端真正会去 `fetch` 的地址
2. **落地页 + 包浏览** —— discovery 是这个产品的差异化，那一块运行时读同一份 `/index.json`

三种语言：`zh-Hans`（站根）/ `zh-Hant`（`/zh-Hant/`）/ `en`（`/en/`）。

---

## 部署到 Cloudflare Pages

在 Pages 里连上这个仓库，然后：

| 设置项 | 填什么 |
| --- | --- |
| 构建命令 | `bash scripts/build-site.sh` |
| 构建输出目录 | `site` |
| 根目录 | 留空（仓库根） |

构建脚本做两件事：

1. 把 `index/toy-index.json` **复制**成 `site/index.json`，
   并校验它是合法 JSON、`version` 是 1、条数大于 0。
   **必须是复制，不能手抄一份** —— 两份必然漂移，而漂移意味着**网页上写着有的包装不上**。
   脚本复制完会 `cmp` 逐字节比对，对不上就整个构建失败：宁可不发布，也不要发布一份
   客户端拉下来会炸的索引。
2. 用 `index.template.html` + `i18n.json` 生成三份页面（下一节）。

**生成物全在 `.gitignore` 里**：`site/index.json`、`site/index.html`、
`site/zh-Hant/`、`site/en/`。仓库里手维护的只有 `index.template.html`、
`i18n.json`、`style.css`、`app.js`、`404.html`、`_headers`。

---

## 三语怎么做的

```
site/index.template.html   一份模板，文案位置是 {{key}}
site/i18n.json             一份内容，一个 key 三种语言
        ↓  scripts/build-site.sh 里的 python3
site/index.html            zh-Hans（默认，站根）
site/zh-Hant/index.html
site/en/index.html
```

**不手维护三份 HTML** —— 必然漂移，而这个站上有三条不许破坏的话（见最后一节），
漂移最先漂掉的就是它们。**也不做纯客户端切换** —— 禁 JS 就只剩一种语言，
搜索引擎也只收得到一份，而这个站的无 JS 降级是验过的、要留着。

几条规矩：

- **key 以 `_html` 结尾的值原样插入，其余一律 `html.escape`。** 那些值里的
  `<code>` / `<strong>` / `<a>` 是我们自己写的可信内容；但"所有值都当 HTML 塞进去"
  是另一回事 —— 后缀让每一个 raw 插入点在模板里就看得见。
- **三语缺一条就整个构建失败**，不生成一份漏字的页面。
- **语言切换器是三个普通链接**（`.langnav`），不是 JS 下拉 —— 禁 JS 也能切。
- **三份都有完整的 `hreflang`**（含 `x-default` 指向简体那份）和自指那一条。
- 子目录页面引用 `../style.css` / `../app.js`；**索引一律用绝对路径 `/index.json`**
  （客户端 fetch 的也是它）。
- **`app.js` 里不硬编码任何一种语言的文案。** 它需要的那几条（复制按钮、
  「⚠ 打包说明来自索引」那个标记、计数、错误提示）由生成器写进
  `<body data-i18n="…">`，`app.js` 开头 `JSON.parse` 一次。
  拿不到就回落成 key 本身 —— 一个显示 key 的按钮，好过一个空白按钮。

**翻译不是直译。** 英文那份面向 HN / Lobsters 的读者，句子重组过、术语用英文圈的
习惯说法（supply chain、install hooks、content-addressed、trust on first use）；
繁体用台港技术圈的词（軟體 / 檔案 / 執行 / 相依 / 雜湊 / 二進位檔 / 指令碼），
**不许出现「文件」当 file 用**。

**信任表左栏是客户端真实打印的字符串，只有简体一种** —— 所以三份页面里那四行
`<code>` 是一模一样的原文，繁体和英文那两份在表格下面明说了这件事。
把它"翻译"成繁体或英文就是在网页上伪造客户端输出。

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
# http://127.0.0.1:8099/  /zh-Hant/  /en/
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
