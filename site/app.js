// Index browsing. The package list is not written into the HTML -- a copy would inevitably
// drift from /index.json, and drift means the site advertises packages that will not install.
// So we fetch those same bytes at runtime.
'use strict';

// Strings are not hardcoded here -- the three language pages are generated at build time, and
// the generator hangs this bundle on <body data-i18n>. If it cannot be read we use an empty
// object and `t()` falls back to the key itself: a button showing a key beats a blank button.
const T = (() => {
  try { return JSON.parse(document.body.dataset.i18n || '{}'); } catch { return {}; }
})();
const t = (k, vars) => {
  let s = T[k] || k;
  if (vars) for (const [n, v] of Object.entries(vars)) s = s.split('{' + n + '}').join(v);
  return s;
};

// The site's own address, substituted into the `toy index add` examples.
for (const el of document.querySelectorAll('[data-host]')) el.textContent = location.host;

/* ---------- copy button ---------- */

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

/* ---------- index ---------- */

const out = document.getElementById('packages-out');
const q = document.getElementById('q');
const count = document.getElementById('count');

// The name on the left of `[bin]` is the command that lands on PATH after install -- it often
// does not match the repo name (ClementTsang/bottom -> btm, BurntSushi/ripgrep -> rg), and that
// is exactly the thing worth showing.
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

// source can only be a forge form like `github:owner/repo` or a bare https:// address (that is
// what index validation enforces). Anything else gets no link at all, rather than a guessed one.
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
  if (text != null) n.textContent = text;   // always textContent: desc is copied from upstream and is not trusted
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
    // JS runs but the index will not load: still give a readable way out, not a blank space.
    out.replaceChildren();
    const p = el('p', 'pkg-error', t('err', { msg: err.message }));
    const a = el('a', null, t('err_link'));
    a.href = '/index.json';
    p.append(a, t('err_tail'));
    out.append(p);
  });
