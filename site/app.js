// Copy buttons everywhere, and filtering and sorting on the package page. The package
// list itself is written into the HTML at build time, from the same index.json the same
// deploy publishes (scripts/build-site.sh), so everything here only rearranges what is
// already on the page: with JavaScript off the whole list is still there.
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

/* ---------- copy buttons ---------- */

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

// A command block with its button inside it...
for (const btn of document.querySelectorAll('pre.cmd > .copy')) {
  const code = btn.parentElement.querySelector('code');
  wireCopy(btn, () => code.textContent);
}
// ...and a package row's button, which carries its command.
for (const btn of document.querySelectorAll('button.copy[data-copy]')) {
  wireCopy(btn, () => btn.dataset.copy);
}

/* ---------- the package page ---------- */

const list = document.getElementById('pkglist');
const tools = document.getElementById('pkg-tools');
if (list && tools) {
  const q = document.getElementById('q');
  const os = document.getElementById('os');
  const sort = document.getElementById('sort');
  const count = document.getElementById('count');
  const rows = [...list.children];
  const none = document.createElement('p');
  none.className = 'fine';
  none.textContent = t('none');
  none.hidden = true;
  list.after(none);

  const apply = () => {
    const needle = q.value.trim().toLowerCase();
    const want = os.value;
    const ordered = rows.slice().sort(sort.value === 'name'
      ? (a, b) => a.dataset.name.localeCompare(b.dataset.name)
      : (a, b) => Number(b.dataset.stars) - Number(a.dataset.stars));
    let shown = 0;
    for (const r of ordered) {
      // "?" is a package whose platforms only its own toy.toml knows: it stays in every
      // platform's list rather than vanishing from all of them.
      const oses = r.dataset.os.split(' ');
      const ok = (!needle || r.dataset.hay.includes(needle)) &&
                 (!want || oses.includes(want) || oses.includes('?'));
      r.hidden = !ok;
      if (ok) shown++;
    }
    list.append(...ordered);
    none.hidden = shown > 0;
    count.textContent = shown === rows.length
      ? t('count_all', { n: rows.length })
      : t('count_some', { n: shown, all: rows.length });
  };
  for (const el of [q, os, sort]) el.addEventListener('input', apply);
  tools.hidden = false;
  apply();
}
