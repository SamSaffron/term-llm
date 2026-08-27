export function errorMessage(value: unknown): string {
  return value instanceof Error ? value.message : String(value);
}
