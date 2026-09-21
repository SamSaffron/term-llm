// Print one free loopback TCP port for the smoke scripts. The port is released
// immediately, so callers must bind it promptly.
import { createServer } from 'node:net';

const server = createServer();
server.on('error', (error) => {
  console.error(`free_port: ${error.message}`);
  process.exit(1);
});
server.listen(0, '127.0.0.1', () => {
  const { port } = server.address();
  server.close(() => console.log(port));
});
