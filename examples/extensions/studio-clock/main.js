export function activate(ui) {
  const clock = document.createElement('time');
  clock.className = 'studio-clock';
  clock.title = 'Studio clock · local time';
  ui.mount.append(clock);
  const update = () => {
    const now = new Date();
    clock.dateTime = now.toISOString();
    clock.textContent = '◷ ' + now.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
  };
  update();
  setInterval(update, 30_000);
}
