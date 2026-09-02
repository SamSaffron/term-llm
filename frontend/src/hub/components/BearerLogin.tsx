import type { HubConfig } from '../config';

export function BearerLogin({ config }: { config: HubConfig }) {
  return (
    <div class="hub-bearer">
      <main class="auth-card">
        <h1>term-llm Hub</h1>
        <p>
          Enter your hub access token to continue. I’ll store it in an HTTP-only cookie on this
          host.
        </p>
        {config.invalidToken && (
          <div class="error" role="alert">
            That hub token was not accepted.
          </div>
        )}
        <form method="get" action={config.formAction}>
          <label for="hub-token">Hub token</label>
          <input
            id="hub-token"
            name="token"
            type="password"
            autoComplete="current-password"
            autoFocus
            required
          />
          <button type="submit">Connect to Hub</button>
        </form>
      </main>
    </div>
  );
}
