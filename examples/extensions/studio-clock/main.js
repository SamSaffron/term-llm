export function activate(ui) {
  const clock = document.createElement('time');
  clock.className = 'studio-clock';
  clock.title = 'Studio clock · local time';
  // ui.icon draws the app's own clock icon, or an icon extension's replacement.
  const label = document.createElement('span');
  clock.append(ui.icon('clock'), label);
  ui.mount.append(clock);
  const update = () => {
    const now = new Date();
    clock.dateTime = now.toISOString();
    label.textContent = now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  };
  update();
  setInterval(update, 30_000);
}
