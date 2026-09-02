// 索引浏览。包列表不写进 HTML —— 抄一份必然和 /index.json 漂移，
// 而漂移意味着网页上写着有的包装不上。所以运行时 fetch 那同一份字节。
'use strict';

// 文案不在这里硬编码 —— 三语页面是构建时生成的，它把这一份挂在 <body data-i18n> 上。
// 拿不到就用空对象，`t()` 会回落成 key 本身：一个显示 key 的按钮，好过一个空白按钮。
const T = (() => {
  try { return JSON.parse(document.body.dataset.i18n || '{}'); } catch { return {}; }
})();
const t = (k, vars) => {
  let s = T[k] || k;
  if (vars) for (const [n, v] of Object.entries(vars)) s = s.split('{' + n + '}').join(v);
  return s;
};

// 站点自己的地址，填进那几条 `toy index add` 的示例里。
for (const el of document.querySelectorAll('[data-host]')) el.textContent = location.host;

/* ---------- 复制按钮 ---------- */

function wireCopy(btn, text) {
  btn.addEventListener('click', async () => {
    try {
      await navigator.clipboard.writeText(text());
      btn.textContent = t('copied');
      btn.dataset.done = '1';
    } catch {
      btn.textContent = t('copy_failed');
    }
    setTimeout(() => { btn.textContent = t('copy'); delete btn.dataset.done; }, 1400);
  });
}

for (const btn of document.querySelectorAll('pre.cmd > .copy')) {
  const code = btn.parentElement.querySelector('code');
  wireCopy(btn, () => code.textContent);
}

/* ---------- 索引 ---------- */

const out = document.getElementById('packages-out');
const q = document.getElementById('q');
const count = document.getElementById('count');

// `[bin]` 左边那个名字才是装完之后 PATH 上的命令 —— 和仓库名经常对不上
// （ClementTsang/bottom → btm，BurntSushi/ripgrep → rg），而那正是要显示的东西。
function binName(toyfile) {
  if (!toyfile) return '';
  const lines = toyfile.split('\n');
  const i = lines.findIndex((l) => l.trim() === '[bin]');
  if (i < 0) return '';
  for (const line of lines.slice(i + 1)) {
    const s = line.trim();
    if (!s || s.startsWith('#')) continue;
    if (s.startsWith('[')) break;
    const m = s.match(/^["']?([A-Za-z0-9._-]+)["']?\s*=/);
    return m ? m[1] : '';
  }
  return '';
}

// source 只可能是 `github:owner/repo` 这种托管方形态或者 https:// 裸地址
// （索引校验就是这么定的）。别的一律不给链接，而不是猜一个出来。
function upstream(source) {
  const host = { github: 'github.com', gitlab: 'gitlab.com', codeberg: 'codeberg.org' };
  const m = /^([a-z]+):([^/\s]+\/[^/\s]+)$/.exec(source || '');
  if (m && host[m[1]]) return 'https://' + host[m[1]] + '/' + m[2];
  if (/^https:\/\//.test(source || '')) return source;
  return '';
}

function el(tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;   // 一律 textContent：desc 是上游抄来的，不可信
  return n;
}

function card(p) {
  const li = el('li', 'pkg');
  const cmd = binName(p.toyfile);

  const head = el('div', 'pkg-head');
  head.append(el('span', 'pkg-cmd', cmd || p.id));

  const url = upstream(p.source);
  if (url) {
    const a = el('a', 'pkg-id', p.id);
    a.href = url;
    a.rel = 'noopener';
    head.append(a);
  } else {
    head.append(el('span', 'pkg-id', p.id));
  }

  if (p.toyfile) {
    const tag = el('span', 'tag-inline', t('inline_tag'));
    tag.title = t('inline_title');
    head.append(tag);
  }
  if (typeof p.stars === 'number') head.append(el('span', 'pkg-stars', '★ ' + p.stars.toLocaleString('en-US')));

  const foot = el('div', 'pkg-foot');
  const pre = el('pre');
  const line = 'toy install ' + p.id;
  pre.append(el('code', null, line));
  const btn = el('button', 'copy', t('copy'));
  btn.type = 'button';
  btn.setAttribute('aria-label', t('copy_aria') + line);
  wireCopy(btn, () => line);
  foot.append(pre, btn);

  li.append(head, el('p', 'pkg-desc', p.desc || ''), foot);
  return li;
}

function render(pkgs, all) {
  out.replaceChildren();
  if (!pkgs.length) {
    out.append(el('p', 'fine', t('none')));
  } else {
    const ul = el('ul', 'pkgs');
    for (const p of pkgs) ul.append(card(p));
    out.append(ul);
  }
  count.textContent = pkgs.length === all
    ? t('count_all', { n: all })
    : t('count_some', { n: pkgs.length, all });
}

fetch('/index.json', { cache: 'no-cache' })
  .then((r) => { if (!r.ok) throw new Error('HTTP ' + r.status); return r.json(); })
  .then((data) => {
    const all = Array.isArray(data.packages) ? data.packages.slice() : [];
    all.sort((a, b) => (b.stars || 0) - (a.stars || 0));
    const hay = new Map(all.map((p) => [p, (p.id + ' ' + (p.desc || '') + ' ' + binName(p.toyfile)).toLowerCase()]));

    const apply = () => {
      const needle = q.value.trim().toLowerCase();
      render(needle ? all.filter((p) => hay.get(p).includes(needle)) : all, all.length);
    };
    q.addEventListener('input', apply);
    apply();
  })
  .catch((err) => {
    // JS 能跑但索引拉不到，也要给一条读得懂的出路，不要留一块空白。
    out.replaceChildren();
    const p = el('p', 'pkg-error', t('err', { msg: err.message }));
    const a = el('a', null, t('err_link'));
    a.href = '/index.json';
    p.append(a, t('err_tail'));
    out.append(p);
  });
