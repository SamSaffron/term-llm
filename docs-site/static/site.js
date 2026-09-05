(() => {
  const root = document.documentElement;
  const themeSelect = document.getElementById("theme-select");
  const colorPreference = window.matchMedia("(prefers-color-scheme: dark)");

  function syncThemeImages() {
    const dark = root.dataset.theme === "dark" || (!root.dataset.theme && colorPreference.matches);
    document.querySelectorAll("[data-theme-image]").forEach((source) => {
      source.media = dark ? "all" : "not all";
      const link = source.closest("a");
      if (link) link.href = dark ? source.srcset : source.parentElement.querySelector("img").src;
    });
  }
  if (themeSelect) {
    themeSelect.closest("label").hidden = false;
    themeSelect.value = root.dataset.theme || "system";
    themeSelect.addEventListener("change", () => {
      const theme = themeSelect.value;
      if (theme === "system") delete root.dataset.theme;
      else root.dataset.theme = theme;
      try {
        if (theme === "system") localStorage.removeItem("term-llm-docs-theme");
        else localStorage.setItem("term-llm-docs-theme", theme);
      } catch (_) {}
      syncThemeImages();
    });
  }
  colorPreference.addEventListener("change", syncThemeImages);
  syncThemeImages();

  const docsNavigation = document.querySelector(".docs-navigation");
  const narrow = window.matchMedia("(max-width: 760px)");
  const syncNavigation = () => { if (docsNavigation) docsNavigation.open = !narrow.matches; };
  syncNavigation();
  narrow.addEventListener("change", syncNavigation);
  const currentDoc = docsNavigation?.querySelector('[aria-current="page"]');
  const sidebar = document.querySelector(".docs-sidebar");
  if (currentDoc && sidebar && !narrow.matches) {
    const offset = currentDoc.getBoundingClientRect().top - sidebar.getBoundingClientRect().top;
    if (offset > sidebar.clientHeight - 60) sidebar.scrollTop = offset - sidebar.clientHeight / 3;
  }
  document.querySelectorAll(".mobile-menu a").forEach((link) => {
    link.addEventListener("click", () => { link.closest("details").open = false; });
  });

  // Keep non-JS installs useful: both commands are visible until tabs are enhanced.
  document.querySelectorAll("[data-install-options]").forEach((options) => {
    const tablist = options.querySelector('[role="tablist"]');
    const tabs = [...tablist.querySelectorAll('[role="tab"]')];
    function activate(tab, focus = false) {
      tabs.forEach((item) => {
        const selected = item === tab;
        item.setAttribute("aria-selected", String(selected));
        item.tabIndex = selected ? 0 : -1;
        const panel = document.getElementById(item.getAttribute("aria-controls"));
        panel.hidden = !selected;
        panel.setAttribute("role", "tabpanel");
        panel.setAttribute("aria-labelledby", item.id);
        panel.tabIndex = 0;
        panel.querySelector(".install-option-label").hidden = true;
      });
      if (focus) tab.focus();
    }
    tablist.hidden = false;
    activate(tabs[0]);
    tabs.forEach((tab, index) => {
      tab.addEventListener("click", () => activate(tab));
      tab.addEventListener("keydown", (event) => {
        let next;
        if (event.key === "ArrowRight") next = (index + 1) % tabs.length;
        if (event.key === "ArrowLeft") next = (index - 1 + tabs.length) % tabs.length;
        if (event.key === "Home") next = 0;
        if (event.key === "End") next = tabs.length - 1;
        if (next === undefined) return;
        event.preventDefault();
        activate(tabs[next], true);
      });
    });
  });

  const copyStatus = document.getElementById("copy-status");
  document.querySelectorAll("[data-copy-text]").forEach((button) => {
    button.hidden = false;
    let reset;
    button.addEventListener("click", async () => {
      clearTimeout(reset);
      copyStatus.textContent = "";
      try {
        await navigator.clipboard.writeText(button.dataset.copyText);
        button.textContent = "Copied";
        copyStatus.textContent = "Copied to clipboard.";
      } catch (_) {
        button.textContent = "Select text";
        const block = button.closest(".command-block, .code-example");
        const code = block?.querySelector("pre code, pre");
        if (code) {
          const range = document.createRange();
          range.selectNodeContents(code);
          const selection = window.getSelection();
          selection.removeAllRanges();
          selection.addRange(range);
        }
        copyStatus.textContent = "Clipboard unavailable. The example is selected; use your browser’s copy command.";
      }
      reset = window.setTimeout(() => { button.textContent = "Copy"; }, 2000);
    });
  });

  // Pagefind is loaded only when needed, and a failed request can be retried.
  let pagefindLoad;
  function loadPagefind() {
    if (typeof window.PagefindUI === "function") return Promise.resolve();
    if (pagefindLoad) return pagefindLoad;
    if (!document.querySelector("[data-pagefind-css]")) {
      const css = document.createElement("link");
      css.rel = "stylesheet";
      css.href = "/pagefind/pagefind-ui.css";
      css.dataset.pagefindCss = "true";
      // The site theme must follow Pagefind's default styles in cascade order.
      document.head.insertBefore(css, document.querySelector('link[href="/styles.css"]'));
    }
    pagefindLoad = new Promise((resolve, reject) => {
      const script = document.createElement("script");
      script.src = "/pagefind/pagefind-ui.js";
      script.onload = resolve;
      script.onerror = () => { script.remove(); reject(new Error("Search unavailable")); };
      document.head.appendChild(script);
    }).catch((error) => { pagefindLoad = null; throw error; });
    return pagefindLoad;
  }
  const initialized = new WeakSet();
  const initializing = new WeakMap();
  function initializeSearch(element) {
    if (initialized.has(element)) return Promise.resolve();
    if (initializing.has(element)) return initializing.get(element);
    const task = loadPagefind().then(() => {
      element.replaceChildren();
      new window.PagefindUI({
        element: `#${element.id}`,
        showSubResults: true,
        resetStyles: false,
        excerptLength: 20,
        translations: { placeholder: "Search commands, guides, and concepts…" },
      });
      initialized.add(element);
    }).catch(() => {
      element.replaceChildren();
      const message = document.createElement("p");
      message.textContent = "Search could not load. Browse the documentation navigation, or ";
      const retry = document.createElement("button");
      retry.type = "button";
      retry.className = "quiet-button";
      retry.textContent = "Try again";
      retry.addEventListener("click", () => initializeSearch(element));
      message.append(retry);
      element.append(message);
    }).finally(() => initializing.delete(element));
    initializing.set(element, task);
    return task;
  }

  const dialog = document.getElementById("search-modal");
  const modalUI = document.getElementById("search-modal-ui");
  let previousFocus;
  async function openSearch() {
    if (dialog.open) return;
    if (typeof dialog.showModal !== "function") { window.location.href = "/search/"; return; }
    previousFocus = document.activeElement;
    dialog.showModal();
    document.body.classList.add("search-open");
    await initializeSearch(modalUI);
    if (dialog.open) modalUI.querySelector("input, button")?.focus();
  }
  document.querySelectorAll("[data-open-search]").forEach((trigger) => {
    trigger.addEventListener("click", (event) => { event.preventDefault(); openSearch(); });
  });
  document.querySelector("[data-close-search]").addEventListener("click", () => dialog.close());
  dialog.addEventListener("close", () => {
    document.body.classList.remove("search-open");
    previousFocus?.focus();
  });
  dialog.addEventListener("click", (event) => {
    const bounds = dialog.getBoundingClientRect();
    if (event.target === dialog && (event.clientX < bounds.left || event.clientX > bounds.right || event.clientY < bounds.top || event.clientY > bounds.bottom)) dialog.close();
  });
  const apple = /Mac|iPhone|iPad/.test(navigator.platform);
  document.querySelectorAll(".search-trigger kbd").forEach((kbd) => { kbd.textContent = apple ? "⌘ K" : "Ctrl K"; });
  document.addEventListener("keydown", (event) => {
    const target = document.activeElement;
    const typing = target?.matches("input, textarea, select") || target?.isContentEditable;
    if (((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") || (event.key === "/" && !typing && !event.metaKey && !event.ctrlKey && !event.altKey)) {
      event.preventDefault();
      openSearch();
    }
    if (event.key === "Escape") document.querySelectorAll(".mobile-menu[open]").forEach((menu) => { menu.open = false; });
  });
  const searchPage = document.getElementById("search-page-ui");
  if (searchPage) initializeSearch(searchPage);

  function revealFragment() {
    let target;
    try { target = document.getElementById(decodeURIComponent(location.hash.slice(1))); } catch (_) { return; }
    if (!target) return;
    const details = target.closest("details");
    if (details && !details.open) {
      details.open = true;
      target.scrollIntoView();
    }
  }
  revealFragment();
  window.addEventListener("hashchange", revealFragment);

  // Show the current article section without changing browser history.
  const tocLinks = [...document.querySelectorAll(".page-toc a")];
  if (tocLinks.length && "IntersectionObserver" in window) {
    const observer = new IntersectionObserver((entries) => {
      const visible = entries.filter((entry) => entry.isIntersecting).sort((a, b) => a.boundingClientRect.top - b.boundingClientRect.top);
      if (!visible.length) return;
      tocLinks.forEach((link) => {
        if (decodeURIComponent(link.hash.slice(1)) === visible[0].target.id) link.setAttribute("aria-current", "location");
        else link.removeAttribute("aria-current");
      });
    }, { rootMargin: "-90px 0px -65% 0px" });
    document.querySelectorAll(".markdown h2[id], .markdown h3[id]").forEach((heading) => observer.observe(heading));
  }
})();
