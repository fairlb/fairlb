(() => {
  let theme = "system";
  try {
    theme = localStorage.getItem("flb-theme") || "system";
  } catch {}
  const dark =
    theme === "dark" || (theme === "system" && matchMedia("(prefers-color-scheme: dark)").matches);
  document.documentElement.dataset.mode = dark ? "dark" : "light";
})();
