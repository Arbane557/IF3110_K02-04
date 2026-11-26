(() => {
    const routes = [
        { match: /^\/login$/, view: "login.html" },
        { match: /^\/databases$/, view: "databases.html" },
        { match: /^\/migrations$/, view: "migrations.html" },
        { match: /^\/auth$/, view: "users.html" },
        { match: /^\/groups$/, view: "groups.html" },
        { match: /^\/groups\/\d+$/, view: "group.html" },
    ];
    const viewCache = new Map();
    let navigation = 0;

    function routeFor(pathname) {
        return routes.find(route => route.match.test(pathname));
    }

    function csrfToken() {
        const cookie = document.cookie.split("; ").find(item => item.startsWith("csrf_token="));
        return cookie ? decodeURIComponent(cookie.split("=").slice(1).join("=")) : "";
    }

    async function loadView(name) {
        if (!viewCache.has(name)) {
            viewCache.set(name, fetch(`/views/${name}`, { credentials: "same-origin" }).then(async response => {
                if (!response.ok) throw new Error(`Could not load view (${response.status})`);
                return response.text();
            }));
        }
        return viewCache.get(name);
    }

    function executeControllers(viewDocument) {
        for (const source of viewDocument.body.querySelectorAll("script:not([src])")) {
            try {
                Function(source.textContent)();
            } catch (error) {
                console.error("Could not initialize SPA view", error);
                showError(error.message);
            }
        }
    }

    function showError(message) {
        const target = document.querySelector(".error, .status");
        if (target) target.textContent = message;
    }

    async function render(pathname, options = {}) {
        const route = routeFor(pathname);
        if (!route) {
            history.replaceState({}, "", "/databases");
            return render("/databases", { replace: true });
        }

        const currentNavigation = ++navigation;
        document.documentElement.setAttribute("aria-busy", "true");
        try {
            let markup = await loadView(route.view);
            if (currentNavigation !== navigation) return;
            markup = markup.replaceAll("{{.CSRFToken}}", csrfToken());
            if (route.view === "login.html") {
                markup = markup.replace(/\{\{if \.Error\}\}[\s\S]*?\{\{end\}\}/, '<p class="error" role="alert"></p>');
            }
            const viewDocument = new DOMParser().parseFromString(markup, "text/html");
            const nextMain = viewDocument.querySelector("main");
            if (!nextMain) throw new Error("View has no main element");

            if (options.push) history.pushState({}, "", pathname);
            else if (options.replace) history.replaceState({}, "", pathname);
            document.title = viewDocument.title || "Database Management Platform";
            document.querySelector("main").replaceWith(document.importNode(nextMain, true));
            document.body.dataset.route = pathname;
            document.querySelectorAll(".topbar a[href]").forEach(link => {
                const active = link.getAttribute("href") === pathname || (pathname.startsWith("/groups/") && link.getAttribute("href") === "/groups");
                if (active) link.setAttribute("aria-current", "page");
            });
            window.dispatchEvent(new CustomEvent("spa:navigated", { detail: { pathname } }));
            executeControllers(viewDocument);
            window.scrollTo({ top: 0, behavior: "instant" });
        } catch (error) {
            showError(error.message);
        } finally {
            document.documentElement.removeAttribute("aria-busy");
        }
    }

    async function submitSessionForm(form) {
        const response = await fetch(form.action, {
            method: "POST",
            body: new URLSearchParams(new FormData(form)),
            credentials: "same-origin",
            headers: { "X-SPA-Navigation": "true" },
        });
        let payload = {};
        try { payload = await response.json(); } catch (_) {}
        if (!response.ok) throw new Error(payload.error || `Request failed (${response.status})`);
        await render(payload.redirect || "/databases", { push: true });
    }

    document.addEventListener("click", event => {
        if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
        const link = event.target.closest("a[href]");
        if (!link || link.target || link.download || link.hasAttribute("data-no-spa")) return;
        const url = new URL(link.href, location.href);
        if (url.origin !== location.origin || !routeFor(url.pathname)) return;
        event.preventDefault();
        render(url.pathname, { push: true });
    });

    document.addEventListener("submit", event => {
        const form = event.target;
        if (!(form instanceof HTMLFormElement)) return;
        const path = new URL(form.action, location.href).pathname;
        if (path !== "/login" && path !== "/logout") return;
        event.preventDefault();
        submitSessionForm(form).catch(error => showError(error.message));
    });

    window.addEventListener("popstate", () => render(location.pathname));
    window.__spaNavigate = pathname => render(pathname, { push: true });
    render(location.pathname, { replace: true });
})();

