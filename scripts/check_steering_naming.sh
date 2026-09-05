#!/usr/bin/env bash
# Audit live steering vocabulary; historical/wire exceptions are deliberately
# path-and-pattern scoped. Include new files, ignore already removed paths.
set -euo pipefail
python3 - <<'PY'
import pathlib, re, subprocess, sys
allowed = {
 'plan.md': r'.*',
 'docs/steering.md': r'.*',
 'scripts/check_steering_naming.sh': r'.*',
 'cmd/serve_steering_wire.go': r'interjection|interject',
 'cmd/serve_steering_test.go': r'interjection|interject',
 'cmd/serve_handlers.go': r'"interjections/"',
 'internal/session/sqlite.go': r'session_pending_interjections|persist pending session interjections|idx_session_pending_interjections_order',
 'internal/session/steering_migration_test.go': r'session_pending_interjections',
 'frontend/src/domain/steering.test.ts': r'interjection|interject',
 'internal/session/steering_schema.go': r'session_pending_interjections|idx_session_pending_interjections_order',
 'internal/llm/grok_acp.go': r'xai-interjection-core|x\.ai/interject',
 'frontend/src/api/steering.ts': r'/interjections/|result\.interjection_id',
 'frontend/src/domain/steering.ts': r'interjection|interject',
 'frontend/src/stores/app-store.test.ts': r'pending_interjection:.*legacy-pending',
 'frontend/payload-baseline.json': r'"app-interject\.js"',
}
paths = subprocess.check_output(['git','ls-files','-z','--cached','--others','--exclude-standard']).decode().split('\0')
errors=[]
for name in sorted(set(paths)):
 p=pathlib.Path(name)
 if not p.is_file(): continue
 if re.search('interject',name,re.I): errors.append(f'{name}: obsolete filename')
 try: lines=p.read_text().splitlines()
 except (UnicodeError,OSError): continue
 for number,line in enumerate(lines,1):
  if re.search('interject',line,re.I) and not re.search(allowed.get(name,r'(?!)'),line):
   errors.append(f'{name}:{number}: {line.strip()}')
if errors: print('\n'.join(errors)); sys.exit(1)
print('Steering naming audit passed')
PY
