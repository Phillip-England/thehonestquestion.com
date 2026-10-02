(() => {
  const key = 'the-honest-question-theme';
  let saved;
  try { saved = localStorage.getItem(key) || localStorage.getItem('breathing-room-theme'); } catch (_) {}
  const preference = window.matchMedia('(prefers-color-scheme: dark)');
  const current = () => saved === 'dark' || (saved !== 'light' && preference.matches) ? 'dark' : 'light';
  const apply = () => {
    const theme = current();
    document.documentElement.dataset.theme = theme;
    document.documentElement.style.colorScheme = theme;
    document.querySelectorAll('.theme-toggle').forEach(button => {
      button.textContent = theme === 'dark' ? 'Light mode' : 'Dark mode';
      button.setAttribute('aria-label', `Switch to ${theme === 'dark' ? 'light' : 'dark'} mode`);
    });
    document.querySelectorAll('meta[name="theme-color"]').forEach(meta => {
      meta.content = theme === 'dark' ? '#141719' : '#ffffff';
    });
  };
  apply();
  document.addEventListener('DOMContentLoaded', () => {
    apply();
    document.querySelectorAll('.theme-toggle').forEach(button => button.addEventListener('click', () => {
      saved = current() === 'dark' ? 'light' : 'dark';
      try { localStorage.setItem(key, saved); } catch (_) {}
      apply();
    }));
  });
  preference.addEventListener('change', () => { if (!saved) apply(); });
})();
