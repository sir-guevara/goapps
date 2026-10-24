const grid = document.querySelector("#bookmark-grid");
const filters = document.querySelector("#filters");
const searchInput = document.querySelector("#search");
const countBadge = document.querySelector("#bookmark-count");
const emptyState = document.querySelector("#empty-state");
const loginDialog = document.querySelector("#login-dialog");
const bookmarkDialog = document.querySelector("#bookmark-dialog");
const loginForm = document.querySelector("#login-form");
const bookmarkForm = document.querySelector("#bookmark-form");
const adminPanel = document.querySelector("#admin-panel");
const backdrop = document.querySelector("#panel-backdrop");

const iconPalettes = [
  ["#e9eee8", "#315a45"],
  ["#f4eae3", "#b26b47"],
  ["#e8edf3", "#536a88"],
  ["#f1eddf", "#8c7540"],
  ["#efe9f1", "#795d83"],
  ["#e4efee", "#407772"],
];

let bookmarks = [];
let activeCategory = "Everything";
let isAdmin = false;
let editingId = null;
let toastTimer;
let mediaRefreshTimer;
let collectionLoaded = false;

async function api(path, options = {}) {
  const response = await fetch(path, {
    ...options,
    headers: { "Content-Type": "application/json", ...options.headers },
  });
  if (response.status === 204) return null;
  const body = await response.json();
  if (response.status === 401 && path !== "/api/login") {
    isAdmin = false;
    closePanel();
    if (bookmarkDialog.open) bookmarkDialog.close();
    renderAdmin();
  }
  if (!response.ok) throw new Error(body.error || "Something went wrong. Please try again.");
  return body;
}

function showToast(message) {
  const toast = document.querySelector("#toast");
  toast.textContent = message;
  toast.classList.add("visible");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => toast.classList.remove("visible"), 2600);
}

function paletteFor(bookmark) {
  const total = [...bookmark.title].reduce((sum, character) => sum + character.charCodeAt(0), 0);
  return iconPalettes[total % iconPalettes.length];
}

function siteIcon(bookmark) {
  const icon = document.createElement("span");
  const [background, color] = paletteFor(bookmark);
  icon.className = "site-icon";
  icon.setAttribute("aria-hidden", "true");
  icon.style.setProperty("--icon-bg", background);
  icon.style.setProperty("--icon-color", color);
  icon.textContent = bookmark.title.trim().charAt(0).toUpperCase();
  if (bookmark.icon_url) {
    const image = document.createElement("img");
    image.alt = "";
    image.decoding = "async";
    image.addEventListener("load", () => icon.replaceChildren(image), { once: true });
    image.src = bookmark.icon_url;
  }
  return icon;
}

function sitePreview(bookmark) {
  const preview = document.createElement("div");
  preview.className = "card-preview";
  preview.setAttribute("aria-hidden", "true");
  const domain = document.createElement("span");
  domain.className = "preview-domain";
  try { domain.textContent = new URL(bookmark.url).hostname.replace(/^www\./, ""); }
  catch { domain.textContent = bookmark.title; }
  preview.append(domain);
  if (bookmark.preview_url) {
    const image = document.createElement("img");
    image.alt = "";
    image.loading = "lazy";
    image.decoding = "async";
    image.addEventListener("error", () => image.remove(), { once: true });
    image.src = bookmark.preview_url;
    preview.append(image);
  }
  return preview;
}

function renderFilters() {
  const categories = ["Everything", ...new Set(bookmarks.map((bookmark) => bookmark.category).sort((a, b) => a.localeCompare(b)))];
  if (!categories.includes(activeCategory)) activeCategory = "Everything";
  filters.replaceChildren(...categories.map((category) => {
    const button = document.createElement("button");
    button.className = `filter-chip${category === activeCategory ? " active" : ""}`;
    button.type = "button";
    button.textContent = category;
    button.setAttribute("aria-pressed", String(category === activeCategory));
    button.addEventListener("click", () => {
      activeCategory = category;
      renderFilters();
      renderBookmarks();
    });
    return button;
  }));
}

function renderBookmarks() {
  const query = searchInput.value.trim().toLocaleLowerCase();
  const visible = bookmarks.filter((bookmark) => {
    const matchesCategory = activeCategory === "Everything" || bookmark.category === activeCategory;
    const matchesSearch = `${bookmark.title} ${bookmark.description} ${bookmark.category}`.toLocaleLowerCase().includes(query);
    return matchesCategory && matchesSearch;
  });
  countBadge.textContent = String(bookmarks.length);
  emptyState.hidden = visible.length > 0;
  grid.hidden = visible.length === 0;
  grid.replaceChildren(...visible.map((bookmark) => {
    const card = document.createElement("a");
    const [background, color] = paletteFor(bookmark);
    card.className = "bookmark-card";
    card.href = bookmark.url;
    card.target = "_blank";
    card.rel = "noopener noreferrer";
    card.style.setProperty("--icon-bg", background);
    card.style.setProperty("--icon-color", color);

    const top = document.createElement("div");
    top.className = "card-top";
    const icon = siteIcon(bookmark);
    const category = document.createElement("span");
    category.className = "card-category";
    category.textContent = bookmark.category;
    top.append(icon, category);

    const nameRow = document.createElement("div");
    nameRow.className = "card-name-row";
    const name = document.createElement("span");
    name.className = "card-name";
    name.textContent = bookmark.title;
    const arrow = document.createElement("span");
    arrow.className = "card-arrow";
    arrow.setAttribute("aria-hidden", "true");
    arrow.textContent = "↗";
    nameRow.append(name, arrow);

    const description = document.createElement("span");
    description.className = "card-description";
    description.textContent = bookmark.description;
    card.append(sitePreview(bookmark), top, nameRow, description);
    card.addEventListener("click", () => {
      bookmark.clicks += 1;
      renderAdmin();
      fetch(`/api/bookmarks/${encodeURIComponent(bookmark.id)}/click`, { method: "POST", keepalive: true })
        .then((response) => {
          if (!response.ok) throw new Error("Could not update the click count.");
        })
        .catch((error) => {
          bookmark.clicks -= 1;
          renderAdmin();
          showToast(error.message);
        });
    });
    return card;
  }));
}

function renderAdmin() {
  const totalClicks = bookmarks.reduce((sum, bookmark) => sum + bookmark.clicks, 0);
  document.querySelector("#admin-total").textContent = String(bookmarks.length);
  document.querySelector("#admin-clicks").textContent = String(totalClicks);
  document.querySelector("#admin-categories").textContent = String(new Set(bookmarks.map((bookmark) => bookmark.category)).size);
  document.querySelector("#total-clicks").textContent = totalClicks.toLocaleString();
  document.querySelector("#admin-trigger").textContent = isAdmin ? "Manage apps  ↗" : "Admin sign in  ↗";
  document.querySelector("#manage-list").replaceChildren(...bookmarks.map((bookmark) => {
    const row = document.createElement("div");
    row.className = "manage-item";
    const icon = siteIcon(bookmark);
    const copy = document.createElement("div");
    copy.className = "manage-copy";
    const title = document.createElement("strong");
    title.textContent = bookmark.title;
    const meta = document.createElement("span");
    meta.textContent = `${bookmark.category} · ${bookmark.clicks} clicks`;
    copy.append(title, meta);
    const actions = document.createElement("div");
    actions.className = "manage-actions";
    const edit = document.createElement("button");
    edit.type = "button";
    edit.title = `Edit ${bookmark.title}`;
    edit.setAttribute("aria-label", `Edit ${bookmark.title}`);
    edit.textContent = "✎";
    edit.addEventListener("click", () => openBookmarkDialog(bookmark));
    const remove = document.createElement("button");
    remove.type = "button";
    remove.title = `Delete ${bookmark.title}`;
    remove.setAttribute("aria-label", `Delete ${bookmark.title}`);
    remove.textContent = "×";
    remove.addEventListener("click", () => deleteBookmark(bookmark));
    actions.append(edit, remove);
    row.append(icon, copy, actions);
    return row;
  }));
}

async function refreshBookmarks() {
  clearTimeout(mediaRefreshTimer);
  const next = await api("/api/bookmarks");
  if (!collectionLoaded || JSON.stringify(next) !== JSON.stringify(bookmarks)) {
    bookmarks = next;
    collectionLoaded = true;
    renderFilters();
    renderBookmarks();
    renderAdmin();
  }
  if (bookmarks.some((bookmark) => bookmark.media_pending)) {
    mediaRefreshTimer = setTimeout(() => refreshBookmarks().catch(() => {}), 3000);
  }
}

function openPanel() {
  adminPanel.hidden = false;
  backdrop.hidden = false;
}

function closePanel() {
  adminPanel.hidden = true;
  backdrop.hidden = true;
}

function openBookmarkDialog(bookmark = null) {
  editingId = bookmark?.id || null;
  bookmarkForm.reset();
  document.querySelector("#bookmark-dialog-title").textContent = editingId ? "Edit this bookmark." : "Add a bookmark.";
  document.querySelector("#bookmark-title").value = bookmark?.title || "";
  document.querySelector("#bookmark-url").value = bookmark?.url || "";
  document.querySelector("#bookmark-description").value = bookmark?.description || "";
  document.querySelector("#bookmark-category").value = bookmark?.category || "";
  document.querySelector("#bookmark-error").hidden = true;
  bookmarkDialog.showModal();
}

async function deleteBookmark(bookmark) {
  if (!confirm(`Remove “${bookmark.title}” from the collection?`)) return;
  try {
    await api(`/api/bookmarks/${encodeURIComponent(bookmark.id)}`, { method: "DELETE" });
    await refreshBookmarks();
    showToast("Bookmark removed.");
  } catch (error) {
    showToast(error.message);
  }
}

document.querySelector("#admin-trigger").addEventListener("click", () => {
  if (isAdmin) openPanel();
  else {
    document.querySelector("#login-error").hidden = true;
    loginForm.reset();
    loginDialog.showModal();
  }
});
document.querySelector("#close-admin").addEventListener("click", closePanel);
backdrop.addEventListener("click", closePanel);
document.querySelector("#add-bookmark").addEventListener("click", () => openBookmarkDialog());
document.querySelector("#sign-out").addEventListener("click", async () => {
  try {
    await api("/api/logout", { method: "POST" });
    isAdmin = false;
    closePanel();
    renderAdmin();
    showToast("You’re signed out.");
  } catch (error) {
    showToast(error.message);
  }
});
document.querySelectorAll("[data-close-dialog]").forEach((button) => {
  button.addEventListener("click", () => button.closest("dialog").close());
});
searchInput.addEventListener("input", renderBookmarks);
document.addEventListener("keydown", (event) => {
  if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "k") {
    event.preventDefault();
    if (loginDialog.open || bookmarkDialog.open || !adminPanel.hidden) return;
    searchInput.focus();
  }
});

loginForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const errorElement = document.querySelector("#login-error");
  errorElement.hidden = true;
  const submit = loginForm.querySelector('button[type="submit"]');
  submit.disabled = true;
  try {
    await api("/api/login", { method: "POST", body: JSON.stringify({
      username: loginForm.elements.username.value,
      password: loginForm.elements.password.value,
    }) });
    isAdmin = true;
    loginDialog.close();
    renderAdmin();
    openPanel();
    showToast("Welcome back.");
  } catch (error) {
    errorElement.textContent = error.message;
    errorElement.hidden = false;
  } finally {
    submit.disabled = false;
  }
});

bookmarkForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const errorElement = document.querySelector("#bookmark-error");
  errorElement.hidden = true;
  const submit = bookmarkForm.querySelector('button[type="submit"]');
  submit.disabled = true;
  const payload = {
    title: bookmarkForm.elements.title.value,
    url: bookmarkForm.elements.url.value,
    description: bookmarkForm.elements.description.value,
    category: bookmarkForm.elements.category.value,
  };
  try {
    await api(editingId ? `/api/bookmarks/${encodeURIComponent(editingId)}` : "/api/bookmarks", {
      method: editingId ? "PUT" : "POST",
      body: JSON.stringify(payload),
    });
    bookmarkDialog.close();
    await refreshBookmarks();
    showToast(editingId ? "Bookmark updated." : "Bookmark added.");
  } catch (error) {
    errorElement.textContent = error.message;
    errorElement.hidden = false;
  } finally {
    submit.disabled = false;
  }
});

async function initialize() {
  try {
    const [session] = await Promise.all([api("/api/session"), refreshBookmarks()]);
    isAdmin = session.admin;
    renderAdmin();
  } catch (error) {
    grid.hidden = true;
    emptyState.hidden = false;
    emptyState.querySelector("strong").textContent = "The collection couldn’t load.";
    emptyState.querySelector("span:last-child").textContent = error.message;
  }
}

initialize();
