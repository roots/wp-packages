(function () {
  'use strict';

  function showCopiedSwap(el) {
    const svg = el.querySelector('button svg') || el.querySelector('svg:last-of-type') || el.querySelector('svg');
    if (!svg || svg.dataset.copied) return;
    svg.dataset.copied = '1';
    const orig = svg.innerHTML;
    const oc = svg.style.color;
    svg.style.transition = 'opacity 150ms,color 200ms';
    svg.style.opacity = '0';
    setTimeout(() => {
      svg.innerHTML = '<path stroke-linecap="round" stroke-linejoin="round" d="m4.5 12.75 6 6 9-13.5"/>';
      svg.style.color = '#16a34a';
      svg.style.opacity = '1';
    }, 150);
    setTimeout(() => {
      svg.style.opacity = '0';
      setTimeout(() => {
        svg.innerHTML = orig;
        svg.style.color = oc;
        svg.style.opacity = '1';
        delete svg.dataset.copied;
      }, 150);
    }, 1500);
  }

  function copyCmd(el, txt) {
    navigator.clipboard.writeText(txt);
    showCopiedSwap(el);
  }

  function copyMarkdown(el, url) {
    fetch(url, { headers: { Accept: 'text/markdown' } })
      .then((r) => (r.ok ? r.text() : Promise.reject(r.status)))
      .then((txt) => {
        navigator.clipboard.writeText(txt);
        showCopiedSwap(el);
      })
      .catch(() => {});
  }

  function copyJson(el, url) {
    fetch(url, { headers: { Accept: 'application/json' } })
      .then((r) => (r.ok ? r.text() : Promise.reject(r.status)))
      .then((txt) => {
        let pretty = txt;
        try { pretty = JSON.stringify(JSON.parse(txt), null, 2); } catch (e) {}
        navigator.clipboard.writeText(pretty);
        showCopiedSwap(el);
      })
      .catch(() => {});
  }

  function openTagRequest(pluginName) {
    const d = document.getElementById('tag-request-dialog');
    if (!d) return;
    const msg = document.getElementById('tag-request-message');
    const link = document.getElementById('tag-request-link');
    msg.textContent = msg.dataset.template;
    link.href = 'https://wordpress.org/support/plugin/' + pluginName + '/';
    d.showModal();
  }

  function copyInstall(btn, slug) {
    navigator.clipboard.writeText('composer require wp-plugin/' + slug + ':dev-trunk').then(() => {
      const t = btn.textContent;
      btn.textContent = 'Copied!';
      setTimeout(() => { btn.textContent = t; }, 1500);
    });
  }

  function toggleSort(desc, asc) {
    const el = document.getElementById('sort-input');
    if (!el) return;
    el.value = el.value === desc ? asc : desc;
    if (window.htmx) htmx.trigger(document.getElementById('untagged-form'), 'submit');
  }

  function activateTab(btn) {
    const container = btn.closest('[data-tabs]');
    if (!container) return;
    const btns = container.querySelectorAll('[data-tab]');
    btns.forEach((b) => {
      const active = b === btn;
      b.classList.toggle('border-brand-primary', active);
      b.classList.toggle('text-brand-primary', active);
      b.classList.toggle('border-transparent', !active);
      b.classList.toggle('text-gray-500', !active);
      b.setAttribute('aria-selected', active ? 'true' : 'false');
      const panelId = b.getAttribute('aria-controls');
      if (panelId) {
        const p = document.getElementById(panelId);
        if (p) p.classList.toggle('hidden', !active);
      }
    });
    const urlBase = container.dataset.tabsUrl;
    if (urlBase) {
      const def = container.dataset.tabsDefault || '';
      const name = btn.dataset.tab;
      history.replaceState(null, '', name === def ? urlBase : urlBase + '?tab=' + name);
    }
  }

  function toggleNext(el) {
    const r = el.nextElementSibling;
    if (!r) return;
    r.classList.toggle('hidden');
    el.setAttribute('aria-expanded', !r.classList.contains('hidden'));
  }

  document.addEventListener('click', (e) => {
    let t;
    if ((t = e.target.closest('[data-copy]'))) { copyCmd(t, t.dataset.copy); return; }
    if ((t = e.target.closest('[data-copy-md]'))) { copyMarkdown(t, t.dataset.copyMd); return; }
    if ((t = e.target.closest('[data-copy-json]'))) { copyJson(t, t.dataset.copyJson); return; }
    if ((t = e.target.closest('[data-tag-request]'))) { openTagRequest(t.dataset.tagRequest); return; }
    if ((t = e.target.closest('[data-copy-install]'))) { copyInstall(t, t.dataset.copyInstall); return; }
    if ((t = e.target.closest('[data-sort]'))) { toggleSort(t.dataset.sort, t.dataset.sortDefault); return; }
    if ((t = e.target.closest('[data-tab]'))) { activateTab(t); return; }
    if ((t = e.target.closest('[data-toggle-next]'))) { toggleNext(t); return; }
    if ((t = e.target.closest('[data-dialog-close]'))) {
      const d = document.getElementById(t.dataset.dialogClose);
      if (d) d.close();
      return;
    }
    if ((t = e.target.closest('[data-copy-from]'))) {
      const src = document.getElementById(t.dataset.copyFrom);
      if (src) {
        navigator.clipboard.writeText(src.textContent);
        const orig = t.textContent;
        t.textContent = 'Copied!';
        setTimeout(() => { t.textContent = orig; }, 1500);
      }
      return;
    }
    if ((t = e.target.closest('[data-author-clear]'))) {
      const input = document.getElementById('author-input');
      if (input) {
        input.value = '';
        t.classList.add('hidden');
        if (window.htmx) htmx.trigger(document.getElementById('untagged-form'), 'submit');
      }
      return;
    }
    if ((t = e.target.closest('[data-versions-toggle]'))) {
      const rows = document.querySelectorAll('.ver-row');
      const showing = t.dataset.expanded === '1';
      for (let i = 5; i < rows.length; i++) {
        rows[i].classList.toggle('hidden', showing);
      }
      t.dataset.expanded = showing ? '0' : '1';
      const count = t.dataset.versionsToggle;
      t.textContent = showing ? 'Show all ' + count + ' versions' : 'Show fewer versions';
      return;
    }
  });

  document.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') {
      const t = e.target.closest && e.target.closest('[data-toggle-next]');
      if (t) { e.preventDefault(); toggleNext(t); return; }
    }
    if (e.key === '/' && !e.ctrlKey && !e.metaKey && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) {
      e.preventDefault();
      const s = document.querySelector('input[name="search"]');
      if (s) s.focus();
    }
  });

  document.addEventListener('htmx:afterRequest', (e) => {
    const xhr = e.detail.xhr;
    if (!xhr) return;
    let u;
    try { u = new URL(xhr.responseURL); } catch (_) { return; }
    if (u.pathname === '/packages-partial') {
      const p = u.searchParams;
      p.delete('');
      const q = p.toString();
      history.pushState(null, '', q ? '/?' + q : '/');
      return;
    }
    if (u.pathname === '/untagged-partial') {
      const pp = u.searchParams.get('page');
      const f = u.searchParams.get('filter');
      const s = u.searchParams.get('search');
      const a = u.searchParams.get('author');
      const so = u.searchParams.get('sort');
      const q2 = [];
      if (f) q2.push('filter=' + f);
      if (s) q2.push('search=' + encodeURIComponent(s));
      if (a) q2.push('author=' + encodeURIComponent(a));
      if (so && so !== 'active_installs') q2.push('sort=' + so);
      if (pp && pp !== '1') q2.push('page=' + pp);
      history.pushState(null, '', q2.length ? '/untagged?' + q2.join('&') : '/untagged');
    }
  });

  document.addEventListener('DOMContentLoaded', () => {
    initMobileNav();
    initTagRequestDialog();
    initLocalTimes();
    initTabFromQuery();
    initTOCSpy();
    initAuthorAutocomplete();
  });

  function initMobileNav() {
    const t = document.getElementById('nav-menu-toggle');
    const m = document.getElementById('nav-menu');
    if (!t || !m) return;
    const openIcon = '<path stroke-linecap="round" stroke-linejoin="round" d="M3.75 6.75h16.5M3.75 12h16.5m-16.5 5.25h16.5"/>';
    const closeIcon = '<path stroke-linecap="round" stroke-linejoin="round" d="M6 18 18 6M6 6l12 12"/>';
    function close() {
      m.classList.add('hidden');
      t.setAttribute('aria-expanded', 'false');
      t.querySelector('svg').innerHTML = openIcon;
    }
    t.addEventListener('click', () => {
      const open = m.classList.toggle('hidden');
      t.setAttribute('aria-expanded', !open);
      t.querySelector('svg').innerHTML = open ? openIcon : closeIcon;
    });
    document.addEventListener('click', (e) => {
      if (!m.classList.contains('hidden') && !m.contains(e.target) && !t.contains(e.target)) close();
    });
  }

  function initTagRequestDialog() {
    const d = document.getElementById('tag-request-dialog');
    if (!d) return;
    d.addEventListener('click', (e) => {
      const r = d.getBoundingClientRect();
      if (e.clientX < r.left || e.clientX > r.right || e.clientY < r.top || e.clientY > r.bottom) d.close();
    });
  }

  function initLocalTimes() {
    document.querySelectorAll('[data-time]').forEach((el) => {
      const raw = el.dataset.time;
      const d = new Date(raw.indexOf('T') > -1 || raw.indexOf('Z') > -1 ? raw : raw.replace(' ', 'T') + 'Z');
      if (isNaN(d)) return;
      el.textContent = d.toLocaleString(undefined, { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit' });
    });
  }

  function initTabFromQuery() {
    const container = document.querySelector('[data-tabs][data-tabs-url]');
    if (!container) return;
    const name = new URLSearchParams(location.search).get('tab');
    if (!name) return;
    const btn = container.querySelector('[data-tab="' + name + '"]');
    if (btn) activateTab(btn);
  }

  function initTOCSpy() {
    const toc = document.getElementById('toc-sidebar');
    if (!toc) return;
    const links = toc.querySelectorAll('a[href^="#"]');
    if (!links.length) return;
    const byId = {};
    const ids = [];
    links.forEach((a) => {
      const id = a.getAttribute('href').slice(1);
      byId[id] = a;
      ids.push(id);
    });
    function activate(id) {
      ids.forEach((i) => {
        byId[i].classList.remove('text-gray-900', 'font-medium');
        byId[i].classList.add('text-gray-500');
      });
      const a = byId[id];
      if (a) {
        a.classList.add('text-gray-900', 'font-medium');
        a.classList.remove('text-gray-500');
      }
    }
    const observer = new IntersectionObserver((entries) => {
      entries.forEach((e) => { if (e.isIntersecting) activate(e.target.id); });
    }, { rootMargin: '-80px 0px -70% 0px' });
    ids.forEach((id) => {
      const el = document.getElementById(id);
      if (el) observer.observe(el);
    });
  }

  function initAuthorAutocomplete() {
    const input = document.getElementById('author-input');
    const box = document.getElementById('author-suggestions');
    const clear = document.getElementById('author-clear');
    if (!input || !box || !clear) return;
    let timer = null;
    let selected = -1;
    let ctrl = null;
    function show(authors) {
      box.innerHTML = '';
      selected = -1;
      if (!authors.length) { box.classList.add('hidden'); return; }
      authors.forEach((a) => {
        const d = document.createElement('div');
        d.textContent = a;
        d.className = 'px-3 py-1.5 text-sm cursor-pointer hover:bg-gray-100';
        d.addEventListener('mousedown', (e) => {
          e.preventDefault();
          input.value = a;
          box.classList.add('hidden');
          clear.classList.remove('hidden');
          if (window.htmx) htmx.trigger(document.getElementById('untagged-form'), 'submit');
        });
        box.appendChild(d);
      });
      box.classList.remove('hidden');
    }
    function highlight(items) {
      items.forEach((el, i) => { el.classList.toggle('bg-gray-100', i === selected); });
    }
    input.addEventListener('input', () => {
      const v = input.value.trim();
      clear.classList.toggle('hidden', !v);
      if (v.length < 2) { box.classList.add('hidden'); return; }
      clearTimeout(timer);
      timer = setTimeout(() => {
        if (ctrl) ctrl.abort();
        ctrl = new AbortController();
        fetch('/untagged-authors?q=' + encodeURIComponent(v), { signal: ctrl.signal })
          .then((r) => r.json())
          .then(show)
          .catch(() => {});
      }, 200);
    });
    input.addEventListener('keydown', (e) => {
      const items = box.querySelectorAll('div');
      if (!items.length) return;
      if (e.key === 'ArrowDown') {
        e.preventDefault();
        selected = Math.min(selected + 1, items.length - 1);
        highlight(items);
        return;
      }
      if (e.key === 'ArrowUp') {
        e.preventDefault();
        selected = Math.max(selected - 1, 0);
        highlight(items);
        return;
      }
      if (e.key === 'Enter' && selected >= 0) {
        e.preventDefault();
        items[selected].dispatchEvent(new Event('mousedown'));
      }
    });
    input.addEventListener('blur', () => { setTimeout(() => { box.classList.add('hidden'); }, 150); });
  }
})();
