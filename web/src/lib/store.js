// web/src/lib/store.js — app-level state (D-WEB-6): Solid signals + tiny stores,
// no external state library. Theme: dark by DEFAULT (the user requirement);
// the choice persists in localStorage and syncs the .dark class on <html>.

import { createSignal } from "solid-js";

const THEME_KEY = "walhub-theme";

function initialTheme() {
  try {
    const saved = localStorage.getItem(THEME_KEY);
    if (saved === "light" || saved === "dark") return saved;
  } catch { /* storage unavailable — dark stays the default */ }
  return "dark";
}

const [getTheme, setTheme] = createSignal(initialTheme());

export function theme() {
  return getTheme();
}

export function toggleTheme() {
  const next = getTheme() === "dark" ? "light" : "dark";
  setTheme(next);
  applyTheme(next);
  try { localStorage.setItem(THEME_KEY, next); } catch { /* non-fatal */ }
  return next;
}

function applyTheme(t) {
  document.documentElement.classList.toggle("dark", t === "dark");
}

// Sync the class once at startup (the shipped default is dark).
applyTheme(getTheme());
